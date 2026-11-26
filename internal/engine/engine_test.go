package engine_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"testing"
	"time"

	"layerforge/internal/apperr"
	"layerforge/internal/build"
	"layerforge/internal/checkpoint"
	"layerforge/internal/engine"
	"layerforge/internal/generator"
	"layerforge/internal/loader"
	"layerforge/internal/model"
	"layerforge/internal/sqlitefile"
)

func TestColdBuildAndCacheRestart(t *testing.T) {
	fixture := mustFixture(t)
	store := checkpoint.Open(filepath.Join(t.TempDir(), "checkpoint.db"))
	eng := mustEngine(t, fixture.Project, store)
	result, err := eng.Build(context.Background(), engine.BuildOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if result.BuildCount != 8 || result.HitCount != 0 {
		t.Fatalf("build=%d hit=%d", result.BuildCount, result.HitCount)
	}
	if len(result.Rounds) != 6 {
		t.Fatalf("rounds = %d, want 6", len(result.Rounds))
	}
	deferred := 0
	for _, round := range result.Rounds {
		deferred += len(round.Deferred)
		if round.LatencyUsedMS > round.LatencyBudgetMS {
			t.Fatalf("round %d exceeded latency budget", round.Round)
		}
	}
	if deferred != 1 {
		t.Fatalf("deferrals = %d, want 1 (package waits for latency headroom)", deferred)
	}
	if !reflect.DeepEqual(result.FinalDigests, mapDigests(fixture.Derivation)) {
		t.Fatal("final digests differ from generator truth")
	}
	snapshot, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.LastRound != 6 || len(snapshot.Index) != 8 {
		t.Fatalf("lastRound=%d indexes=%d", snapshot.LastRound, len(snapshot.Index))
	}

	restarted := mustEngine(t, fixture.Project, store)
	second, err := restarted.Build(context.Background(), engine.BuildOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if second.HitCount != 8 || second.BuildCount != 0 || len(second.Rounds) != 0 {
		t.Fatalf("restart hit=%d build=%d rounds=%d", second.HitCount, second.BuildCount, len(second.Rounds))
	}
}

func TestDifferentWorkerCountsProduceSameDigests(t *testing.T) {
	fixture := mustFixture(t)
	var digests map[string]string
	for _, workers := range []int{1, 2, 8} {
		store := checkpoint.Open(filepath.Join(t.TempDir(), "checkpoint.db"))
		eng := mustEngine(t, fixture.Project, store)
		result, err := eng.Build(context.Background(), engine.BuildOptions{
			Budget: &engine.BudgetOverride{
				Latency:         17 * time.Millisecond,
				MaxRounds:       8,
				ThreadLimit:     workers,
				MemoryPerThread: 64 * 1024,
				BufferPerThread: 2,
			},
		})
		if err != nil {
			t.Fatalf("workers=%d: %v", workers, err)
		}
		if digests == nil {
			digests = result.FinalDigests
		} else if !reflect.DeepEqual(digests, result.FinalDigests) {
			t.Fatalf("workers=%d changed final digests", workers)
		}
	}
}

func TestCacheIsSharedAcrossSchemaVersions(t *testing.T) {
	fixture := mustFixture(t)
	store := checkpoint.Open(filepath.Join(t.TempDir(), "cross-schema.db"))
	v1, err := loader.Load(fixture.Versions["project.v1.json"])
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mustEngine(t, v1.Project, store).Build(context.Background(), engine.BuildOptions{}); err != nil {
		t.Fatal(err)
	}
	v3, err := loader.Load(fixture.Versions["project.v3.json"])
	if err != nil {
		t.Fatal(err)
	}
	result, err := mustEngine(t, v3.Project, store).Build(context.Background(), engine.BuildOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if result.HitCount != 8 || result.BuildCount != 0 {
		t.Fatalf("cross-schema hit=%d build=%d", result.HitCount, result.BuildCount)
	}
}

func TestInterruptedBuildResumesFromLastCompleteRound(t *testing.T) {
	fixture := mustFixture(t)
	store := checkpoint.Open(filepath.Join(t.TempDir(), "checkpoint.db"))
	eng := mustEngine(t, fixture.Project, store)
	first, err := eng.Build(context.Background(), engine.BuildOptions{InterruptAfterRound: 2})
	if !apperr.IsCode(err, apperr.Interrupted) {
		t.Fatalf("error = %v", err)
	}
	if len(first.Rounds) != 2 {
		t.Fatalf("rounds = %d", len(first.Rounds))
	}
	snapshot, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.LastRound != 2 {
		t.Fatalf("last round = %d", snapshot.LastRound)
	}

	resumedEngine := mustEngine(t, fixture.Project, store)
	resumed, err := resumedEngine.Build(context.Background(), engine.BuildOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if resumed.HitCount != 4 || resumed.BuildCount != 4 {
		t.Fatalf("resume hit=%d build=%d, want 4/4", resumed.HitCount, resumed.BuildCount)
	}
	if !reflect.DeepEqual(resumed.FinalDigests, mapDigests(fixture.Derivation)) {
		t.Fatal("resume digests differ")
	}
}

func TestBudgetDegradesWithoutChangingLayers(t *testing.T) {
	fixture := mustFixture(t)
	baselineStore := checkpoint.Open(filepath.Join(t.TempDir(), "baseline.db"))
	baseline, err := mustEngine(t, fixture.Project, baselineStore).Build(context.Background(), engine.BuildOptions{})
	if err != nil {
		t.Fatal(err)
	}
	lowMemoryStore := checkpoint.Open(filepath.Join(t.TempDir(), "low-memory.db"))
	lowMemory, err := mustEngine(t, fixture.Project, lowMemoryStore).Build(context.Background(), engine.BuildOptions{
		Budget: &engine.BudgetOverride{
			Latency:         17 * time.Millisecond,
			MaxRounds:       2,
			ThreadLimit:     2,
			MemoryPerThread: 1,
			BufferPerThread: 2,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !hasDegradation(lowMemory, apperr.BudgetMemory) || !hasDegradation(lowMemory, apperr.BudgetLatency) {
		t.Fatalf("degradations = %#v", lowMemory.Degradations)
	}
	for _, round := range lowMemory.Rounds {
		if round.PeakMemoryBytes > 1 {
			t.Fatalf("peak memory %d exceeds budget", round.PeakMemoryBytes)
		}
	}
	if !reflect.DeepEqual(baseline.FinalDigests, lowMemory.FinalDigests) {
		t.Fatal("degraded build changed digests")
	}

	lowLatencyStore := checkpoint.Open(filepath.Join(t.TempDir(), "low-latency.db"))
	lowLatency, err := mustEngine(t, fixture.Project, lowLatencyStore).Build(context.Background(), engine.BuildOptions{
		Budget: &engine.BudgetOverride{
			Latency:         time.Millisecond,
			MaxRounds:       20,
			ThreadLimit:     2,
			MemoryPerThread: 64 * 1024,
			BufferPerThread: 2,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !hasDegradation(lowLatency, apperr.BudgetLatency) {
		t.Fatalf("latency degradation missing: %#v", lowLatency.Degradations)
	}
	if !reflect.DeepEqual(baseline.FinalDigests, lowLatency.FinalDigests) {
		t.Fatal("latency-relaxed build changed digests")
	}
}

func TestInvalidBudgetsHaveStableCodes(t *testing.T) {
	fixture := mustFixture(t)
	cases := []struct {
		name   string
		budget engine.BudgetOverride
		code   apperr.Code
	}{
		{"threads", engine.BudgetOverride{Latency: time.Millisecond, MaxRounds: 1, ThreadLimit: 0, MemoryPerThread: 1024, BufferPerThread: 1}, apperr.BudgetThreads},
		{"memory", engine.BudgetOverride{Latency: time.Millisecond, MaxRounds: 1, ThreadLimit: 1, MemoryPerThread: 0, BufferPerThread: 1}, apperr.BudgetMemory},
		{"buffer", engine.BudgetOverride{Latency: time.Millisecond, MaxRounds: 1, ThreadLimit: 1, MemoryPerThread: 1024, BufferPerThread: 0}, apperr.BackpressureQueue},
		{"latency", engine.BudgetOverride{Latency: 0, MaxRounds: 1, ThreadLimit: 1, MemoryPerThread: 1024, BufferPerThread: 1}, apperr.BudgetLatency},
	}
	for _, test := range cases {
		test := test
		t.Run(test.name, func(t *testing.T) {
			eng := mustEngine(t, fixture.Project, checkpoint.Open(filepath.Join(t.TempDir(), "checkpoint.db")))
			_, err := eng.Build(context.Background(), engine.BuildOptions{Budget: &test.budget})
			if !apperr.IsCode(err, test.code) {
				t.Fatalf("code = %s, error = %v", apperr.CodeOf(err), err)
			}
		})
	}
}

func TestIncrementalEditOnlyRebuildsDownstream(t *testing.T) {
	fixture := mustFixture(t)
	oldProject := fixture.Project
	edited, err := engine.EditStep(oldProject, 4, func(step *model.Step) {
		step.CommandFingerprint = model.HashFields("test-edit", "package-command")
	})
	if err != nil {
		t.Fatal(err)
	}
	store := checkpoint.Open(filepath.Join(t.TempDir(), "incremental.db"))
	if _, err := mustEngine(t, oldProject, store).Build(context.Background(), engine.BuildOptions{}); err != nil {
		t.Fatal(err)
	}
	editedEngine := mustEngine(t, edited, store)
	result, err := editedEngine.Build(context.Background(), engine.BuildOptions{PreviousProject: &oldProject})
	if err != nil {
		t.Fatal(err)
	}
	if result.HitCount != 4 || result.BuildCount != 4 {
		t.Fatalf("incremental hit=%d build=%d", result.HitCount, result.BuildCount)
	}
	wantBuilt := []int{4, 5, 6, 8}
	var gotBuilt []int
	for _, decision := range result.Decisions {
		if !decision.Hit {
			gotBuilt = append(gotBuilt, decision.Step)
		}
	}
	if !reflect.DeepEqual(gotBuilt, wantBuilt) {
		t.Fatalf("built steps = %v, want %v", gotBuilt, wantBuilt)
	}
	for _, decision := range result.Decisions {
		switch decision.Step {
		case 4:
			if decision.Source != "edit:command" {
				t.Fatalf("step 4 source = %s", decision.Source)
			}
		case 5, 6, 8:
			if decision.Source != "upstream:4:edit:command" {
				t.Fatalf("step %d source = %s", decision.Step, decision.Source)
			}
		default:
			if !decision.Hit || decision.Source != "cache" {
				t.Fatalf("step %d should be a cache hit, got %#v", decision.Step, decision)
			}
		}
	}

	freshStore := checkpoint.Open(filepath.Join(t.TempDir(), "fresh-edited.db"))
	fresh, err := mustEngine(t, edited, freshStore).Build(context.Background(), engine.BuildOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(result.FinalDigests, fresh.FinalDigests) {
		t.Fatal("incremental digests differ from full rebuild")
	}
}

func TestCleanupRejectsSharedReferencesAndRebuildsTerminalLayer(t *testing.T) {
	fixture := mustFixture(t)
	store := checkpoint.Open(filepath.Join(t.TempDir(), "cleanup.db"))
	eng := mustEngine(t, fixture.Project, store)
	if _, err := eng.Build(context.Background(), engine.BuildOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := eng.Cleanup([]string{"step:package"}); !apperr.IsCode(err, apperr.CleanupConflict) {
		t.Fatalf("shared cleanup code = %s, error = %v", apperr.CodeOf(err), err)
	}
	clean, err := eng.Cleanup([]string{"step:release"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(clean.RebuildSteps, []int{8}) {
		t.Fatalf("rebuild steps = %v", clean.RebuildSteps)
	}
	snapshot, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := snapshot.Layers["step:release"]; exists {
		t.Fatal("terminal layer was not removed")
	}
	if _, exists := snapshot.Layers["step:package"]; !exists {
		t.Fatal("shared layer was removed after conflict")
	}
	rebuildEngine := mustEngine(t, fixture.Project, store)
	rebuilt, err := rebuildEngine.Build(context.Background(), engine.BuildOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if rebuilt.HitCount != 7 || rebuilt.BuildCount != 1 {
		t.Fatalf("post-cleanup hit=%d build=%d", rebuilt.HitCount, rebuilt.BuildCount)
	}
}

func TestCorruptCheckpointErrorsAndVersionReset(t *testing.T) {
	fixture := mustFixture(t)
	path := filepath.Join(t.TempDir(), "checkpoint.db")
	store := checkpoint.Open(path)
	if _, err := mustEngine(t, fixture.Project, store).Build(context.Background(), engine.BuildOptions{}); err != nil {
		t.Fatal(err)
	}

	truncated := filepath.Join(t.TempDir(), "truncated.db")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(truncated, data[:len(data)/2], 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = mustEngine(t, fixture.Project, checkpoint.Open(truncated)).Build(context.Background(), engine.BuildOptions{})
	if !apperr.IsCode(err, apperr.CheckpointTruncated) {
		t.Fatalf("truncated code = %s, error = %v", apperr.CodeOf(err), err)
	}

	db, err := sqlitefile.Read(path)
	if err != nil {
		t.Fatal(err)
	}
	for tableIndex := range db.Tables {
		if db.Tables[tableIndex].Name != "meta" {
			continue
		}
		for rowIndex := range db.Tables[tableIndex].Rows {
			if db.Tables[tableIndex].Rows[rowIndex][0] == "version" {
				db.Tables[tableIndex].Rows[rowIndex][1] = "layerforge-checkpoint/v0"
			}
		}
	}
	if err := sqlitefile.WriteAtomic(path, *db, nil); err != nil {
		t.Fatal(err)
	}
	reset, err := mustEngine(t, fixture.Project, store).Build(context.Background(), engine.BuildOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !reset.Recovery.Reset || reset.Recovery.Code != apperr.CheckpointVersion {
		t.Fatalf("recovery = %#v", reset.Recovery)
	}
	if reset.BuildCount != 8 || reset.HitCount != 0 {
		t.Fatalf("version reset build=%d hit=%d", reset.BuildCount, reset.HitCount)
	}
}

func TestCheckpointRejectsIndexWithoutCommittedLayer(t *testing.T) {
	store := checkpoint.Open(filepath.Join(t.TempDir(), "invalid.db"))
	snapshot := checkpoint.NewSnapshot("fixture")
	snapshot.Index["sha256:key"] = checkpoint.IndexRecord{
		CacheKey:     "sha256:key",
		LayerID:      "step:missing",
		Digest:       "sha256:digest",
		SizeBytes:    1,
		ProducerStep: 1,
		Round:        1,
	}
	err := store.CommitRound(snapshot)
	if !apperr.IsCode(err, apperr.CheckpointIntegrity) {
		t.Fatalf("code = %s, error = %v", apperr.CodeOf(err), err)
	}
}

func TestCommitFaultsLeaveCheckpointRecoverable(t *testing.T) {
	fixture := mustFixture(t)
	store := checkpoint.Open(filepath.Join(t.TempDir(), "fault-before-rename.db"))
	eng := mustEngine(t, fixture.Project, store)
	_, err := eng.Build(context.Background(), engine.BuildOptions{
		CommitHook: func(round int, stage sqlitefile.CommitStage) error {
			if round == 3 && stage == sqlitefile.StageBeforeRename {
				return errors.New("simulated process loss before rename")
			}
			return nil
		},
	})
	if !apperr.IsCode(err, apperr.CheckpointIO) {
		t.Fatalf("fault code = %s, error = %v", apperr.CodeOf(err), err)
	}
	snapshot, loadErr := store.Load()
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if snapshot.LastRound != 2 {
		t.Fatalf("last round after pre-rename fault = %d, want 2", snapshot.LastRound)
	}
	resumed, resumeErr := mustEngine(t, fixture.Project, store).Build(context.Background(), engine.BuildOptions{})
	if resumeErr != nil {
		t.Fatal(resumeErr)
	}
	if resumed.BuildCount != 4 || resumed.HitCount != 4 {
		t.Fatalf("resume after fault hit=%d build=%d", resumed.HitCount, resumed.BuildCount)
	}

	afterRenameStore := checkpoint.Open(filepath.Join(t.TempDir(), "fault-after-rename.db"))
	_, err = mustEngine(t, fixture.Project, afterRenameStore).Build(context.Background(), engine.BuildOptions{
		CommitHook: func(round int, stage sqlitefile.CommitStage) error {
			if round == 2 && stage == sqlitefile.StageAfterRename {
				return errors.New("simulated process loss after rename")
			}
			return nil
		},
	})
	if !apperr.IsCode(err, apperr.CheckpointIO) {
		t.Fatalf("after-rename fault code = %s, error = %v", apperr.CodeOf(err), err)
	}
	afterRenameSnapshot, loadErr := afterRenameStore.Load()
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if afterRenameSnapshot.LastRound != 2 {
		t.Fatalf("last round after rename fault = %d, want 2", afterRenameSnapshot.LastRound)
	}
	if _, integrityErr := mustEngine(t, fixture.Project, afterRenameStore).Build(context.Background(), engine.BuildOptions{}); integrityErr != nil {
		t.Fatal(integrityErr)
	}
}

func TestCancellationStopsAfterCompleteRound(t *testing.T) {
	fixture := mustFixture(t)
	store := checkpoint.Open(filepath.Join(t.TempDir(), "cancel.db"))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result, err := mustEngine(t, fixture.Project, store).Build(ctx, engine.BuildOptions{
		CommitHook: func(round int, stage sqlitefile.CommitStage) error {
			if round == 1 && stage == sqlitefile.StageAfterRename {
				cancel()
			}
			return nil
		},
	})
	if !apperr.IsCode(err, apperr.Interrupted) {
		t.Fatalf("code = %s, error = %v", apperr.CodeOf(err), err)
	}
	if !result.Interrupted || len(result.Rounds) != 1 {
		t.Fatalf("interrupted=%t rounds=%d", result.Interrupted, len(result.Rounds))
	}
	snapshot, loadErr := store.Load()
	if loadErr != nil {
		t.Fatal(loadErr)
	}
	if snapshot.LastRound != 1 {
		t.Fatalf("persisted rounds = %d", snapshot.LastRound)
	}
}

func TestBufferCapacityEmitsBackpressureCode(t *testing.T) {
	project := model.Project{
		ID:   "buffer-fixture",
		Name: "buffer-fixture",
		Budget: model.Budget{
			Latency:         100 * time.Millisecond,
			MaxRounds:       2,
			ThreadLimit:     2,
			MemoryPerThread: 4096,
			BufferPerThread: 2,
		},
	}
	for number := 1; number <= 5; number++ {
		project.Steps = append(project.Steps, model.Step{
			Number:             number,
			Name:               "independent-" + strconv.Itoa(number),
			Inputs:             []model.FileInput{{Path: "input.txt", Digest: model.HashFields("input", strconv.Itoa(number))}},
			CommandFingerprint: model.HashFields("command", strconv.Itoa(number)),
			Output:             model.Layer{ID: "step:independent-" + strconv.Itoa(number), SizeBytes: 1024},
		})
	}
	store := checkpoint.Open(filepath.Join(t.TempDir(), "buffer.db"))
	result, err := mustEngine(t, project, store).Build(context.Background(), engine.BuildOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Backpressure) != 1 || result.Backpressure[0].Code != apperr.BackpressureQueue {
		t.Fatalf("backpressure = %#v", result.Backpressure)
	}
	if !containsCode(result.Codes, apperr.BackpressureQueue) {
		t.Fatalf("codes = %v", result.Codes)
	}
	if len(result.Rounds) != 2 || len(result.Rounds[0].Deferred) != 1 || result.Rounds[0].Deferred[0].Reason != "buffer" {
		t.Fatalf("rounds = %#v", result.Rounds)
	}
}

func mustFixture(t *testing.T) *generator.Fixture {
	t.Helper()
	fixture, err := generator.Generate(generator.DefaultSeed)
	if err != nil {
		t.Fatal(err)
	}
	return fixture
}

func mustEngine(t *testing.T, project model.Project, store *checkpoint.Store) *engine.Engine {
	t.Helper()
	eng, err := engine.New(project, store)
	if err != nil {
		t.Fatal(err)
	}
	return eng
}

func mapDigests(derivation *build.Derivation) map[string]string {
	out := make(map[string]string, len(derivation.Layers))
	for layerID, layer := range derivation.Layers {
		out[layerID] = layer.Digest
	}
	return out
}

func hasDegradation(result engine.BuildResult, code apperr.Code) bool {
	for _, degradation := range result.Degradations {
		if degradation.Code == code {
			return true
		}
	}
	return false
}

func containsCode(codes []apperr.Code, code apperr.Code) bool {
	for _, candidate := range codes {
		if candidate == code {
			return true
		}
	}
	return false
}
