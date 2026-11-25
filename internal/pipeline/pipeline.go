package pipeline

import (
	"context"
	"path/filepath"
	"reflect"

	"semanticshading/internal/bounds"
	"semanticshading/internal/checkpoint"
	"semanticshading/internal/eval"
	"semanticshading/internal/model"
	"semanticshading/internal/runner"
	"semanticshading/internal/sample"
	"semanticshading/internal/verify"
)

type PathResult struct {
	Name             string                  `json:"name"`
	DBPath           string                  `json:"db_path"`
	ResetInfo        *checkpoint.ResetInfo   `json:"reset_info,omitempty"`
	RunReport        *runner.Report          `json:"run_report,omitempty"`
	InitialRunReport *runner.Report          `json:"initial_run_report,omitempty"`
	Regions          []model.RegionMetrics   `json:"regions"`
	Bounds           []model.ErrorBound      `json:"bounds"`
	Propagation      []model.PropagationPath `json:"propagation"`
	Verification     *verify.Report          `json:"verification,omitempty"`
}

type Difference struct {
	Field    string `json:"field"`
	RegionID int    `json:"region_id,omitempty"`
	Expected any    `json:"expected,omitempty"`
	Actual   any    `json:"actual,omitempty"`
	Message  string `json:"message"`
}

type Comparison struct {
	OK          bool         `json:"ok"`
	Differences []Difference `json:"differences,omitempty"`
}

type Report struct {
	Manifest     model.Manifest `json:"manifest"`
	InputHash    string         `json:"input_hash"`
	ManifestHash string         `json:"manifest_hash"`
	Truth        model.Truth    `json:"truth"`
	Realtime     PathResult     `json:"realtime"`
	Resume       PathResult     `json:"resume"`
	Full         PathResult     `json:"full"`
	Comparison   Comparison     `json:"comparison"`
	Verification *verify.Report `json:"verification,omitempty"`
}

type Options struct {
	Dir            string
	Parallelism    int
	QueueBudget    int
	ResumeAfter    int
	BufferLimit    int
	VerifyEachPath bool
}

func defaultOptions(opts Options) Options {
	if opts.Dir == "" {
		opts.Dir = filepath.Join("data", "checkpoints")
	}
	if opts.Parallelism == 0 {
		opts.Parallelism = 3
	}
	if opts.QueueBudget == 0 {
		opts.QueueBudget = 2
	}
	if opts.ResumeAfter == 0 {
		opts.ResumeAfter = 3
	}
	if opts.BufferLimit == 0 {
		opts.BufferLimit = 16384
	}
	return opts
}

func identity(s *sample.Sample) checkpoint.Identity {
	return runner.IdentityFor(s, s.Truth.TruthFingerprint)
}

func loadResult(name, dbPath string, s *sample.Sample, store *checkpoint.Store, runReport *runner.Report, reset *checkpoint.ResetInfo, verifyPath bool) (PathResult, error) {
	records, err := store.LoadBlockRecords()
	if err != nil {
		return PathResult{}, err
	}
	regions := eval.RegionMetricsFromRecords(s.Manifest, records)
	propagation := eval.PropagationPaths(s.Manifest, records)
	boundList := make([]model.ErrorBound, 0, len(regions))
	propMax := map[int]int64{}
	for _, path := range propagation {
		if path.JumpAvgQ8 > propMax[path.RegionLabel] {
			propMax[path.RegionLabel] = path.JumpAvgQ8
		}
	}
	for _, region := range regions {
		bound := bounds.Compute(s.Manifest.QuantGear, s.Manifest.QuantBits, s.Manifest.TileSize, model.FixedFractionBits, region.Label)
		boundList = append(boundList, bound)
		measured := bounds.Measurements{
			MeanQ8:        region.MeanDistanceQ8,
			StdQ8:         region.StdDistanceQ8,
			BoundaryQ8:    region.BoundaryJumpQ8,
			PropagationQ8: propMax[region.Label],
		}
		if boundErr := bounds.Check(region.Label, measured, bound); boundErr != nil {
			return PathResult{}, boundErr
		}
	}
	result := PathResult{
		Name:        name,
		DBPath:      dbPath,
		ResetInfo:   reset,
		RunReport:   runReport,
		Regions:     regions,
		Bounds:      boundList,
		Propagation: propagation,
	}
	if verifyPath {
		verification, err := verify.Verify(context.Background(), store, s.Truth)
		if err != nil {
			return PathResult{}, err
		}
		result.Verification = verification
	}
	return result, nil
}

func runFresh(ctx context.Context, s *sample.Sample, dbPath string, cfg runner.Config, verifyPath bool) (PathResult, error) {
	id := identity(s)
	store, reset, err := checkpoint.Open(dbPath, id)
	if err != nil {
		return PathResult{}, err
	}
	if err := store.Reset(id); err != nil {
		_ = store.Close()
		return PathResult{}, err
	}
	report, runErr := runner.Run(ctx, s, store, cfg, reset)
	if runErr != nil {
		_ = store.Close()
		return PathResult{}, runErr
	}
	result, err := loadResult("full", dbPath, s, store, report, reset, verifyPath)
	_ = store.Close()
	return result, err
}

func RunThreePaths(ctx context.Context, s *sample.Sample, opts Options) (*Report, error) {
	opts = defaultOptions(opts)
	if err := checkpoint.PrepareDir(opts.Dir); err != nil {
		return nil, err
	}
	id := identity(s)

	realtimePath := filepath.Join(opts.Dir, "realtime.sqlite")
	realtimeStore, resetRT, err := checkpoint.Open(realtimePath, id)
	if err != nil {
		return nil, err
	}
	_ = realtimeStore.Reset(id)
	rtCfg := runner.Config{Parallelism: minInt(2, s.Manifest.BlockCount()), QueueBudget: 2, ThreadPixelBufferLimit: opts.BufferLimit}
	rtReport, err := runner.Run(ctx, s, realtimeStore, rtCfg, resetRT)
	if err != nil {
		_ = realtimeStore.Close()
		return nil, err
	}
	realtime, err := loadResult("realtime", realtimePath, s, realtimeStore, rtReport, resetRT, opts.VerifyEachPath)
	_ = realtimeStore.Close()
	if err != nil {
		return nil, err
	}

	resumePath := filepath.Join(opts.Dir, "resume.sqlite")
	resumeStore, resetResume, err := checkpoint.Open(resumePath, id)
	if err != nil {
		return nil, err
	}
	_ = resumeStore.Reset(id)
	pauseCfg := runner.Config{Parallelism: opts.Parallelism, QueueBudget: opts.QueueBudget, ThreadPixelBufferLimit: opts.BufferLimit, MaxCommits: opts.ResumeAfter}
	pauseReport, err := runner.Run(ctx, s, resumeStore, pauseCfg, resetResume)
	if err != nil {
		_ = resumeStore.Close()
		return nil, err
	}
	if err := resumeStore.Close(); err != nil {
		return nil, err
	}
	resumeStore2, resetReopen, err := checkpoint.Open(resumePath, id)
	if err != nil {
		return nil, err
	}
	resumeCfg := runner.Config{Parallelism: opts.Parallelism, QueueBudget: opts.QueueBudget, ThreadPixelBufferLimit: opts.BufferLimit}
	resumeRunReport, err := runner.Run(ctx, s, resumeStore2, resumeCfg, resetReopen)
	if err != nil {
		_ = resumeStore2.Close()
		return nil, err
	}
	resumeRunReport.ResetInfo = mergeReset(resetResume, resetReopen)
	resume, err := loadResult("resume", resumePath, s, resumeStore2, resumeRunReport, resumeRunReport.ResetInfo, true)
	_ = resumeStore2.Close()
	if err != nil {
		return nil, err
	}
	resume.InitialRunReport = pauseReport

	fullPath := filepath.Join(opts.Dir, "full.sqlite")
	full, err := runFresh(ctx, s, fullPath, runner.Config{Parallelism: opts.Parallelism, QueueBudget: opts.QueueBudget, ThreadPixelBufferLimit: opts.BufferLimit}, opts.VerifyEachPath)
	if err != nil {
		return nil, err
	}

	comparison := ComparePaths(realtime, resume, full)
	verifyStore, _, err := checkpoint.Open(resumePath, id)
	if err != nil {
		return nil, err
	}
	verification, err := verify.Verify(ctx, verifyStore, s.Truth)
	closeErr := verifyStore.Close()
	if err != nil {
		return nil, err
	}
	if closeErr != nil {
		return nil, closeErr
	}

	return &Report{
		Manifest:     s.Manifest,
		InputHash:    s.InputHash,
		ManifestHash: s.ManifestHash,
		Truth:        s.Truth,
		Realtime:     realtime,
		Resume:       resume,
		Full:         full,
		Comparison:   comparison,
		Verification: verification,
	}, nil
}
func mergeReset(first, second *checkpoint.ResetInfo) *checkpoint.ResetInfo {
	if second != nil && second.Reset {
		return second
	}
	return first
}

func ComparePaths(paths ...PathResult) Comparison {
	result := Comparison{OK: true}
	if len(paths) != 3 {
		result.OK = false
		result.Differences = append(result.Differences, Difference{Field: "paths", Message: "expected exactly three paths"})
		return result
	}
	base := paths[2]
	for _, candidate := range paths[:2] {
		if !reflect.DeepEqual(candidate.RunReport.CommitSequence, base.RunReport.CommitSequence) {
			result.OK = false
			result.Differences = append(result.Differences, Difference{Field: "commit_sequence", Expected: base.RunReport.CommitSequence, Actual: candidate.RunReport.CommitSequence, Message: candidate.Name + " commit sequence differs from full recompute"})
		}
		if len(candidate.Regions) != len(base.Regions) {
			result.OK = false
			result.Differences = append(result.Differences, Difference{Field: "regions", Expected: len(base.Regions), Actual: len(candidate.Regions), Message: candidate.Name + " region count differs"})
			continue
		}
		for i := range base.Regions {
			if !reflect.DeepEqual(candidate.Regions[i], base.Regions[i]) {
				result.OK = false
				result.Differences = append(result.Differences, Difference{Field: "region_metrics", RegionID: base.Regions[i].Label, Expected: base.Regions[i], Actual: candidate.Regions[i], Message: candidate.Name + " region metrics differ"})
			}
			if !reflect.DeepEqual(candidate.Bounds[i], base.Bounds[i]) {
				result.OK = false
				result.Differences = append(result.Differences, Difference{Field: "error_bounds", RegionID: base.Regions[i].Label, Expected: base.Bounds[i], Actual: candidate.Bounds[i], Message: candidate.Name + " error bounds differ"})
			}
		}
	}
	return result
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
