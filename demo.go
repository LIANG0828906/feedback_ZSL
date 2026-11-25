package textureconv

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
)

type DemoOptions struct {
	Dir      string
	Seed     int64
	Workers  int
	TileSize int
}

type DemoResult struct {
	BaselineSample string   `json:"baselineSample"`
	FinalSample    string   `json:"finalSample"`
	ImageID        string   `json:"imageId"`
	TileCount      int      `json:"tileCount"`
	EditSummary    string   `json:"editSummary"`
	Realtime       DemoPath `json:"realtime"`
	Resume         DemoPath `json:"resume"`
	FullParallel   DemoPath `json:"fullParallel"`
	FullSerial     DemoPath `json:"fullSerial"`
	Verified       bool     `json:"verified"`
	AllEqual       bool     `json:"allEqual"`
}

type DemoPath struct {
	Database       string   `json:"database"`
	CacheHits      int      `json:"cacheHits"`
	CacheMisses    int      `json:"cacheMisses"`
	SubmittedNew   int      `json:"submittedNew"`
	FinalHash      string   `json:"finalHash"`
	SummaryHashes  []string `json:"summaryHashes"`
	SamplesChecked int      `json:"samplesChecked"`
	MaxDeviation   uint16   `json:"maxDeviation"`
}

// RunDemo executes real-time, restart/resume, parallel full, and serial full
// paths against deterministic samples, then cross-checks every stage.
func RunDemo(opts DemoOptions) (*DemoResult, error) {
	if opts.Dir == "" {
		opts.Dir = "demo-out"
	}
	if opts.Seed == 0 {
		opts.Seed = 20261125
	}
	if opts.Workers == 0 {
		opts.Workers = 4
	}
	if opts.TileSize == 0 {
		opts.TileSize = 32
	}
	if err := os.MkdirAll(opts.Dir, 0o755); err != nil {
		return nil, ioErr("createDemoDir", err)
	}
	specs := []ImageSpec{{ID: "demo_atlas", Width: 301, Height: 217, BitDepth: Depth8, Layout: LayoutRGBA}}
	planA := SamplePlan()
	baseDir := filepath.Join(opts.Dir, "sample-baseline")
	base, err := GenerateSample(baseDir, GenerateOptions{Seed: opts.Seed, Images: specs, Plan: planA})
	if err != nil {
		return nil, err
	}
	planB := clonePlan(planA)
	planB.Entries[2].Alpha = AlphaPremultiply
	planB.Entries[4].Fill[0].Value = 32768
	if err := planB.ValidateForImage(base.Images["demo_atlas"]); err != nil {
		return nil, err
	}
	finalDir := filepath.Join(opts.Dir, "sample-final")
	finalBundle, err := GenerateSample(finalDir, GenerateOptions{Seed: opts.Seed, Images: specs, Plan: planB})
	if err != nil {
		return nil, err
	}
	imageID := "demo_atlas"
	source := base.Images[imageID]
	sourceHash := finalBundle.Hashes[imageID]
	ctx := context.Background()
	result := &DemoResult{
		BaselineSample: baseDir, FinalSample: finalDir, ImageID: imageID,
		EditSummary: "entry 3 alpha straighten -> premultiply; entry 5 fill 65535 -> 32768",
	}
	// Real-time: process four tiles, explicitly unfreeze affected chunks, then
	// finish incrementally.
	rtDB := filepath.Join(opts.Dir, "checkpoints-realtime.db")
	_ = os.Remove(rtDB)
	rtStore, err := OpenStore(rtDB)
	if err != nil {
		return nil, err
	}
	rtEngine, err := NewEngine(source, planA, EngineOptions{
		ImageID: imageID, SourceHash: sourceHash, ParamsVersion: "demo-v1",
		TileSize: opts.TileSize, Workers: opts.Workers, Store: rtStore,
	})
	if err != nil {
		rtStore.Close()
		return nil, err
	}
	_, _ = rtEngine.ProcessLimit(ctx, 4)
	manifest, err := rtEngine.ApplyEdit(planB, EditOptions{AllowUnfreeze: true})
	if err != nil {
		rtStore.Close()
		return nil, err
	}
	rtRun, err := rtEngine.Process(ctx)
	if err != nil {
		rtStore.Close()
		return nil, err
	}
	rtSnap, err := rtEngine.Snapshot()
	if err != nil {
		rtStore.Close()
		return nil, err
	}
	rtReport, err := VerifyPersisted(finalBundle, imageID, rtStore, VerifyOptions{
		ExpectedParamsVersion: "demo-v1", ExpectedAlgorithmVersion: AlgorithmVersion,
	})
	if err != nil {
		rtStore.Close()
		return nil, fmt.Errorf("realtime verification: %w", err)
	}
	result.Realtime = demoPath(rtDB, rtRun, rtSnap, rtReport)
	if err := rtStore.Close(); err != nil {
		return nil, ioErr("closeRealtimeStore", err)
	}
	_ = manifest

	// Resume: perform the same partial run, close the store, reopen it, then
	// apply the edit and continue solely from persisted data.
	rsDB := filepath.Join(opts.Dir, "checkpoints-resume.db")
	_ = os.Remove(rsDB)
	rsStore, err := OpenStore(rsDB)
	if err != nil {
		return nil, err
	}
	rsEngine, err := NewEngine(source, planA, EngineOptions{
		ImageID: imageID, SourceHash: sourceHash, ParamsVersion: "demo-v1",
		TileSize: opts.TileSize, Workers: opts.Workers, Store: rsStore,
	})
	if err != nil {
		rsStore.Close()
		return nil, err
	}
	_, _ = rsEngine.ProcessLimit(ctx, 4)
	if err := rsStore.Close(); err != nil {
		return nil, ioErr("closeResumeStore", err)
	}
	rsStore, err = OpenStore(rsDB)
	if err != nil {
		return nil, err
	}
	rsEngine, err = NewEngine(source, planA, EngineOptions{
		ImageID: imageID, SourceHash: sourceHash, ParamsVersion: "demo-v1",
		TileSize: opts.TileSize, Workers: opts.Workers, Store: rsStore,
	})
	if err != nil {
		rsStore.Close()
		return nil, err
	}
	if _, err := rsEngine.ApplyEdit(planB, EditOptions{AllowUnfreeze: true}); err != nil {
		rsStore.Close()
		return nil, err
	}
	rsRun, err := rsEngine.Process(ctx)
	if err != nil {
		rsStore.Close()
		return nil, err
	}
	rsSnap, err := rsEngine.Snapshot()
	if err != nil {
		rsStore.Close()
		return nil, err
	}
	rsReport, err := VerifyPersisted(finalBundle, imageID, rsStore, VerifyOptions{
		ExpectedParamsVersion: "demo-v1", ExpectedAlgorithmVersion: AlgorithmVersion,
	})
	if err != nil {
		rsStore.Close()
		return nil, fmt.Errorf("resume verification: %w", err)
	}
	result.Resume = demoPath(rsDB, rsRun, rsSnap, rsReport)
	if err := rsStore.Close(); err != nil {
		return nil, ioErr("closeResumeStore", err)
	}

	// Parallel full and serial full both start from empty stores under plan B.
	fullParallel, err := runFull(ctx, opts, finalBundle, planB, "checkpoints-full-parallel.db", opts.Workers)
	if err != nil {
		return nil, err
	}
	result.FullParallel = fullParallel.path
	fullSerial, err := runFull(ctx, opts, finalBundle, planB, "checkpoints-full-serial.db", 1)
	if err != nil {
		return nil, err
	}
	result.FullSerial = fullSerial.path

	result.TileCount = len(rtSnap.Tiles)
	if err := CompareSnapshots(rtSnap, rsSnap); err != nil {
		return nil, fmt.Errorf("realtime vs resume: %w", err)
	}
	if err := CompareSnapshots(rtSnap, fullParallel.snapshot); err != nil {
		return nil, fmt.Errorf("incremental vs full parallel: %w", err)
	}
	if err := CompareSnapshots(fullParallel.snapshot, fullSerial.snapshot); err != nil {
		return nil, fmt.Errorf("parallel vs serial: %w", err)
	}
	result.Verified = true
	result.AllEqual = true
	return result, nil
}

type fullRun struct {
	path     DemoPath
	snapshot *RunSnapshot
}

func runFull(ctx context.Context, opts DemoOptions, bundle *SampleBundle, plan *Plan, dbName string, workers int) (*fullRun, error) {
	imageID := "demo_atlas"
	dbPath := filepath.Join(opts.Dir, dbName)
	_ = os.Remove(dbPath)
	store, err := OpenStore(dbPath)
	if err != nil {
		return nil, err
	}
	engine, err := NewEngine(bundle.Images[imageID], plan, EngineOptions{
		ImageID: imageID, SourceHash: bundle.Hashes[imageID], ParamsVersion: "demo-v1",
		TileSize: opts.TileSize, Workers: workers, Store: store,
	})
	if err != nil {
		store.Close()
		return nil, err
	}
	report, err := engine.FullRecompute(ctx)
	if err != nil {
		store.Close()
		return nil, err
	}
	snap, err := engine.Snapshot()
	if err != nil {
		store.Close()
		return nil, err
	}
	vr, err := VerifyPersisted(bundle, imageID, store, VerifyOptions{
		ExpectedParamsVersion: "demo-v1", ExpectedAlgorithmVersion: AlgorithmVersion,
	})
	if err != nil {
		store.Close()
		return nil, fmt.Errorf("full verification: %w", err)
	}
	dp := demoPath(dbPath, report, snap, vr)
	if err := store.Close(); err != nil {
		return nil, ioErr("closeFullStore", err)
	}
	return &fullRun{path: dp, snapshot: snap}, nil
}

func demoPath(db string, report RunReport, snap *RunSnapshot, verify *VerifyReport) DemoPath {
	out := DemoPath{
		Database: db, CacheHits: report.CacheHits, CacheMisses: report.CacheMisses,
		SubmittedNew: len(report.Submitted), SamplesChecked: verify.SamplesChecked,
		MaxDeviation: verify.MaxDeviation,
	}
	if snap.Final != nil {
		out.FinalHash = HashStageImage(snap.Final)
	}
	for _, s := range verify.StageSummaries {
		out.SummaryHashes = append(out.SummaryHashes, s.Hash)
	}
	return out
}
