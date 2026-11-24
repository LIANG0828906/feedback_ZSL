package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"mediaforge/internal/codec"
	"mediaforge/internal/domain"
	"mediaforge/internal/errs"
	"mediaforge/internal/mfxfmt"
	"mediaforge/internal/resource"
	"mediaforge/internal/scale"
	"mediaforge/internal/state"
	"mediaforge/internal/store"
)

type Mode string

const (
	ModeRealtime Mode = "realtime"
	ModeFull     Mode = "full"
	ModeResume   Mode = "resume"
)

type Options struct {
	Mode       Mode
	Workers    int
	OutDir     string
	PauseAfter int
	ForcePath  string
	Only       []string
}

type Event struct {
	Variant string      `json:"variant,omitempty"`
	State   state.State `json:"state,omitempty"`
	Message string      `json:"message"`
}

type VariantReport struct {
	ID               string      `json:"id"`
	Ordinal          int         `json:"ordinal"`
	State            state.State `json:"state"`
	Path             string      `json:"path,omitempty"`
	BlockRows        int         `json:"blockRows,omitempty"`
	DownsampleFactor int         `json:"downsampleFactor,omitempty"`
	DecisionReason   string      `json:"decisionReason,omitempty"`
	OutputSHA256     string      `json:"outputSha256,omitempty"`
	OutputSize       int64       `json:"outputSize,omitempty"`
	MetadataFields   []string    `json:"metadataFields,omitempty"`
	CommitSeq        int64       `json:"commitSeq,omitempty"`
	ErrorCode        string      `json:"errorCode,omitempty"`
	ErrorMessage     string      `json:"errorMessage,omitempty"`
	recipeJSON       string
	decisionJSON     string
	summaryJSON      string
	truthJSON        string
	recipeVersion    string
	inputHash        string
	bytes            []byte
}

type Report struct {
	ProjectID      string              `json:"projectId"`
	Mode           Mode                `json:"mode"`
	Scale          scale.BatchDecision `json:"scale"`
	Progress       store.Progress      `json:"progress"`
	Variants       []VariantReport     `json:"variants"`
	Ensure         store.EnsureResult  `json:"ensure"`
	Paused         bool                `json:"paused"`
	StartedLogical int64               `json:"-"`
	Finished       bool                `json:"finished"`
}

type Runner struct {
	Store       *store.Store
	Project     *domain.Project
	Resolved    *domain.ResolvedProject
	Fingerprint string
	ProjectDir  string
	Emit        func(Event)
	stallsMu    sync.Mutex
	stalls      []store.StallRecord
}

func New(st *store.Store, p *domain.Project, rp *domain.ResolvedProject, fingerprint, projectDir string) *Runner {
	return &Runner{Store: st, Project: p, Resolved: rp, Fingerprint: fingerprint, ProjectDir: projectDir}
}

func (r *Runner) emit(ev Event) {
	if r.Emit != nil {
		r.Emit(ev)
	}
}

type taskResult struct {
	ordinal  int
	id       string
	run      VariantReport
	err      error
	canceled bool
}

type job struct {
	ordinal int
	rec     store.VariantRecord
}

func (r *Runner) Run(ctx context.Context, opts Options) (*Report, error) {
	if opts.Mode == "" {
		opts.Mode = ModeRealtime
	}
	if opts.Mode != ModeRealtime && opts.Mode != ModeFull && opts.Mode != ModeResume {
		return nil, errs.Field(errs.EPara, "mode", "must be realtime, full or resume")
	}
	workers := opts.Workers
	if workers <= 0 {
		workers = r.Project.Budget.MaxWorkers
	}
	if workers > r.Project.Budget.MaxWorkers {
		workers = r.Project.Budget.MaxWorkers
	}
	if workers < 1 {
		workers = 1
	}
	batchScale := scale.Decide(r.Resolved)
	if opts.ForcePath != "" {
		if opts.ForcePath != codec.PathFine && opts.ForcePath != codec.PathAdaptive {
			return nil, errs.Field(errs.EPara, "forcePath", "must be fine or adaptive")
		}
		batchScale.Path = opts.ForcePath
		for id, d := range batchScale.Variants {
			d.Path = opts.ForcePath
			if opts.ForcePath == codec.PathAdaptive && d.BlockRows >= r.Resolved.Project.BaseRecipe.TargetHeight {
				d.BlockRows = maxInt(1, minInt(d.BlockRows, r.Resolved.Project.BaseRecipe.TargetHeight/2))
			}
			batchScale.Variants[id] = d
		}
	}
	ensure, err := r.Store.Ensure(ctx, r.Project, r.Resolved,
		r.Fingerprint, opts.Mode == ModeFull)
	if err != nil {
		return nil, err
	}
	records, err := r.Store.Variants(ctx)
	if err != nil {
		return nil, err
	}
	report := &Report{
		ProjectID: r.Project.ID, Mode: opts.Mode, Scale: batchScale,
		Ensure: ensure, Variants: make([]VariantReport, 0, len(records)),
	}
	byOrdinal := map[int]store.VariantRecord{}
	var pending []store.VariantRecord
	only := map[string]bool{}
	for _, id := range opts.Only {
		only[id] = true
	}
	for _, rec := range records {
		byOrdinal[rec.Ordinal] = rec
		report.Variants = append(report.Variants, reportFromRecord(rec))
		if rec.State == state.Pending {
			if len(only) == 0 || only[rec.ID] {
				pending = append(pending, rec)
			}
		}
	}
	progress, err := r.Store.Progress(ctx)
	if err != nil {
		return nil, err
	}
	report.Progress = progress
	if len(pending) == 0 {
		report.Finished = true
		return report, nil
	}
	gate := resource.New(r.Project.Budget.MaxPixelsPerWorker,
		r.Project.Budget.MaxSamplesPerWorker, workers)
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	jobs := make(chan job)
	results := make(chan taskResult, workers*2)
	inflight := make(chan struct{}, workers*2)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range jobs {
				results <- r.runOne(runCtx, j, batchScale, gate, opts)
			}
		}()
	}
	dispatchDone := make(chan struct{})
	go func() {
		defer close(dispatchDone)
		defer close(jobs)
		for _, rec := range pending {
			if rec.State != state.Pending {
				continue
			}
			select {
			case inflight <- struct{}{}:
			case <-runCtx.Done():
				return
			}
			select {
			case jobs <- job{ordinal: rec.Ordinal, rec: rec}:
			case <-runCtx.Done():
				return
			}
		}
	}()
	go func() {
		wg.Wait()
		close(results)
	}()
	ready := map[int]taskResult{}
	nextOrdinal := 0
	newCommits := 0
	processed := 0
	for result := range results {
		processed++
		ready[result.ordinal] = result
		for {
			rec, exists := byOrdinal[nextOrdinal]
			if !exists {
				break
			}
			if rec.State != state.Pending {
				nextOrdinal++
				continue
			}
			if len(only) > 0 && !only[rec.ID] {
				nextOrdinal++
				continue
			}
			res, ok := ready[nextOrdinal]
			if !ok {
				break
			}
			delete(ready, nextOrdinal)
			<-inflight
			if runCtx.Err() == nil && !res.canceled {
				if res.err == nil {
					seq, commitErr := r.commit(ctx, res)
					if commitErr != nil {
						res.err = commitErr
						res.run.State = state.Failed
						res.run.ErrorCode = string(errs.CodeOf(commitErr))
						res.run.ErrorMessage = commitErr.Error()
						_ = r.Store.Fail(context.Background(), res.id, state.Encoding,
							errs.CodeOf(commitErr), commitErr.Error())
					} else {
						res.run.CommitSeq = seq
						res.run.State = state.Committed
						newCommits++
						_ = r.export(opts.OutDir, &res)
					}
				}
			} else {
				res.run.State = state.Pending
			}
			res.run.bytes = nil
			r.replaceReport(report, res)
			nextOrdinal++
			if opts.PauseAfter > 0 && newCommits >= opts.PauseAfter && runCtx.Err() == nil {
				report.Paused = true
				cancel()
			}
		}
	}
	<-dispatchDone
	if report.Paused || runCtx.Err() != nil {
		_ = r.Store.CancelActive(context.Background())
		report.Paused = true
	}
	progress, _ = r.Store.Progress(ctx)
	report.Progress = progress
	report.Finished = !report.Paused && progress.Total == progress.Committed+progress.Failed
	if ctx.Err() != nil {
		return report, errs.New(errs.ECancel, "run interrupted; exit report is not guaranteed")
	}
	return report, nil
}

func (r *Runner) runOne(ctx context.Context, j job, bs scale.BatchDecision, gate *resource.Gate,
	opts Options) taskResult {
	res := taskResult{ordinal: j.ordinal, id: j.rec.ID, run: VariantReport{
		ID: j.rec.ID, Ordinal: j.ordinal, State: state.Decoding,
	}}
	if ctx.Err() != nil {
		res.canceled = true
		res.run.State = state.Pending
		return res
	}
	if err := r.Store.Transition(ctx, j.rec.ID, state.Decoding, "", ""); err != nil {
		res.err = err
		res.run.State = state.Failed
		res.run.ErrorCode = string(errs.CodeOf(err))
		res.run.ErrorMessage = err.Error()
		return res
	}
	r.emit(Event{Variant: j.rec.ID, State: state.Decoding, Message: "decoding and normalizing inputs"})
	v, _ := r.Resolved.Variant(j.rec.ID)
	decision := bs.Variants[j.rec.ID]
	img := r.Resolved.Sources[v.ImageSourceID]
	aud := r.Resolved.Sources[v.AudioSourceID]
	pixels := int64(img.Width) * int64(img.Height) * 4
	samples := aud.SamplesPerChannel * int64(aud.AudioChannels)
	lease, waited, err := gate.Acquire(ctx, pixels, samples)
	if err != nil {
		if ctx.Err() != nil {
			res.canceled = true
			res.run.State = state.Pending
			return res
		}
		r.fail(&res, err)
		return res
	}
	if waited {
		stall := store.StallRecord{
			Variant: j.rec.ID, Reason: "pixel/sample buffer exhausted; task blocked in queue",
			Bytes: pixels*2 + samples*4, At: time.Now().UTC().Format(time.RFC3339Nano),
		}
		r.stallsMu.Lock()
		r.stalls = append(r.stalls, stall)
		r.stallsMu.Unlock()
		_ = r.Store.RecordStall(context.Background(), stall)
		r.emit(Event{Variant: j.rec.ID, State: state.Decoding, Message: stall.Reason})
	}
	in := codec.Input{
		ImagePath:     codec.ResolveImagePath(r.projectDir(), img.Path),
		AudioPath:     codec.ResolveImagePath(r.projectDir(), aud.Path),
		ImageChannels: img.Channels, AudioChannels: aud.AudioChannels,
		ImageMetadata: img.Metadata, AudioMetadata: aud.Metadata,
	}
	out, err := codec.Transcode(in, v.Recipe, codec.Options{
		Path: decision.Path, BlockRows: decision.BlockRows,
		DownsampleFactor:    decision.DownsampleFactor,
		MaxPixelsPerWorker:  r.Project.Budget.MaxPixelsPerWorker,
		MaxSamplesPerWorker: r.Project.Budget.MaxSamplesPerWorker,
	})
	lease.Release()
	if err != nil {
		if ctx.Err() != nil {
			res.canceled = true
			res.run.State = state.Pending
			return res
		}
		r.fail(&res, err)
		return res
	}
	if ctx.Err() != nil {
		res.canceled = true
		res.run.State = state.Pending
		return res
	}
	if out.Preview != nil {
		decision.Preview = out.Preview
		decision.Reason += fmt.Sprintf("; preview=%dx%d factor=%dx sha256=%s",
			out.Preview.Width, out.Preview.Height, out.Preview.Downsample, out.Preview.SHA256[:12])
	}
	if decision.Path == codec.PathAdaptive && r.Project.Consistency.RequireByteIdentical {
		fine, fineErr := codec.Transcode(in, v.Recipe, codec.Options{Path: codec.PathFine, BlockRows: v.Recipe.TargetHeight})
		if fineErr != nil {
			r.fail(&res, errs.Wrap(errs.EScale, "fine-path consistency run failed", fineErr))
			return res
		}
		if _, cmpErr := codec.CompareProducts(fine.Bytes, out.Bytes, r.Project.Consistency); cmpErr != nil {
			r.fail(&res, cmpErr)
			return res
		}
	}
	if err := r.checkTruth(v.ID, out); err != nil {
		r.fail(&res, err)
		return res
	}
	if err := r.Store.Transition(ctx, j.rec.ID, state.Encoding, "", ""); err != nil {
		if ctx.Err() != nil {
			res.canceled = true
			res.run.State = state.Pending
			return res
		}
		r.fail(&res, err)
		return res
	}
	r.emit(Event{Variant: j.rec.ID, State: state.Encoding,
		Message: fmt.Sprintf("encoding %d bytes via %s path", len(out.Bytes), out.Path)})
	summary, _ := mfxfmt.Summary(out.Bytes)
	summaryJSON, _ := json.Marshal(summary)
	truth := r.truth(v.ID)
	truthJSON, _ := json.Marshal(truth)
	decisionJSON, _ := json.Marshal(decision)
	recipeJSON, _ := json.Marshal(v.Recipe)
	res.run = VariantReport{
		ID: j.rec.ID, Ordinal: j.ordinal, State: state.Encoding,
		Path: out.Path, BlockRows: out.BlockRows, DownsampleFactor: out.DownsampleFactor,
		DecisionReason: decision.Reason, OutputSHA256: out.SHA256, OutputSize: int64(len(out.Bytes)),
		MetadataFields: append([]string(nil), out.MetadataFields...),
	}
	res.run.recipeJSON = string(recipeJSON)
	res.run.decisionJSON = string(decisionJSON)
	res.run.summaryJSON = string(summaryJSON)
	res.run.truthJSON = string(truthJSON)
	res.run.recipeVersion = v.RecipeVersion
	res.run.inputHash = v.InputHash
	res.run.bytes = out.Bytes
	return res
}

func (r *Runner) fail(res *taskResult, err error) {
	app := errs.As(err)
	res.err = app
	res.run.State = state.Failed
	res.run.ErrorCode = string(app.Code)
	res.run.ErrorMessage = app.Error()
	target := state.Encoding
	_ = r.Store.Fail(context.Background(), res.id, target, app.Code, app.Error())
	r.emit(Event{Variant: res.id, State: state.Failed, Message: app.Error()})
}

func (r *Runner) commit(ctx context.Context, res taskResult) (int64, error) {
	return r.Store.Commit(ctx, store.CommitRecord{
		ID: res.id, Path: res.run.Path, BlockRows: res.run.BlockRows,
		DownsampleFactor: res.run.DownsampleFactor, DecisionJSON: res.run.decisionJSON,
		RecipeJSON: res.run.recipeJSON, RecipeVersion: res.run.recipeVersion,
		InputHash: res.run.inputHash, AlgorithmVersion: domain.AlgorithmVersion,
		OutputSHA256: res.run.OutputSHA256, OutputSize: res.run.OutputSize,
		MetadataFields: res.run.MetadataFields, SummaryJSON: res.run.summaryJSON,
		TruthJSON: res.run.truthJSON, OutputBlob: res.run.bytes,
	})
}

func (r *Runner) export(outDir string, res *taskResult) error {
	if outDir == "" {
		return nil
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return err
	}
	name := strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			return r
		}
		return '_'
	}, res.id)
	final := filepath.Join(outDir, name+".mfx")
	tmp := final + ".tmp"
	if err := os.WriteFile(tmp, res.run.bytes, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, final)
}

func (r *Runner) checkTruth(id string, out *codec.Result) error {
	t := r.truth(id)
	if out.Header.Width != t.Width {
		return truthDiff(id, "width", out.Header.Width, t.Width)
	}
	if out.Header.Height != t.Height {
		return truthDiff(id, "height", out.Header.Height, t.Height)
	}
	if out.Header.ImageChannels != t.Channels {
		return truthDiff(id, "channels", out.Header.ImageChannels, t.Channels)
	}
	if out.Header.AudioSamples != t.AudioSamples {
		return truthDiff(id, "audioSamples", out.Header.AudioSamples, t.AudioSamples)
	}
	if !equalStrings(out.MetadataFields, t.MetadataFields) {
		return truthDiff(id, "metadataFields", out.MetadataFields, t.MetadataFields)
	}
	if out.SHA256 != t.Fingerprint {
		return truthDiff(id, "fingerprint", out.SHA256, t.Fingerprint)
	}
	return nil
}

func truthDiff(variant, field string, actual, expected any) error {
	return errs.Diff(errs.Mismatch{
		Variant: variant, Artifact: "mfx", Field: field, Actual: actual, Expected: expected,
	}, "transcoded artifact differs from declared truth")
}

func (r *Runner) truth(id string) domain.TruthVariant {
	for _, t := range r.Project.Truth.Variants {
		if t.ID == id {
			return t
		}
	}
	return domain.TruthVariant{}
}

func (r *Runner) projectDir() string {
	if r.ProjectDir == "" {
		return "."
	}
	return r.ProjectDir
}

func (r *Runner) replaceReport(report *Report, res taskResult) {
	for i := range report.Variants {
		if report.Variants[i].Ordinal == res.ordinal {
			report.Variants[i] = res.run
			return
		}
	}
}

func reportFromRecord(rec store.VariantRecord) VariantReport {
	return VariantReport{
		ID: rec.ID, Ordinal: rec.Ordinal, State: rec.State, Path: rec.Path,
		BlockRows: rec.BlockRows, DownsampleFactor: rec.DownsampleFactor,
		OutputSHA256: rec.OutputSHA256, OutputSize: rec.OutputSize,
		MetadataFields: rec.MetadataFields, CommitSeq: rec.CommitSeq,
		ErrorCode: rec.ErrorCode, ErrorMessage: rec.ErrorMessage,
	}
}

func (r *Runner) Stalls() []store.StallRecord {
	r.stallsMu.Lock()
	defer r.stallsMu.Unlock()
	return append([]store.StallRecord(nil), r.stalls...)
}

func equalStrings(a, b []string) bool {
	aa := append([]string(nil), a...)
	bb := append([]string(nil), b...)
	sort.Strings(aa)
	sort.Strings(bb)
	if len(aa) != len(bb) {
		return false
	}
	for i := range aa {
		if aa[i] != bb[i] {
			return false
		}
	}
	return true
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
