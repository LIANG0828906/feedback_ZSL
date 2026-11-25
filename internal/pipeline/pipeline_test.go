package pipeline_test

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"

	"qualitybench/internal/checkpoint"
	"qualitybench/internal/core"
	"qualitybench/internal/generator"
	"qualitybench/internal/model"
	"qualitybench/internal/pipeline"
	"qualitybench/internal/runner"
	"qualitybench/internal/store"
	"qualitybench/internal/verify"
)

func TestRunMatchesGeneratorReference(t *testing.T) {
	ds := generateOne(t)
	st := openStore(t)
	eng := newEngine(t, ds, st, "full-1", pipeline.ModeFull, runner.Config{Workers: 3})
	res, err := eng.Run(context.Background(), pipeline.ModeFull)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Completed {
		t.Fatal("run did not complete")
	}
	final := res.StepFrames[len(res.StepFrames)-1]
	want := ds.Reference["img-00"][len(ds.Reference["img-00"])-1]
	if final.Hash() != want.Hash() {
		t.Fatalf("final frame hash %s != reference %s", final.Hash(), want.Hash())
	}
	manifests, err := st.LoadBlockManifests(context.Background(), "full-1", "img-00")
	if err != nil {
		t.Fatal(err)
	}
	if len(manifests) != 96 {
		t.Fatalf("expected 96 block manifests, got %d", len(manifests))
	}
	for _, manifest := range manifests {
		if len(manifest.Steps) != len(ds.Project.Chains[0].Steps)+1 {
			t.Fatalf("manifest step count = %d", len(manifest.Steps))
		}
		if manifest.ContentHash != manifest.ComputeContentHash() {
			t.Fatal("manifest content hash mismatch")
		}
	}
}

func TestLocalizedEditAndRollbackMatchDirectRecompute(t *testing.T) {
	ds := generateOne(t)
	st := openStore(t)
	eng := newEngine(t, ds, st, "edit-live", pipeline.ModeFull, runner.Config{Workers: 3})
	res, err := eng.Run(context.Background(), pipeline.ModeFull)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Completed {
		t.Fatal("initial run did not complete")
	}
	baseVersion := res.VersionID
	baseFinal := res.StepFrames[len(res.StepFrames)-1].Hash()

	strength := 0.71
	editRes, err := eng.ApplyEdit(context.Background(), modelEditStrength("img-00", 2, strength))
	if err != nil {
		t.Fatal(err)
	}
	if editRes.RecomputedBlocks == 0 {
		t.Fatal("edit did not recompute any block")
	}
	if editRes.UnaffectedReused == 0 {
		t.Fatal("local edit did not preserve unaffected blocks")
	}
	for _, event := range editRes.Invalidations {
		if event.Step < 1 || event.Block == nil || event.Reason == "" {
			t.Fatalf("invalidation misses step/block/reason: %+v", event)
		}
	}

	chain := ds.Project.Chains[0].Clone()
	chain.Steps[1].Strength = strength
	direct := runChain(t, ds, st, "edit-direct-1", chain, pipeline.ModeFull, runner.Config{Workers: 3})
	assertSameFrame(t, eng.Frames(), direct.StepFrames)

	move := modelMove(5, 2)
	if _, err := eng.ApplyEdit(context.Background(), move); err != nil {
		t.Fatal(err)
	}
	reordered := ds.Project.Chains[0].Clone()
	step := reordered.Steps[4]
	reordered.Steps = append(reordered.Steps[:1], append([]model.Step{step}, reordered.Steps[1:4]...)...)
	reordered.Steps[2].Strength = strength
	direct2 := runChain(t, ds, st, "edit-direct-2", reordered, pipeline.ModeFull, runner.Config{Workers: 3})
	assertSameFrame(t, eng.Frames(), direct2.StepFrames)

	rollback, err := eng.Rollback(context.Background(), baseVersion)
	if err != nil {
		t.Fatal(err)
	}
	if rollback.Cache.Hits == 0 {
		t.Fatal("rollback did not hit the content-addressed cache")
	}
	frames := eng.Frames()
	if got := frames[len(frames)-1].Hash(); got != baseFinal {
		t.Fatalf("rollback final hash %s != %s", got, baseFinal)
	}
	for i := 0; i < 3; i++ {
		if _, err := eng.Rollback(context.Background(), baseVersion); err != nil {
			t.Fatal(err)
		}
		reapply, err := eng.ApplyEdit(context.Background(), modelEditStrength("img-00", 2, strength))
		if err != nil {
			t.Fatal(err)
		}
		if reapply.Cache.Hits == 0 {
			t.Fatalf("rollback cycle %d did not reuse cache", i)
		}
		assertSameFrame(t, eng.Frames(), direct.StepFrames)
	}
}

func TestInvalidTransitionKeepsState(t *testing.T) {
	ds := generateOne(t)
	st := openStore(t)
	eng := newEngine(t, ds, st, "state", pipeline.ModeFull, runner.Config{Workers: 1})
	if err := eng.TryBlockTransition(1, 0, core.StateCommitted); err == nil || core.CodeOf(err) != core.CodeState {
		t.Fatalf("expected E_STATE, got %v", err)
	}
	if got := eng.BlockState(1, 0); got != core.StatePending {
		t.Fatalf("illegal transition changed state to %s", got)
	}
}

func TestBufferContentionRecordsWait(t *testing.T) {
	ds := generateOne(t)
	st := openStore(t)
	scratch := int64(192*128*3*2 + 4096)
	eng := newEngine(t, ds, st, "buffer", pipeline.ModeFull, runner.Config{
		Workers: 3, MaxThreadBufferBytes: scratch, BufferCapacityBytes: scratch,
	})
	res, err := eng.Run(context.Background(), pipeline.ModeFull)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.BufferWaits) == 0 {
		t.Fatal("expected buffer queue wait events")
	}
	for _, event := range res.BufferWaits {
		if event.Reason == "" || event.Duration <= 0 {
			t.Fatalf("bad wait event: %+v", event)
		}
	}
}

func TestStageFaultReportsStepAndBlock(t *testing.T) {
	ds := generateOne(t)
	st := openStore(t)
	eng := newEngine(t, ds, st, "fault", pipeline.ModeFull, runner.Config{Workers: 1})
	eng.SetFault(func(step int, block core.Rect) error {
		if step == 2 && block.X == 16 {
			return errors.New("synthetic stage fault")
		}
		return nil
	})
	_, err := eng.Run(context.Background(), pipeline.ModeFull)
	if err == nil || core.CodeOf(err) != core.CodeStage {
		t.Fatalf("expected E_STAGE, got %v", err)
	}
	qe, _ := core.As(err)
	if qe.Step != 2 || qe.Block == nil || qe.Block.X != 16 {
		t.Fatalf("stage error lacks step/block: %+v", qe)
	}
}

func TestResumeHashMismatchInvalidatesAndRecomputes(t *testing.T) {
	ds := generateOne(t)
	path := filepath.Join(t.TempDir(), "resume.db")
	st1, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	eng1 := newEngine(t, ds, st1, "resume", pipeline.ModeLive, runner.Config{Workers: 2, StopAfter: 17})
	first, err := eng1.Run(context.Background(), pipeline.ModeLive)
	if err != nil {
		t.Fatal(err)
	}
	if !first.Interrupted {
		t.Fatal("expected an interrupted first run")
	}
	_ = st1.Close()

	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = raw.Exec(`UPDATE block_steps SET output_hash='corrupt'
WHERE rowid=(SELECT MIN(rowid) FROM block_steps WHERE step=1)`)
	if err != nil {
		t.Fatal(err)
	}
	_ = raw.Close()

	st2, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()
	eng2 := newEngine(t, ds, st2, "resume", pipeline.ModeResume, runner.Config{Workers: 2})
	second, err := eng2.Run(context.Background(), pipeline.ModeResume)
	if err != nil {
		t.Fatal(err)
	}
	if !second.Invalidated || !second.FallbackFull || !second.Completed {
		t.Fatalf("expected invalidate+full fallback, got %+v", second)
	}
	truth, fp, err := generatorTruth(ds)
	if err != nil {
		t.Fatal(err)
	}
	report, err := verify.Run(context.Background(), st2, "resume", truth, fp)
	if err != nil {
		t.Fatalf("independent verify failed: %v\n%+v", err, report)
	}
	if !report.Verified {
		t.Fatal("verifier did not mark run verified")
	}
}

func generatorTruth(ds *generator.Dataset) (*model.Truth, string, error) {
	return ds.Truth, ds.TruthFP, nil
}

func generateOne(t *testing.T) *generator.Dataset {
	t.Helper()
	opts := generator.DefaultOptions()
	opts.ImageCount = 1
	opts.Seed = 9001
	ds, err := generator.Generate(opts)
	if err != nil {
		t.Fatal(err)
	}
	return ds
}

func openStore(t *testing.T) checkpoint.Store {
	t.Helper()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func newEngine(t *testing.T, ds *generator.Dataset, st checkpoint.Store, runID string,
	mode pipeline.Mode, cfg runner.Config) *pipeline.Engine {
	t.Helper()
	if cfg.Workers == 0 {
		cfg.Workers = 2
	}
	eng, err := pipeline.New(ds.Baseline["img-00"], ds.Project.Chains[0], pipeline.Options{
		RunID: runID, ImageID: "img-00", ImageSeed: ds.Project.Images[0].Seed,
		ProjectFingerprint: ds.ProjectFP, TruthFingerprint: ds.TruthFP,
		AlgorithmVersion: ds.Project.AlgorithmVersion, BlockSize: ds.Project.BlockSize,
		Workers: cfg.Workers, MaxThreadBufferBytes: cfg.MaxThreadBufferBytes,
		BufferCapacityBytes: cfg.BufferCapacityBytes, PersistentCache: true,
		StopAfter: cfg.StopAfter, Store: st, AttributionShare: ds.Project.AttributionShare,
	})
	if err != nil {
		t.Fatal(err)
	}
	return eng
}

func modelEditStrength(imageID string, step int, strength float64) model.Edit {
	return model.Edit{Op: "set_strength", ImageID: imageID, Step: step, Strength: &strength}
}

func modelMove(from, to int) model.Edit {
	return model.Edit{Op: "move_step", ImageID: "img-00", From: from, To: to}
}

func runChain(t *testing.T, ds *generator.Dataset, st checkpoint.Store, runID string,
	chain model.Chain, mode pipeline.Mode, cfg runner.Config) *pipeline.Result {
	t.Helper()
	base := ds.Baseline["img-00"]
	eng, err := pipeline.New(base, chain, pipeline.Options{
		RunID: runID, ImageID: "img-00", ImageSeed: ds.Project.Images[0].Seed,
		ProjectFingerprint: ds.ProjectFP, TruthFingerprint: ds.TruthFP,
		AlgorithmVersion: ds.Project.AlgorithmVersion, BlockSize: ds.Project.BlockSize,
		Workers: cfg.Workers, MaxThreadBufferBytes: cfg.MaxThreadBufferBytes,
		BufferCapacityBytes: cfg.BufferCapacityBytes, PersistentCache: true,
		Store: st, AttributionShare: ds.Project.AttributionShare,
	})
	if err != nil {
		t.Fatal(err)
	}
	res, err := eng.Run(context.Background(), mode)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func assertSameFrame(t *testing.T, a, b []*core.Frame) {
	t.Helper()
	if len(a) != len(b) {
		t.Fatalf("frame count %d != %d", len(a), len(b))
	}
	for i := range a {
		if a[i].Hash() != b[i].Hash() {
			t.Fatalf("step %d differs: %s != %s", i, a[i].Hash(), b[i].Hash())
		}
	}
}
