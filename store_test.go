package dressbatch

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestSQLiteCheckpointRestartAndTamperRecovery(t *testing.T) {
	project := testProject(t)
	result, candidate := testPack(t, project)
	dbPath := filepath.Join(t.TempDir(), "checkpoints.db")
	run := NewRunRecord("restart-test", project)
	store, err := OpenStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CommitResult(context.Background(), run, project, result); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = OpenStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	recovery, err := store.RecoverRun(context.Background(), run.RunID, project)
	if err != nil {
		t.Fatal(err)
	}
	if recovery.Truncated || recovery.ValidPrefix != len(result.Batches) {
		t.Fatalf("unexpected clean recovery: %+v", recovery)
	}
	if err := compareRecovered(result, recovery); err != nil {
		t.Fatal(err)
	}

	if err := store.TamperForTest(context.Background(), run.RunID, 2, "batch_content_hash", "tampered"); err != nil {
		t.Fatal(err)
	}
	recovery, err = store.RecoverRun(context.Background(), run.RunID, project)
	if err != nil {
		t.Fatal(err)
	}
	if !recovery.Truncated || recovery.ValidPrefix != 1 || recovery.DiscardedFrom != 2 {
		t.Fatalf("unexpected tamper recovery: %+v", recovery)
	}
	if recovery.NextCommitSeq != 2 {
		t.Fatalf("next commit sequence expected 2 got %d", recovery.NextCommitSeq)
	}
	count, err := store.CountCheckpoints(context.Background(), run.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("checkpoint suffix was not discarded: %d", count)
	}
	if _, err := store.AppendNextBatch(context.Background(), run.RunID, project, result.Batches[1]); err != nil {
		t.Fatalf("continue from committed prefix: %v", err)
	}
	if _, err := store.AppendNextBatch(context.Background(), run.RunID, project, result.Batches[2]); err != nil {
		t.Fatalf("continue suffix: %v", err)
	}
	recovery, err = store.RecoverRun(context.Background(), run.RunID, project)
	if err != nil {
		t.Fatal(err)
	}
	if recovery.Truncated || recovery.ValidPrefix != len(result.Batches) {
		t.Fatalf("resumed checkpoint is not valid: %+v", recovery)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	_ = candidate
}

func TestSQLiteVersionTamperDiscardsSuffix(t *testing.T) {
	project := testProject(t)
	result, _ := testPack(t, project)
	dbPath := filepath.Join(t.TempDir(), "version.db")
	store, err := OpenStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	run := NewRunRecord("version-test", project)
	if err := store.CommitResult(context.Background(), run, project, result); err != nil {
		t.Fatal(err)
	}
	if err := store.TamperForTest(context.Background(), run.RunID, 2, "material_version", "bad-version"); err != nil {
		t.Fatal(err)
	}
	recovery, err := store.RecoverRun(context.Background(), run.RunID, project)
	if err != nil {
		t.Fatal(err)
	}
	if recovery.ValidPrefix != 1 || recovery.DiscardedFrom != 2 {
		t.Fatalf("unexpected recovery: %+v", recovery)
	}
}

func TestAtomicReplacePlan(t *testing.T) {
	project := testProject(t)
	result, _ := testPack(t, project)
	store, err := OpenStore(filepath.Join(t.TempDir(), "replace.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	run := NewRunRecord("replace-test", project)
	if err := store.ReplacePlan(context.Background(), run, project, result.Batches); err != nil {
		t.Fatal(err)
	}
	recovery, err := store.RecoverRun(context.Background(), run.RunID, project)
	if err != nil {
		t.Fatal(err)
	}
	if recovery.ValidPrefix != len(result.Batches) {
		t.Fatalf("replace plan not recoverable: %+v", recovery)
	}
	if err := compareRecovered(result, recovery); err != nil {
		t.Fatal(err)
	}
}

func TestForcedTerminationRecovery(t *testing.T) {
	if os.Getenv("DRESSBATCH_CRASH_HELPER") == "1" {
		runCrashHelper()
		return
	}
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "crash.db")
	readyPath := filepath.Join(dir, "ready")
	command := exec.Command(os.Args[0], "-test.run=TestForcedTerminationRecovery")
	command.Env = append(os.Environ(),
		"DRESSBATCH_CRASH_HELPER=1",
		"DRESSBATCH_DB="+dbPath,
		"DRESSBATCH_READY="+readyPath,
	)
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(readyPath); err == nil {
			break
		}
		if time.Now().After(deadline) {
			_ = command.Process.Kill()
			t.Fatal("crash helper did not become ready")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := command.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	_ = command.Wait()

	project, err := GenerateProject(777, 0)
	if err != nil {
		t.Fatal(err)
	}
	store, err := OpenStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	recovery, err := store.RecoverRun(context.Background(), "forced-kill", project)
	if err != nil {
		t.Fatal(err)
	}
	if recovery.ValidPrefix != 2 || recovery.Truncated {
		t.Fatalf("committed prefix was not recovered after SIGKILL: %+v", recovery)
	}
}

func runCrashHelper() {
	project, err := GenerateProject(777, 0)
	if err != nil {
		os.Exit(2)
	}
	candidate := testLegalCandidateNoTesting(project)
	result, err := Pack(project, candidate)
	if err != nil || len(result.Batches) < 2 {
		os.Exit(3)
	}
	store, err := OpenStore(os.Getenv("DRESSBATCH_DB"))
	if err != nil {
		os.Exit(4)
	}
	run := NewRunRecord("forced-kill", project)
	if err := store.BeginRun(context.Background(), run); err != nil {
		os.Exit(5)
	}
	for i := 0; i < 2; i++ {
		if err := store.AppendBatch(context.Background(), BatchCheckpoint{
			RunID:             run.RunID,
			CommitSeq:         i + 1,
			ProjectHash:       run.ProjectHash,
			ConstraintVersion: run.ConstraintVersion,
			AlgorithmVersion:  run.AlgorithmVersion,
			MaterialVersion:   MaterialVersion(project, result.Batches[i].MaterialSlots),
			Batch:             result.Batches[i],
		}); err != nil {
			os.Exit(6)
		}
	}
	if err := os.WriteFile(os.Getenv("DRESSBATCH_READY"), []byte("ready"), 0o644); err != nil {
		os.Exit(7)
	}
	select {}
}

func testLegalCandidateNoTesting(project *Project) []string {
	for _, sample := range project.Truth.Assemblies {
		if sample.ExpectedLegal {
			return sample.CandidatePartIDs
		}
	}
	return nil
}

func compareRecovered(result *PackResult, recovery *RecoveryReport) error {
	if recovery.ValidPrefix != len(result.Batches) {
		return newError(CodeRecovery, "recovery", "valid_prefix", "recovered prefix does not match batch count")
	}
	copyResult := *result
	copyResult.Batches = recovery.ValidBatches
	copyResult.CommitSequence = append([]string(nil), recovery.ValidBatchIDs...)
	return ComparePackResults(result, &copyResult)
}
