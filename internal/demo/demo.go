package demo

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"maskpreview/internal/core"
	"maskpreview/internal/engine"
	"maskpreview/internal/sample"
	"maskpreview/internal/segment"
	"maskpreview/internal/verifier"
)

type Config struct {
	SampleDir   string
	ArtifactDir string
	ReportPath  string
	Seed        int64
	Run         engine.RunConfig
}

type PathSummary struct {
	Mode              string                `json:"mode"`
	MaskHash          string                `json:"mask_hash"`
	RegionCount       int                   `json:"region_count"`
	StageCount        int                   `json:"stage_count"`
	Progress          engine.Progress       `json:"progress"`
	TruthCentroid     float64               `json:"truth_centroid_deviation"`
	TruthAreaRelative float64               `json:"truth_area_relative"`
	TruthMatches      []segment.RegionDelta `json:"truth_matches,omitempty"`
	CheckpointReused  bool                  `json:"checkpoint_reused"`
	InvalidatedReason string                `json:"invalidated_reason,omitempty"`
	Scheduler         engine.SchedulerStats `json:"scheduler"`
	Regions           []segment.RegionStat  `json:"regions"`
}

type PreviewSummary struct {
	SnapshotVersion       string  `json:"snapshot_version"`
	DownsampleFactor      int     `json:"downsample_factor"`
	GrowthIterations      int     `json:"growth_iterations"`
	BudgetMS              int     `json:"budget_ms"`
	ElapsedMS             float64 `json:"elapsed_ms"`
	WithinBudget          bool    `json:"within_budget"`
	StatisticsPlaceholder bool    `json:"statistics_placeholder"`
	RegionCount           int     `json:"region_count"`
	CentroidDeviation     float64 `json:"centroid_deviation"`
	AreaRelative          float64 `json:"area_relative"`
	FirstDifferenceX      int     `json:"first_difference_x"`
	FirstDifferenceY      int     `json:"first_difference_y"`
	HasPixelDifference    bool    `json:"has_pixel_difference"`
	Rationale             string  `json:"rationale"`
}

type SampleReport struct {
	ID                      string         `json:"id"`
	Width                   int            `json:"width"`
	Height                  int            `json:"height"`
	SnapshotVersion         string         `json:"snapshot_version"`
	Preview                 PreviewSummary `json:"preview"`
	Full                    PathSummary    `json:"full"`
	Incremental             PathSummary    `json:"incremental"`
	Resume                  PathSummary    `json:"resume"`
	PixelConsistent         bool           `json:"pixel_consistent"`
	RegionStatsConsistent   bool           `json:"region_stats_consistent"`
	StageSequenceConsistent bool           `json:"stage_sequence_consistent"`
}

type ExceptionProbe struct {
	Name         string `json:"name"`
	ExpectedCode string `json:"expected_code"`
	ActualCode   string `json:"actual_code"`
	OK           bool   `json:"ok"`
	Message      string `json:"message"`
}

type ResourceProbe struct {
	MaxParallel      int                 `json:"max_parallel"`
	MaxPerBlockBytes int64               `json:"max_per_block_bytes"`
	GlobalBytes      int64               `json:"global_bytes"`
	CompletedJobs    int                 `json:"completed_jobs"`
	MaxActive        int                 `json:"max_active_observed"`
	QueuedEvents     []engine.QueueEvent `json:"queued_events"`
	OK               bool                `json:"ok"`
}

type TamperProbe struct {
	InvalidatedReason string `json:"invalidated_reason"`
	CheckpointReused  bool   `json:"checkpoint_reused"`
	RecoveredMaskHash string `json:"recovered_mask_hash"`
	ExpectedMaskHash  string `json:"expected_mask_hash"`
	OK                bool   `json:"ok"`
}

type Report struct {
	Schema         string                      `json:"schema"`
	Status         string                      `json:"status"`
	Seed           int64                       `json:"seed"`
	GeneratedAtUTC string                      `json:"generated_at_utc"`
	SampleDir      string                      `json:"sample_dir"`
	ArtifactDir    string                      `json:"artifact_dir"`
	ReportPath     string                      `json:"report_path"`
	FullDB         string                      `json:"full_db"`
	IncrementalDB  string                      `json:"incremental_db"`
	BlockSize      int                         `json:"block_size"`
	Samples        []SampleReport              `json:"samples"`
	Verification   map[string]*verifier.Report `json:"verification"`
	Exceptions     []ExceptionProbe            `json:"exceptions"`
	ResourceBudget ResourceProbe               `json:"resource_budget"`
	TamperRecovery TamperProbe                 `json:"tamper_recovery"`
}

func Run(ctx context.Context, cfg Config) (*Report, error) {
	if cfg.SampleDir == "" {
		cfg.SampleDir = "samples"
	}
	if cfg.ArtifactDir == "" {
		cfg.ArtifactDir = "artifacts"
	}
	if cfg.ReportPath == "" {
		cfg.ReportPath = filepath.Join(cfg.ArtifactDir, "run-report.json")
	}
	if cfg.Seed == 0 {
		cfg.Seed = 424242
	}
	if cfg.Run.BlockSize == 0 {
		cfg.Run = engine.DefaultRunConfig()
	}
	if err := os.MkdirAll(cfg.SampleDir, 0o755); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(cfg.ArtifactDir, 0o755); err != nil {
		return nil, err
	}
	fullDB := filepath.Join(cfg.ArtifactDir, "full.db")
	incDB := filepath.Join(cfg.ArtifactDir, "incremental.db")
	tamperDB := filepath.Join(cfg.ArtifactDir, "tamper.db")
	for _, path := range []string{fullDB, incDB, tamperDB} {
		removeSQLiteFiles(path)
	}

	manifest, err := sample.Generate(sample.GenerateConfig{
		Seed: cfg.Seed, OutDir: cfg.SampleDir, Sizes: []int{64, 96, 144},
	})
	if err != nil {
		return nil, err
	}
	fullStore, err := engine.OpenStore(fullDB)
	if err != nil {
		return nil, err
	}
	defer fullStore.Close()
	incStore, err := engine.OpenStore(incDB)
	if err != nil {
		return nil, err
	}
	defer incStore.Close()

	report := &Report{
		Schema:         "maskpreview/demo-report/v1",
		Status:         "pass",
		Seed:           cfg.Seed,
		GeneratedAtUTC: time.Now().UTC().Format(time.RFC3339),
		SampleDir:      cfg.SampleDir,
		ArtifactDir:    cfg.ArtifactDir,
		ReportPath:     cfg.ReportPath,
		FullDB:         fullDB,
		IncrementalDB:  incDB,
		BlockSize:      cfg.Run.BlockSize,
		Verification:   map[string]*verifier.Report{},
	}

	var firstLoaded *sample.LoadedSample
	var firstSpec core.SampleSpec
	var firstSnap core.Snapshot
	var firstFull *engine.RunResult
	var firstPreview *segment.PreviewResult
	for _, spec := range manifest.Samples {
		loaded, err := sample.LoadSample(cfg.SampleDir, spec)
		if err != nil {
			return nil, err
		}
		snapshot, err := core.NewSnapshot(spec.ID, spec.ImageSHA256, spec.DefaultParams, spec.Seeds, spec.Quantization)
		if err != nil {
			return nil, err
		}
		full, err := engine.Execute(ctx, loaded.Image, snapshot, spec.TruthFingerprint, engine.ModeFull, fullStore, cfg.Run)
		if err != nil {
			return nil, fmt.Errorf("%s full: %w", spec.ID, err)
		}
		session := engine.NewSession(spec.PreviewTolerance)
		preview, err := session.Preview(loaded.Image, snapshot, spec.PreviewDelayMS)
		if err != nil {
			return nil, fmt.Errorf("%s preview: %w", spec.ID, err)
		}
		finalized, err := session.Finalize(ctx, loaded.Image, snapshot, spec.TruthFingerprint, incStore, cfg.Run)
		if err != nil {
			return nil, fmt.Errorf("%s finalize: %w", spec.ID, err)
		}
		resumed, err := engine.Execute(ctx, loaded.Image, snapshot, spec.TruthFingerprint, engine.ModeResume, incStore, cfg.Run)
		if err != nil {
			return nil, fmt.Errorf("%s resume: %w", spec.ID, err)
		}
		truthReport, _ := segment.CompareToTruth(full.Regions, spec.Regions, spec.PreviewTolerance)
		incTruth, _ := segment.CompareToTruth(finalized.Run.Regions, spec.Regions, spec.PreviewTolerance)
		resumeTruth, _ := segment.CompareToTruth(resumed.Regions, spec.Regions, spec.PreviewTolerance)
		sr := SampleReport{
			ID:              spec.ID,
			Width:           spec.Width,
			Height:          spec.Height,
			SnapshotVersion: snapshot.Version,
			Preview: PreviewSummary{
				SnapshotVersion:       preview.SnapshotVersion,
				DownsampleFactor:      preview.DownsampleFactor,
				GrowthIterations:      preview.GrowthIterations,
				BudgetMS:              preview.BudgetMS,
				ElapsedMS:             preview.ElapsedMS,
				WithinBudget:          preview.WithinBudget,
				StatisticsPlaceholder: preview.StatisticsPlaceholder,
				RegionCount:           finalized.Comparison.PreviewRegionCount,
				CentroidDeviation:     finalized.Comparison.CentroidDeviation,
				AreaRelative:          finalized.Comparison.AreaRelative,
				FirstDifferenceX:      finalized.Comparison.DifferenceX,
				FirstDifferenceY:      finalized.Comparison.DifferenceY,
				HasPixelDifference:    finalized.Comparison.HasPixelDifference,
				Rationale:             preview.Rationale,
			},
			Full:            pathSummary("full", full, truthReport),
			Incremental:     pathSummary("incremental", finalized.Run, incTruth),
			Resume:          pathSummary("resume", resumed, resumeTruth),
			PixelConsistent: full.MaskHash == finalized.Run.MaskHash && full.MaskHash == resumed.MaskHash,
			RegionStatsConsistent: regionsEqual(full.Regions, finalized.Run.Regions) &&
				regionsEqual(full.Regions, resumed.Regions),
			StageSequenceConsistent: engine.SequenceEqual(full.Records, finalized.Run.Records) &&
				engine.SequenceEqual(full.Records, resumed.Records),
		}
		if sr.Full.StageCount != sr.Incremental.StageCount || sr.Full.StageCount != sr.Resume.StageCount {
			sr.StageSequenceConsistent = false
		}
		if !preview.WithinBudget || finalized.Comparison == nil || !finalized.Comparison.OK ||
			truthReport == nil || !truthReport.WithinTolerance || incTruth == nil || !incTruth.WithinTolerance ||
			resumeTruth == nil || !resumeTruth.WithinTolerance || !sr.PixelConsistent ||
			!sr.RegionStatsConsistent || !sr.StageSequenceConsistent {
			report.Status = "fail"
		}
		if firstLoaded == nil {
			firstLoaded = loaded
			firstSpec = spec
			firstSnap = snapshot
			firstFull = full
			firstPreview = preview
		}
		report.Samples = append(report.Samples, sr)
	}
	if firstLoaded == nil {
		return nil, fmt.Errorf("no samples generated")
	}

	for name, db := range map[string]string{"full": fullDB, "incremental": incDB} {
		vr, err := verifier.Verify(cfg.SampleDir, db, "")
		if err != nil {
			return nil, fmt.Errorf("verify %s: %w", name, err)
		}
		report.Verification[name] = vr
		if vr.Status != "pass" {
			report.Status = "fail"
		}
	}
	report.Exceptions = runExceptionProbes(cfg.SampleDir, firstSpec, firstLoaded, firstSnap, firstFull, firstPreview)
	for _, probe := range report.Exceptions {
		if !probe.OK {
			report.Status = "fail"
		}
	}
	report.ResourceBudget = runResourceProbe()
	if !report.ResourceBudget.OK {
		report.Status = "fail"
	}
	tamper, err := runTamperProbe(ctx, cfg, tamperDB, firstSpec, firstLoaded, firstSnap, firstFull)
	if err != nil {
		return nil, err
	}
	report.TamperRecovery = tamper
	if !tamper.OK {
		report.Status = "fail"
	}
	if err := writeAtomic(cfg.ReportPath, mustJSON(report)); err != nil {
		return nil, err
	}
	return report, nil
}

func pathSummary(mode string, run *engine.RunResult, truth *segment.MatchReport) PathSummary {
	summary := PathSummary{
		Mode:              mode,
		MaskHash:          run.MaskHash,
		RegionCount:       run.RegionCount,
		StageCount:        len(run.Records),
		Progress:          run.Progress,
		CheckpointReused:  run.CheckpointReused,
		InvalidatedReason: run.InvalidatedReason,
		Scheduler:         run.Scheduler,
		Regions:           run.Regions,
	}
	if truth != nil {
		summary.TruthCentroid = truth.MaxCentroidDeviation
		summary.TruthAreaRelative = truth.MaxAreaRelative
		summary.TruthMatches = truth.Matches
	}
	return summary
}

func regionsEqual(a, b []segment.RegionStat) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].ID != b[i].ID || a[i].Area != b[i].Area || a[i].Centroid != b[i].Centroid || a[i].BBox != b[i].BBox {
			return false
		}
	}
	return true
}

func runExceptionProbes(dir string, spec core.SampleSpec, loaded *sample.LoadedSample, snap core.Snapshot, full *engine.RunResult, preview *segment.PreviewResult) []ExceptionProbe {
	var probes []ExceptionProbe
	record := func(name, code string, err error, mustContain ...string) {
		p := ExceptionProbe{Name: name, ExpectedCode: code, ActualCode: core.CodeOf(err)}
		if err != nil {
			p.Message = err.Error()
		}
		p.OK = p.ActualCode == code
		for _, fragment := range mustContain {
			if !strings.Contains(p.Message, fragment) {
				p.OK = false
			}
		}
		probes = append(probes, p)
	}

	badParams := spec.DefaultParams
	badParams.ColorTolerance = 999
	err := badParams.Validate()
	record("parameter_range", core.CodeParam, err, "color_tolerance", "[0,255]")

	mixed := spec.DefaultParams
	mixed.GradientThreshold++
	snap2, _ := core.NewSnapshot(spec.ID, spec.ImageSHA256, mixed, spec.Seeds, spec.Quantization)
	session := engine.NewSession(spec.PreviewTolerance)
	_, _ = session.Preview(loaded.Image, snap, spec.PreviewDelayMS)
	_, err = session.Finalize(context.Background(), loaded.Image, snap2, spec.TruthFingerprint, nil, engine.DefaultRunConfig())
	record("snapshot_mix", core.CodeVersion, err, "left=", "right=")

	fsm := engine.NewBlockFSM(1)
	err = fsm.Commit(engine.StageGrow, snap.Version, "digest")
	record("illegal_transition", core.CodeState, err, "state", "committed")

	extra := append([]segment.RegionStat(nil), full.Regions...)
	extra = append(extra, segment.RegionStat{ID: 99, Area: 7, Centroid: core.PointF{X: 1, Y: 1}, BBox: core.BBox{}})
	_, err = segment.CompareToTruth(extra, spec.Regions, spec.PreviewTolerance)
	record("region_count", core.CodeRegion, err, "extra_regions", "missing_regions")

	mangled := *preview
	mangled.ProjectedRegions = append([]segment.RegionStat(nil), preview.ProjectedRegions...)
	if len(mangled.ProjectedRegions) > 0 {
		mangled.ProjectedRegions[0].Area *= 3
		mangled.ProjectedRegions[0].Centroid.X += 12
	}
	_, err = segment.ComparePreview(full.Exact, &mangled, spec.PreviewTolerance)
	record("preview_divergence", core.CodeDiverge, err, "centroid_deviation", "area_relative", "difference_x")

	corruptSpec := spec
	corruptSpec.ImageFile = "probe_corrupt.png"
	corruptSpec.ImageSHA256 = ""
	probePath := filepath.Join(dir, corruptSpec.ImageFile)
	_ = os.WriteFile(probePath, []byte("broken"), 0o644)
	_, err = sample.LoadSample(dir, corruptSpec)
	_ = os.Remove(probePath)
	record("decode", core.CodeDecode, err)

	metaSpec := spec
	metaSpec.Channels = 3
	_, err = sample.LoadSample(dir, metaSpec)
	record("metadata", core.CodeMeta, err, "field=channels", "got=4", "want=3")
	return probes
}

func runResourceProbe() ResourceProbe {
	out := ResourceProbe{
		MaxParallel:      3,
		MaxPerBlockBytes: 100,
		GlobalBytes:      150,
	}
	scheduler, err := engine.NewScheduler(engine.Budget{
		MaxParallel:      out.MaxParallel,
		MaxPerBlockBytes: out.MaxPerBlockBytes,
		GlobalBytes:      out.GlobalBytes,
	})
	if err != nil {
		return out
	}
	jobs := make([]engine.Job, 0, 5)
	for i := 0; i < 5; i++ {
		i := i
		jobs = append(jobs, engine.Job{
			BlockID: 1 + i%2,
			Run: func(ctx context.Context) error {
				release, err := scheduler.Acquire(ctx, 1+i%2, 60, "probe-stage")
				if err != nil {
					return err
				}
				time.Sleep(3 * time.Millisecond)
				release()
				return nil
			},
		})
	}
	if err := scheduler.Run(context.Background(), jobs); err != nil {
		return out
	}
	stats := scheduler.Stats()
	out.CompletedJobs = stats.CompletedJobs
	out.MaxActive = stats.MaxActiveObserved
	out.QueuedEvents = stats.QueuedEvents
	out.OK = stats.CompletedJobs == len(jobs) && stats.MaxActiveObserved <= out.MaxParallel && len(stats.QueuedEvents) > 0
	return out
}

func runTamperProbe(ctx context.Context, cfg Config, path string, spec core.SampleSpec, loaded *sample.LoadedSample, snap core.Snapshot, full *engine.RunResult) (TamperProbe, error) {
	out := TamperProbe{ExpectedMaskHash: full.MaskHash}
	store, err := engine.OpenStore(path)
	if err != nil {
		return out, err
	}
	_, err = engine.Execute(ctx, loaded.Image, snap, spec.TruthFingerprint, engine.ModeFull, store, cfg.Run)
	if err != nil {
		store.Close()
		return out, err
	}
	if err := store.Close(); err != nil {
		return out, err
	}
	db, err := sql.Open("sqlite", "file:"+filepath.ToSlash(path)+"?_pragma=busy_timeout(5000)")
	if err != nil {
		return out, err
	}
	if _, err := db.Exec(`UPDATE stages SET payload = x'7b7d' WHERE canonical_index = 0`); err != nil {
		db.Close()
		return out, err
	}
	if err := db.Close(); err != nil {
		return out, err
	}
	store, err = engine.OpenStore(path)
	if err != nil {
		return out, err
	}
	defer store.Close()
	resumed, err := engine.Execute(ctx, loaded.Image, snap, spec.TruthFingerprint, engine.ModeResume, store, cfg.Run)
	if err != nil {
		return out, err
	}
	out.InvalidatedReason = resumed.InvalidatedReason
	out.CheckpointReused = resumed.CheckpointReused
	out.RecoveredMaskHash = resumed.MaskHash
	out.OK = resumed.InvalidatedReason != "" && !resumed.CheckpointReused && resumed.MaskHash == full.MaskHash
	return out, nil
}

func removeSQLiteFiles(path string) {
	for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
		_ = os.Remove(path + suffix)
	}
}

func mustJSON(v any) []byte {
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		panic(err)
	}
	return append(raw, '\n')
}

func writeAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".run-report-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	defer os.Remove(name)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(name, path)
}
