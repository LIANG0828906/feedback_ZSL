package engine

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"sync"

	"dialoguefit/checkpoint"
	envcalc "dialoguefit/envelope"
	fitcalc "dialoguefit/fit"
	"dialoguefit/model"
	"dialoguefit/sched"
	"dialoguefit/state"
)

var ErrSimulatedKill = errors.New("simulated checkpoint kill")

type Engine struct {
	Project *model.Project
	Truth   *model.TruthFile
	Store   *checkpoint.Store
	Pool    *sched.Pool
	Cache   *Cache
	mu      sync.Mutex
	opMu    sync.Mutex

	Results  map[string]*model.DialogueResult
	States   map[string]*state.Machine
	Failures map[string]*model.Error

	InjectedFailures  map[string]bool
	CrashAfterSegment int

	truthByID map[string]model.TruthDialogue
}

func New(project *model.Project, truth *model.TruthFile, store *checkpoint.Store, pool *sched.Pool) *Engine {
	if pool == nil {
		pool = sched.New(sched.DefaultConfig())
	}
	e := &Engine{
		Project: project, Truth: truth, Store: store, Pool: pool, Cache: NewCache(),
		Results: map[string]*model.DialogueResult{}, States: map[string]*state.Machine{},
		Failures: map[string]*model.Error{}, InjectedFailures: map[string]bool{},
		truthByID: map[string]model.TruthDialogue{},
	}
	if truth != nil {
		for _, td := range truth.Dialogues {
			e.truthByID[td.DialogueID] = td
		}
	}
	for _, d := range project.Dialogues {
		e.States[d.ID] = state.New(d.ID)
	}
	return e
}

func (e *Engine) TruthFingerprint(dialogueID string) string {
	if td, ok := e.truthByID[dialogueID]; ok {
		return td.Fingerprint
	}
	return ""
}

func (e *Engine) DialogueOrder() []string {
	out := make([]string, 0, len(e.Project.Dialogues))
	for _, d := range e.Project.Dialogues {
		out = append(out, d.ID)
	}
	return out
}

func (e *Engine) SnapshotResults() []*model.DialogueResult {
	out := make([]*model.DialogueResult, 0, len(e.Project.Dialogues))
	for _, d := range e.Project.Dialogues {
		if r, ok := e.Results[d.ID]; ok {
			out = append(out, r)
		}
	}
	return out
}

func (e *Engine) CommitSequence() []string {
	out := make([]string, 0)
	for _, r := range e.SnapshotResults() {
		out = append(out, r.CommitSequence...)
	}
	return out
}

type buildOutput struct {
	result *model.DialogueResult
	report *model.InvalidationReport
}

func (e *Engine) Build(d *model.Dialogue, old *model.DialogueResult) (*model.DialogueResult, *model.InvalidationReport, error) {
	contentHash := model.ContentHash(*d)
	paramVersion := model.ParamVersion(e.Project.Envelope, d.Params)
	envNodeVersion := model.SHA256JSON(struct {
		Algorithm string               `json:"algorithm"`
		Config    model.EnvelopeConfig `json:"config"`
	}{envcalc.AlgorithmVersion, e.Project.Envelope})
	stressNodeVersion := model.SHA256JSON(struct {
		Algorithm string `json:"algorithm"`
		Tolerance int    `json:"tolerance"`
	}{envcalc.AlgorithmVersion, d.Params.StressToleranceSamples})
	fitNodeVersion := model.SHA256JSON(struct {
		Algorithm string          `json:"algorithm"`
		Params    model.FitParams `json:"params"`
	}{fitcalc.AlgorithmVersion, d.Params})

	var frames []model.Frame
	if old != nil && old.ContentHash == contentHash && len(old.Frames) > 0 {
		frames = append([]model.Frame(nil), old.Frames...)
	} else {
		var err error
		frames, err = envcalc.Extract(*d, e.Project.Envelope)
		if err != nil {
			return nil, nil, err
		}
	}

	segments, err := fitcalc.Allocate(*d)
	if err != nil {
		return nil, nil, err
	}

	var rawMarks []model.StressMark
	toleranceChanged := old != nil && old.Params.StressToleranceSamples != d.Params.StressToleranceSamples
	if old != nil && old.ContentHash == contentHash && !toleranceChanged && len(old.Stress) > 0 {
		for _, m := range old.Stress {
			m.AlignedSample = m.Sample
			rawMarks = append(rawMarks, m)
		}
	} else {
		rawMarks, err = envcalc.LocateStress(*d, frames)
		if err != nil {
			return nil, nil, err
		}
	}
	if td, ok := e.truthByID[d.ID]; ok {
		truthStress := map[int]int{}
		for _, ts := range td.Stress {
			truthStress[ts.Index] = ts.Sample
		}
		for _, mark := range rawMarks {
			if truthSample, exists := truthStress[mark.Index]; exists {
				distance := truthSample - mark.Sample
				if distance < 0 {
					distance = -distance
				}
				if distance > d.Params.StressToleranceSamples {
					return nil, nil, model.FitError(d.ID, mark.Index, "stress.sample",
						"stress sample=%d truth=%d distance=%d tolerance=%d",
						mark.Sample, truthSample, distance, d.Params.StressToleranceSamples)
				}
			}
		}
	}
	alignedMarks := envcalc.ApplyStretchAlignment(rawMarks, d.Phonemes, segments)

	n := len(d.Phonemes)
	affected := make([]bool, n)
	alignedChanged := make([]bool, n)
	stressChanged := make([]bool, n)
	fitChanged := make([]bool, n)
	entriesByIndex := make([][]model.InvalidationEntry, n)

	report := &model.InvalidationReport{Entries: []model.InvalidationEntry{}}
	if old == nil {
		for i, ph := range d.Phonemes {
			affected[i] = true
			entriesByIndex[i] = append(entriesByIndex[i], model.InvalidationEntry{
				DialogueID: d.ID, PhonemeIndex: i, PhonemeID: ph.ID,
				Reason: "cache-miss", Node: "phoneme-result",
			})
		}
	} else {
		oldStress := map[int]model.StressMark{}
		for _, m := range old.Stress {
			oldStress[m.Index] = m
		}
		for i, ph := range d.Phonemes {
			if old.ContentHash != contentHash {
				affected[i] = true
				entriesByIndex[i] = append(entriesByIndex[i], model.InvalidationEntry{
					DialogueID: d.ID, PhonemeIndex: i, PhonemeID: ph.ID,
					Reason: "content-hash", Node: "phoneme-result",
				})
			}
			if toleranceChanged && ph.Category == model.CategoryVowel {
				affected[i] = true
				stressChanged[i] = true
			}
			if i < len(old.Segments) && old.Segments[i] != segments[i] {
				affected[i] = true
				fitChanged[i] = true
			}
			if oldMark, ok := oldStress[i]; ok {
				for _, nm := range alignedMarks {
					if nm.Index == i && oldMark.AlignedSample != nm.AlignedSample {
						affected[i] = true
						alignedChanged[i] = true
					}
				}
			}
		}
	}

	type keySet struct {
		env       string
		stress    string
		coef      string
		rnge      string
		aligned   string
		composite string
		upstream  []string
		nodes     map[string]string
	}
	keySets := make([]keySet, n)
	previousRangeKey := ""
	for i, ph := range d.Phonemes {
		envKey := model.NodeCacheKey(contentHash, ph.ID, envNodeVersion, "envelope")
		coefKey := model.NodeCacheKey(contentHash, ph.ID, fitNodeVersion, "stretch-coefficient")
		rangeDeps := []string{coefKey}
		if previousRangeKey != "" {
			rangeDeps = append(rangeDeps, previousRangeKey)
		}
		rangeKey := model.NodeCacheKey(contentHash, ph.ID, fitNodeVersion, "stretch-range", rangeDeps...)
		previousRangeKey = rangeKey
		nodes := map[string]string{
			"envelope":            envKey,
			"stretch_coefficient": coefKey,
			"stretch_range":       rangeKey,
		}
		upstream := []string{envKey, coefKey, rangeKey}
		ks := keySet{env: envKey, coef: coefKey, rnge: rangeKey, nodes: nodes, upstream: upstream}
		if ph.Category == model.CategoryVowel {
			stressKey := model.NodeCacheKey(contentHash, ph.ID, stressNodeVersion, "stress", envKey)
			alignedKey := model.NodeCacheKey(contentHash, ph.ID, stressNodeVersion, "aligned-stress", stressKey, rangeKey)
			ks.stress = stressKey
			ks.aligned = alignedKey
			ks.upstream = append(ks.upstream, stressKey, alignedKey)
			ks.nodes["stress"] = stressKey
			ks.nodes["aligned_stress"] = alignedKey
		}
		ks.composite = model.NodeCacheKey(contentHash, ph.ID, paramVersion, "phoneme-result", ks.upstream...)
		keySets[i] = ks
	}

	phonemeResults := make([]model.PhonemeResult, n)
	recomputed := 0
	for i, ph := range d.Phonemes {
		if affected[i] {
			recomputed++
		}
		if !affected[i] && old != nil && i < len(old.PhonemeResults) {
			phonemeResults[i] = old.PhonemeResults[i]
			continue
		}

		keys := keySets[i]
		pr := model.PhonemeResult{
			DialogueID: d.ID, Index: i, PhonemeID: ph.ID,
			Category: ph.Category,
			Frames:   envcalc.FramesForPhoneme(frames, ph),
			Segment:  &segments[i], CacheKey: keys.composite, Upstream: keys.upstream, Nodes: keys.nodes,
		}
		for mi := range alignedMarks {
			if alignedMarks[mi].Index == i {
				mark := alignedMarks[mi]
				pr.Stress = &mark
			}
		}
		e.Cache.Put(pr)
		phonemeResults[i] = pr
	}

	for i, ph := range d.Phonemes {
		if alignedChanged[i] {
			entriesByIndex[i] = append(entriesByIndex[i], model.InvalidationEntry{
				DialogueID: d.ID, PhonemeIndex: i, PhonemeID: ph.ID,
				Reason: "aligned-stress", Node: "aligned-stress",
			})
		}
		if stressChanged[i] {
			entriesByIndex[i] = append(entriesByIndex[i], model.InvalidationEntry{
				DialogueID: d.ID, PhonemeIndex: i, PhonemeID: ph.ID,
				Reason: "stress-tolerance-parameter", Node: "stress",
			})
		}
		if fitChanged[i] {
			entriesByIndex[i] = append(entriesByIndex[i], model.InvalidationEntry{
				DialogueID: d.ID, PhonemeIndex: i, PhonemeID: ph.ID,
				Reason: "fit-boundary", Node: "fit",
			})
		}
	}
	for i := 0; i < n; i++ {
		report.Entries = append(report.Entries, entriesByIndex[i]...)
	}
	report.RecomputedPhonemes = recomputed
	report.ReusedPhonemes = n - recomputed

	commitSequence := make([]string, n)
	stressOut := make([]model.StressMark, 0, len(alignedMarks))
	for i, ph := range d.Phonemes {
		commitSequence[i] = d.ID + "/" + ph.ID
		if phonemeResults[i].Stress != nil {
			stressOut = append(stressOut, *phonemeResults[i].Stress)
		}
	}

	result := &model.DialogueResult{
		DialogueID: d.ID, ContentHash: contentHash, ParamVersion: paramVersion,
		AlgorithmVersion: model.AlgorithmVersion, TruthFingerprint: e.TruthFingerprint(d.ID),
		Params: d.Params, Frames: frames, Stress: stressOut, Segments: segments,
		PhonemeResults: phonemeResults, CommitSequence: commitSequence, State: "ready",
	}
	return result, report, nil
}

func (e *Engine) RunAll(ctx context.Context) (*model.InvalidationReport, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	olds := make([]*model.DialogueResult, len(e.Project.Dialogues))
	tasks := make([]sched.Task, 0, len(e.Project.Dialogues))
	for i := range e.Project.Dialogues {
		d := &e.Project.Dialogues[i]
		if old, ok := e.Results[d.ID]; ok {
			olds[i] = old
		}
		if err := e.beginState(d.ID); err != nil {
			return nil, err
		}
		tasks = append(tasks, sched.Task{ID: d.ID, BufferSamples: d.PCM.SampleCount, Run: func(ctx context.Context) error {
			if e.InjectedFailures[d.ID] {
				derr := model.StateError(d.ID, "injected", "injected dialogue failure")
				e.markFailure(d.ID, derr)
				return derr
			}
			return nil
		}})
	}

	// Build is kept outside the tiny closure only to capture i/d safely. The
	// pool still bounds parallel dialogue work and buffer acquisition.
	outputs := make([]*model.DialogueResult, len(e.Project.Dialogues))
	reports := make([]*model.InvalidationReport, len(e.Project.Dialogues))
	buildTasks := make([]sched.Task, len(tasks))
	copy(buildTasks, tasks)
	for i := range e.Project.Dialogues {
		i := i
		d := &e.Project.Dialogues[i]
		old := olds[i]
		buildTasks[i].Run = func(ctx context.Context) error {
			if e.InjectedFailures[d.ID] {
				derr := model.StateError(d.ID, "injected", "injected dialogue failure")
				e.markFailure(d.ID, derr)
				return derr
			}
			result, report, err := e.Build(d, old)
			if err != nil {
				domainErr, _ := model.AsError(err)
				if domainErr == nil {
					domainErr = model.StateError(d.ID, "build", "%v", err)
				}
				e.markFailure(d.ID, domainErr)
				return err
			}
			outputs[i] = result
			reports[i] = report
			return nil
		}
	}

	results := e.Pool.Run(ctx, buildTasks)
	aggregate := &model.InvalidationReport{Entries: []model.InvalidationEntry{}}
	for i, taskResult := range results {
		d := &e.Project.Dialogues[i]
		if taskResult.Err != nil {
			if _, ok := e.Failures[d.ID]; !ok {
				derr, isDomain := model.AsError(taskResult.Err)
				if !isDomain {
					derr = model.StateError(d.ID, "scheduler", "%v", taskResult.Err)
				}
				e.markFailure(d.ID, derr)
			}
			continue
		}
		if outputs[i] == nil {
			continue
		}
		if e.Store != nil {
			if err := e.commitResult(i, outputs[i]); err != nil {
				if errors.Is(err, ErrSimulatedKill) {
					return aggregate, err
				}
				derr, _ := model.AsError(err)
				if derr == nil {
					derr = model.CheckpointError(d.ID, "commit", "%v", err)
				}
				e.markFailure(d.ID, derr)
				continue
			}
		}
		e.muLockResults(func() {
			e.Results[d.ID] = outputs[i]
			delete(e.Failures, d.ID)
		})
		if err := e.markCommitted(d.ID); err != nil {
			return aggregate, err
		}
		if reports[i] != nil {
			aggregate.Entries = append(aggregate.Entries, reports[i].Entries...)
			aggregate.RecomputedPhonemes += reports[i].RecomputedPhonemes
			aggregate.ReusedPhonemes += reports[i].ReusedPhonemes
		}
	}
	return aggregate, nil
}

func (e *Engine) ApplyEdit(edit model.Edit) (*model.InvalidationReport, error) {
	e.opMu.Lock()
	defer e.opMu.Unlock()
	idx := -1
	for i := range e.Project.Dialogues {
		if e.Project.Dialogues[i].ID == edit.DialogueID {
			idx = i
			break
		}
	}
	if idx < 0 {
		return nil, model.DecodeError(edit.DialogueID, "DialogueID", "edit target dialogue not found")
	}
	d := &e.Project.Dialogues[idx]
	if edit.TargetSamples != nil {
		d.Params.TargetSamples = *edit.TargetSamples
	}
	if edit.MaxStretch != nil {
		d.Params.MaxStretch = *edit.MaxStretch
	}
	if edit.MinSilenceSamples != nil {
		d.Params.MinSilenceSamples = *edit.MinSilenceSamples
	}
	if edit.StressToleranceSamples != nil {
		d.Params.StressToleranceSamples = *edit.StressToleranceSamples
	}
	if err := e.beginState(d.ID); err != nil {
		return nil, err
	}
	if e.InjectedFailures[d.ID] {
		derr := model.StateError(d.ID, "injected", "injected dialogue failure")
		e.markFailure(d.ID, derr)
		return nil, derr
	}
	old := e.Results[d.ID]
	result, report, err := e.Build(d, old)
	if err != nil {
		derr, ok := model.AsError(err)
		if !ok {
			derr = model.StateError(d.ID, "build", "%v", err)
		}
		e.markFailure(d.ID, derr)
		return report, err
	}
	if e.Store != nil {
		if err := e.commitResult(idx, result); err != nil {
			if !errors.Is(err, ErrSimulatedKill) {
				derr, _ := model.AsError(err)
				if derr == nil {
					derr = model.CheckpointError(d.ID, "commit", "%v", err)
				}
				e.markFailure(d.ID, derr)
			}
			return report, err
		}
	}
	e.muLockResults(func() {
		e.Results[d.ID] = result
		delete(e.Failures, d.ID)
	})
	if err := e.markCommitted(d.ID); err != nil {
		return report, err
	}
	report.ComputedDialogues = 1
	report.ReusedDialogues = len(e.Project.Dialogues) - 1
	return report, nil
}

func (e *Engine) RetryDialogue(ctx context.Context, dialogueID string) (*model.InvalidationReport, error) {
	e.opMu.Lock()
	defer e.opMu.Unlock()
	idx := -1
	for i := range e.Project.Dialogues {
		if e.Project.Dialogues[i].ID == dialogueID {
			idx = i
		}
	}
	if idx < 0 {
		return nil, model.DecodeError(dialogueID, "DialogueID", "retry target not found")
	}
	d := &e.Project.Dialogues[idx]
	e.muLockResults(func() {
		delete(e.InjectedFailures, dialogueID)
	})
	if err := e.beginState(dialogueID); err != nil {
		return nil, err
	}
	// A retry starts from the last known project parameters but deliberately
	// does not inherit the failed attempt's provisional node state.
	result, report, err := e.Build(d, nil)
	if err != nil {
		derr, _ := model.AsError(err)
		if derr == nil {
			derr = model.StateError(dialogueID, "retry", "%v", err)
		}
		e.markFailure(dialogueID, derr)
		return report, err
	}
	if e.Store != nil {
		if err := e.commitResult(idx, result); err != nil {
			e.markFailure(dialogueID, model.CheckpointError(dialogueID, "commit", "%v", err))
			return report, err
		}
	}
	e.muLockResults(func() {
		e.Results[dialogueID] = result
		delete(e.Failures, dialogueID)
	})
	if err := e.markCommitted(dialogueID); err != nil {
		return report, err
	}
	return report, nil
}

func (e *Engine) SetTruthFile(truth *model.TruthFile) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.Truth = truth
	e.truthByID = map[string]model.TruthDialogue{}
	if truth != nil {
		for _, td := range truth.Dialogues {
			e.truthByID[td.DialogueID] = td
		}
	}
	for id, r := range e.Results {
		r.TruthFingerprint = e.TruthFingerprint(id)
	}
}

// RecommitAll writes current results in canonical dialogue/phoneme order. It
// is used when the final truth manifest is regenerated after edit planning.
func (e *Engine) RecommitAll(ctx context.Context) error {
	if e.Store == nil {
		return nil
	}
	for i := range e.Project.Dialogues {
		d := &e.Project.Dialogues[i]
		r, ok := e.Results[d.ID]
		if !ok {
			continue
		}
		r.TruthFingerprint = e.TruthFingerprint(d.ID)
		if err := e.commitResult(i, r); err != nil {
			return err
		}
	}
	return nil
}

func (e *Engine) commitResult(dialogueIndex int, result *model.DialogueResult) error {
	d := e.Project.Dialogues[dialogueIndex]
	for i, pr := range result.PhonemeResults {
		digest := model.FrameDigest(pr.Frames)
		stressJSON := ""
		if pr.Stress != nil {
			stressJSON = checkpoint.JSONString(pr.Stress)
		}
		segmentJSON := ""
		if pr.Segment != nil {
			segmentJSON = checkpoint.JSONString(pr.Segment)
		}
		payload, err := json.Marshal(pr)
		if err != nil {
			return err
		}
		rec := checkpoint.SegmentRecord{
			DialogueID: d.ID, PhonemeIndex: i, PhonemeID: pr.PhonemeID,
			CommitSeq:   int64(dialogueIndex*1000 + i + 1),
			ContentHash: result.ContentHash, ParamVersion: result.ParamVersion,
			AlgorithmVersion: result.AlgorithmVersion, TruthFingerprint: result.TruthFingerprint,
			State: "committed", EnvelopeDigest: digest, StressJSON: stressJSON,
			SegmentJSON: segmentJSON, PayloadJSON: string(payload),
		}
		ctx := context.Background()
		if err := e.Store.CommitSegment(ctx, rec); err != nil {
			return err
		}
		if e.CrashAfterSegment > 0 && i+1 == e.CrashAfterSegment {
			e.CrashAfterSegment = 0
			return ErrSimulatedKill
		}
	}
	return nil
}

func (e *Engine) muLockResults(fn func()) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if fn != nil {
		fn()
	}
}

func (e *Engine) beginState(id string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	m := e.States[id]
	if m == nil {
		m = state.New(id)
		e.States[id] = m
	}
	if m.Current == state.Failed {
		return m.Retry()
	}
	if m.Current == state.Committed {
		return m.Transition(state.Running)
	}
	if m.Current == state.Pending {
		return m.Transition(state.Running)
	}
	return nil
}

func (e *Engine) markCommitted(id string) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	m := e.States[id]
	if m == nil {
		return model.StateError(id, "state", "missing state machine")
	}
	if m.Current == state.Committed {
		return nil
	}
	if m.Current != state.Running {
		return model.StateError(id, "state", "cannot commit from %s", m.Current)
	}
	return m.Transition(state.Committed)
}

func (e *Engine) markFailure(id string, err *model.Error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.Failures[id] = err
	if m := e.States[id]; m != nil && m.Current == state.Running {
		_ = m.Transition(state.Failed)
	}
}

func (e *Engine) State(id string) state.State {
	e.mu.Lock()
	defer e.mu.Unlock()
	if m, ok := e.States[id]; ok {
		return m.Current
	}
	return ""
}

func (e *Engine) SchedulerEvents() []model.QueueEvent {
	if e.Pool == nil {
		return nil
	}
	return e.Pool.Events()
}

func sortedFrameMap(byIndex map[int]model.Frame) []model.Frame {
	indices := make([]int, 0, len(byIndex))
	for idx := range byIndex {
		indices = append(indices, idx)
	}
	sort.Ints(indices)
	out := make([]model.Frame, 0, len(indices))
	for _, idx := range indices {
		out = append(out, byIndex[idx])
	}
	return out
}
