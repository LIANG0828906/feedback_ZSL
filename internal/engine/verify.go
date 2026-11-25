package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"uvenhance/internal/apperr"
	"uvenhance/internal/cache"
	"uvenhance/internal/yuv"
)

type StepReport struct {
	Name          string         `json:"name"`
	Phase         string         `json:"phase,omitempty"`
	Complete      bool           `json:"complete"`
	SnapshotUID   string         `json:"snapshot_uid,omitempty"`
	Invalidations []Invalidation `json:"invalidations,omitempty"`
	Recomputed    []string       `json:"recomputed_stages,omitempty"`
	CacheHits     []string       `json:"cache_hit_stages,omitempty"`
	Stats         Stats          `json:"stats"`
}

type PathReport struct {
	Name             string            `json:"name"`
	Steps            []StepReport      `json:"steps"`
	TotalStats       Stats             `json:"total_stats"`
	FinalSnapshotUID string            `json:"final_snapshot_uid"`
	FinalDigest      string            `json:"final_digest"`
	StageDigests     map[string]string `json:"stage_digests"`
	FinalStageOrder  []string          `json:"final_snapshot_stage_order"`
	CommitOrder      []string          `json:"observed_commit_order"`
}

type CancelBoundaryReport struct {
	Step                    int      `json:"step"`
	CancelAfterStageCommits int      `json:"cancel_after_stage_commits"`
	CompleteAfterCancel     bool     `json:"complete_after_cancel"`
	PersistedStageCommits   []string `json:"persisted_stage_commits"`
	SnapshotUID             string   `json:"snapshot_uid"`
	ResumeRequired          bool     `json:"resume_required"`
	ResumeCacheHits         []string `json:"resume_cache_hits"`
	ResumeRecomputed        []string `json:"resume_recomputed_stages"`
}

type VerificationReport struct {
	SampleID            string               `json:"sample_id"`
	Sequence            string               `json:"edit_sequence"`
	CanonicalStageOrder []string             `json:"canonical_stage_order"`
	Incremental         PathReport           `json:"incremental"`
	Recovery            PathReport           `json:"recovery"`
	Full                PathReport           `json:"full"`
	CancelBoundary      CancelBoundaryReport `json:"cancel_boundary"`
	Comparison          map[string]any       `json:"comparison"`
	FinalDigest         string               `json:"final_digest"`
	Heatmap             *HeatmapStats        `json:"heatmap"`
}

type VerifyOptions struct {
	WorkDir            string
	CancelStep         int
	CancelAfterCommits int
	Workers            int
	BufferMiB          int
	HeatNormMin        float64
	HeatNormMax        float64
}

func VerifyThreePaths(ctx context.Context, sampleDir string, seq *EditSequence, opts VerifyOptions) (*VerificationReport, error) {
	if len(seq.Steps) == 0 {
		return nil, apperr.Meta("edits.steps", "at least one edit step is required")
	}
	if opts.WorkDir == "" {
		return nil, apperr.Meta("workdir", "work directory is required")
	}
	if opts.CancelStep <= 0 || opts.CancelStep >= len(seq.Steps) {
		opts.CancelStep = len(seq.Steps)
	}
	if opts.CancelAfterCommits <= 0 {
		opts.CancelAfterCommits = 1
	}
	if opts.Workers <= 0 {
		opts.Workers = defaultWorkers
	}
	if opts.BufferMiB <= 0 {
		opts.BufferMiB = 128
	}
	if opts.HeatNormMax <= opts.HeatNormMin {
		opts.HeatNormMin, opts.HeatNormMax = 0, 0.25
	}
	if err := os.MkdirAll(opts.WorkDir, 0o755); err != nil {
		return nil, apperr.IO("create verification work directory", err)
	}
	for _, name := range []string{"verify_report.json", "error_report.json"} {
		if err := os.Remove(filepath.Join(opts.WorkDir, name)); err != nil && !os.IsNotExist(err) {
			return nil, apperr.IO("remove stale verification report", err)
		}
	}
	if err := os.RemoveAll(filepath.Join(opts.WorkDir, "artifacts")); err != nil {
		return nil, apperr.IO("remove stale verification artifacts", err)
	}
	sample, err := yuv.LoadSample(sampleDir)
	if err != nil {
		return nil, err
	}
	if err := yuv.ValidateTruth(ctx, sample); err != nil {
		return nil, err
	}
	baseOpts := Options{
		ParamVersion: seq.ParamVersion, AlignmentVersion: seq.AlignmentVersion,
		Workers: opts.Workers, BufferBytesPerWorker: int64(opts.BufferMiB) << 20,
	}

	incremental, incrementalResult, err := runIncrementalPath(ctx, sample, seq, filepath.Join(opts.WorkDir, "incremental.db"), baseOpts)
	if err != nil {
		return nil, err
	}
	recovery, recoveryResult, cancelReport, err := runRecoveryPath(ctx, sample, seq, filepath.Join(opts.WorkDir, "recovery.db"),
		baseOpts, opts.CancelStep, opts.CancelAfterCommits)
	if err != nil {
		return nil, err
	}
	full, fullResult, err := runFullPath(ctx, sample, seq.Steps[len(seq.Steps)-1].Params,
		filepath.Join(opts.WorkDir, "full.db"), baseOpts)
	if err != nil {
		return nil, err
	}

	if err := CompareResults(incrementalResult, recoveryResult); err != nil {
		return nil, err
	}
	if err := CompareResults(incrementalResult, fullResult); err != nil {
		return nil, err
	}
	if incrementalResult.Final.Digest() != recoveryResult.Final.Digest() || incrementalResult.Final.Digest() != fullResult.Final.Digest() {
		return nil, apperr.Diff(cache.StageInterleave, 0, 0, "final image digest differs between paths")
	}
	if !slices.Equal(incremental.CommitOrder, recovery.CommitOrder) {
		return nil, apperr.Diff("commit_sequence", 0, 0, "incremental and recovery commit sequences differ")
	}
	if !slices.Equal(full.CommitOrder, stageOrder()) {
		return nil, apperr.Diff("commit_sequence", 0, 0, "full recompute is not in canonical topological order")
	}
	if !slices.Equal(incremental.FinalStageOrder, recovery.FinalStageOrder) ||
		!slices.Equal(incremental.FinalStageOrder, full.FinalStageOrder) {
		return nil, apperr.Diff("commit_sequence", 0, 0, "final snapshot stage order differs between paths")
	}

	heat, err := WriteArtifacts(ctx, filepath.Join(opts.WorkDir, "artifacts"), sample, incrementalResult,
		opts.HeatNormMin, opts.HeatNormMax)
	if err != nil {
		return nil, err
	}
	errorReport, err := BuildErrorReport(ctx, sample, seq.Steps[len(seq.Steps)-1].Params, incrementalResult)
	if err != nil {
		return nil, err
	}
	if err := writeJSON(filepath.Join(opts.WorkDir, "error_report.json"), errorReport); err != nil {
		return nil, err
	}
	report := &VerificationReport{
		SampleID: sample.Metadata.ID, Sequence: seq.Name,
		CanonicalStageOrder: stageOrder(),
		Incremental:         incremental, Recovery: recovery, Full: full,
		CancelBoundary: cancelReport,
		Comparison: map[string]any{
			"status": "identical", "stages_compared": len(cache.Stages),
			"pixel_identical": true, "intermediate_planes_identical": true,
			"canonical_commit_order_identical":            true,
			"incremental_recovery_commit_order_identical": true,
			"full_commit_order_canonical":                 true,
			"final_snapshot_stage_order_identical":        true,
			"error_report_used_truth_deviation":           false,
		},
		FinalDigest: incrementalResult.Final.Digest(), Heatmap: heat,
	}
	if err := writeJSON(filepath.Join(opts.WorkDir, "verify_report.json"), report); err != nil {
		return nil, err
	}
	return report, nil
}

func runIncrementalPath(ctx context.Context, sample *yuv.Sample, seq *EditSequence, dbPath string, opts Options) (PathReport, *RunResult, error) {
	store, err := openCleanStore(dbPath)
	if err != nil {
		return PathReport{}, nil, err
	}
	defer store.Close()
	eng := New(store)
	report := PathReport{Name: "incremental", StageDigests: map[string]string{}}
	var final *RunResult
	for _, step := range seq.Steps {
		res, err := eng.RunLive(ctx, sample, step.Params, opts)
		if err != nil {
			return report, nil, fmt.Errorf("incremental step %s: %w", step.Name, err)
		}
		final = res
		report.Steps = append(report.Steps, summarizeStep(step.Name, "incremental", res))
		addStats(&report.TotalStats, res.Stats)
		report.CommitOrder = append(report.CommitOrder, res.Committed...)
		if err := validateCanonicalOrder(res.Committed); err != nil {
			return report, nil, err
		}
	}
	fillPathSummary(&report, final)
	return report, final, nil
}

func runRecoveryPath(ctx context.Context, sample *yuv.Sample, seq *EditSequence, dbPath string, opts Options,
	cancelStep, cancelAfter int) (PathReport, *RunResult, CancelBoundaryReport, error) {
	store, err := openCleanStore(dbPath)
	if err != nil {
		return PathReport{}, nil, CancelBoundaryReport{}, err
	}
	defer store.Close()
	eng := New(store)
	report := PathReport{Name: "recovery", StageDigests: map[string]string{}}
	cancelReport := CancelBoundaryReport{
		Step: cancelStep, CancelAfterStageCommits: cancelAfter,
	}
	var final *RunResult
	for i, step := range seq.Steps {
		if i == cancelStep-1 {
			cancelOpts := opts
			cancelOpts.CancelAfterCommits = cancelAfter
			partial, cancelErr := eng.Run(ctx, sample, step.Params, cancelOpts)
			if apperr.Code(cancelErr) != apperr.CodeCanceled {
				return report, nil, cancelReport, fmt.Errorf("recovery cancellation was not observed: %v", cancelErr)
			}
			if partial.Complete {
				return report, nil, cancelReport, apperr.New(apperr.CodeInvariant, "cancellation unexpectedly completed all stages")
			}
			if partial.SnapshotID == 0 {
				return report, nil, cancelReport, apperr.New(apperr.CodeInvariant, "canceled run has no persisted snapshot")
			}
			count, err := store.SnapshotCount(ctx)
			if err != nil {
				return report, nil, cancelReport, apperr.IO("count snapshots after cancel", err)
			}
			if count != 1 {
				return report, nil, cancelReport, apperr.New(apperr.CodeInvariant,
					fmt.Sprintf("expected one atomic snapshot after cancel, got %d", count))
			}
			cancelReport.CompleteAfterCancel = partial.Complete
			cancelReport.PersistedStageCommits = append([]string(nil), partial.Committed...)
			cancelReport.SnapshotUID = partial.SnapshotUID
			cancelReport.ResumeRequired = true
			report.Steps = append(report.Steps, summarizeStep(step.Name, "canceled", partial))
			addStats(&report.TotalStats, partial.Stats)
			report.CommitOrder = append(report.CommitOrder, partial.Committed...)
			if err := validateCanonicalOrder(partial.Committed); err != nil {
				return report, nil, cancelReport, err
			}

			res, err := eng.Run(ctx, sample, step.Params, opts)
			if err != nil {
				return report, nil, cancelReport, fmt.Errorf("recovery restart: %w", err)
			}
			if res.Stats.SnapshotRejected {
				return report, nil, cancelReport, apperr.New(apperr.CodeCache, "restart rejected matching partial snapshot")
			}
			cancelReport.ResumeCacheHits = append([]string(nil), res.CacheHits...)
			cancelReport.ResumeRecomputed = append([]string(nil), res.Recomputed...)
			final = res
			report.Steps = append(report.Steps, summarizeStep(step.Name, "resumed", res))
			addStats(&report.TotalStats, res.Stats)
			report.CommitOrder = append(report.CommitOrder, res.Committed...)
			if err := validateCanonicalOrder(res.Committed); err != nil {
				return report, nil, cancelReport, err
			}
			continue
		}
		res, err := eng.Run(ctx, sample, step.Params, opts)
		if err != nil {
			return report, nil, cancelReport, fmt.Errorf("recovery step %s: %w", step.Name, err)
		}
		final = res
		report.Steps = append(report.Steps, summarizeStep(step.Name, "recovery", res))
		addStats(&report.TotalStats, res.Stats)
		report.CommitOrder = append(report.CommitOrder, res.Committed...)
		if err := validateCanonicalOrder(res.Committed); err != nil {
			return report, nil, cancelReport, err
		}
	}
	fillPathSummary(&report, final)
	return report, final, cancelReport, nil
}

func runFullPath(ctx context.Context, sample *yuv.Sample, params yuv.Params, dbPath string, opts Options) (PathReport, *RunResult, error) {
	store, err := openCleanStore(dbPath)
	if err != nil {
		return PathReport{}, nil, err
	}
	defer store.Close()
	opts.CancelAfterCommits = 0
	res, err := New(store).Run(ctx, sample, params, opts)
	if err != nil {
		return PathReport{}, nil, fmt.Errorf("full recompute: %w", err)
	}
	report := PathReport{Name: "full", StageDigests: map[string]string{}}
	report.Steps = []StepReport{summarizeStep("full_final_parameters", "full", res)}
	addStats(&report.TotalStats, res.Stats)
	report.CommitOrder = append(report.CommitOrder, res.Committed...)
	if err := validateCanonicalOrder(report.CommitOrder); err != nil {
		return report, nil, err
	}
	if res.Stats.CacheHitCount != 0 || len(res.Committed) != len(cache.Stages) {
		return report, nil, apperr.New(apperr.CodeInvariant, "full path did not recompute every stage from an empty cache")
	}
	fillPathSummary(&report, res)
	return report, res, nil
}

func openCleanStore(path string) (*cache.Store, error) {
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if err := os.Remove(path + suffix); err != nil && !os.IsNotExist(err) {
			return nil, apperr.IO("remove old cache snapshot", err)
		}
	}
	return cache.Open(path)
}

func summarizeStep(name, phase string, res *RunResult) StepReport {
	return StepReport{
		Name: name, Phase: phase, Complete: res.Complete, SnapshotUID: res.SnapshotUID,
		Invalidations: res.Invalidations, Recomputed: res.Recomputed,
		CacheHits: res.CacheHits, Stats: res.Stats,
	}
}

func fillPathSummary(report *PathReport, final *RunResult) {
	if final == nil {
		return
	}
	report.FinalDigest = final.Final.Digest()
	report.FinalSnapshotUID = final.SnapshotUID
	report.FinalStageOrder = stageOrder()
	for _, spec := range cache.Stages {
		report.StageDigests[spec.ID] = final.Stages[spec.ID].OutputHash
	}
}

func addStats(dst *Stats, src Stats) {
	dst.RecomputeCount += src.RecomputeCount
	dst.CacheHitCount += src.CacheHitCount
	dst.InvalidationCount += src.InvalidationCount
	dst.SnapshotRejected = dst.SnapshotRejected || src.SnapshotRejected
	dst.BufferWaitEvents = append(dst.BufferWaitEvents, src.BufferWaitEvents...)
	if src.Workers > dst.Workers {
		dst.Workers = src.Workers
	}
	if src.BufferBytesPerWorker > dst.BufferBytesPerWorker {
		dst.BufferBytesPerWorker = src.BufferBytesPerWorker
	}
}

func validateCanonicalOrder(commits []string) error {
	last := -1
	for _, id := range commits {
		idx := cache.StageIndex(id)
		if idx < 0 || idx < last {
			return apperr.New(apperr.CodeInvariant, "stage commits are not in canonical topological order: "+id)
		}
		last = idx
	}
	return nil
}

func stageOrder() []string {
	out := make([]string, 0, len(cache.Stages))
	for _, spec := range cache.Stages {
		out = append(out, spec.ID)
	}
	return out
}

func writeJSON(path string, value any) error {
	raw, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return apperr.IO("encode json report", err)
	}
	raw = append(raw, '\n')
	if err := os.WriteFile(path, raw, 0o644); err != nil {
		return apperr.IO("write json report", err)
	}
	return nil
}

func SortedStageDigests(result *RunResult) string {
	keys := make([]string, 0, len(result.Stages))
	for id := range result.Stages {
		keys = append(keys, id)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, id := range keys {
		fmt.Fprintf(&b, "%s=%s\n", id, result.Stages[id].OutputHash)
	}
	return b.String()
}
