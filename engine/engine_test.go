package engine_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"maskqc/checkpoint"
	"maskqc/engine"
	"maskqc/model"
	"maskqc/sample"
	"maskqc/schema"
)

func TestThreeVersionsAndThreeRunPaths(t *testing.T) {
	bundle, err := sample.Generate(424242, []int{96, 128})
	if err != nil {
		t.Fatal(err)
	}
	if len(bundle.Projects) != 2 {
		t.Fatalf("expected two resolutions, got %d", len(bundle.Projects))
	}
	generated := bundle.Projects[0]
	versionHashes := map[model.SchemaVersion]string{}
	for _, version := range []model.SchemaVersion{model.SchemaV1, model.SchemaV2, model.SchemaV3} {
		project, _, err := schema.Load(generated.Artifacts[version])
		if err != nil {
			t.Fatalf("load %s: %v", version, err)
		}
		if project.ID != generated.ID || project.Width != 96 {
			t.Fatalf("unexpected %s project identity", version)
		}
		if len(project.Truth) != 3 {
			t.Fatalf("expected three truth domains in %s, got %d", version, len(project.Truth))
		}
		versionStore, err := checkpoint.Open(filepath.Join(t.TempDir(), string(version)+".db"))
		if err != nil {
			t.Fatal(err)
		}
		versionConfig := engine.DefaultConfig()
		versionConfig.Tolerance.PixelPercent = 50
		versionConfig.Tolerance.AreaPercent = 50
		versionEngine, err := engine.New(project, versionConfig, versionStore)
		if err != nil {
			t.Fatal(err)
		}
		versionResult, err := versionEngine.Run(context.Background(), engine.ModeFull)
		if err != nil {
			if versionResult != nil {
				t.Logf("%s issues: %v", version, versionResult.Issues)
			}
			t.Fatalf("%s verification failed: %v", version, err)
		}
		if versionResult.Analysis == nil || len(versionResult.Analysis.Domains) != 3 {
			t.Fatalf("%s did not preserve three connected domains", version)
		}
		versionHashes[version] = versionResult.AccumulatedHash
		versionStore.Close()
	}
	if versionHashes[model.SchemaV1] != versionHashes[model.SchemaV2] ||
		versionHashes[model.SchemaV1] != versionHashes[model.SchemaV3] {
		t.Fatalf("schema versions produced different accumulated masks: %+v", versionHashes)
	}
	project, _, err := schema.Load(generated.Artifacts[model.SchemaV3])
	if err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(t.TempDir(), "state.db")
	store, err := checkpoint.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	config := engine.DefaultConfig()
	config.Tolerance.PixelPercent = 50
	config.Tolerance.AreaPercent = 50
	first, err := engine.New(project, config, store)
	if err != nil {
		t.Fatal(err)
	}
	full, err := first.Run(context.Background(), engine.ModeFull)
	if err != nil {
		if full != nil {
			t.Logf("full issues: %v", full.Issues)
		}
		t.Fatalf("full run: %v", err)
	}
	if len(full.Analysis.Domains) != 3 {
		t.Fatalf("expected 3 domains, got %d", len(full.Analysis.Domains))
	}
	incremental, err := first.Run(context.Background(), engine.ModeIncremental)
	if err != nil {
		t.Fatalf("incremental run: %v", err)
	}
	second, err := engine.New(project, config, store)
	if err != nil {
		t.Fatal(err)
	}
	resume, err := second.Run(context.Background(), engine.ModeResume)
	if err != nil {
		t.Fatalf("resume run: %v", err)
	}
	if mismatch := engine.CompareRuns(full, incremental); mismatch != nil {
		t.Fatalf("full vs incremental mismatch: %+v", mismatch)
	}
	if mismatch := engine.CompareRuns(full, resume); mismatch != nil {
		t.Fatalf("full vs resume mismatch: %+v", mismatch)
	}
	if len(resume.ReusedBlocks) != len(resume.Blocks) {
		t.Fatalf("resume did not reuse all blocks: reused=%d total=%d", len(resume.ReusedBlocks), len(resume.Blocks))
	}
}

func TestParallelDegreeBudgetAndDeterminism(t *testing.T) {
	bundle, err := sample.Generate(8881, []int{96})
	if err != nil {
		t.Fatal(err)
	}
	project, _, err := schema.Load(bundle.Projects[0].Artifacts[model.SchemaV3])
	if err != nil {
		t.Fatal(err)
	}
	baseConfig := engine.DefaultConfig()
	sequentialConfig := baseConfig
	sequentialConfig.MaxParallel = 1
	sequentialConfig.QueueCapacity = 1
	sequentialEngine, err := engine.New(project, sequentialConfig, nil)
	if err != nil {
		t.Fatal(err)
	}
	sequential, err := sequentialEngine.Run(context.Background(), engine.ModeFull)
	if err != nil && !model.IsVerificationError(err) {
		t.Fatal(err)
	}
	parallelConfig := baseConfig
	parallelConfig.MaxParallel = 4
	parallelConfig.QueueCapacity = 1
	parallelEngine, err := engine.New(project, parallelConfig, nil)
	if err != nil {
		t.Fatal(err)
	}
	parallelEngine.SetStrokeFailureHook(func(model.Stroke) error {
		time.Sleep(2 * time.Millisecond)
		return nil
	})
	parallel, err := parallelEngine.Run(context.Background(), engine.ModeFull)
	if err != nil && !model.IsVerificationError(err) {
		t.Fatal(err)
	}
	if mismatch := engine.CompareRuns(sequential, parallel); mismatch != nil {
		t.Fatalf("parallelism changed output: %+v", mismatch)
	}
	if parallel.Budget.MaxObservedParallel < 2 || parallel.Budget.MaxObservedParallel > 4 {
		t.Fatalf("unexpected observed parallelism: %+v", parallel.Budget)
	}
	foundQueue := false
	for _, event := range parallel.Budget.Events {
		if event.Stage == "queue" && event.Reason == "queue_budget" && event.WaitMicros > 0 {
			foundQueue = true
		}
	}
	if !foundQueue {
		t.Fatalf("queue budget exhaustion was not recorded: %+v", parallel.Budget)
	}
}

func TestSingleStrokeFailureIsolationAndRetry(t *testing.T) {
	bundle, err := sample.Generate(8882, []int{96})
	if err != nil {
		t.Fatal(err)
	}
	project, _, err := schema.Load(bundle.Projects[0].Artifacts[model.SchemaV3])
	if err != nil {
		t.Fatal(err)
	}
	store, err := checkpoint.Open(filepath.Join(t.TempDir(), "failure.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	config := engine.DefaultConfig()
	config.Tolerance.PixelPercent = 60
	config.Tolerance.AreaPercent = 60
	verificationEngine, err := engine.New(project, config, store)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := verificationEngine.Run(context.Background(), engine.ModeFull); err != nil {
		t.Fatal(err)
	}
	failingID := "s02"
	verificationEngine.SetStrokeFailureHook(func(stroke model.Stroke) error {
		if stroke.ID == failingID {
			return model.NewError(model.CodeSchema, "stroke."+stroke.ID, stroke.ID, "injected failure")
		}
		return nil
	})
	edited := project.Strokes[1]
	edited.Hardness = 73
	failed, failureErr := verificationEngine.ApplyStrokeEdit(context.Background(), edited)
	if failureErr == nil || failed == nil || len(failed.StrokeFailures) != 1 || failed.StrokeFailures[0] != failingID {
		t.Fatalf("failure was not isolated: result=%+v err=%v", failed, failureErr)
	}
	restored, err := store.Restore(checkpoint.Stamp{
		ProjectID: project.ID, ContentHash: canvasHashForTest(project),
		ConversionVersion: model.ConversionVersion, AlgorithmVersion: model.AlgorithmVersion,
		Width: project.Width, Height: project.Height,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !restored.Valid || restored.Strokes[failingID].Status != "failed" {
		t.Fatalf("failed stroke checkpoint missing: %+v", restored)
	}
	if restored.Strokes["s01"].Status != "committed" {
		t.Fatal("unrelated stroke was polluted")
	}
	verificationEngine.SetStrokeFailureHook(nil)
	retry, err := verificationEngine.Run(context.Background(), engine.ModeIncremental)
	if err != nil && !model.IsVerificationError(err) {
		t.Fatalf("retry failed: %v", err)
	}
	fullEngine, err := engine.New(project, config, store)
	if err != nil {
		t.Fatal(err)
	}
	full, err := fullEngine.Run(context.Background(), engine.ModeFull)
	if err != nil && !model.IsVerificationError(err) {
		t.Fatal(err)
	}
	if mismatch := engine.CompareRuns(retry, full); mismatch != nil {
		t.Fatalf("retry differs from full: %+v", mismatch)
	}
}

func TestCompareRunsLocatesFirstPixel(t *testing.T) {
	block := engine.BlockResult{
		Index: 0, BlockX: 0, BlockY: 0, Width: 2, Height: 2,
		Rect: model.Rect{MinX: 5, MinY: 7, MaxX: 6, MaxY: 8},
		Mask: []byte{10, 20, 30, 40},
	}
	left := &engine.RunResult{
		ProjectID: "p", Width: 10, Height: 10, CommitSequence: []int{0},
		Blocks: []engine.BlockResult{block}, Accumulated: []byte{1},
	}
	rightBlock := block
	rightBlock.Mask = []byte{10, 20, 30, 41}
	right := &engine.RunResult{
		ProjectID: "p", Width: 10, Height: 10, CommitSequence: []int{0},
		Blocks: []engine.BlockResult{rightBlock}, Accumulated: []byte{1},
	}
	mismatch := engine.CompareRuns(left, right)
	if mismatch == nil || mismatch.BlockIndex != 0 || mismatch.X != 6 || mismatch.Y != 8 {
		t.Fatalf("first mismatch not located: %+v", mismatch)
	}
}

func TestInvalidConfigRejected(t *testing.T) {
	bundle, err := sample.Generate(8883, []int{64})
	if err != nil {
		t.Fatal(err)
	}
	project, _, err := schema.Load(bundle.Projects[0].Artifacts[model.SchemaV3])
	if err != nil {
		t.Fatal(err)
	}
	config := engine.DefaultConfig()
	config.BlockSize = 32
	config.PixelBufferPerWorker = 10
	if _, err := engine.New(project, config, nil); err == nil || !model.HasCode(err, model.CodeParam) {
		t.Fatalf("undersized pixel budget was accepted: %v", err)
	}
}

func canvasHashForTest(project *model.Project) string {
	return model.HashStrings("canvas", model.HashBytes(project.CandidateMask), "96", "96")
}

func TestPreciseInvalidationAndRecovery(t *testing.T) {
	bundle, err := sample.Generate(777, []int{96})
	if err != nil {
		t.Fatal(err)
	}
	project, _, err := schema.Load(bundle.Projects[0].Artifacts[model.SchemaV3])
	if err != nil {
		t.Fatal(err)
	}
	store, err := checkpoint.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	config := engine.DefaultConfig()
	config.Tolerance.PixelPercent = 50
	config.Tolerance.AreaPercent = 50
	first, err := engine.New(project, config, store)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := first.Run(context.Background(), engine.ModeFull); err != nil {
		t.Fatal(err)
	}
	edited := project.Strokes[0]
	edited.X1 += 3
	edited.Hardness = 81
	result, err := first.ApplyStrokeEdit(context.Background(), edited)
	if err != nil && !model.IsVerificationError(err) {
		if result != nil {
			t.Logf("edit issues: %v", result.Issues)
			for _, domain := range result.Analysis.Domains {
				t.Logf("domain %s area=%d bbox=%+v dir=%s", domain.ID, domain.Area, domain.BBox, domain.Direction)
			}
			for _, truth := range project.Truth {
				t.Logf("truth %s area=%d bbox=%+v dir=%s", truth.ID, truth.Area, truth.BBox, truth.Direction)
			}
		}
		t.Fatalf("edit run: %v", err)
	}
	if result.Invalidation == nil || len(result.Invalidation.InvalidatedBlocks) == 0 {
		t.Fatal("expected invalidated blocks")
	}
	if len(result.ReusedBlocks) == 0 {
		t.Fatal("expected unaffected blocks to be reused")
	}
	editedProject, _, err := schema.Load(bundle.Projects[0].Artifacts[model.SchemaV3])
	if err != nil {
		t.Fatal(err)
	}
	editedProject.Strokes[0] = edited
	editedProject.Normalize()
	resumeEngine, err := engine.New(editedProject, config, store)
	if err != nil {
		t.Fatal(err)
	}
	resume, err := resumeEngine.Run(context.Background(), engine.ModeResume)
	if err != nil && !model.IsVerificationError(err) {
		t.Fatalf("resume after edit: %v", err)
	}
	fullEngine, err := engine.New(editedProject, config, store)
	if err != nil {
		t.Fatal(err)
	}
	full, err := fullEngine.Run(context.Background(), engine.ModeFull)
	if err != nil && !model.IsVerificationError(err) {
		t.Fatalf("full after edit: %v", err)
	}
	if mismatch := engine.CompareRuns(result, resume); mismatch != nil {
		t.Fatalf("edit vs resume mismatch: %+v", mismatch)
	}
	if mismatch := engine.CompareRuns(result, full); mismatch != nil {
		t.Fatalf("edit vs full mismatch: %+v", mismatch)
	}
}
