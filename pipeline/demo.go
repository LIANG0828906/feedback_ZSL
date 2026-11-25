package pipeline

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
)

// VersionReport is the demo view of one loaded schema version.
type VersionReport struct {
	Version           string     `json:"version"`
	Warnings          []string   `json:"warnings,omitempty"`
	FeaturesMatch     bool       `json:"features_match"`
	EventsMatch       bool       `json:"events_match"`
	WindowBoundsMatch bool       `json:"window_bounds_match"`
	WatermarkEnabled  bool       `json:"watermark_enabled"`
	WatermarkApplied  bool       `json:"watermark_applied"`
	Result            *RunResult `json:"result"`
}

// IncrementalReport summarizes an applied edit.
type IncrementalReport struct {
	EditSource          InvalidationSource   `json:"edit_source"`
	InvalidatedWindows  int                  `json:"invalidated_windows"`
	RecomputedWindows   int                  `json:"recomputed_windows"`
	ReusedWindows       int                  `json:"reused_windows"`
	MatchesFull         bool                 `json:"matches_full"`
	Mismatches          []string             `json:"mismatches,omitempty"`
	Edits               []EditClosure        `json:"edits"`
	WindowInvalidations []WindowInvalidation `json:"window_invalidations"`
}

// ResumeReport compares a restarted run with an uninterrupted run.
type ResumeReport struct {
	PartialCommittedSeq     int64  `json:"partial_committed_seq"`
	ResumedFromSeq          int64  `json:"resumed_from_seq"`
	FinalCommittedSeq       int64  `json:"final_committed_seq"`
	WindowsMatchFull        bool   `json:"windows_match_full"`
	DiscardsMatchFull       bool   `json:"discards_match_full"`
	VersionMismatchCode     string `json:"version_mismatch_code"`
	VersionFallback         bool   `json:"version_fallback"`
	TruncatedCheckpointCode string `json:"truncated_checkpoint_code,omitempty"`
}

// AnomalyOutcome is one injected failure with its stable code/context.
type AnomalyOutcome struct {
	Name      string            `json:"name"`
	Code      string            `json:"code"`
	Context   map[string]string `json:"context,omitempty"`
	Recovered bool              `json:"recovered,omitempty"`
	Note      string            `json:"note,omitempty"`
}

// DemoReport is deterministic and contains every acceptance surface.
type DemoReport struct {
	Seed        int64              `json:"seed"`
	Versions    []VersionReport    `json:"versions"`
	Incremental *IncrementalReport `json:"incremental"`
	Resume      *ResumeReport      `json:"resume"`
	Anomalies   []AnomalyOutcome   `json:"anomalies"`
}

// RunDemo executes the local end-to-end acceptance demonstration.
func RunDemo(ctx context.Context, seed int64) (*DemoReport, error) {
	sample, err := GenerateSample(seed)
	if err != nil {
		return nil, err
	}
	docs, err := sample.VersionDocuments()
	if err != nil {
		return nil, err
	}
	opts := DefaultRunOptions()
	opts.Workers = 4
	opts.WindowBufferPerFeature = 32
	opts.MaxWindowBufferPerFeature = 128
	opts.CommitEvery = 8

	report := &DemoReport{Seed: seed}
	var reference *Project
	loaded := map[string]*Project{}
	for _, version := range []string{"v1", "v2", "v3"} {
		doc, err := LoadJSON(docs[version])
		if err != nil {
			return nil, fmt.Errorf("load %s: %w", version, err)
		}
		loaded[version] = doc.Project
		result, err := Run(ctx, doc.Project, opts)
		if err != nil {
			return nil, fmt.Errorf("run %s: %w", version, err)
		}
		vr := VersionReport{
			Version: version, Warnings: doc.Warnings,
			WatermarkEnabled: doc.Project.WatermarkEnabled,
			WatermarkApplied: result.WatermarkApplied, Result: result,
		}
		if reference == nil {
			reference = doc.Project
			vr.FeaturesMatch, vr.EventsMatch, vr.WindowBoundsMatch = true, true, true
		} else {
			vr.FeaturesMatch = featuresEquivalent(reference.Features, doc.Project.Features)
			vr.EventsMatch = eventsEquivalent(reference.Events, doc.Project.Events)
			vr.WindowBoundsMatch = windowBoundsEquivalent(reference, doc.Project)
		}
		report.Versions = append(report.Versions, vr)
	}

	v2 := report.Versions[1]
	edited := loaded["v2"].Clone()
	var editedSeq int64
	for i := range edited.Events {
		if edited.Events[i].FeatureID == "F_TRAFFIC" && edited.Events[i].ObservedAtMs == edited.TimeOriginMs {
			edited.Events[i].Value += 17_000
			editedSeq = edited.Events[i].Seq
			break
		}
	}
	increment, err := ApplyIncrement(ctx, loaded["v2"], v2.Result, edited, opts)
	if err != nil {
		return nil, fmt.Errorf("incremental demo: %w", err)
	}
	source := InvalidationSource{
		Kind: "event_value", FeatureID: "F_TRAFFIC", EventSeq: editedSeq, ReasonCode: InvalidateEventValue,
	}
	report.Incremental = &IncrementalReport{
		EditSource:         source,
		InvalidatedWindows: len(increment.Invalidations),
		RecomputedWindows:  len(increment.RecomputedWindows),
		ReusedWindows:      len(increment.ReusedWindows),
		MatchesFull:        increment.MatchesFull, Mismatches: increment.Mismatches,
		Edits: increment.Edits, WindowInvalidations: increment.Invalidations,
	}

	resumeReport, err := runResumeDemo(ctx, docs["v2"], opts)
	if err != nil {
		return nil, err
	}
	report.Resume = resumeReport
	anomalies, err := runAnomalyDemo(ctx, docs["v2"], opts)
	if err != nil {
		return nil, err
	}
	report.Anomalies = anomalies
	return report, nil
}

type cancelAfterFirstCheckpoint struct {
	inner    CheckpointBackend
	cancel   context.CancelFunc
	saveCall int
}

func (c *cancelAfterFirstCheckpoint) Reset() error { return c.inner.Reset() }
func (c *cancelAfterFirstCheckpoint) Load(p *Project, f string) (*CheckpointSnapshot, ResumeInfo, error) {
	return c.inner.Load(p, f)
}
func (c *cancelAfterFirstCheckpoint) Save(s *CheckpointSnapshot) error {
	if err := c.inner.Save(s); err != nil {
		return err
	}
	c.saveCall++
	if c.saveCall == 1 {
		c.cancel()
	}
	return nil
}

func runResumeDemo(ctx context.Context, document []byte, opts RunOptions) (*ResumeReport, error) {
	project, err := LoadJSON(document)
	if err != nil {
		return nil, err
	}
	full, err := Run(ctx, project.Project, opts)
	if err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp("", "featurepipe-resume-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	store, err := OpenCheckpointStore(filepath.Join(dir, "resume.db"))
	if err != nil {
		return nil, err
	}
	defer store.Close()
	runCtx, cancel := context.WithCancel(ctx)
	backend := &cancelAfterFirstCheckpoint{inner: store, cancel: cancel}
	partial, err := Run(runCtx, project.Project, RunOptions{
		Workers: opts.Workers, MaxWorkers: opts.MaxWorkers,
		WindowBufferPerFeature:    opts.WindowBufferPerFeature,
		MaxWindowBufferPerFeature: opts.MaxWindowBufferPerFeature,
		RatioThreshold:            opts.RatioThreshold, CommitEvery: 5,
		Checkpoint: backend, Resume: true,
	})
	if ErrorCode(err) != CodeInterrupted {
		return nil, fmt.Errorf("resume demo expected E_INTERRUPTED, got %v", err)
	}
	resumed, err := Run(ctx, project.Project, RunOptions{
		Workers: opts.Workers, MaxWorkers: opts.MaxWorkers,
		WindowBufferPerFeature:    opts.WindowBufferPerFeature,
		MaxWindowBufferPerFeature: opts.MaxWindowBufferPerFeature,
		RatioThreshold:            opts.RatioThreshold, CommitEvery: 7,
		Checkpoint: store, Resume: true,
	})
	if err != nil {
		return nil, err
	}
	fullSemantic := cloneWindowsNoTraversal(full.Windows)
	resumedSemantic := cloneWindowsNoTraversal(resumed.Windows)
	report := &ResumeReport{
		PartialCommittedSeq: partial.LastCommittedSeq,
		ResumedFromSeq:      resumed.ResumedFromSeq, FinalCommittedSeq: resumed.LastCommittedSeq,
		WindowsMatchFull:  reflect.DeepEqual(fullSemantic, resumedSemantic),
		DiscardsMatchFull: reflect.DeepEqual(full.Discards, resumed.Discards),
	}
	if err := store.InjectVersionMismatch(); err != nil {
		return nil, err
	}
	_, info, err := store.Load(project.Project, mustFingerprintForDemo(project.Project))
	if err != nil {
		return nil, err
	}
	report.VersionMismatchCode = info.ReasonCode
	report.VersionFallback = info.FullRebuild
	truncStore, err := OpenCheckpointStore(filepath.Join(dir, "truncated.db"))
	if err != nil {
		return nil, err
	}
	defer truncStore.Close()
	if _, err := Run(ctx, project.Project, RunOptions{
		Workers: opts.Workers, CommitEvery: 5, Checkpoint: truncStore, Resume: true,
	}); err != nil {
		return nil, err
	}
	if err := truncStore.InjectTruncatedCheckpoint(); err != nil {
		return nil, err
	}
	_, _, err = truncStore.Load(project.Project, mustFingerprintForDemo(project.Project))
	if be, ok := AsBusinessError(err); ok {
		report.TruncatedCheckpointCode = be.Code
	} else {
		report.TruncatedCheckpointCode = ErrorCode(err)
	}
	return report, nil
}

func cloneWindowsNoTraversal(windows []WindowResult) []WindowResult {
	out := append([]WindowResult(nil), windows...)
	for i := range out {
		out[i].Traversal = ""
		out[i].PathReasonCode = ""
	}
	return out
}

func mustFingerprintForDemo(p *Project) string {
	fp, err := ProjectFingerprint(p)
	if err != nil {
		panic(err)
	}
	return fp
}

func runAnomalyDemo(ctx context.Context, document []byte, opts RunOptions) ([]AnomalyOutcome, error) {
	base, err := LoadJSON(document)
	if err != nil {
		return nil, err
	}
	project := base.Project
	var outcomes []AnomalyOutcome
	addRun := func(name string, mutate func(*Project), ro RunOptions) {
		bad := project.Clone()
		mutate(bad)
		_, err := Run(ctx, bad, ro)
		outcome := AnomalyOutcome{Name: name, Code: ErrorCode(err)}
		if be, ok := AsBusinessError(err); ok {
			outcome.Context = be.Context
		}
		outcomes = append(outcomes, outcome)
	}
	addRun("negative_window_width", func(p *Project) { p.Features[0].WindowWidthMs = -1 }, opts)
	addRun("zero_window_step", func(p *Project) { p.Features[0].WindowStepMs = 0 }, opts)
	addRun("aggregation_weight_out_of_range", func(p *Project) {
		p.Features[0].Aggregations[0].Weight = -1
	}, opts)
	addRun("watermark_before_committed_window", func(p *Project) {
		v := p.TimeOriginMs
		p.Events[len(p.Events)-1].WatermarkMs = &v
	}, opts)
	threadOpts := opts
	threadOpts.Workers = opts.MaxWorkers + 1
	addRun("thread_budget_exceeded", func(*Project) {}, threadOpts)
	bufferOpts := opts
	bufferOpts.WindowBufferPerFeature = opts.MaxWindowBufferPerFeature + 1
	addRun("buffer_budget_exceeded", func(*Project) {}, bufferOpts)
	backpressureOpts := opts
	backpressureOpts.Workers = 2
	backpressureOpts.WindowBufferPerFeature = 1
	backpressureOpts.MaxWindowBufferPerFeature = 2
	addRun("streaming_backpressure", func(*Project) {}, backpressureOpts)

	dir, err := os.MkdirTemp("", "featurepipe-anomaly-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	store, err := OpenCheckpointStore(filepath.Join(dir, "truncated.db"))
	if err != nil {
		return nil, err
	}
	defer store.Close()
	if _, err := Run(ctx, project, RunOptions{
		Workers: opts.Workers, CommitEvery: 5, Checkpoint: store, Resume: true,
	}); err != nil {
		return nil, err
	}
	if err := store.InjectTruncatedCheckpoint(); err != nil {
		return nil, err
	}
	_, _, err = store.Load(project, mustFingerprintForDemo(project))
	outcome := AnomalyOutcome{Name: "truncated_checkpoint", Code: ErrorCode(err)}
	if be, ok := AsBusinessError(err); ok {
		outcome.Context = be.Context
	}
	outcomes = append(outcomes, outcome)

	store2, err := OpenCheckpointStore(filepath.Join(dir, "version.db"))
	if err != nil {
		return nil, err
	}
	defer store2.Close()
	if _, err := Run(ctx, project, RunOptions{
		Workers: opts.Workers, CommitEvery: 5, Checkpoint: store2, Resume: true,
	}); err != nil {
		return nil, err
	}
	if err := store2.InjectVersionMismatch(); err != nil {
		return nil, err
	}
	_, info, err := store2.Load(project, mustFingerprintForDemo(project))
	if err != nil {
		return nil, err
	}
	outcomes = append(outcomes, AnomalyOutcome{
		Name: "checkpoint_version_mismatch", Code: info.ReasonCode,
		Recovered: info.FullRebuild,
		Note:      "checkpoint rejected; full rebuild from event 1 was requested",
	})
	return outcomes, nil
}

func featuresEquivalent(a, b []FeatureDefinition) bool {
	return reflect.DeepEqual(a, b)
}

func eventsEquivalent(a, b []ObservationEvent) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Seq != b[i].Seq || a[i].FeatureID != b[i].FeatureID ||
			a[i].ObservedAtMs != b[i].ObservedAtMs || a[i].Channel != b[i].Channel ||
			a[i].Value != b[i].Value {
			return false
		}
	}
	return true
}

func windowBoundsEquivalent(a, b *Project) bool {
	pa, err := WindowPlans(a)
	if err != nil {
		return false
	}
	pb, err := WindowPlans(b)
	if err != nil {
		return false
	}
	return reflect.DeepEqual(allPlanKeys(pa), allPlanKeys(pb))
}

// RenderDemoText produces a human-readable acceptance report including every
// window and every discard.
func RenderDemoText(report *DemoReport) string {
	var b strings.Builder
	fmt.Fprintf(&b, "featurepipe demo seed=%d\n", report.Seed)
	for _, version := range report.Versions {
		fmt.Fprintf(&b, "\n[%s] features=%t events=%t windows=%t watermark=%t/%t recompute=%d discards=%d\n",
			version.Version, version.FeaturesMatch, version.EventsMatch, version.WindowBoundsMatch,
			version.WatermarkEnabled, version.WatermarkApplied,
			version.Result.RecomputeTotal, len(version.Result.Discards))
		for _, warning := range version.Warnings {
			fmt.Fprintf(&b, "  warning: %s\n", warning)
		}
		for _, w := range version.Result.Windows {
			fmt.Fprintf(&b, "  %s [%d,%d) count=%d aggs=%s status=%s path=%s/%s\n",
				w.Key.FeatureID, w.Key.StartMs, w.Key.EndMs, w.ObservationCount,
				renderAggs(w.Aggregates), w.Status, w.Traversal, w.PathReasonCode)
		}
		for _, d := range version.Result.Discards {
			fmt.Fprintf(&b, "  DISCARD event=%d feature=%s window=%s late_ms=%d reason=%s\n",
				d.EventSeq, d.FeatureID, d.Window.String(), d.LateByMs, d.ReasonCode)
		}
	}
	if report.Incremental != nil {
		fmt.Fprintf(&b, "\n[incremental] source=%s/%s recomputed=%d reused=%d matches_full=%t\n",
			report.Incremental.EditSource.Kind, report.Incremental.EditSource.ReasonCode,
			report.Incremental.RecomputedWindows, report.Incremental.ReusedWindows,
			report.Incremental.MatchesFull)
		for _, edit := range report.Incremental.Edits {
			fmt.Fprintf(&b, "  edit=%s/%s initial=%d closure=%d\n",
				edit.Source.Kind, edit.Source.ReasonCode, len(edit.InitialWindows), len(edit.PropagationClosure))
		}
	}
	if report.Resume != nil {
		fmt.Fprintf(&b, "\n[resume] partial_seq=%d resumed_from=%d final_seq=%d windows_match=%t discards_match=%t\n",
			report.Resume.PartialCommittedSeq, report.Resume.ResumedFromSeq,
			report.Resume.FinalCommittedSeq, report.Resume.WindowsMatchFull, report.Resume.DiscardsMatchFull)
		fmt.Fprintf(&b, "  mismatch_code=%s fallback=%t truncated_code=%s\n",
			report.Resume.VersionMismatchCode, report.Resume.VersionFallback,
			report.Resume.TruncatedCheckpointCode)
	}
	fmt.Fprintf(&b, "\n[anomalies]\n")
	for _, a := range report.Anomalies {
		fmt.Fprintf(&b, "  %s -> %s %v\n", a.Name, a.Code, a.Context)
	}
	return b.String()
}

func renderAggs(aggs []AggregateResult) string {
	parts := make([]string, 0, len(aggs))
	for _, agg := range aggs {
		parts = append(parts, fmt.Sprintf("%s=%d", agg.Kind, agg.Value))
	}
	return "{" + strings.Join(parts, ",") + "}"
}
