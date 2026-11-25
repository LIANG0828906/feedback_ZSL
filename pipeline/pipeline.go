package pipeline

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"sort"

	"lipsync/domain"
	"lipsync/merge"
	"lipsync/quant"
	"lipsync/schedule"
	"lipsync/stage"
	"lipsync/store"
)

type Mode string

const (
	ModeRealtime Mode = "realtime"
	ModeRecovery Mode = "recovery"
	ModeFull     Mode = "full"
)

func (m Mode) Valid() bool {
	return m == ModeRealtime || m == ModeRecovery || m == ModeFull
}

type Options struct {
	Mode             Mode
	TruthFingerprint string
	FailShard        map[int]error
	StopAfterCommits int
}

type Result struct {
	Mode             Mode                      `json:"mode"`
	Shards           []domain.ShardArtifact    `json:"shards"`
	Packages         []domain.ScheduledPackage `json:"packages"`
	Frames           []domain.FrameRecord      `json:"frames"`
	Events           []domain.DispatchEvent    `json:"events"`
	CommitSequence   []int64                   `json:"commit_sequence"`
	TruthFingerprint string                    `json:"truth_fingerprint"`
}

type ReuseReport struct {
	PackageIDs   []string                  `json:"package_ids"`
	Packages     []domain.ScheduledPackage `json:"packages"`
	Frames       []domain.FrameRecord      `json:"frames"`
	ShardIndexes []int                     `json:"shard_indexes"`
}

// Run executes the selected path. E_ORDER, E_CLASS, E_RATE, E_OVF, E_PARAM,
// E_BUDGET, E_STAGE continue to carry their stable domain code; only
// unexpected implementation errors are wrapped as E_STAGE.
func Run(ctx context.Context, p *domain.Project, st *store.Store, opts Options) (*Result, error) {
	if p == nil || st == nil {
		return nil, &domain.Error{Code: domain.CodeSchema, Message: "project and store are required"}
	}
	if !opts.Mode.Valid() {
		return nil, &domain.Error{
			Code: domain.CodeSchema, Field: "mode", Value: string(opts.Mode),
			Message: "unknown pipeline mode",
		}
	}
	if opts.TruthFingerprint == "" {
		return nil, &domain.Error{
			Code: domain.CodeSchema, Field: "truth_fingerprint",
			Message: "truth fingerprint is required",
		}
	}

	expected, err := ExpectedShards(p, opts.TruthFingerprint)
	if err != nil {
		return nil, err
	}
	existing := make(map[int]domain.ShardArtifact)
	if opts.Mode == ModeRecovery {
		if _, err := st.ValidateExpected(ctx, expected); err != nil {
			return nil, err
		}
		all, err := st.LoadAllShards(ctx)
		if err != nil {
			return nil, err
		}
		for _, a := range all {
			existing[a.ShardIndex] = a
		}
		if allPresent(expected, existing) {
			res := resultFromArtifacts(opts.Mode, existing, expected)
			if err := validateArtifactSchedule(p, res); err != nil {
				return res, err
			}
			return res, nil
		}
	}

	if err := schedule.ValidateProject(p); err != nil {
		return emptyResult(opts.Mode, opts.TruthFingerprint), annotate(err, 0, "")
	}
	dialogues := append([]domain.Dialogue(nil), p.Dialogues...)
	sort.Slice(dialogues, func(i, j int) bool {
		if dialogues[i].ShardIndex != dialogues[j].ShardIndex {
			return dialogues[i].ShardIndex < dialogues[j].ShardIndex
		}
		return dialogues[i].ID < dialogues[j].ID
	})

	machines := make(map[int]*stage.Machine, len(dialogues))
	var packages []domain.Package
	merged := make(map[int][]domain.Package)
	var firstShardErr error
	failed := make(map[int]bool)
	for _, d := range dialogues {
		if err := ctx.Err(); err != nil {
			return emptyResult(opts.Mode, opts.TruthFingerprint), canceled(err)
		}
		m := stage.New(d.ShardIndex, d.ID)
		machines[d.ShardIndex] = m
		if err := m.Transition(stage.Quantizing, 10); err != nil {
			return emptyResult(opts.Mode, opts.TruthFingerprint), err
		}
		if err := quantizeShard(p, d); err != nil {
			err = annotate(err, d.ShardIndex, d.ID)
			_ = m.Fail(20, err)
			failed[d.ShardIndex] = true
			if firstShardErr == nil {
				firstShardErr = err
			}
			continue
		}
		if injected := opts.FailShard[d.ShardIndex]; injected != nil {
			firstPhone := ""
			if len(d.Phonemes) > 0 {
				firstPhone = d.Phonemes[0].ID
			}
			err := &domain.Error{
				Code: domain.CodeStage, ShardIndex: d.ShardIndex, DialogueID: d.ID,
				PhonemeID: firstPhone, Field: "shard.stage",
				Message: "injected shard failure", Cause: injected,
			}
			_ = m.Fail(25, err)
			failed[d.ShardIndex] = true
			if firstShardErr == nil {
				firstShardErr = err
			}
			continue
		}
		if err := m.Transition(stage.Merging, 35); err != nil {
			return emptyResult(opts.Mode, opts.TruthFingerprint), err
		}
		pkgs, err := merge.Partition(d, p.Render)
		if err != nil {
			err = annotate(err, d.ShardIndex, d.ID)
			_ = m.Fail(45, err)
			failed[d.ShardIndex] = true
			if firstShardErr == nil {
				firstShardErr = err
			}
			continue
		}
		if err := m.Transition(stage.Dispatching, 60); err != nil {
			return emptyResult(opts.Mode, opts.TruthFingerprint), err
		}
		merged[d.ShardIndex] = pkgs
		packages = append(packages, pkgs...)
	}

	result := emptyResult(opts.Mode, opts.TruthFingerprint)
	sched, err := schedule.Schedule(p, packages)
	if err != nil {
		for idx, m := range machines {
			if !failed[idx] {
				_ = m.Fail(70, err)
			}
		}
		return result, annotate(err, 0, "")
	}

	artifacts := make(map[int]domain.ShardArtifact)
	for _, d := range dialogues {
		if failed[d.ShardIndex] {
			continue
		}
		artifact, err := makeArtifact(p, d, sched, opts.TruthFingerprint)
		if err != nil {
			_ = machines[d.ShardIndex].Fail(75, err)
			if firstShardErr == nil {
				firstShardErr = annotate(err, d.ShardIndex, d.ID)
			}
			continue
		}
		artifacts[d.ShardIndex] = artifact
	}

	commitCount := 0
	finalArtifacts := make(map[int]domain.ShardArtifact)
	for _, d := range dialogues {
		if failed[d.ShardIndex] {
			continue
		}
		if err := ctx.Err(); err != nil {
			_ = machines[d.ShardIndex].Fail(80, err)
			return resultFromArtifacts(opts.Mode, finalArtifacts, expected), canceled(err)
		}
		artifact, ok := artifacts[d.ShardIndex]
		if !ok {
			continue
		}
		if old, ok := existing[d.ShardIndex]; ok && sameArtifact(old, artifact) {
			artifact = old
			_ = machines[d.ShardIndex].Transition(stage.Committed, 100)
		} else {
			if ok {
				if err := st.DeleteShard(ctx, d.ShardIndex); err != nil {
					_ = machines[d.ShardIndex].Fail(85, err)
					if firstShardErr == nil {
						firstShardErr = annotate(err, d.ShardIndex, d.ID)
					}
					continue
				}
			}
			committed, err := st.CommitShard(ctx, artifact)
			if err != nil {
				_ = machines[d.ShardIndex].Fail(90, err)
				if firstShardErr == nil {
					firstShardErr = annotate(err, d.ShardIndex, d.ID)
				}
				continue
			}
			artifact = committed
			_ = machines[d.ShardIndex].Transition(stage.Committed, 100)
		}
		artifacts[d.ShardIndex] = artifact
		finalArtifacts[d.ShardIndex] = artifact
		commitCount++
		if opts.StopAfterCommits > 0 && commitCount >= opts.StopAfterCommits {
			return resultFromArtifacts(opts.Mode, finalArtifacts, expected),
				&domain.Error{Code: domain.CodeCanceled, Message: "stopped after requested committed shard count"}
		}
	}

	result = resultFromArtifacts(opts.Mode, finalArtifacts, expected)
	if firstShardErr != nil {
		return result, firstShardErr
	}
	return result, nil
}

func ExpectedShards(p *domain.Project, truthFingerprint string) ([]store.ExpectedShard, error) {
	if truthFingerprint == "" {
		return nil, &domain.Error{Code: domain.CodeSchema, Field: "truth_fingerprint"}
	}
	ds := append([]domain.Dialogue(nil), p.Dialogues...)
	sort.Slice(ds, func(i, j int) bool { return ds[i].ShardIndex < ds[j].ShardIndex })
	out := make([]store.ExpectedShard, 0, len(ds))
	for _, d := range ds {
		hash, err := domain.DialogueContentHash(p.ID, d)
		if err != nil {
			return nil, err
		}
		out = append(out, store.ExpectedShard{
			ShardIndex: d.ShardIndex, DialogueID: d.ID, ContentHash: hash,
			SourceVersion:    p.SourceVersion,
			QuantVersion:     domain.QuantizationVersion(d.PCM.SampleRate, p.Render),
			AlgorithmVersion: domain.AlgorithmVersion,
			TruthFingerprint: truthFingerprint,
		})
	}
	return out, nil
}

func quantizeShard(p *domain.Project, d domain.Dialogue) error {
	for _, ph := range d.Phonemes {
		basis := ph.SourceBasis
		if basis == "" {
			basis = "frame"
		}
		startSource := ph.SourceStart
		endSource := ph.SourceEnd
		if basis == "frame" && ph.SourceBasis == "" {
			startSource = ph.StartFrame
			endSource = ph.EndFrame
		}
		start, err := quant.ToFrame(startSource, basis, d.PCM.SampleRate,
			p.Render.TargetFPS, p.Render.Rounding, "phoneme.start")
		if err != nil {
			return &domain.Error{
				Code: domain.ErrorCode(err), PhonemeID: ph.ID, DialogueID: d.ID,
				ShardIndex: d.ShardIndex, Field: "phoneme.start", Cause: err,
			}
		}
		end, err := quant.ToFrame(endSource, basis, d.PCM.SampleRate,
			p.Render.TargetFPS, p.Render.Rounding, "phoneme.end")
		if err != nil {
			return &domain.Error{
				Code: domain.ErrorCode(err), PhonemeID: ph.ID, DialogueID: d.ID,
				ShardIndex: d.ShardIndex, Field: "phoneme.end", Cause: err,
			}
		}
		if start.Value != ph.StartFrame || end.Value != ph.EndFrame {
			return &domain.Error{
				Code: domain.CodeSchema, PhonemeID: ph.ID, DialogueID: d.ID,
				ShardIndex: d.ShardIndex, Field: "phoneme.interval",
				Expected: fmt.Sprintf("%d..%d", start.Value, end.Value),
				Actual:   fmt.Sprintf("%d..%d", ph.StartFrame, ph.EndFrame),
				Message:  "internal frame interval is not the declared source quantization",
			}
		}
		if ph.SampleEnd > 0 {
			ss, err := quant.SampleToFrame(ph.SampleStart, d.PCM.SampleRate,
				p.Render.TargetFPS, p.Render.Rounding, "phoneme.start_sample")
			if err != nil {
				return &domain.Error{
					Code: domain.ErrorCode(err), PhonemeID: ph.ID, DialogueID: d.ID,
					ShardIndex: d.ShardIndex, Field: "phoneme.start_sample", Cause: err,
				}
			}
			se, err := quant.SampleToFrame(ph.SampleEnd, d.PCM.SampleRate,
				p.Render.TargetFPS, p.Render.Rounding, "phoneme.end_sample")
			if err != nil {
				return &domain.Error{
					Code: domain.ErrorCode(err), PhonemeID: ph.ID, DialogueID: d.ID,
					ShardIndex: d.ShardIndex, Field: "phoneme.end_sample", Cause: err,
				}
			}
			if ss.Value != ph.StartFrame || se.Value != ph.EndFrame {
				return &domain.Error{
					Code: domain.CodeSchema, PhonemeID: ph.ID, DialogueID: d.ID,
					ShardIndex: d.ShardIndex, Field: "phoneme.sample_interval",
					Expected: fmt.Sprintf("%d..%d", ph.StartFrame, ph.EndFrame),
					Actual:   fmt.Sprintf("%d..%d", ss.Value, se.Value),
					Message:  "sample annotation no longer maps to the internal frame interval",
				}
			}
		}
		scale := ph.SourceWeightScale
		if scale == 0 {
			scale = p.WeightScale
		}
		w, err := quant.WeightToPermyriad(ph.SourceWeight, scale, p.Render.Rounding, "phoneme.weight")
		if err != nil {
			return &domain.Error{
				Code: domain.ErrorCode(err), PhonemeID: ph.ID, DialogueID: d.ID,
				ShardIndex: d.ShardIndex, Field: "phoneme.weight", Cause: err,
			}
		}
		if w.Value != ph.WeightPermyriad {
			return &domain.Error{
				Code: domain.CodeSchema, PhonemeID: ph.ID, DialogueID: d.ID,
				ShardIndex: d.ShardIndex, Field: "phoneme.weight",
				Expected: fmt.Sprintf("%d", w.Value), Actual: fmt.Sprintf("%d", ph.WeightPermyriad),
				Message: "internal weight is not the declared source quantization",
			}
		}
	}
	return nil
}

func makeArtifact(p *domain.Project, d domain.Dialogue, sched *domain.ScheduleResult,
	truthFingerprint string) (domain.ShardArtifact, error) {
	hash, err := domain.DialogueContentHash(p.ID, d)
	if err != nil {
		return domain.ShardArtifact{}, err
	}
	var pkgs []domain.ScheduledPackage
	for _, sp := range sched.Packages {
		if sp.ShardIndex == d.ShardIndex {
			pkgs = append(pkgs, sp)
		}
	}
	shardPackages := make(map[string]struct{}, len(pkgs))
	for _, pkg := range pkgs {
		shardPackages[pkg.ID] = struct{}{}
	}
	var frames []domain.FrameRecord
	for _, fr := range sched.Frames {
		var ids []string
		for _, id := range fr.PackageIDs {
			if _, ok := shardPackages[id]; ok {
				ids = append(ids, id)
			}
		}
		if len(ids) > 0 {
			sort.Strings(ids)
			frames = append(frames, domain.FrameRecord{Frame: fr.Frame, PackageIDs: ids})
		}
	}
	var events []domain.DispatchEvent
	for _, ev := range sched.Events {
		if ev.ShardIndex == d.ShardIndex {
			events = append(events, ev)
		}
	}
	return domain.ShardArtifact{
		ShardIndex: d.ShardIndex, DialogueID: d.ID, State: "committed",
		SourceVersion: p.SourceVersion, ContentHash: hash,
		QuantVersion:     domain.QuantizationVersion(d.PCM.SampleRate, p.Render),
		AlgorithmVersion: domain.AlgorithmVersion, TruthFingerprint: truthFingerprint,
		PackageSummary: merge.PackageSummary(pkgs), Packages: pkgs,
		Frames: frames, Events: events,
	}, nil
}

func resultFromArtifacts(mode Mode, artifacts map[int]domain.ShardArtifact,
	expected []store.ExpectedShard) *Result {
	res := emptyResult(mode, "")
	expectedSet := make(map[int]struct{}, len(expected))
	for _, e := range expected {
		expectedSet[e.ShardIndex] = struct{}{}
	}
	indexes := make([]int, 0, len(artifacts))
	for idx := range artifacts {
		if _, ok := expectedSet[idx]; ok {
			indexes = append(indexes, idx)
		}
	}
	sort.Ints(indexes)
	frameMap := make(map[int64][]string)
	for _, idx := range indexes {
		a := artifacts[idx]
		res.Shards = append(res.Shards, a)
		res.Packages = append(res.Packages, a.Packages...)
		res.Events = append(res.Events, a.Events...)
		res.CommitSequence = append(res.CommitSequence, a.CommitSeq)
		if a.TruthFingerprint != "" {
			res.TruthFingerprint = a.TruthFingerprint
		}
		for _, fr := range a.Frames {
			frameMap[fr.Frame] = append(frameMap[fr.Frame], fr.PackageIDs...)
		}
	}
	sort.Slice(res.Packages, func(i, j int) bool {
		a, b := res.Packages[i], res.Packages[j]
		if a.StartFrame != b.StartFrame {
			return a.StartFrame < b.StartFrame
		}
		if a.Channel != b.Channel {
			return a.Channel < b.Channel
		}
		if a.DialogueID != b.DialogueID {
			return a.DialogueID < b.DialogueID
		}
		return a.ID < b.ID
	})
	frames := make([]int64, 0, len(frameMap))
	for f := range frameMap {
		frames = append(frames, f)
	}
	sort.Slice(frames, func(i, j int) bool { return frames[i] < frames[j] })
	for _, f := range frames {
		ids := frameMap[f]
		sort.Strings(ids)
		res.Frames = append(res.Frames, domain.FrameRecord{Frame: f, PackageIDs: ids})
	}
	sort.Slice(res.CommitSequence, func(i, j int) bool { return res.CommitSequence[i] < res.CommitSequence[j] })
	return res
}

func validateArtifactSchedule(p *domain.Project, res *Result) error {
	sr := &domain.ScheduleResult{Packages: res.Packages, Frames: res.Frames, Events: res.Events}
	return schedule.ValidateSchedule(p.Render, sr)
}

func allPresent(expected []store.ExpectedShard, existing map[int]domain.ShardArtifact) bool {
	for _, e := range expected {
		if _, ok := existing[e.ShardIndex]; !ok {
			return false
		}
	}
	return true
}

func sameArtifact(a, b domain.ShardArtifact) bool {
	aa, bb := a, b
	aa.CommitSeq, bb.CommitSeq = 0, 0
	return reflect.DeepEqual(aa, bb)
}

func emptyResult(mode Mode, fingerprint string) *Result {
	return &Result{Mode: mode, TruthFingerprint: fingerprint}
}

func annotate(err error, shard int, dialogue string) error {
	if err == nil {
		return nil
	}
	if de, ok := domain.AsError(err); ok {
		cp := *de
		if cp.ShardIndex == 0 && shard != 0 {
			cp.ShardIndex = shard
		}
		if cp.ShardIndex == 0 && shard == 0 && cp.DialogueID == "" {
			cp.ShardIndex = shard
		}
		if cp.DialogueID == "" {
			cp.DialogueID = dialogue
		}
		return &cp
	}
	return &domain.Error{
		Code: domain.CodeStage, ShardIndex: shard, DialogueID: dialogue,
		Message: "unexpected shard failure", Cause: err,
	}
}

func canceled(cause error) error {
	return &domain.Error{
		Code: domain.CodeCanceled, Message: "pipeline canceled; only committed shards are reusable",
		Cause: cause,
	}
}

func IsCanceled(err error) bool {
	return domain.ErrorCode(err) == domain.CodeCanceled || errors.Is(err, context.Canceled)
}

// ReusePersisted loads only package results marked reusable by an invalidation
// plan. Callers can merge these package/frame records with recomputed invalid
// packages instead of rerunning the unchanged dispatch path.
func ReusePersisted(ctx context.Context, st *store.Store, inv *domain.Invalidation) (*ReuseReport, error) {
	if st == nil || inv == nil {
		return nil, &domain.Error{Code: domain.CodeParam, Message: "store and invalidation are required"}
	}
	allowed := make(map[string]struct{}, len(inv.ReusedPackages))
	for _, id := range inv.ReusedPackages {
		allowed[id] = struct{}{}
	}
	artifacts, err := st.LoadAllShards(ctx)
	if err != nil {
		return nil, err
	}
	report := &ReuseReport{}
	shardSet := make(map[int]struct{})
	for _, a := range artifacts {
		seenShard := false
		for _, p := range a.Packages {
			if _, ok := allowed[p.ID]; !ok {
				continue
			}
			report.Packages = append(report.Packages, p)
			report.PackageIDs = append(report.PackageIDs, p.ID)
			seenShard = true
		}
		for _, fr := range a.Frames {
			var ids []string
			for _, id := range fr.PackageIDs {
				if _, ok := allowed[id]; ok {
					ids = append(ids, id)
				}
			}
			if len(ids) > 0 {
				sort.Strings(ids)
				report.Frames = append(report.Frames, domain.FrameRecord{Frame: fr.Frame, PackageIDs: ids})
			}
		}
		if seenShard {
			shardSet[a.ShardIndex] = struct{}{}
		}
	}
	sort.Strings(report.PackageIDs)
	sort.Slice(report.Packages, func(i, j int) bool { return report.Packages[i].ID < report.Packages[j].ID })
	sort.Slice(report.Frames, func(i, j int) bool { return report.Frames[i].Frame < report.Frames[j].Frame })
	for idx := range shardSet {
		report.ShardIndexes = append(report.ShardIndexes, idx)
	}
	sort.Ints(report.ShardIndexes)
	return report, nil
}
