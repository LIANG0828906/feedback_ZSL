package checkpoint_test

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"

	"quotaflow/internal/checkpoint"
	"quotaflow/internal/domain"
	"quotaflow/internal/engine"
	"quotaflow/internal/generator"
)

const testVersion = "checkpoint-test-v1"

func TestCheckpointCrashHelper(t *testing.T) {
	if os.Getenv("QUOTAFLOW_CRASH_HELPER") != "1" {
		return
	}
	path := os.Getenv("QUOTAFLOW_CRASH_DB")
	project := generator.Scenario(42)
	eng, err := engine.New(project, engine.Config{ThreadBudget: 2, QueueLimit: 64, CheckpointVersion: testVersion})
	if err != nil {
		os.Exit(90)
	}
	store, _, err := checkpoint.Open(path, testVersion, project.CanonicalFingerprint())
	if err != nil {
		os.Exit(91)
	}
	if err := store.EnsureInitial(eng.InitialState()); err != nil {
		os.Exit(92)
	}
	for tick := int64(1); tick <= 6; tick++ {
		if err := eng.Step(); err != nil {
			os.Exit(93)
		}
		if err := store.Commit(tick, eng.LastState()); err != nil {
			os.Exit(94)
		}
	}
	if err := eng.Step(); err != nil {
		os.Exit(95)
	}
	_ = store.CommitWithHook(7, eng.LastState(), func(phase string) error {
		if phase == checkpoint.PhaseBeforeGlobal {
			os.Exit(137)
		}
		return nil
	})
	os.Exit(96)
}

func TestForcedTerminationRecoversOnlyCommittedTick(t *testing.T) {
	project := generator.Scenario(42)
	full, err := engine.New(project, engine.Config{ThreadBudget: 2, QueueLimit: 64, CheckpointVersion: testVersion})
	if err != nil {
		t.Fatalf("new full: %v", err)
	}
	if err := full.RunAll(); err != nil {
		t.Fatalf("run full: %v", err)
	}
	wantFinal, _ := full.Tick(project.MaxTick)
	path := filepath.Join(t.TempDir(), "forced-termination.db")
	command := exec.Command(os.Args[0], "-test.run=TestCheckpointCrashHelper")
	command.Env = append(os.Environ(),
		"QUOTAFLOW_CRASH_HELPER=1",
		"QUOTAFLOW_CRASH_DB="+path,
	)
	output, err := command.CombinedOutput()
	exitErr, ok := err.(*exec.ExitError)
	if !ok || exitErr.ExitCode() != 137 {
		t.Fatalf("helper exit=%v output=%s", err, output)
	}
	store, rebuilt, err := checkpoint.Open(path, testVersion, project.CanonicalFingerprint())
	if err != nil {
		t.Fatalf("reopen after forced exit: %v", err)
	}
	if rebuilt {
		t.Fatal("matching checkpoint was rebuilt after forced exit")
	}
	state, meta, err := store.Load()
	if err != nil {
		t.Fatalf("load after forced exit: %v", err)
	}
	store.Close()
	if meta.Tick != 6 {
		t.Fatalf("restart observed tick %d, want 6", meta.Tick)
	}
	resumed, err := engine.New(project, engine.Config{ThreadBudget: 2, QueueLimit: 64, CheckpointVersion: testVersion})
	if err != nil {
		t.Fatalf("new resumed: %v", err)
	}
	if err := resumed.ResumeFrom(state); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if err := resumed.RunAll(); err != nil {
		t.Fatalf("run resumed: %v", err)
	}
	got, _ := resumed.Tick(project.MaxTick)
	if !reflect.DeepEqual(got, wantFinal) {
		t.Fatal("forced-termination resume differs from uninterrupted run")
	}
}

func TestCheckpointCommitAndResumeFromPausedMigration(t *testing.T) {
	project := generator.Scenario(42)
	full, err := engine.New(project, engine.Config{ThreadBudget: 4, QueueLimit: 64, CheckpointVersion: testVersion})
	if err != nil {
		t.Fatalf("new full: %v", err)
	}
	if err := full.RunAll(); err != nil {
		t.Fatalf("run full: %v", err)
	}
	wantFinal, _ := full.Tick(project.MaxTick)

	path := filepath.Join(t.TempDir(), "checkpoint.db")
	eng, err := engine.New(project, engine.Config{ThreadBudget: 4, QueueLimit: 64, CheckpointVersion: testVersion})
	if err != nil {
		t.Fatalf("new checkpoint engine: %v", err)
	}
	store, rebuilt, err := checkpoint.Open(path, testVersion, project.CanonicalFingerprint())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if rebuilt {
		t.Fatal("new checkpoint should not report rebuild")
	}
	if err := store.EnsureInitial(eng.InitialState()); err != nil {
		t.Fatalf("ensure initial: %v", err)
	}
	for tick := int64(1); tick <= 6; tick++ {
		if err := eng.Step(); err != nil {
			t.Fatalf("step %d: %v", tick, err)
		}
		state := eng.LastState()
		if err := store.Commit(tick, state); err != nil {
			t.Fatalf("commit %d: %v", tick, err)
		}
	}
	loaded, meta, err := store.Load()
	if err != nil {
		t.Fatalf("load tick6: %v", err)
	}
	if meta.Tick != 6 || loaded.Tick != 6 {
		t.Fatalf("loaded tick %d/%d, want 6", loaded.Tick, meta.Tick)
	}
	if len(loaded.Pauses) == 0 {
		t.Fatal("tick6 checkpoint did not persist a paused migration")
	}
	// Simulate the next tick being computed and interrupted before the global
	// commit marker moves. The transaction must roll back to tick 6.
	if err := eng.Step(); err != nil {
		t.Fatalf("step 7: %v", err)
	}
	err = store.CommitWithHook(7, eng.LastState(), func(phase string) error {
		if phase == checkpoint.PhaseBeforeGlobal {
			return errors.New("simulated forced termination")
		}
		return nil
	})
	if domain.CodeOf(err) != domain.ECheckpointIO {
		t.Fatalf("interrupted commit got %v (%s)", err, domain.CodeOf(err))
	}
	store.Close()

	reopened, rebuilt, err := checkpoint.Open(path, testVersion, project.CanonicalFingerprint())
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if rebuilt {
		t.Fatal("matching checkpoint unexpectedly rebuilt")
	}
	loaded, meta, err = reopened.Load()
	if err != nil {
		t.Fatalf("load after interruption: %v", err)
	}
	if meta.Tick != 6 {
		t.Fatalf("restart observed uncommitted tick %d", meta.Tick)
	}
	resumed, err := engine.New(project, engine.Config{ThreadBudget: 3, QueueLimit: 64, CheckpointVersion: testVersion})
	if err != nil {
		t.Fatalf("new resumed: %v", err)
	}
	if err := resumed.ResumeFrom(loaded); err != nil {
		t.Fatalf("resume: %v", err)
	}
	if err := resumed.RunAll(); err != nil {
		t.Fatalf("run resumed: %v", err)
	}
	gotFinal, _ := resumed.Tick(project.MaxTick)
	if !reflect.DeepEqual(gotFinal, wantFinal) {
		t.Fatal("resumed final tick differs from uninterrupted full run")
	}
	reopened.Close()
}

func TestCheckpointVersionMismatchRebuilds(t *testing.T) {
	project := generator.Scenario(42)
	path := filepath.Join(t.TempDir(), "checkpoint.db")
	store, _, err := checkpoint.Open(path, testVersion, project.CanonicalFingerprint())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	eng, _ := engine.New(project, engine.Config{ThreadBudget: 1, QueueLimit: 16, CheckpointVersion: testVersion})
	if err := store.EnsureInitial(eng.InitialState()); err != nil {
		t.Fatalf("initial: %v", err)
	}
	if err := eng.Step(); err != nil {
		t.Fatalf("step: %v", err)
	}
	if err := store.Commit(1, eng.LastState()); err != nil {
		t.Fatalf("commit: %v", err)
	}
	store.Close()
	rebuiltStore, rebuilt, err := checkpoint.Open(path, "checkpoint-test-v2", "different-hash")
	if err != nil {
		t.Fatalf("open mismatch: %v", err)
	}
	if !rebuilt {
		t.Fatal("version mismatch did not rebuild checkpoint")
	}
	_, meta, err := rebuiltStore.Load()
	if err != nil {
		t.Fatalf("load rebuilt: %v", err)
	}
	if meta.Tick != 0 || meta.Version != "checkpoint-test-v2" {
		t.Fatalf("rebuild metadata = %+v", meta)
	}
	rebuiltStore.Close()
}

func TestTruncatedCheckpointRejected(t *testing.T) {
	project := generator.Scenario(42)
	path := filepath.Join(t.TempDir(), "checkpoint.db")
	store, _, err := checkpoint.Open(path, testVersion, project.CanonicalFingerprint())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	eng, _ := engine.New(project, engine.Config{ThreadBudget: 1, QueueLimit: 16, CheckpointVersion: testVersion})
	if err := store.EnsureInitial(eng.InitialState()); err != nil {
		t.Fatalf("initial: %v", err)
	}
	if err := eng.Step(); err != nil {
		t.Fatalf("step: %v", err)
	}
	if err := store.Commit(1, eng.LastState()); err != nil {
		t.Fatalf("commit: %v", err)
	}
	store.Close()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read db: %v", err)
	}
	if len(raw) < 512 {
		t.Fatalf("database unexpectedly small: %d", len(raw))
	}
	if err := os.WriteFile(path, raw[:len(raw)/2], 0o644); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	_, _, err = checkpoint.Open(path, testVersion, project.CanonicalFingerprint())
	if code := domain.CodeOf(err); code != domain.ECheckpointTruncated && code != domain.ECheckpointCorrupt {
		t.Fatalf("truncated checkpoint got %v (%s)", err, code)
	}
}
