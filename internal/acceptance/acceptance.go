package acceptance

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"time"

	"lightprobe/internal/checkpoint"
	"lightprobe/internal/diag"
	"lightprobe/internal/probe"
	"lightprobe/internal/scene"
)

type Options struct {
	WorkDir        string
	MaxParallelism int
	KeepCheckpoint bool
}

type CaseResult struct {
	Name     string      `json:"name"`
	Pass     bool        `json:"pass"`
	Code     string      `json:"code,omitempty"`
	Detail   string      `json:"detail,omitempty"`
	Mismatch *diag.Error `json:"mismatch,omitempty"`
}

type QueueSummary struct {
	Events    int   `json:"events"`
	WaitNanos int64 `json:"wait_nanos"`
}

type Report struct {
	OK                     bool                     `json:"ok"`
	GeneratedAt            time.Time                `json:"generated_at"`
	Seed                   int64                    `json:"seed"`
	RoomCount              int                      `json:"room_count"`
	ProbeCount             int                      `json:"probe_count"`
	PixelCount             int                      `json:"pixel_count"`
	Config                 probe.Config             `json:"config"`
	Cases                  []CaseResult             `json:"cases"`
	Precision              *probe.PrecisionReport   `json:"precision,omitempty"`
	Queue                  QueueSummary             `json:"queue"`
	Invalidation           []probe.InvalidationItem `json:"invalidation,omitempty"`
	ResolutionInvalidation []probe.InvalidationItem `json:"resolution_invalidation,omitempty"`
	Recovery               *probe.RecoveryReport    `json:"recovery,omitempty"`
	Progress               probe.Progress           `json:"progress"`
	WorkDir                string                   `json:"work_dir,omitempty"`
}

func Run(options Options) (*Report, error) {
	ctx := context.Background()
	workDir := options.WorkDir
	if workDir == "" {
		var err error
		workDir, err = os.MkdirTemp("", "lightprobe-acceptance-")
		if err != nil {
			return nil, err
		}
	} else if err := os.MkdirAll(workDir, 0o755); err != nil {
		return nil, err
	}

	cfg := probe.DefaultConfig()
	if options.MaxParallelism > 0 {
		cfg.MaxParallelism = options.MaxParallelism
	}
	cfg = cfg.Normalize()

	baseScene := scene.Generate()
	report := &Report{
		OK: true, GeneratedAt: time.Now().UTC(), Seed: baseScene.Seed,
		RoomCount: len(baseScene.Rooms), ProbeCount: len(baseScene.Probes),
		PixelCount: len(baseScene.Pixels), Config: cfg, WorkDir: workDir,
	}

	addCase := func(name string, err error, detail string) {
		result := CaseResult{Name: name, Pass: err == nil, Detail: detail}
		if err != nil {
			report.OK = false
			var de *diag.Error
			if errors.As(err, &de) {
				result.Code = de.Code
				result.Mismatch = de
			} else {
				result.Code = "ERROR"
				result.Detail = err.Error()
			}
		}
		report.Cases = append(report.Cases, result)
	}

	baseline, err := probe.NewEngine(scene.Generate(), cfg)
	if err != nil {
		return nil, err
	}
	if err := baseline.BuildAll(ctx); err != nil {
		addCase("baseline_build", err, "baseline build returned probe fault")
	}
	precision, err := baseline.VerifyTruth()
	report.Precision = precision
	addCase("truth_precision", err, "probe interpolation, pixel shading, and boundary jump tolerances")

	serialCfg := probe.Config{MaxParallelism: 1, ProbeBufferPerWorker: 1, SHBufferCoeffBudget: scene.SHCoefficientCount * 3}
	serial, err := probe.NewEngine(scene.Generate(), serialCfg)
	if err != nil {
		return nil, err
	}
	if err := serial.BuildAll(ctx); err != nil {
		addCase("serial_build", err, "")
	}
	parallelCfg := probe.Config{MaxParallelism: 4, ProbeBufferPerWorker: 1, SHBufferCoeffBudget: scene.SHCoefficientCount * 3}
	parallel, err := probe.NewEngine(scene.Generate(), parallelCfg)
	if err != nil {
		return nil, err
	}
	if err := parallel.BuildAll(ctx); err != nil {
		addCase("parallel_build", err, "")
	}
	err = probe.CompareSnapshots(serial.Snapshot(), parallel.Snapshot())
	addCase("single_vs_parallel_bit_exact", err, "SH fixed coefficients and reconstructed radiance")
	err = compareSubmissionIDs(serial.Submissions(), parallel.Submissions())
	addCase("parallel_stable_submission_order", err, "serial and parallel probe submission IDs")
	events := parallel.MetricsEvents()
	report.Queue = summarizeQueue(events)

	fullEngine, err := probe.NewEngine(scene.Generate(), cfg)
	if err != nil {
		return nil, err
	}
	err = fullEngine.BuildAndCommitAll(ctx)
	addCase("full_build_commit", err, "one-shot full reconstruction and room commit sequence")
	report.Progress = fullEngine.Progress()

	incScene := scene.Generate()
	incEngine, err := probe.NewEngine(incScene, cfg)
	if err != nil {
		return nil, err
	}
	if err := incEngine.BuildAll(ctx); err != nil {
		addCase("incremental_initial_build", err, "")
	}
	if err := incEngine.CommitRoom(ctx, "room-a"); err != nil {
		addCase("incremental_commit_a", err, "")
	}
	if err := incEngine.CommitRoom(ctx, "room-b"); err != nil {
		addCase("incremental_commit_b", err, "")
	}
	target := "probe:room-c:0:0:0"
	targetRecord, err := incEngine.Record(target)
	if err != nil {
		addCase("incremental_target_exists", err, target)
	}
	originalRadiance := targetRecord.Radiance
	editedRadiance := plusRadiance(originalRadiance, scene.Vec3{0.16, -0.07, 0.05})
	plan, err := incEngine.EditRadiance(target, editedRadiance)
	if err != nil {
		addCase("incremental_edit", err, target)
	} else {
		report.Invalidation = plan.Invalidation
	}
	incReport, err := incEngine.RecomputeInvalid(ctx)
	if incReport != nil {
		report.Queue.Events += len(incReport.QueueEvents)
		for _, event := range incReport.QueueEvents {
			report.Queue.WaitNanos += event.WaitNanos
		}
	}
	addCase("incremental_recompute", err, "one edited probe invalidates exact downstream nodes")
	if incReport != nil {
		if incReport.Jobs != 1 || incReport.CacheHits != report.ProbeCount-1 {
			report.Cases = append(report.Cases, CaseResult{
				Name: "incremental_cache_precision", Pass: false,
				Detail: "expected 1 job and 41 cache hits",
			})
			report.OK = false
		} else if len(incReport.PixelsRecomputed) != 1 || incReport.PixelsRecomputed[0] != "pixel-ac-boundary" {
			report.Cases = append(report.Cases, CaseResult{
				Name: "incremental_pixel_scope", Pass: false,
				Detail: "only pixel-ac-boundary should recompute",
			})
			report.OK = false
		} else {
			report.Cases = append(report.Cases, CaseResult{
				Name: "incremental_cache_precision", Pass: true,
				Detail: "1 probe recomputed, 41 probe cache hits, 3 pixel cache hits",
			})
		}
	}

	editedScene, err := probe.CloneScene(incEngine.Scene())
	if err != nil {
		return nil, err
	}
	editedFull, err := probe.NewEngine(editedScene, cfg)
	if err != nil {
		return nil, err
	}
	if err := editedFull.BuildAll(ctx); err != nil {
		addCase("edited_full_build", err, "")
	}
	if err := editedFull.CommitRoom(ctx, "room-a"); err != nil {
		addCase("edited_full_commit_a", err, "")
	}
	if err := editedFull.CommitRoom(ctx, "room-b"); err != nil {
		addCase("edited_full_commit_b", err, "")
	}
	err = probe.CompareSnapshots(editedFull.Snapshot(), incEngine.Snapshot())
	addCase("incremental_vs_empty_full_after_edit", err, "bit-exact probe and pixel values")

	_, err = incEngine.EditRadiance(target, originalRadiance)
	addCase("incremental_revert_edit", err, "")
	_, err = incEngine.RecomputeInvalid(ctx)
	addCase("incremental_revert_recompute", err, "")
	err = incEngine.CommitRoom(ctx, "room-c")
	addCase("incremental_commit_c", err, "")
	err = probe.CompareSnapshots(fullEngine.Snapshot(), incEngine.Snapshot())
	addCase("three_way_incremental_matches_full", err, "reverted incremental final state matches full commit sequence")

	checkpointPath := filepath.Join(workDir, "lightprobe.db")
	_ = checkpoint.RemoveFiles(checkpointPath)
	prefixEngine, err := probe.NewEngine(scene.Generate(), cfg)
	if err != nil {
		return nil, err
	}
	if err := prefixEngine.EnableCheckpoint(ctx, checkpointPath); err != nil {
		addCase("checkpoint_open", err, "")
	}
	if err := prefixEngine.BuildAll(ctx); err != nil {
		addCase("checkpoint_prefix_build", err, "")
	}
	if err := prefixEngine.CommitRoom(ctx, "room-a"); err != nil {
		addCase("checkpoint_commit_a", err, "")
	}
	if err := prefixEngine.CommitRoom(ctx, "room-b"); err != nil {
		addCase("checkpoint_commit_b", err, "")
	}
	if err := prefixEngine.Close(); err != nil {
		addCase("checkpoint_prefix_close", err, "")
	}

	recoveryCfg := cfg
	recoveryCfg.CheckpointPath = checkpointPath
	recoveryEngine, err := probe.NewEngine(scene.Generate(), recoveryCfg)
	if err != nil {
		return nil, err
	}
	recoveryReport, err := recoveryEngine.RecoverAndResume(ctx)
	report.Recovery = recoveryReport
	addCase("checkpoint_recover_resume", err, "valid SQLite snapshot restores room-a/b and rebuilds room-c")
	err = probe.CompareSnapshots(fullEngine.Snapshot(), recoveryEngine.Snapshot())
	addCase("recovery_matches_full", err, "recovered/restarted path matches one-shot full rebuild")
	if recoveryReport == nil ||
		(!recoveryReport.Reset && len(recoveryReport.RestoredRooms) != 2) ||
		len(recoveryReport.RebuiltRooms) != 1 {
		report.Cases = append(report.Cases, CaseResult{Name: "recovery_scope", Pass: false, Detail: "unexpected recovery scope"})
		report.OK = false
	} else {
		report.Cases = append(report.Cases, CaseResult{Name: "recovery_scope", Pass: true, Detail: "2 rooms restored, 1 rebuilt"})
	}
	if err := recoveryEngine.Close(); err != nil {
		addCase("recovery_close", err, "")
	}

	mismatchStore, err := checkpoint.Open(checkpointPath)
	if err == nil {
		_, loadErr := mismatchStore.Load(ctx, "wrong-hash", scene.SamplingParameterVersion, scene.AlgorithmVersion)
		if errors.Is(loadErr, checkpoint.ErrSnapshotMismatch) {
			addCase("checkpoint_hash_mismatch_detected", nil, "E_DIFF/checkpoint: wrong input hash invalidates snapshot")
		} else {
			addCase("checkpoint_hash_mismatch_detected", loadErr, "expected checkpoint.ErrSnapshotMismatch")
		}
		_ = mismatchStore.Close()
		_, snapshots, reset, resetErr := checkpoint.OpenValidOrReset(ctx, checkpointPath, "wrong-hash", scene.SamplingParameterVersion, scene.AlgorithmVersion)
		if resetErr != nil {
			addCase("checkpoint_mismatch_full_reset", resetErr, "")
		} else if !reset || snapshots != nil {
			addCase("checkpoint_mismatch_full_reset", nil, "mismatch did not reset snapshot")
			report.Cases = append(report.Cases, CaseResult{Name: "checkpoint_mismatch_full_reset", Pass: false, Detail: "snapshot was not discarded"})
			report.OK = false
		} else {
			report.Cases = append(report.Cases, CaseResult{Name: "checkpoint_mismatch_full_reset", Pass: true, Detail: "mismatched snapshot discarded for full rebuild"})
		}
	} else {
		addCase("checkpoint_hash_mismatch_detected", err, "open checkpoint for mismatch test")
	}

	resEngine, err := probe.NewEngine(scene.Generate(), cfg)
	if err != nil {
		return nil, err
	}
	if err := resEngine.BuildAll(ctx); err != nil {
		addCase("resolution_initial_build", err, "")
	}
	resPlan, err := resEngine.SetResolution("room-c", scene.Resolution{X: 3, Y: 3, Z: 2})
	if err != nil {
		addCase("resolution_change", err, "")
	} else {
		report.ResolutionInvalidation = resPlan.Invalidation
	}
	resReport, err := resEngine.RecomputeInvalid(ctx)
	addCase("resolution_recompute", err, "")
	if resReport != nil && (resReport.Jobs != 18 || resReport.CacheHits != 30) {
		report.Cases = append(report.Cases, CaseResult{
			Name: "resolution_cache_scope", Pass: false,
			Detail: "expected 18 new room-c jobs and 30 unchanged room-a/b cache hits",
		})
		report.OK = false
	} else {
		report.Cases = append(report.Cases, CaseResult{
			Name: "resolution_cache_scope", Pass: true,
			Detail: "room-c rebuilt; room-a/b reused",
		})
	}
	resizedScene, err := probe.CloneScene(resEngine.Scene())
	if err != nil {
		return nil, err
	}
	resizedFull, err := probe.NewEngine(resizedScene, cfg)
	if err != nil {
		return nil, err
	}
	if err := resizedFull.BuildAll(ctx); err != nil {
		addCase("resolution_full_build", err, "")
	}
	err = probe.CompareSnapshots(resizedFull.Snapshot(), resEngine.Snapshot())
	addCase("resolution_incremental_matches_full", err, "")

	faultEngine, err := probe.NewEngine(scene.Generate(), cfg)
	if err != nil {
		return nil, err
	}
	faultProbe := "probe:room-c:1:0:1"
	if err := faultEngine.InjectProbeFault(faultProbe, 1); err != nil {
		addCase("fault_inject", err, faultProbe)
	}
	_ = faultEngine.BuildAll(ctx)
	failedRecord, recordErr := faultEngine.Record(faultProbe)
	if recordErr != nil || failedRecord.Status != probe.ProbeFailed {
		report.Cases = append(report.Cases, CaseResult{Name: "single_probe_isolation", Pass: false, Detail: "injected probe was not marked failed"})
		report.OK = false
	} else {
		other, _ := faultEngine.Record("probe:room-a:0:0:0")
		if other == nil || other.Status != probe.ProbeReconstructed {
			report.Cases = append(report.Cases, CaseResult{Name: "single_probe_isolation", Pass: false, Detail: "other probe was polluted"})
			report.OK = false
		} else {
			report.Cases = append(report.Cases, CaseResult{Name: "single_probe_isolation", Pass: true, Detail: "only injected probe failed; other probes reconstructed"})
		}
	}
	_, retryErr := faultEngine.RetryProbe(ctx, faultProbe)
	addCase("single_probe_retry", retryErr, faultProbe)
	err = probe.CompareSnapshots(baseline.Snapshot(), faultEngine.Snapshot())
	addCase("fault_retry_matches_baseline", err, "retry replaces only failed grid point and restores exact state")

	runErrorCases(ctx, addCase, fullEngine)

	for _, c := range report.Cases {
		if !c.Pass {
			report.OK = false
			break
		}
	}
	return report, nil
}

func runErrorCases(ctx context.Context, addCase func(string, error, string), committed *probe.Engine) {
	_, err := scene.ParseScene([]byte(`{"schema_version":"lightprobe.scene.v1"}`))
	expectCode("schema_missing_field", diag.CodeSchema, err, addCase)

	overlap := scene.Generate()
	for i := range overlap.Rooms {
		if overlap.Rooms[i].ID == "room-c" {
			overlap.Rooms[i].Bounds = scene.AABB{Min: scene.Vec3{1, 1, 0}, Max: scene.Vec3{2, 2, 3}}
		}
	}
	expectCode("geom_overlap", diag.CodeGeom, scene.Validate(overlap), addCase)

	negative := scene.Generate()
	negative.Probes[0].VisibilitySamples = -1
	expectCode("visibility_non_negative", diag.CodeSchema, scene.Validate(negative), addCase)

	missing := scene.Generate()
	missing.Probes = missing.Probes[1:]
	expectCode("probe_count_resolution", diag.CodeGeom, scene.Validate(missing), addCase)

	_, rangeErr := committed.Interpolate("room-a", scene.Vec3{99, 99, 99})
	expectCode("range_outside_query", diag.CodeRange, rangeErr, addCase)

	fresh, _ := probe.NewEngine(scene.Generate(), probe.DefaultConfig())
	expectCode("state_illegal_commit", diag.CodeState, fresh.CommitRoom(ctx, "room-a"), addCase)

	_, verErr := committed.EditRadiance("probe:room-a:0:0:0", scene.Vec3{1, 1, 1})
	expectCode("version_committed_room", diag.CodeVer, verErr, addCase)
}

func expectCode(name, code string, err error, addCase func(string, error, string)) {
	de, ok := diag.AsError(err)
	if !ok {
		addCase(name, err, "expected "+code)
		return
	}
	if de.Code != code {
		addCase(name, de, "expected code "+code+" got "+de.Code)
		return
	}
	addCase(name, nil, code+" returned with room/field context")
}

func compareSubmissionIDs(a, b []probe.Submission) error {
	if len(a) != len(b) {
		return diag.New(diag.CodeDiff, "submission length differs: %d != %d", len(a), len(b))
	}
	for i := range a {
		if a[i].ProbeID != b[i].ProbeID {
			return diag.New(diag.CodeDiff, "submission mismatch at %d: %s != %s", i, a[i].ProbeID, b[i].ProbeID)
		}
	}
	return nil
}

func summarizeQueue(events []probe.QueueEvent) QueueSummary {
	summary := QueueSummary{Events: len(events)}
	for _, event := range events {
		summary.WaitNanos += event.WaitNanos
	}
	return summary
}

func plusRadiance(a, b scene.Vec3) scene.Vec3 {
	var out scene.Vec3
	for i := 0; i < 3; i++ {
		out[i] = a[i] + b[i]
	}
	return out
}
