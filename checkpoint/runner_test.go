package checkpoint

import (
	"context"
	"path/filepath"
	"sync"
	"testing"

	"skillbudget"
	"skillbudget/samplegen"
)

func fixture(t *testing.T) (*skillcore.Project, *samplegen.Bundle) {
	t.Helper()
	bundle, err := samplegen.Generate(42)
	if err != nil {
		t.Fatal(err)
	}
	project, err := skillcore.LoadVersion(bundle.Versions[3], 3)
	if err != nil {
		t.Fatal(err)
	}
	return project, bundle
}

func baseConfig(totalTicks int64) RunConfig {
	return RunConfig{
		WindowSize: 7, TotalTicks: totalTicks, Workers: 3, BufferLimit: 1,
		SampleEvery: 1, MaxAttempts: 1,
	}
}

func TestThreePathsMatchTruth(t *testing.T) {
	project, bundle := fixture(t)
	store, err := Open(filepath.Join(t.TempDir(), "checkpoint.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	runner := NewRunner(store)

	fullCfg := baseConfig(bundle.TotalTicks)
	fullCfg.RunID = "full"
	full, err := runner.Run(context.Background(), project, bundle.Intents, RunFull, fullCfg)
	if err != nil {
		t.Fatal(err)
	}
	if mismatch := skillcore.CompareSnapshots(bundle.Truth, full.Ticks); mismatch != nil {
		t.Fatalf("full path vs truth: %v", mismatch)
	}

	realtimeCfg := baseConfig(bundle.TotalTicks)
	realtimeCfg.RunID = "paths"
	realtime, err := runner.Run(context.Background(), project, bundle.Intents, RunRealtime, realtimeCfg)
	if err != nil {
		t.Fatal(err)
	}
	if mismatch := skillcore.CompareSnapshots(full.Ticks, realtime.Ticks); mismatch != nil {
		t.Fatalf("realtime path: %v", mismatch)
	}

	partialCfg := baseConfig(bundle.TotalTicks)
	partialCfg.RunID = "paths"
	partialCfg.StopAfterWindows = 2
	partial, err := runner.Run(context.Background(), project, bundle.Intents, RunRealtime, partialCfg)
	if err != nil {
		t.Fatal(err)
	}
	if partial.Complete {
		t.Fatal("partial run unexpectedly complete")
	}
	resumeCfg := baseConfig(bundle.TotalTicks)
	resumeCfg.RunID = "paths"
	resumed, err := runner.Run(context.Background(), project, bundle.Intents, RunResume, resumeCfg)
	if err != nil {
		t.Fatal(err)
	}
	if !resumed.Complete || len(resumed.ResumedWindows) != 2 || resumed.ResumedTick != 14 {
		t.Fatalf("unexpected resume metadata: complete=%v reused=%v tick=%d",
			resumed.Complete, resumed.ResumedWindows, resumed.ResumedTick)
	}
	if mismatch := skillcore.CompareSnapshots(realtime.Ticks, resumed.Ticks); mismatch != nil {
		t.Fatalf("resume path: %v", mismatch)
	}
}

func TestFailedWindowCanBeRetriedWithoutChangingCommittedWindows(t *testing.T) {
	project, bundle := fixture(t)
	store, err := Open(filepath.Join(t.TempDir(), "retry.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	runner := NewRunner(store)
	cfg := baseConfig(bundle.TotalTicks)
	cfg.RunID = "retry"
	cfg.InjectFailure = true
	cfg.FailureWindow = 2
	cfg.FailureAttempts = 100
	partial, err := runner.Run(context.Background(), project, bundle.Intents, RunRealtime, cfg)
	if err == nil {
		t.Fatal("expected injected window failure")
	}
	if partial == nil || len(partial.Windows) != 2 {
		t.Fatalf("expected two committed windows, got %#v", partial)
	}
	state, err := store.WindowState(context.Background(), cfg.RunID, 2)
	if err != nil {
		t.Fatal(err)
	}
	if state != skillcore.WindowFailed {
		t.Fatalf("window 2 state=%s, want failed", state)
	}
	resumeCfg := baseConfig(bundle.TotalTicks)
	resumeCfg.RunID = "retry"
	resumed, err := runner.Run(context.Background(), project, bundle.Intents, RunResume, resumeCfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(resumed.ResumedWindows) != 2 {
		t.Fatalf("committed windows were not reused: %v", resumed.ResumedWindows)
	}
	if mismatch := skillcore.CompareSnapshots(bundle.Truth, resumed.Ticks); mismatch != nil {
		t.Fatalf("retry result: %v", mismatch)
	}
}

func TestSourceVersionMismatchInvalidatesEntireRun(t *testing.T) {
	_, bundle := fixture(t)
	v3, _ := skillcore.LoadVersion(bundle.Versions[3], 3)
	v1, _ := skillcore.LoadVersion(bundle.Versions[1], 1)
	store, err := Open(filepath.Join(t.TempDir(), "invalidate.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	runner := NewRunner(store)
	cfg := baseConfig(bundle.TotalTicks)
	cfg.RunID = "versioned"
	cfg.StopAfterWindows = 3
	if _, err := runner.Run(context.Background(), v3, bundle.Intents, RunRealtime, cfg); err != nil {
		t.Fatal(err)
	}
	resumeCfg := baseConfig(bundle.TotalTicks)
	resumeCfg.RunID = "versioned"
	resumed, err := runner.Run(context.Background(), v1, bundle.Intents, RunResume, resumeCfg)
	if err != nil {
		t.Fatal(err)
	}
	if !resumed.Invalidated || resumed.ResumedTick != 0 || len(resumed.ResumedWindows) != 0 {
		t.Fatalf("expected full invalidation, got %#v", resumed)
	}
	if mismatch := skillcore.CompareSnapshots(bundle.Truth, resumed.Ticks); mismatch != nil {
		t.Fatalf("recomputed result: %v", mismatch)
	}
}

func TestWindowCachePersistsAcrossReopen(t *testing.T) {
	project, bundle := fixture(t)
	path := filepath.Join(t.TempDir(), "cache.db")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	runner := NewRunner(store)
	windowCount := int((bundle.TotalTicks + bundle.WindowSize - 1) / bundle.WindowSize)
	keys := make([]string, windowCount)
	for i := 0; i < windowCount; i++ {
		start := int64(i) * bundle.WindowSize
		end := start + bundle.WindowSize
		if end > bundle.TotalTicks {
			end = bundle.TotalTicks
		}
		keys[i] = skillcore.WindowDependencyKey(project, start, end, bundle.Intents)
	}
	cfg := baseConfig(bundle.TotalTicks)
	cfg.RunID = "cache"
	cfg.UseCache = true
	cfg.CacheKeys = keys
	first, err := runner.Run(context.Background(), project, bundle.Intents, RunRealtime, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.CacheHits) != 0 {
		t.Fatalf("unexpected first-run cache hits: %v", first.CacheHits)
	}
	cfg.RunID = "cache"
	second, err := runner.Run(context.Background(), project, bundle.Intents, RunRealtime, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(second.CacheHits) != windowCount {
		t.Fatalf("cache hits=%v, want all %d", second.CacheHits, windowCount)
	}
	if mismatch := skillcore.CompareSnapshots(first.Ticks, second.Ticks); mismatch != nil {
		t.Fatal(mismatch)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	runner = NewRunner(store)
	cfg.RunID = "cache-reopen"
	third, err := runner.Run(context.Background(), project, bundle.Intents, RunRealtime, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(third.CacheHits) != windowCount {
		t.Fatalf("cache after reopen hits=%v, want all %d", third.CacheHits, windowCount)
	}
	if mismatch := skillcore.CompareSnapshots(first.Ticks, third.Ticks); mismatch != nil {
		t.Fatal(mismatch)
	}
}

func TestEditAwareCacheReusesOnlyUnaffectedWindows(t *testing.T) {
	project, bundle := fixture(t)
	lateSkill := skillcore.Skill{
		ID: "late_blade", PoolID: "mana", Cost: 100, Cooldown: skillcore.Const(8),
		CastTicks: 0, CDRBp: 0, Priority: 40, Enabled: true,
	}
	project.Skills = append(project.Skills, lateSkill)
	if err := project.Normalize(); err != nil {
		t.Fatal(err)
	}
	intents := append([]skillcore.Intent(nil), bundle.Intents...)
	intents = append(intents, skillcore.Intent{
		Tick: 23, SkillID: "late_blade", TargetID: "enemy-alpha", Sequence: 1000,
	})
	windowCount := int((bundle.TotalTicks + bundle.WindowSize - 1) / bundle.WindowSize)
	keys, plan, err := skillcore.EditAwareWindowKeys(
		project, intents, bundle.TotalTicks, bundle.WindowSize, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.ReusableWindows) != windowCount {
		t.Fatalf("unexpected initial plan: %#v", plan)
	}
	store, err := Open(filepath.Join(t.TempDir(), "edit-cache.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	runner := NewRunner(store)
	cfg := baseConfig(bundle.TotalTicks)
	cfg.RunID = "edit-cache"
	cfg.UseCache = true
	cfg.CacheKeys = keys
	if _, err := runner.Run(context.Background(), project, intents, RunRealtime, cfg); err != nil {
		t.Fatal(err)
	}

	edited := project.Clone()
	edited.SkillByID("late_blade").Cooldown = skillcore.Const(16)
	if err := edited.Normalize(); err != nil {
		t.Fatal(err)
	}
	edits := []skillcore.Edit{{
		AtTick: 21, Kind: skillcore.EditCooldown, SkillID: "late_blade",
		Reason: "late skill cooldown changed",
	}}
	editedKeys, editPlan, err := skillcore.EditAwareWindowKeys(
		edited, intents, bundle.TotalTicks, bundle.WindowSize, edits, keys)
	if err != nil {
		t.Fatal(err)
	}
	if !containsIndex(editPlan.AffectedWindows, 3) || !containsIndex(editPlan.ReusableWindows, 2) {
		t.Fatalf("unexpected edit plan: affected=%v reusable=%v", editPlan.AffectedWindows, editPlan.ReusableWindows)
	}
	cfg.RunID = "edit-cache-after"
	cfg.CacheKeys = editedKeys
	cachedResult, err := runner.Run(context.Background(), edited, intents, RunRealtime, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !containsIndex(cachedResult.CacheHits, 0) || !containsIndex(cachedResult.CacheHits, 1) ||
		!containsIndex(cachedResult.CacheHits, 2) {
		t.Fatalf("unaffected windows were not reused from persistent cache: %v", cachedResult.CacheHits)
	}
	if containsIndex(cachedResult.CacheHits, 3) {
		t.Fatalf("affected window unexpectedly hit cache: %v", cachedResult.CacheHits)
	}
	fullCfg := baseConfig(bundle.TotalTicks)
	fullCfg.RunID = "edit-cache-full"
	full, err := runner.Run(context.Background(), edited, intents, RunFull, fullCfg)
	if err != nil {
		t.Fatal(err)
	}
	if mismatch := skillcore.CompareSnapshots(full.Ticks, cachedResult.Ticks); mismatch != nil {
		t.Fatal(mismatch)
	}
}

func TestCorruptRuleHashInvalidatesWholeRun(t *testing.T) {
	project, bundle := fixture(t)
	store, err := Open(filepath.Join(t.TempDir(), "hash.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	runner := NewRunner(store)
	cfg := baseConfig(bundle.TotalTicks)
	cfg.RunID = "hash"
	cfg.StopAfterWindows = 2
	if _, err := runner.Run(context.Background(), project, bundle.Intents, RunRealtime, cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(context.Background(),
		`UPDATE checkpoint_windows SET rule_hash='tampered' WHERE run_id=? AND window_index=1`, cfg.RunID); err != nil {
		t.Fatal(err)
	}
	resumeCfg := baseConfig(bundle.TotalTicks)
	resumeCfg.RunID = cfg.RunID
	result, err := runner.Run(context.Background(), project, bundle.Intents, RunResume, resumeCfg)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Invalidated || result.ResumedTick != 0 || len(result.ResumedWindows) != 0 {
		t.Fatalf("corrupt run was not fully invalidated: %#v", result)
	}
	if mismatch := skillcore.CompareSnapshots(bundle.Truth, result.Ticks); mismatch != nil {
		t.Fatal(mismatch)
	}
}

func TestPanicIsIsolatedToFailedWindow(t *testing.T) {
	project, bundle := fixture(t)
	store, err := Open(filepath.Join(t.TempDir(), "panic.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	runner := NewRunner(store)
	cfg := baseConfig(bundle.TotalTicks)
	cfg.RunID = "panic"
	cfg.InjectPanic = true
	cfg.PanicWindow = 1
	result, err := runner.Run(context.Background(), project, bundle.Intents, RunRealtime, cfg)
	if err == nil {
		t.Fatal("expected isolated panic failure")
	}
	if result == nil || len(result.Windows) != 1 {
		t.Fatalf("committed prefix was polluted: %#v", result)
	}
	state, err := store.WindowState(context.Background(), cfg.RunID, 1)
	if err != nil || state != skillcore.WindowFailed {
		t.Fatalf("panic window state=%s err=%v", state, err)
	}
	resumeCfg := baseConfig(bundle.TotalTicks)
	resumeCfg.RunID = cfg.RunID
	resumed, err := runner.Run(context.Background(), project, bundle.Intents, RunResume, resumeCfg)
	if err != nil {
		t.Fatal(err)
	}
	if mismatch := skillcore.CompareSnapshots(bundle.Truth, resumed.Ticks); mismatch != nil {
		t.Fatal(mismatch)
	}
}

func TestThreePathsAgreeAfterRuleEdit(t *testing.T) {
	project, bundle := fixture(t)
	edited := project.Clone()
	edited.SkillByID("arc_nova").Cooldown = skillcore.Const(20)
	if err := edited.Normalize(); err != nil {
		t.Fatal(err)
	}
	store, err := Open(filepath.Join(t.TempDir(), "edited-paths.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	runner := NewRunner(store)

	cfg := baseConfig(bundle.TotalTicks)
	cfg.RunID = "edited-full"
	full, err := runner.Run(context.Background(), edited, bundle.Intents, RunFull, cfg)
	if err != nil {
		t.Fatal(err)
	}
	cfg.RunID = "edited-realtime"
	realtime, err := runner.Run(context.Background(), edited, bundle.Intents, RunRealtime, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if mismatch := skillcore.CompareSnapshots(full.Ticks, realtime.Ticks); mismatch != nil {
		t.Fatal(mismatch)
	}
	partialCfg := cfg
	partialCfg.RunID = "edited-resume"
	partialCfg.StopAfterWindows = 2
	if _, err := runner.Run(context.Background(), edited, bundle.Intents, RunRealtime, partialCfg); err != nil {
		t.Fatal(err)
	}
	resumeCfg := partialCfg
	resumeCfg.StopAfterWindows = 0
	resumed, err := runner.Run(context.Background(), edited, bundle.Intents, RunResume, resumeCfg)
	if err != nil {
		t.Fatal(err)
	}
	if mismatch := skillcore.CompareSnapshots(full.Ticks, resumed.Ticks); mismatch != nil {
		t.Fatal(mismatch)
	}
	if len(resumed.ResumedWindows) != 2 {
		t.Fatalf("edit rerun did not resume checkpoint prefix: %v", resumed.ResumedWindows)
	}
}

func TestConcurrentWindowTransitionsRemainSerialized(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "concurrent.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	const count = 8
	var wg sync.WaitGroup
	errs := make(chan error, count)
	for i := 0; i < count; i++ {
		index := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx := context.Background()
			start := int64(index) * 7
			end := start + 7
			if err := store.EnsureWindow(ctx, "concurrent", index, start, end); err != nil {
				errs <- err
				return
			}
			if err := store.BeginWindow(ctx, BeginWindowOptions{
				RunID: "concurrent", Index: index, StartTick: start, EndTick: end,
				RuleHash: "hash", SourceVersion: 3, AlgorithmVersion: skillcore.AlgorithmVersion,
				StateBeforeJSON: "{}",
			}); err != nil {
				errs <- err
				return
			}
			if err := store.CommitWindow(ctx, CommitWindowOptions{
				RunID: "concurrent", Index: index, StateAfterJSON: "{}",
				ResultJSON: "{}", DecisionDigest: "digest",
			}); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	rows, err := store.LoadWindows(context.Background(), "concurrent")
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != count {
		t.Fatalf("rows=%d", len(rows))
	}
	for index, row := range rows {
		if row.Index != index || row.Status != skillcore.WindowCommitted {
			t.Fatalf("row %d not serialized/committed: %#v", index, row)
		}
	}
}

func containsIndex(values []int, needle int) bool {
	for _, value := range values {
		if value == needle {
			return true
		}
	}
	return false
}

func TestIllegalWindowTransitionIsStable(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "fsm.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	if err := store.EnsureWindow(ctx, "run", 0, 0, 7); err != nil {
		t.Fatal(err)
	}
	if err := store.BeginWindow(ctx, BeginWindowOptions{
		RunID: "run", Index: 0, StartTick: 0, EndTick: 7, RuleHash: "hash",
		SourceVersion: 3, AlgorithmVersion: skillcore.AlgorithmVersion, StateBeforeJSON: "{}",
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.CommitWindow(ctx, CommitWindowOptions{
		RunID: "run", Index: 0, StateAfterJSON: "{}", ResultJSON: "{}", DecisionDigest: "d",
	}); err != nil {
		t.Fatal(err)
	}
	err = store.BeginWindow(ctx, BeginWindowOptions{
		RunID: "run", Index: 0, StartTick: 0, EndTick: 7, RuleHash: "hash",
		SourceVersion: 3, AlgorithmVersion: skillcore.AlgorithmVersion, StateBeforeJSON: "{}",
	})
	if !skillcore.IsCode(err, skillcore.CodeState) {
		t.Fatalf("expected E_STATE, got %v", err)
	}
	state, err := store.WindowState(ctx, "run", 0)
	if err != nil {
		t.Fatal(err)
	}
	if state != skillcore.WindowCommitted {
		t.Fatalf("state mutated to %s", state)
	}
}
