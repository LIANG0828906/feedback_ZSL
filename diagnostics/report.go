// Package diagnostics provides the repeatable local acceptance demonstration.
package diagnostics

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"

	"dialogueaudio/audioerr"
	"dialogueaudio/codec"
	"dialogueaudio/engine"
	"dialogueaudio/fixed"
	"dialogueaudio/generator"
	"dialogueaudio/graph"
	"dialogueaudio/model"
	"dialogueaudio/pipeline"
	"dialogueaudio/state"
)

// SchemaLoad is a compact loader/conversion audit.
type SchemaLoad struct {
	Version           int    `json:"version"`
	ClipCount         int    `json:"clip_count"`
	ConversionTraces  int    `json:"conversion_traces"`
	Defaults          int    `json:"defaults"`
	TargetLoudnessQ16 int64  `json:"target_loudness_q16"`
	CoreMatchesSource bool   `json:"core_matches_source"`
	FirstClipID       string `json:"first_clip_id"`
}

// LossSummary summarizes E_LOSSY downgrade handling.
type LossSummary struct {
	Code     audioerr.Code    `json:"code"`
	Items    int              `json:"items"`
	Examples []codec.LossItem `json:"examples"`
}

// RoundTripCheck proves lower-schema export/load/re-export consistency.
type RoundTripCheck struct {
	FromVersion        int             `json:"from_version"`
	ViaVersion         int             `json:"via_version"`
	ToVersion          int             `json:"to_version"`
	Losses             LossSummary     `json:"losses"`
	AllBoundariesEqual bool            `json:"all_boundaries_equal"`
	AllGainsEqual      bool            `json:"all_gains_equal"`
	AllWeightsEqual    bool            `json:"all_weights_equal"`
	ResidualsStable    bool            `json:"residuals_stable"`
	Clips              []ClipRoundTrip `json:"clips"`
}

// ClipRoundTrip reports one clip's high-low-high conversion residuals.
type ClipRoundTrip struct {
	ClipID           string `json:"clip_id"`
	BoundaryEqual    bool   `json:"boundary_equal"`
	GainEqual        bool   `json:"gain_equal"`
	WeightEqual      bool   `json:"weight_equal"`
	BoundaryResidual int64  `json:"boundary_residual"`
	GainResidual     int64  `json:"gain_residual"`
	WeightResidual   int64  `json:"weight_residual"`
	InitialGainQ16   int64  `json:"initial_gain_q16"`
	RestoredGainQ16  int64  `json:"restored_gain_q16"`
}

// TruthReport records the generator oracle comparison.
type TruthReport struct {
	ToleranceQ16        int64                     `json:"tolerance_q16"`
	Comparison          generator.TruthComparison `json:"comparison"`
	TightToleranceQ16   int64                     `json:"tight_tolerance_q16"`
	TightComparison     generator.TruthComparison `json:"tight_comparison"`
	DeviationDidNotGrow bool                      `json:"deviation_did_not_grow"`
	SegmentsMatch       bool                      `json:"segments_match"`
	AdjustmentsMatch    bool                      `json:"adjustments_match"`
}

// PathReport proves the three run modes agree.
type PathReport struct {
	RealtimeProgress    state.Progress     `json:"realtime_progress"`
	ResumeFirstProgress state.Progress     `json:"resume_first_progress"`
	ResumeFinalProgress state.Progress     `json:"resume_final_progress"`
	FullProgress        state.Progress     `json:"full_progress"`
	CommitOrder         []string           `json:"commit_order"`
	RealtimeVsResume    *pipeline.Mismatch `json:"realtime_vs_resume,omitempty"`
	RealtimeVsFull      *pipeline.Mismatch `json:"realtime_vs_full,omitempty"`
	CancelCode          audioerr.Code      `json:"cancel_code,omitempty"`
	CancelProgress      state.Progress     `json:"cancel_progress"`
	ResumedFrom         int                `json:"resumed_from"`
	CheckpointRebuilt   bool               `json:"checkpoint_rebuilt"`
	RebuildCommitCount  int                `json:"rebuild_commit_count"`
}

// InvalidationCase records one designer edit and the actual cache hits.
type InvalidationCase struct {
	Name                  string             `json:"name"`
	Edit                  graph.Edit         `json:"edit"`
	Invalidation          graph.Invalidation `json:"invalidation"`
	RecomputedEnvelope    []string           `json:"recomputed_envelope_clips"`
	ReusedEnvelope        []string           `json:"reused_envelope_clips"`
	RecomputedCalibration []string           `json:"recomputed_calibration_clips"`
	ReusedCalibration     []string           `json:"reused_calibration_clips"`
	Complete              bool               `json:"complete"`
}

// ErrorRepro records a reproducible failure code.
type ErrorRepro struct {
	Code    audioerr.Code  `json:"code"`
	Field   string         `json:"field,omitempty"`
	Message string         `json:"message,omitempty"`
	OpA     int64          `json:"op_a,omitempty"`
	OpB     int64          `json:"op_b,omitempty"`
	Values  map[string]any `json:"values,omitempty"`
}

// ErrorReport groups every acceptance-visible error.
type ErrorReport struct {
	SchemaUnknown       ErrorRepro `json:"schema_unknown"`
	SchemaMissing       ErrorRepro `json:"schema_missing"`
	LoudnessUnreachable ErrorRepro `json:"loudness_unreachable"`
	StateIllegal        ErrorRepro `json:"state_illegal"`
	Overflow            ErrorRepro `json:"overflow"`
}

// QueueSummary is the bounded-budget observation.
type QueueSummary struct {
	MaxParallel         int                 `json:"max_parallel"`
	SampleBufferSamples int                 `json:"sample_buffer_samples"`
	QueueBudget         int                 `json:"queue_budget"`
	Events              []engine.QueueEvent `json:"events"`
}

// Report is written to report.json and stdout.
type Report struct {
	Format        string             `json:"format"`
	Seed          int64              `json:"seed"`
	ProjectID     string             `json:"project_id"`
	SampleDir     string             `json:"sample_dir"`
	ManifestValid bool               `json:"manifest_valid"`
	SchemaLoads   []SchemaLoad       `json:"schema_loads"`
	RoundTrip     RoundTripCheck     `json:"round_trip"`
	Truth         TruthReport        `json:"truth"`
	Paths         PathReport         `json:"paths"`
	Invalidation  []InvalidationCase `json:"invalidation"`
	Errors        ErrorReport        `json:"errors"`
	Queue         QueueSummary       `json:"queue"`
}

// Run performs the complete deterministic acceptance demonstration.
func Run(seed int64, outDir string, opts pipeline.Options) (*Report, error) {
	if err := prepareOutput(outDir); err != nil {
		return nil, err
	}
	sampleDir := filepath.Join(outDir, "samples")
	set, err := generator.Build(seed)
	if err != nil {
		return nil, err
	}
	if err := set.Write(sampleDir); err != nil {
		return nil, err
	}
	if err := generator.ValidateManifest(sampleDir); err != nil {
		return nil, err
	}
	report := &Report{
		Format: "dialogue-audio-diagnostic", Seed: seed, ProjectID: set.Project.ProjectID,
		SampleDir: sampleDir, ManifestValid: true,
	}
	loaded := make(map[int]*model.Project)
	for version := 1; version <= 3; version++ {
		path := generator.SchemaFilename(sampleDir, version)
		project, err := codec.LoadFile(path)
		if err != nil {
			return nil, err
		}
		loaded[version] = project
		report.SchemaLoads = append(report.SchemaLoads, SchemaLoad{
			Version: version, ClipCount: len(project.Clips),
			ConversionTraces: len(project.ConversionTraces),
			Defaults:         len(project.Defaults), TargetLoudnessQ16: project.TargetLoudnessQ16,
			CoreMatchesSource: coreMatches(set.Project, *project),
			FirstClipID:       project.Clips[0].ID,
		})
	}
	roundTrip, err := buildRoundTrip(set.Project)
	if err != nil {
		return nil, err
	}
	report.RoundTrip = roundTrip

	baseOpts := opts
	if baseOpts.Batch.MaxParallel <= 0 {
		baseOpts.Batch.MaxParallel = 2
	}
	if baseOpts.Batch.QueueBudget <= 0 {
		baseOpts.Batch.QueueBudget = 2
	}
	if baseOpts.Batch.SampleBufferSamples == 0 {
		baseOpts.Batch.SampleBufferSamples = 960
	}
	rtOpts := baseOpts
	rtOpts.CheckpointDir = filepath.Join(outDir, "checkpoint-realtime")
	rtOpts.CacheDir = ""
	rtRunner, err := pipeline.NewRunner(set.Project, rtOpts)
	if err != nil {
		return nil, err
	}
	realtime, err := rtRunner.Run(context.Background())
	if err != nil {
		return nil, err
	}
	report.Queue = QueueSummary{
		MaxParallel: baseOpts.Batch.MaxParallel, SampleBufferSamples: baseOpts.Batch.SampleBufferSamples,
		QueueBudget: baseOpts.Batch.QueueBudget, Events: realtime.QueueEvents,
	}

	normal := generator.CompareEnvelopes(set.Truth, realtime.Envelopes, set.Truth.EnvelopeToleranceQ16)
	tightTolerance := int64(0)
	tight := generator.CompareEnvelopes(set.Truth, realtime.Envelopes, tightTolerance)
	report.Truth = TruthReport{
		ToleranceQ16: set.Truth.EnvelopeToleranceQ16, Comparison: normal,
		TightToleranceQ16: tightTolerance, TightComparison: tight,
		DeviationDidNotGrow: tight.MaxDeviationQ16 <= normal.MaxDeviationQ16,
		SegmentsMatch:       calibrationTruthMatches(set.Truth, realtime),
		AdjustmentsMatch:    adjustmentTruthMatches(set.Truth, realtime),
	}

	resumeOpts := baseOpts
	resumeOpts.CheckpointDir = filepath.Join(outDir, "checkpoint-resume")
	resumeOpts.MaxNewCommits = 2
	resumeRunner, err := pipeline.NewRunner(set.Project, resumeOpts)
	if err != nil {
		return nil, err
	}
	resumeFirst, err := resumeRunner.Run(context.Background())
	if err != nil {
		return nil, err
	}
	cancelCtx, cancel := context.WithCancel(context.Background())
	cancel()
	canceledRunner, err := pipeline.NewRunner(set.Project, resumeOpts)
	if err != nil {
		return nil, err
	}
	canceledResult, cancelErr := canceledRunner.Run(cancelCtx)
	cancel()
	resumeOpts.MaxNewCommits = 0
	resumeRunner, err = pipeline.NewRunner(set.Project, resumeOpts)
	if err != nil {
		return nil, err
	}
	resumeFinal, err := resumeRunner.Run(context.Background())
	if err != nil {
		return nil, err
	}

	fullOpts := baseOpts
	fullOpts.CheckpointDir = ""
	fullOpts.CacheDir = ""
	fullRunner, err := pipeline.NewRunner(set.Project, fullOpts)
	if err != nil {
		return nil, err
	}
	full, err := fullRunner.Run(context.Background())
	if err != nil {
		return nil, err
	}
	report.Paths = PathReport{
		RealtimeProgress: realtime.Progress, ResumeFirstProgress: resumeFirst.Progress,
		ResumeFinalProgress: resumeFinal.Progress, FullProgress: full.Progress,
		CommitOrder:      realtime.CommitOrder,
		RealtimeVsResume: pipeline.CompareRuns(realtime, resumeFinal),
		RealtimeVsFull:   pipeline.CompareRuns(realtime, full),
		CancelProgress:   canceledResult.Progress,
		ResumedFrom:      resumeFinal.CommittedFromCheckpoint,
	}
	if coded, ok := audioerr.As(cancelErr); ok {
		report.Paths.CancelCode = coded.Code
	} else if cancelErr == nil {
		report.Paths.CancelCode = ""
	}

	changed := set.Project.Clone()
	changed.Clips[0].PCM[17] ^= 1
	changed.Clips[0].ContentHash = model.ContentHashForPCM(changed.Clips[0].PCM, changed.SampleRate)
	rebuildRunner, err := pipeline.NewRunner(changed, rtOpts)
	if err != nil {
		return nil, err
	}
	rebuilt, err := rebuildRunner.Run(context.Background())
	if err != nil {
		return nil, err
	}
	report.Paths.CheckpointRebuilt = rebuilt.Rebuilt
	report.Paths.RebuildCommitCount = len(rebuilt.CommitOrder)

	invalidations, err := runInvalidation(set.Project, baseOpts, outDir)
	if err != nil {
		return nil, err
	}
	report.Invalidation = invalidations
	report.Errors, err = runErrorReproductions(set, realtime.Envelopes)
	if err != nil {
		return nil, err
	}
	return report, writeReport(outDir, report)
}

func prepareOutput(outDir string) error {
	if outDir == "" {
		return fmt.Errorf("output directory is required")
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return err
	}
	for _, name := range []string{
		"samples", "checkpoint-realtime", "checkpoint-resume",
		"cache-invalidation", "report.json",
	} {
		if err := os.RemoveAll(filepath.Join(outDir, name)); err != nil {
			return err
		}
	}
	return nil
}

func coreMatches(source, loaded model.Project) bool {
	if len(source.Clips) != len(loaded.Clips) {
		return false
	}
	index := make(map[string]model.Clip, len(loaded.Clips))
	for _, clip := range loaded.Clips {
		index[clip.ID] = clip
	}
	for _, want := range source.Clips {
		got, ok := index[want.ID]
		if !ok {
			return false
		}
		if want.StartSample != got.StartSample || want.EndSample != got.EndSample ||
			want.GainQ16 != got.GainQ16 || want.LoudnessWeightQ16 != got.LoudnessWeightQ16 ||
			want.ContentHash != got.ContentHash || want.Category != got.Category {
			return false
		}
	}
	return true
}

func calibrationTruthMatches(truth generator.TruthBundle, result pipeline.RunResult) bool {
	for _, clip := range truth.Clips {
		envelope, ok := result.Envelopes[clip.ClipID]
		if !ok || envelope.SegmentEnvelopeQ16 != clip.SegmentEnvelopeQ16 ||
			envelope.SegmentEnergy != clip.SegmentEnergy {
			return false
		}
	}
	return true
}

func adjustmentTruthMatches(truth generator.TruthBundle, result pipeline.RunResult) bool {
	for _, clip := range truth.Clips {
		calibration, ok := result.Calibrations[clip.ClipID]
		if !ok || calibration.BeforeLoudnessQ16 != clip.SegmentEnvelopeQ16 ||
			calibration.RequiredAdjustmentDBQ16 != clip.ExpectedAdjustmentDBQ16 {
			return false
		}
	}
	return true
}

func buildRoundTrip(source model.Project) (RoundTripCheck, error) {
	low, err := codec.Export(source, 1)
	if err != nil {
		return RoundTripCheck{}, err
	}
	loadedLow, err := codec.Load(low.Data)
	if err != nil {
		return RoundTripCheck{}, err
	}
	high, err := codec.Export(*loadedLow, 3)
	if err != nil {
		return RoundTripCheck{}, err
	}
	loadedHigh, err := codec.Load(high.Data)
	if err != nil {
		return RoundTripCheck{}, err
	}
	byID := make(map[string]model.Clip, len(loadedHigh.Clips))
	for _, clip := range loadedHigh.Clips {
		byID[clip.ID] = clip
	}
	check := RoundTripCheck{
		FromVersion: 3, ViaVersion: 1, ToVersion: 3,
		Losses:             LossSummary{Code: low.Losses.Code, Items: len(low.Losses.Items)},
		AllBoundariesEqual: true, AllGainsEqual: true, AllWeightsEqual: true, ResidualsStable: true,
	}
	for i := 0; i < len(low.Losses.Items) && i < 4; i++ {
		check.Losses.Examples = append(check.Losses.Examples, low.Losses.Items[i])
	}
	for _, original := range source.Clips {
		restored, ok := byID[original.ID]
		if !ok {
			return RoundTripCheck{}, fmt.Errorf("roundtrip lost clip %s", original.ID)
		}
		_, boundaryTrace, err := fixed.SamplesToMillis(original.StartSample, source.SampleRate,
			fixed.RoundNearest, "roundtrip.boundary")
		if err != nil {
			return RoundTripCheck{}, err
		}
		_, restoredTrace, err := fixed.SamplesToMillis(restored.StartSample, source.SampleRate,
			fixed.RoundNearest, "roundtrip.boundary")
		if err != nil {
			return RoundTripCheck{}, err
		}
		_, gainTrace, err := fixed.Rescale(original.GainQ16, fixed.Q16, 1000, fixed.RoundNearest, "roundtrip.gain")
		if err != nil {
			return RoundTripCheck{}, err
		}
		_, restoredGainTrace, err := fixed.Rescale(restored.GainQ16, fixed.Q16, 1000, fixed.RoundNearest, "roundtrip.gain")
		if err != nil {
			return RoundTripCheck{}, err
		}
		_, weightTrace, err := fixed.Rescale(original.LoudnessWeightQ16, fixed.Q16, 1000, fixed.RoundNearest, "roundtrip.weight")
		if err != nil {
			return RoundTripCheck{}, err
		}
		_, restoredWeightTrace, err := fixed.Rescale(restored.LoudnessWeightQ16, fixed.Q16, 1000, fixed.RoundNearest, "roundtrip.weight")
		if err != nil {
			return RoundTripCheck{}, err
		}
		item := ClipRoundTrip{
			ClipID:           original.ID,
			BoundaryEqual:    original.StartSample == restored.StartSample && original.EndSample == restored.EndSample,
			GainEqual:        original.GainQ16 == restored.GainQ16,
			WeightEqual:      original.LoudnessWeightQ16 == restored.LoudnessWeightQ16,
			BoundaryResidual: boundaryTrace.Residual,
			GainResidual:     gainTrace.Residual,
			WeightResidual:   weightTrace.Residual,
			InitialGainQ16:   original.GainQ16, RestoredGainQ16: restored.GainQ16,
		}
		check.AllBoundariesEqual = check.AllBoundariesEqual && item.BoundaryEqual
		check.AllGainsEqual = check.AllGainsEqual && item.GainEqual
		check.AllWeightsEqual = check.AllWeightsEqual && item.WeightEqual
		check.ResidualsStable = check.ResidualsStable &&
			boundaryTrace.Residual == restoredTrace.Residual &&
			gainTrace.Residual == restoredGainTrace.Residual &&
			weightTrace.Residual == restoredWeightTrace.Residual
		check.Clips = append(check.Clips, item)
	}
	return check, nil
}

func runInvalidation(source model.Project, opts pipeline.Options, outDir string) ([]InvalidationCase, error) {
	cacheDir := filepath.Join(outDir, "cache-invalidation")
	cacheOpts := opts
	cacheOpts.CheckpointDir = ""
	cacheOpts.CacheDir = cacheDir
	warmRunner, err := pipeline.NewRunner(source, cacheOpts)
	if err != nil {
		return nil, err
	}
	if _, err := warmRunner.Run(context.Background()); err != nil {
		return nil, err
	}
	baseGraph := warmRunner.Graph()
	type scenario struct {
		name   string
		edit   graph.Edit
		mutate func(model.Project) model.Project
	}
	scenarios := []scenario{
		{name: "clip_gain_c02", edit: graph.Edit{Kind: graph.EditClipGain, ClipID: "c02"},
			mutate: func(p model.Project) model.Project {
				for i := range p.Clips {
					if p.Clips[i].ID == "c02" {
						p.Clips[i].GainQ16 += 6554
					}
				}
				return p
			}},
		{name: "target_loudness", edit: graph.Edit{Kind: graph.EditTarget},
			mutate: func(p model.Project) model.Project {
				p.TargetLoudnessQ16 += fixed.Q16
				return p
			}},
		{name: "frame_rate", edit: graph.Edit{Kind: graph.EditFrameRate},
			mutate: func(p model.Project) model.Project {
				p.FrameLenSamples = 240
				p.HopSamples = 240
				return p
			}},
	}
	out := make([]InvalidationCase, 0, len(scenarios))
	for _, scenario := range scenarios {
		invalidation, err := baseGraph.Invalidate(scenario.edit)
		if err != nil {
			return nil, err
		}
		edited := scenario.mutate(source.Clone())
		runner, err := pipeline.NewRunner(edited, cacheOpts)
		if err != nil {
			return nil, err
		}
		result, err := runner.Run(context.Background())
		if err != nil {
			return nil, err
		}
		out = append(out, InvalidationCase{
			Name: scenario.name, Edit: scenario.edit, Invalidation: invalidation,
			RecomputedEnvelope:    result.RecomputedEnvelopeClips,
			ReusedEnvelope:        result.ReusedEnvelopeClips,
			RecomputedCalibration: result.RecomputedCalibrationClips,
			ReusedCalibration:     result.ReusedCalibrationClips,
			Complete:              result.Complete,
		})
	}
	return out, nil
}

func runErrorReproductions(set *generator.SampleSet, envelopes map[string]engine.ClipEnvelope) (ErrorReport, error) {
	var report ErrorReport
	_, err := codec.Load([]byte(`{"schema_version":99}`))
	report.SchemaUnknown = errorRepro(err)
	_, err = codec.Load([]byte(`{"schema_version":1,"project":{}}`))
	report.SchemaMissing = errorRepro(err)

	unreachable := set.Project.Clone()
	unreachable.TargetLoudnessQ16 = set.Project.TargetLoudnessQ16 + 20*fixed.Q16
	_, err = engine.CalibrateProject(unreachable, envelopes, pipeline.DefaultToleranceQ16)
	report.LoudnessUnreachable = errorRepro(err)

	ids := set.Project.ClipIDs()
	sort.Strings(ids)
	manager, err := state.New(ids)
	if err != nil {
		return report, err
	}
	err = manager.Transition(ids[0], state.Calibrating)
	report.StateIllegal = errorRepro(err)

	_, err = fixed.MulDiv(math.MaxInt64, 2, 1, "gain_q16")
	report.Overflow = errorRepro(err)
	return report, nil
}

func errorRepro(err error) ErrorRepro {
	if err == nil {
		return ErrorRepro{}
	}
	if coded, ok := audioerr.As(err); ok {
		return ErrorRepro{
			Code: coded.Code, Field: coded.Field, Message: coded.Message,
			OpA: coded.OpA, OpB: coded.OpB, Values: coded.Values,
		}
	}
	return ErrorRepro{Message: err.Error()}
}

func writeReport(outDir string, report *Report) error {
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	path := filepath.Join(outDir, "report.json")
	temp := path + ".tmp"
	if err := os.WriteFile(temp, data, 0o644); err != nil {
		return err
	}
	if err := os.Rename(temp, path); err != nil {
		return err
	}
	sum := sha256.Sum256(data)
	fmt.Fprintf(os.Stderr, "report_sha256=%s\n", hex.EncodeToString(sum[:]))
	return nil
}

// ValidateReport checks every acceptance invariant represented in Report.
func ValidateReport(r *Report) error {
	if !r.ManifestValid {
		return fmt.Errorf("manifest is not valid")
	}
	if len(r.SchemaLoads) != 3 {
		return fmt.Errorf("expected three schema loads")
	}
	for _, load := range r.SchemaLoads {
		if !load.CoreMatchesSource {
			return fmt.Errorf("schema v%d does not match canonical clip core", load.Version)
		}
	}
	if !r.RoundTrip.AllBoundariesEqual || !r.RoundTrip.AllGainsEqual ||
		!r.RoundTrip.AllWeightsEqual || !r.RoundTrip.ResidualsStable {
		return fmt.Errorf("high-low-high round trip failed")
	}
	if !r.Truth.Comparison.Pass || !r.Truth.TightComparison.Pass ||
		r.Truth.TightComparison.MaxDeviationQ16 > r.Truth.Comparison.MaxDeviationQ16 {
		return fmt.Errorf("truth comparison failed")
	}
	if !r.Truth.SegmentsMatch || !r.Truth.AdjustmentsMatch {
		return fmt.Errorf("segment/calibration truth comparison failed")
	}
	if r.Paths.RealtimeVsResume != nil || r.Paths.RealtimeVsFull != nil {
		return fmt.Errorf("three-path comparison failed")
	}
	if r.Paths.CancelCode != audioerr.CodeCancelled {
		return fmt.Errorf("cancellation did not return E_CANCELLED")
	}
	if r.Paths.CancelProgress.Committed != r.Paths.ResumeFirstProgress.Committed {
		return fmt.Errorf("cancellation changed committed progress")
	}
	if !r.Paths.CheckpointRebuilt {
		return fmt.Errorf("content mismatch did not trigger whole rebuild")
	}
	type expectedSets struct {
		env map[string]bool
		cal map[string]bool
	}
	allClips := map[string]bool{"c01": true, "c02": true, "c03": true, "c04": true, "c05": true, "c06": true}
	expected := map[string]expectedSets{
		"clip_gain_c02":   {env: map[string]bool{"c02": true}, cal: map[string]bool{"c02": true}},
		"target_loudness": {env: map[string]bool{}, cal: allClips},
		"frame_rate":      {env: allClips, cal: allClips},
	}
	for _, inv := range r.Invalidation {
		want, ok := expected[inv.Name]
		if !ok {
			continue
		}
		if !sameSet(inv.RecomputedEnvelope, want.env) ||
			!sameSet(inv.RecomputedCalibration, want.cal) {
			return fmt.Errorf("invalidation set mismatch for %s", inv.Name)
		}
	}
	for name, repro := range map[string]ErrorRepro{
		"schema_unknown": r.Errors.SchemaUnknown, "schema_missing": r.Errors.SchemaMissing,
		"loudness": r.Errors.LoudnessUnreachable, "state": r.Errors.StateIllegal, "overflow": r.Errors.Overflow,
	} {
		if repro.Code == "" {
			return fmt.Errorf("missing error reproduction %s", name)
		}
	}
	if r.Errors.SchemaUnknown.Code != audioerr.CodeSchema ||
		r.Errors.SchemaMissing.Code != audioerr.CodeSchema ||
		r.Errors.LoudnessUnreachable.Code != audioerr.CodeLoudness ||
		r.Errors.StateIllegal.Code != audioerr.CodeState ||
		r.Errors.Overflow.Code != audioerr.CodeOverflow {
		return fmt.Errorf("unexpected error code reproduction")
	}
	if r.RoundTrip.Losses.Items == 0 {
		return fmt.Errorf("downgrade did not report E_LOSSY items")
	}
	return nil
}

func sameSet(got []string, want map[string]bool) bool {
	if len(got) != len(want) {
		return false
	}
	for _, value := range got {
		if !want[value] {
			return false
		}
	}
	return true
}
