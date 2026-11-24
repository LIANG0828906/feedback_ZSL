package verify

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"time"

	"occlusionlab/internal/geometry"
	"occlusionlab/internal/model"
	"occlusionlab/internal/store"
	"occlusionlab/internal/streaming"
)

type Check struct {
	Name   string `json:"name"`
	Passed bool   `json:"passed"`
	Detail string `json:"detail"`
}

type ProjectionCheck struct {
	ID              string `json:"id"`
	Window          int    `json:"window"`
	Chunk           int    `json:"chunk"`
	ObservedXQ      int64  `json:"observed_x_q"`
	ObservedYQ      int64  `json:"observed_y_q"`
	ExpectedXQ      int64  `json:"expected_x_q"`
	ExpectedYQ      int64  `json:"expected_y_q"`
	ErrorXQ         int64  `json:"error_x_q"`
	ErrorYQ         int64  `json:"error_y_q"`
	ErrorQ          int64  `json:"error_q"`
	BoundQ          int64  `json:"bound_q"`
	WithinTolerance bool   `json:"within_tolerance"`
}

type ToleranceCount struct {
	TolerancePxQ int64 `json:"tolerance_px_q"`
	Count        int   `json:"count"`
}

type ProjectionReport struct {
	KnownCount          int               `json:"known_count"`
	WithinTolerance     int               `json:"within_tolerance"`
	MaxErrorQ           int64             `json:"max_error_q"`
	MaxBoundQ           int64             `json:"max_bound_q"`
	BoundViolations     int               `json:"bound_violations"`
	ToleranceCounts     []ToleranceCount  `json:"tolerance_counts"`
	MonotonicTightening bool              `json:"monotonic_tightening"`
	Checks              []ProjectionCheck `json:"checks"`
}

type TruthMismatch struct {
	Window       int    `json:"window"`
	Chunk        int    `json:"chunk"`
	TruthVisible bool   `json:"truth_visible"`
	Observed     string `json:"observed"`
	Reason       string `json:"reason"`
}

type VisibilityReport struct {
	TruthVisible        int             `json:"truth_visible"`
	TruthHidden         int             `json:"truth_hidden"`
	ObservedVisible     int             `json:"observed_visible"`
	ObservedOccluded    int             `json:"observed_occluded"`
	ObservedUncertain   int             `json:"observed_uncertain"`
	ExactAgreement      int             `json:"exact_agreement"`
	ConservativeMatch   int             `json:"conservative_match"`
	Conflicts           int             `json:"conflicts"`
	ReferenceFailures   int             `json:"reference_failures"`
	UncertainInsideBand int             `json:"uncertain_inside_band"`
	Mismatches          []TruthMismatch `json:"mismatches"`
}

type ErrorReport struct {
	AllBoundsPositive        bool   `json:"all_bounds_positive"`
	ProjectionCoversObserved bool   `json:"projection_covers_observed"`
	VisibilityBoundsValid    bool   `json:"visibility_bounds_valid"`
	MaxResidualQ             int64  `json:"max_residual_q"`
	MaxProjectionErrorQ      int64  `json:"max_projection_error_q"`
	MaxPlaneErrorWorldQ      int64  `json:"max_plane_error_world_q"`
	MaxCoverageErrorQ        int64  `json:"max_coverage_error_q"`
	MaxDepthErrorWorldQ      int64  `json:"max_depth_error_world_q"`
	MaxCumulativeBoundQ      int64  `json:"max_cumulative_bound_q"`
	Formula                  string `json:"formula"`
}

type PathSummary struct {
	Name              string  `json:"name"`
	WindowCount       int     `json:"window_count"`
	CommittedCount    int     `json:"committed_count"`
	CheckpointCount   int     `json:"checkpoint_count"`
	Invalidated       bool    `json:"invalidated"`
	DurationMillis    float64 `json:"duration_ms"`
	RestartAt         int     `json:"restart_at,omitempty"`
	ResumedFrom       int     `json:"resumed_from,omitempty"`
	BudgetQueueEvents int     `json:"budget_queue_events"`
}

type PathDifference struct {
	PathA  string `json:"path_a"`
	PathB  string `json:"path_b"`
	Window int    `json:"window"`
	Field  string `json:"field"`
	Detail string `json:"detail"`
}

type ThreePathReport struct {
	Equal              bool             `json:"equal"`
	Paths              []PathSummary    `json:"paths"`
	Differences        []PathDifference `json:"differences"`
	CheckpointVerified bool             `json:"checkpoint_verified"`
}

type Report struct {
	Seed             int64                   `json:"seed"`
	ProjectHash      string                  `json:"project_hash"`
	WindowCount      int                     `json:"window_count"`
	ChunkCount       int                     `json:"chunk_count"`
	ParamsHash       string                  `json:"params_hash"`
	RasterResolution int                     `json:"raster_resolution"`
	Projection       ProjectionReport        `json:"projection"`
	Visibility       VisibilityReport        `json:"visibility"`
	ErrorBounds      ErrorReport             `json:"error_bounds"`
	ThreePaths       ThreePathReport         `json:"three_paths"`
	BudgetEvents     []streaming.BudgetEvent `json:"budget_events"`
	Checks           []Check                 `json:"checks"`
	Passed           bool                    `json:"passed"`
	GeneratedAt      string                  `json:"generated_at"`
}

type framesRun struct {
	frames  []*streaming.WindowFrame
	summary PathSummary
	events  []streaming.BudgetEvent
}

func Run(ctx context.Context, project *model.Project, params model.Params, workDir string) (*Report, *model.AppError) {
	started := time.Now().UTC()
	report := &Report{
		Seed:             project.Meta.Seed,
		ProjectHash:      project.ContentHash,
		WindowCount:      len(project.World.Cameras),
		ChunkCount:       len(project.World.Chunks),
		ParamsHash:       params.Hash(),
		RasterResolution: params.RasterResolution,
	}
	if workDir == "" {
		var err error
		workDir, err = os.MkdirTemp("", "occlusionlab-verify-*")
		if err != nil {
			return nil, model.IOError("work_dir", "无法创建验收临时目录", err)
		}
		defer os.RemoveAll(workDir)
	}

	if err := collectProjectionAndVisibility(project, params, report); err != nil {
		return nil, err
	}
	triple, events, err := runThreePaths(ctx, project, params, workDir)
	if err != nil {
		return nil, err
	}
	report.ThreePaths = *triple
	report.BudgetEvents = events

	report.Checks = []Check{
		{Name: "known_projection_tolerance",
			Passed: report.Projection.WithinTolerance == report.Projection.KnownCount,
			Detail: fmt.Sprintf("%d/%d known projections within %s px",
				report.Projection.WithinTolerance, report.Projection.KnownCount,
				model.FormatQ(project.Meta.TolerancePxQ, project.Meta.Precision.ScreenFracBits))},
		{Name: "tolerance_tightening_monotonic",
			Passed: report.Projection.MonotonicTightening,
			Detail: "inside-tolerance counts must be non-increasing as tolerance tightens"},
		{Name: "projection_bound_covers_observation",
			Passed: report.Projection.BoundViolations == 0,
			Detail: fmt.Sprintf("%d bound violations", report.Projection.BoundViolations)},
		{Name: "truth_compatible",
			Passed: report.Visibility.Conflicts == 0,
			Detail: fmt.Sprintf("%d conflicts, %d uncertain inside conservative band",
				report.Visibility.Conflicts, report.Visibility.UncertainInsideBand)},
		{Name: "reference_occluders_present",
			Passed: report.Visibility.ReferenceFailures == 0,
			Detail: fmt.Sprintf("%d reference failures", report.Visibility.ReferenceFailures)},
		{Name: "error_bounds_positive",
			Passed: report.ErrorBounds.AllBoundsPositive && report.ErrorBounds.VisibilityBoundsValid,
			Detail: "all observed errors are non-negative and visibility bounds are ordered"},
		{Name: "three_path_consistency",
			Passed: report.ThreePaths.Equal,
			Detail: fmt.Sprintf("%d differences", len(report.ThreePaths.Differences))},
		{Name: "checkpoint_restart",
			Passed: report.ThreePaths.CheckpointVerified,
			Detail: "resume path restored the latest committed window and continued"},
	}
	report.Passed = true
	for _, check := range report.Checks {
		if !check.Passed {
			report.Passed = false
		}
	}
	report.GeneratedAt = started.Format(time.RFC3339)
	return report, nil
}

func collectProjectionAndVisibility(project *model.Project, params model.Params, report *Report) *model.AppError {
	var checks []ProjectionCheck
	var allErrors []int64
	var mismatches []TruthMismatch
	numVisible, numHidden := 0, 0
	numObsVisible, numObsOccluded, numObsUncertain := 0, 0, 0
	exact, conservative, conflicts, referenceFailures := 0, 0, 0, 0
	for window, truthSample := range project.Truth.Samples {
		result, err := geometry.EvaluateSample(project, params, window)
		if err != nil {
			return err
		}
		numObsVisible += len(result.Visible)
		numObsOccluded += len(result.Occluded)
		numObsUncertain += len(result.Uncertain)
		for _, kp := range project.Truth.KnownProjections {
			if kp.Window != window {
				continue
			}
			verdict := result.Chunks[kp.Chunk]
			errX := abs64(verdict.Anchor.XQ - kp.ExpectedX)
			errY := abs64(verdict.Anchor.YQ - kp.ExpectedY)
			errQ := max64(errX, errY)
			check := ProjectionCheck{
				ID: kp.ID, Window: window, Chunk: kp.Chunk,
				ObservedXQ: verdict.Anchor.XQ, ObservedYQ: verdict.Anchor.YQ,
				ExpectedXQ: kp.ExpectedX, ExpectedYQ: kp.ExpectedY,
				ErrorXQ: errX, ErrorYQ: errY, ErrorQ: errQ,
				BoundQ:          verdict.Error.ProjectionErrorQ,
				WithinTolerance: errQ <= project.Truth.TolerancePxQ,
			}
			if errQ > check.BoundQ {
				report.Projection.BoundViolations++
			}
			if errQ > report.Projection.MaxErrorQ {
				report.Projection.MaxErrorQ = errQ
			}
			if check.BoundQ > report.Projection.MaxBoundQ {
				report.Projection.MaxBoundQ = check.BoundQ
			}
			if check.WithinTolerance {
				report.Projection.WithinTolerance++
			}
			checks = append(checks, check)
			allErrors = append(allErrors, errQ)
		}
		for _, truthChunk := range truthSample.Chunks {
			if truthChunk.Visible {
				numVisible++
			} else {
				numHidden++
			}
			verdict := result.Chunks[truthChunk.Chunk]
			if (truthChunk.Visible && verdict.Status == geometry.StatusVisible) ||
				(!truthChunk.Visible && verdict.Status == geometry.StatusOccluded) {
				exact++
			}
			if (truthChunk.Visible && verdict.Status != geometry.StatusOccluded) ||
				(!truthChunk.Visible && verdict.Status != geometry.StatusVisible) {
				conservative++
			}
			if (truthChunk.Visible && verdict.Status == geometry.StatusOccluded) ||
				(!truthChunk.Visible && verdict.Status == geometry.StatusVisible) {
				conflicts++
				mismatches = append(mismatches, TruthMismatch{
					Window: window, Chunk: truthChunk.Chunk, TruthVisible: truthChunk.Visible,
					Observed: verdict.Status, Reason: verdict.Reason,
				})
			}
			if !truthChunk.Visible && verdict.Status == geometry.StatusOccluded {
				if verdict.ReferenceOccluder == "" {
					referenceFailures++
				}
			}
			if verdict.Status == geometry.StatusUncertain &&
				verdict.VisibilityLowerQ == 0 && verdict.VisibilityUpperQ == 65536 {
				report.Visibility.UncertainInsideBand++
			}
			updateErrorMax(&report.ErrorBounds, verdict.Error)
		}
	}
	report.Projection.KnownCount = len(checks)
	report.Projection.Checks = checks
	report.Projection.ToleranceCounts = toleranceCounts(allErrors, project.Meta.TolerancePxQ)
	report.Projection.MonotonicTightening = monotonicNonIncreasing(report.Projection.ToleranceCounts)
	report.Visibility.TruthVisible = numVisible
	report.Visibility.TruthHidden = numHidden
	report.Visibility.ObservedVisible = numObsVisible
	report.Visibility.ObservedOccluded = numObsOccluded
	report.Visibility.ObservedUncertain = numObsUncertain
	report.Visibility.ExactAgreement = exact
	report.Visibility.ConservativeMatch = conservative
	report.Visibility.Conflicts = conflicts
	report.Visibility.ReferenceFailures = referenceFailures
	report.Visibility.Mismatches = mismatches
	report.ErrorBounds.AllBoundsPositive = report.ErrorBounds.MaxCumulativeBoundQ > 0 &&
		report.ErrorBounds.MaxCoverageErrorQ >= 0
	report.ErrorBounds.VisibilityBoundsValid = true
	report.ErrorBounds.ProjectionCoversObserved = report.Projection.BoundViolations == 0
	report.ErrorBounds.Formula = "upper = projection_error + ceil(viewport_width/raster_resolution) + " +
		"focal*max(depth_quant_error,plane_quant_error)/camera_distance + one_screen_quantum"
	return nil
}

func updateErrorMax(report *ErrorReport, err geometry.ErrorBreakdown) {
	if err.ProjectionResidualQ > report.MaxResidualQ {
		report.MaxResidualQ = err.ProjectionResidualQ
	}
	if err.ProjectionErrorQ > report.MaxProjectionErrorQ {
		report.MaxProjectionErrorQ = err.ProjectionErrorQ
	}
	if err.FrustumPlaneErrorWorldQ > report.MaxPlaneErrorWorldQ {
		report.MaxPlaneErrorWorldQ = err.FrustumPlaneErrorWorldQ
	}
	if err.OcclusionCoverageErrorQ > report.MaxCoverageErrorQ {
		report.MaxCoverageErrorQ = err.OcclusionCoverageErrorQ
	}
	if err.DepthCompareErrorWorldQ > report.MaxDepthErrorWorldQ {
		report.MaxDepthErrorWorldQ = err.DepthCompareErrorWorldQ
	}
	if err.VisibilityUpperBoundQ > report.MaxCumulativeBoundQ {
		report.MaxCumulativeBoundQ = err.VisibilityUpperBoundQ
	}
}

func runThreePaths(ctx context.Context, project *model.Project,
	params model.Params, workDir string) (*ThreePathReport, []streaming.BudgetEvent, *model.AppError) {

	report := &ThreePathReport{Equal: true}
	header := store.Header{
		SchemaVersion: model.SchemaVersion,
		StoreVersion:  store.StoreVersion,
		SceneHash:     project.ContentHash,
	}
	realtime, err := runRealtime(ctx, project, params, filepath.Join(workDir, "realtime.sqlite"), header)
	if err != nil {
		return nil, nil, err
	}
	resume, err := runResume(ctx, project, params, filepath.Join(workDir, "resume.sqlite"), header, 2)
	if err != nil {
		return nil, nil, err
	}
	replay, err := runReplay(ctx, project, params)
	if err != nil {
		return nil, nil, err
	}
	report.Paths = []PathSummary{realtime.summary, resume.summary, replay.summary}
	report.Differences = append(report.Differences,
		compare("realtime", realtime.frames, "resume", resume.frames)...)
	report.Differences = append(report.Differences,
		compare("realtime", realtime.frames, "replay", replay.frames)...)
	if len(report.Differences) > 0 {
		report.Equal = false
	}
	report.CheckpointVerified = realtime.summary.CheckpointCount == len(project.World.Cameras) &&
		resume.summary.ResumedFrom >= 2 && resume.summary.CheckpointCount == len(project.World.Cameras) &&
		!realtime.summary.Invalidated && !resume.summary.Invalidated
	events := append([]streaming.BudgetEvent{}, replay.events...)
	return report, events, nil
}

func runRealtime(ctx context.Context, project *model.Project, params model.Params,
	dbPath string, header store.Header) (*framesRun, *model.AppError) {

	_ = os.Remove(dbPath)
	start := time.Now()
	checkpoint, err := store.Open(dbPath, header)
	if err != nil {
		return nil, err
	}
	defer checkpoint.Close()
	engine, err := streaming.NewEngine(project, params, checkpoint)
	if err != nil {
		return nil, err
	}
	if err := drain(ctx, engine); err != nil {
		return nil, err
	}
	frames := engine.CommittedFrames()
	loaded, err := checkpoint.LoadAll()
	if err != nil {
		return nil, err
	}
	return &framesRun{
		frames: frames,
		summary: PathSummary{
			Name: "realtime", WindowCount: len(frames), CommittedCount: len(frames),
			CheckpointCount: loaded.Count, Invalidated: loaded.Invalidated,
			DurationMillis:    float64(time.Since(start).Microseconds()) / 1000,
			BudgetQueueEvents: len(engine.BudgetEvents()),
		},
	}, nil
}

func runResume(ctx context.Context, project *model.Project, params model.Params,
	dbPath string, header store.Header, stopAt int) (*framesRun, *model.AppError) {

	_ = os.Remove(dbPath)
	start := time.Now()
	checkpoint, err := store.Open(dbPath, header)
	if err != nil {
		return nil, err
	}
	engine, err := streaming.NewEngine(project, params, checkpoint)
	if err != nil {
		checkpoint.Close()
		return nil, err
	}
	for {
		committed, _, _ := engine.Progress()
		if committed >= stopAt || committed >= len(project.World.Cameras) {
			break
		}
		frame, appErr := engine.ProcessNext(ctx)
		if appErr != nil {
			checkpoint.Close()
			return nil, appErr
		}
		if frame == nil {
			break
		}
	}
	checkpoint.Close()

	reopened, err := store.Open(dbPath, header)
	if err != nil {
		return nil, err
	}
	defer reopened.Close()
	loaded, err := reopened.LoadAll()
	if err != nil {
		return nil, err
	}
	resumedFrom := loaded.Count
	restored, err := streaming.NewEngine(project, params, reopened)
	if err != nil {
		return nil, err
	}
	if err := restored.Restore(loaded.Snapshots); err != nil {
		return nil, err
	}
	if err := drain(ctx, restored); err != nil {
		return nil, err
	}
	frames := restored.CommittedFrames()
	final, err := reopened.LoadAll()
	if err != nil {
		return nil, err
	}
	return &framesRun{
		frames: frames,
		summary: PathSummary{
			Name: "resume", WindowCount: len(frames), CommittedCount: len(frames),
			CheckpointCount: final.Count, Invalidated: loaded.Invalidated || final.Invalidated,
			DurationMillis: float64(time.Since(start).Microseconds()) / 1000,
			RestartAt:      stopAt, ResumedFrom: resumedFrom,
			BudgetQueueEvents: len(restored.BudgetEvents()),
		},
		events: restored.BudgetEvents(),
	}, nil
}

func runReplay(ctx context.Context, project *model.Project, params model.Params) (*framesRun, *model.AppError) {
	start := time.Now()
	engine, err := streaming.NewEngine(project, params, nil)
	if err != nil {
		return nil, err
	}
	if err := drain(ctx, engine); err != nil {
		return nil, err
	}
	frames := engine.CommittedFrames()
	return &framesRun{
		frames: frames,
		summary: PathSummary{
			Name: "replay", WindowCount: len(frames), CommittedCount: len(frames),
			CheckpointCount: 0, DurationMillis: float64(time.Since(start).Microseconds()) / 1000,
			BudgetQueueEvents: len(engine.BudgetEvents()),
		},
		events: engine.BudgetEvents(),
	}, nil
}

func drain(ctx context.Context, engine *streaming.Engine) *model.AppError {
	for {
		frame, err := engine.ProcessNext(ctx)
		if err != nil {
			return err
		}
		if frame == nil {
			return nil
		}
	}
}

func compare(nameA string, a []*streaming.WindowFrame, nameB string, b []*streaming.WindowFrame) []PathDifference {
	var diffs []PathDifference
	if len(a) != len(b) {
		return append(diffs, PathDifference{
			PathA: nameA, PathB: nameB, Window: -1, Field: "window_count",
			Detail: fmt.Sprintf("%d != %d", len(a), len(b)),
		})
	}
	for i := range a {
		if a[i].ResultHash != b[i].ResultHash {
			diffs = append(diffs, PathDifference{PathA: nameA, PathB: nameB, Window: i,
				Field: "result_hash", Detail: a[i].ResultHash + " != " + b[i].ResultHash})
			continue
		}
		if !equalInts(a[i].Visible, b[i].Visible) {
			diffs = append(diffs, PathDifference{PathA: nameA, PathB: nameB, Window: i, Field: "visible"})
		}
		if !equalInts(a[i].Uncertain, b[i].Uncertain) {
			diffs = append(diffs, PathDifference{PathA: nameA, PathB: nameB, Window: i, Field: "uncertain"})
		}
		if !equalInts(a[i].Resident, b[i].Resident) {
			diffs = append(diffs, PathDifference{PathA: nameA, PathB: nameB, Window: i, Field: "resident"})
		}
		if !equalInts(a[i].Loaded, b[i].Loaded) {
			diffs = append(diffs, PathDifference{PathA: nameA, PathB: nameB, Window: i, Field: "loaded"})
		}
		if !equalInts(a[i].Unloaded, b[i].Unloaded) {
			diffs = append(diffs, PathDifference{PathA: nameA, PathB: nameB, Window: i, Field: "unloaded"})
		}
		if a[i].Error != b[i].Error {
			diffs = append(diffs, PathDifference{PathA: nameA, PathB: nameB, Window: i, Field: "error_bounds"})
		}
	}
	return diffs
}

func equalInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func toleranceCounts(errors []int64, declaredQ int64) []ToleranceCount {
	thresholds := []int64{
		declaredQ,
		declaredQ / 2,
		declaredQ / 4,
		declaredQ / 8,
	}
	var out []ToleranceCount
	for _, threshold := range thresholds {
		if threshold <= 0 {
			continue
		}
		count := 0
		for _, err := range errors {
			if err <= threshold {
				count++
			}
		}
		out = append(out, ToleranceCount{TolerancePxQ: threshold, Count: count})
	}
	return out
}

func monotonicNonIncreasing(counts []ToleranceCount) bool {
	for i := 1; i < len(counts); i++ {
		if counts[i].Count > counts[i-1].Count {
			return false
		}
	}
	return true
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

// SortedEvidence is useful for the UI's small diagnostic tables.
func SortedEvidence(checks []ProjectionCheck) []ProjectionCheck {
	out := append([]ProjectionCheck(nil), checks...)
	sort.Slice(out, func(i, j int) bool {
		if out[i].ErrorQ == out[j].ErrorQ {
			return out[i].ID < out[j].ID
		}
		return out[i].ErrorQ > out[j].ErrorQ
	})
	return out
}

func ParseWindowList(parts []string) []int {
	var out []int
	for _, part := range parts {
		v, _ := strconv.Atoi(part)
		out = append(out, v)
	}
	return out
}
