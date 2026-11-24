package verify

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"economycore/internal/core"
	"economycore/internal/store"
)

type SceneReport struct {
	ID              string  `json:"id"`
	NodeCount       int     `json:"node_count"`
	IntentCount     int     `json:"intent_count"`
	FinalTick       int64   `json:"final_tick"`
	TruthSamples    int     `json:"truth_samples"`
	TruthOK         bool    `json:"truth_ok"`
	PreviewOK       bool    `json:"preview_ok"`
	IncrementalOK   bool    `json:"incremental_ok"`
	ResumeOK        bool    `json:"resume_ok"`
	ParallelOK      bool    `json:"parallel_ok"`
	WindowSize      int64   `json:"window_size"`
	Windows         int     `json:"windows"`
	ParallelWorkers int     `json:"parallel_workers"`
	BufferPerWorker int     `json:"buffer_per_worker"`
	DurationMS      int64   `json:"duration_ms"`
	CheckpointPath  string  `json:"checkpoint_path,omitempty"`
	Checks          []Check `json:"checks"`
}

type Check struct {
	Name    string `json:"name"`
	OK      bool   `json:"ok"`
	Samples int    `json:"samples"`
	Detail  string `json:"detail,omitempty"`
}

type Report struct {
	GeneratedAt string        `json:"generated_at"`
	Scenes      []SceneReport `json:"scenes"`
	AllOK       bool          `json:"all_ok"`
}

type Options struct {
	WorkDir     string
	WindowSize  int64
	PreviewStep int64
	Workers     int
	Buffer      int
	ResumeAfter int
}

func DefaultOptions(workDir string) Options {
	return Options{
		WorkDir:     workDir,
		WindowSize:  64,
		PreviewStep: 8,
		Workers:     3,
		Buffer:      2,
		ResumeAfter: 2,
	}
}

func Run(ctx context.Context, scenarios []*core.Scenario, opts Options) (*Report, error) {
	if opts.WorkDir == "" {
		return nil, core.NewError(core.CodeParam, "verification work directory is empty").WithField("workdir")
	}
	if opts.WindowSize <= 0 {
		opts.WindowSize = 64
	}
	if opts.PreviewStep <= 0 {
		opts.PreviewStep = 8
	}
	if opts.Workers <= 0 {
		opts.Workers = 3
	}
	if opts.Buffer <= 0 {
		opts.Buffer = 2
	}
	if opts.ResumeAfter <= 0 {
		opts.ResumeAfter = 2
	}
	if err := os.MkdirAll(opts.WorkDir, 0o755); err != nil {
		return nil, core.NewError(core.CodeIO, "failed to create verification work directory").Wrap(err)
	}
	report := &Report{GeneratedAt: time.Now().UTC().Format(time.RFC3339), AllOK: true}
	for _, scene := range scenarios {
		sr, err := runScene(ctx, scene, opts)
		if err != nil {
			return nil, err
		}
		report.Scenes = append(report.Scenes, sr)
		if !sr.TruthOK || !sr.PreviewOK || !sr.IncrementalOK || !sr.ResumeOK || !sr.ParallelOK {
			report.AllOK = false
		}
	}
	return report, nil
}

func runScene(ctx context.Context, scene *core.Scenario, opts Options) (SceneReport, error) {
	started := time.Now()
	sr := SceneReport{
		ID:              scene.ID,
		NodeCount:       len(scene.Nodes),
		IntentCount:     len(scene.Intents),
		FinalTick:       scene.FinalTick,
		TruthSamples:    len(scene.Truth.SampleTicks),
		WindowSize:      opts.WindowSize,
		ParallelWorkers: opts.Workers,
		BufferPerWorker: opts.Buffer,
	}
	sampleTicks := append([]int64(nil), scene.Truth.SampleTicks...)
	sampleTicks = append(sampleTicks, scene.FinalTick)
	sampleTicks = uniqueTicks(sampleTicks)

	exact, err := core.RunFull(scene, sampleTicks)
	if err != nil {
		return sr, fmt.Errorf("%s full settlement: %w", scene.ID, err)
	}
	exactByTick := snapshotMap(exact.Snapshots)
	sr.Checks = append(sr.Checks, Check{Name: "full", OK: true, Samples: len(exact.Snapshots)})

	if err := verifyTruth(scene, exactByTick); err != nil {
		return sr, err
	}
	sr.TruthOK = true
	sr.Checks = append(sr.Checks, Check{Name: "truth", OK: true, Samples: len(scene.Truth.Snapshots)})

	preview, err := core.RunPreview(scene, opts.PreviewStep, sampleTicks, scene.FinalTick)
	if err != nil {
		return sr, fmt.Errorf("%s preview: %w", scene.ID, err)
	}
	previewByTick := make(map[int64]core.Snapshot, len(preview.Points))
	for _, p := range preview.Points {
		previewByTick[p.Tick] = p.Snapshot
	}
	for _, tick := range sampleTicks {
		expected, ok := exactByTick[tick]
		if !ok {
			return sr, core.NewError(core.CodeState, "full snapshot missing during preview comparison").
				WithField("tick")
		}
		actual, ok := previewByTick[tick]
		if !ok {
			// Preview always includes final, but a sample exactly on a coarse
			// boundary is recorded by the builder; all sample ticks are also
			// recorded in exact mode. This is defensive.
			return sr, core.NewError(core.CodeDiverge, "preview has no snapshot for sample tick").
				WithValues(tick, nil)
		}
		if diff := core.CompareSnapshots(expected, actual); diff != nil {
			diff.Message = "preview diverged from exact settlement"
			return sr, diff
		}
	}
	if diff := core.CompareSnapshots(exact.Final, preview.Final); diff != nil {
		diff.Message = "preview final state diverged from exact settlement"
		return sr, diff
	}
	sr.PreviewOK = true
	sr.Checks = append(sr.Checks, Check{Name: "preview", OK: true, Samples: len(preview.Points)})

	incPath := filepath.Join(opts.WorkDir, sanitize(scene.ID)+"-incremental.db")
	resetSQLiteFiles(incPath)
	incStore, err := store.Open(incPath)
	if err != nil {
		return sr, err
	}
	inc, err := core.RunWindowed(ctx, scene, incStore, core.WindowOptions{
		WindowSize: opts.WindowSize, SampleTicks: sampleTicks,
	})
	closeErr := incStore.Close()
	if err != nil {
		return sr, fmt.Errorf("%s incremental settlement: %w", scene.ID, err)
	}
	if closeErr != nil {
		return sr, closeErr
	}
	if diff := compareSet(exactByTick, snapshotMap(inc.Snapshots), sampleTicks); diff != nil {
		diff.Message = "incremental settlement diverged from full settlement"
		return sr, diff
	}
	sr.IncrementalOK = true
	sr.Windows = len(inc.Windows)
	sr.Checks = append(sr.Checks, Check{Name: "incremental", OK: true, Samples: len(inc.Snapshots)})

	resumePath := filepath.Join(opts.WorkDir, sanitize(scene.ID)+"-resume.db")
	resetSQLiteFiles(resumePath)
	rs1, err := store.Open(resumePath)
	if err != nil {
		return sr, err
	}
	phase1, err := core.RunWindowed(ctx, scene, rs1, core.WindowOptions{
		WindowSize: opts.WindowSize, SampleTicks: sampleTicks,
		StopAfterWindows: opts.ResumeAfter,
	})
	closeErr = rs1.Close()
	if err != nil {
		return sr, fmt.Errorf("%s resume phase 1: %w", scene.ID, err)
	}
	if closeErr != nil {
		return sr, closeErr
	}
	if !phase1.Stopped {
		return sr, core.NewError(core.CodeState, "resume phase 1 did not stop at requested window")
	}
	rs2, err := store.Open(resumePath)
	if err != nil {
		return sr, err
	}
	phase2, err := core.RunWindowed(ctx, scene, rs2, core.WindowOptions{
		WindowSize: opts.WindowSize, SampleTicks: sampleTicks,
	})
	count, countErr := rs2.Count(ctx, scene.ID)
	closeErr = rs2.Close()
	if err != nil {
		return sr, fmt.Errorf("%s resume phase 2: %w", scene.ID, err)
	}
	if countErr != nil {
		return sr, countErr
	}
	if closeErr != nil {
		return sr, closeErr
	}
	if !phase2.Resumed {
		return sr, core.NewError(core.CodeState, "resume phase 2 did not load a checkpoint")
	}
	if count < opts.ResumeAfter || count == 0 {
		return sr, core.NewError(core.CodeState, "resume did not persist expected windows")
	}
	combined := make(map[int64]core.Snapshot)
	for _, s := range append(phase1.Snapshots, phase2.Snapshots...) {
		combined[s.Tick] = s
	}
	if diff := compareSet(exactByTick, combined, sampleTicks); diff != nil {
		diff.Message = "resumed settlement diverged from full settlement"
		return sr, diff
	}
	sr.ResumeOK = true
	sr.CheckpointPath = resumePath
	sr.Checks = append(sr.Checks, Check{Name: "resume", OK: true, Samples: len(combined)})

	parPath := filepath.Join(opts.WorkDir, sanitize(scene.ID)+"-parallel.db")
	resetSQLiteFiles(parPath)
	parStore, err := store.Open(parPath)
	if err != nil {
		return sr, err
	}
	parallel, stats, err := core.RunWindowedParallel(ctx, scene, parStore, core.ParallelOptions{
		WindowSize: opts.WindowSize, Workers: opts.Workers,
		BufferPerWorker: opts.Buffer, SampleTicks: sampleTicks,
	})
	closeErr = parStore.Close()
	if err != nil {
		return sr, fmt.Errorf("%s parallel settlement: %w", scene.ID, err)
	}
	if closeErr != nil {
		return sr, closeErr
	}
	if diff := compareSet(exactByTick, snapshotMap(parallel.Snapshots), sampleTicks); diff != nil {
		diff.Message = "parallel settlement diverged from serial settlement"
		return sr, diff
	}
	if len(parallel.Windows) != len(inc.Windows) {
		return sr, core.NewError(core.CodeDiverge, "parallel/ serial window counts differ").
			WithValues(len(parallel.Windows), len(inc.Windows))
	}
	for i := range parallel.Windows {
		if parallel.Windows[i].Index != inc.Windows[i].Index ||
			parallel.Windows[i].EndTick != inc.Windows[i].EndTick ||
			parallel.Windows[i].EndIntentOffset != inc.Windows[i].EndIntentOffset {
			return sr, core.NewError(core.CodeDiverge, "parallel/serial window order differs").
				WithValues(parallel.Windows[i], inc.Windows[i])
		}
	}
	sr.ParallelOK = true
	sr.Checks = append(sr.Checks, Check{Name: "parallel", OK: true, Samples: stats.Planned})
	sr.DurationMS = time.Since(started).Milliseconds()
	return sr, nil
}

func verifyTruth(scene *core.Scenario, exact map[int64]core.Snapshot) error {
	for _, expected := range scene.Truth.Snapshots {
		actual, ok := exact[expected.Tick]
		if !ok {
			return core.NewError(core.CodeDiverge, "truth sample was not produced by exact run").
				WithValues(expected.Tick, nil)
		}
		if diff := core.CompareTruth(expected, actual); diff != nil {
			diff.Message = "generated truth diverged from exact settlement"
			return diff
		}
	}
	return nil
}

func compareSet(expected map[int64]core.Snapshot, actual map[int64]core.Snapshot, ticks []int64) *core.Error {
	for _, tick := range ticks {
		e, ok := expected[tick]
		if !ok {
			return core.NewError(core.CodeDiverge, "expected snapshot is missing").WithValues(tick, nil)
		}
		a, ok := actual[tick]
		if !ok {
			return core.NewError(core.CodeDiverge, "actual snapshot is missing").WithValues(tick, nil)
		}
		if diff := core.CompareSnapshots(e, a); diff != nil {
			return diff
		}
	}
	return nil
}

func snapshotMap(in []core.Snapshot) map[int64]core.Snapshot {
	out := make(map[int64]core.Snapshot, len(in))
	for _, s := range in {
		out[s.Tick] = s
	}
	return out
}

func uniqueTicks(in []int64) []int64 {
	seen := make(map[int64]struct{}, len(in))
	out := make([]int64, 0, len(in))
	for _, tick := range in {
		if _, ok := seen[tick]; ok {
			continue
		}
		seen[tick] = struct{}{}
		out = append(out, tick)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

func sanitize(id string) string {
	return strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			return r
		}
		return '_'
	}, id)
}

func resetSQLiteFiles(path string) {
	_ = os.Remove(path)
	_ = os.Remove(path + "-wal")
	_ = os.Remove(path + "-shm")
}
