package locklab

import "testing"

func TestGeneratedProjectExecutes(t *testing.T) {
	project := GenerateProject(20261127, DefaultGenerateOptions())
	result, err := Run(project, RunOptions{})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	t.Logf("commits=%v", commitsByTx(result))
	t.Logf("rollbacks=%v", rollbacksByTx(result))
	t.Logf("victims=%v", result.DeadlockVictims)
	t.Logf("anomalies=%v", result.Anomalies)
	for _, step := range result.Steps {
		if step.Outcome == OutcomeBlocked || step.EventuallyGranted || step.LockDecision == LockRejected {
			t.Logf("step=%d tx=%s op=%s outcome=%s lock=%s seat=%d conflict=%v wait=%s exec=%d",
				step.GlobalStep, step.TxID, step.Kind, step.Outcome, step.LockDecision,
				step.WaitSeat, step.ConflictWith, step.WaitOutcome, step.ExecutedStep)
		}
	}
	for _, read := range result.Reads {
		t.Logf("read tx=%s item=%s step=%d version=%d writer=%s value=%s dirty=%t",
			read.TxID, read.ItemID, read.GlobalStep, read.VersionNumber, read.WriterTx, read.Value, read.Dirty)
	}
	for item, version := range result.FinalWrites {
		t.Logf("final %s=%s writer=%s seq=%d", item, version.Value, version.WriterTx, version.CommitSeq)
	}
}

func TestThreeSchemaSemanticEquality(t *testing.T) {
	project := GenerateProject(20261127, DefaultGenerateOptions())
	var reference string
	for version := 1; version <= 3; version++ {
		data, err := EncodeProject(project, version)
		if err != nil {
			t.Fatalf("encode v%d: %v", version, err)
		}
		loaded, err := LoadProject(data)
		if err != nil {
			t.Fatalf("load v%d: %v", version, err)
		}
		result, err := Run(loaded, RunOptions{})
		if err != nil {
			t.Fatalf("run v%d: %v", version, err)
		}
		if version == 1 {
			reference = result.SemanticDigest()
		} else if got := result.SemanticDigest(); got != reference {
			t.Fatalf("v%d semantic result differs from v1", version)
		}
	}
}

func TestTimeoutWithoutDeadlockDetection(t *testing.T) {
	options := DefaultGenerateOptions()
	options.DeadlockDetection = false
	project := GenerateProject(20261127, options)
	result, err := Run(project, RunOptions{})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	reasons := rollbacksByTx(result)
	if reasons["T4"] != "wait_timeout" || reasons["T5"] != "wait_timeout" {
		t.Fatalf("want both deadlock participants to time out, got %v", reasons)
	}
	if len(result.DeadlockVictims) != 0 {
		t.Fatalf("disabled deadlock detection produced victims: %v", result.DeadlockVictims)
	}
}

func TestCoreOperationsDoNotMutateSourceProject(t *testing.T) {
	project := GenerateProject(20261127, DefaultGenerateOptions())
	before := project.CanonicalDigest()
	if _, err := Run(project, RunOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := RunParallel(project, ParallelOptions{Workers: 2, BatchSize: 2}); err != nil {
		t.Fatal(err)
	}
	edited := project.Clone()
	edited.Transactions[1].Isolation = RepeatableRead
	if _, err := RunIncremental(project, edited, "T2"); err != nil {
		t.Fatal(err)
	}
	if got := project.CanonicalDigest(); got != before {
		t.Fatalf("source project mutated: got %s want %s", got, before)
	}
}

func TestTimeoutVerdictSameAcrossSchemas(t *testing.T) {
	options := DefaultGenerateOptions()
	options.DeadlockDetection = false
	project := GenerateProject(20261127, options)
	var reference string
	for version := 1; version <= 3; version++ {
		data, err := EncodeProject(project, version)
		if err != nil {
			t.Fatalf("encode v%d: %v", version, err)
		}
		loaded, err := LoadProject(data)
		if err != nil {
			t.Fatalf("load v%d: %v", version, err)
		}
		result, err := Run(loaded, RunOptions{})
		if err != nil {
			t.Fatalf("run v%d: %v", version, err)
		}
		if reasons := rollbacksByTx(result); reasons["T4"] != "wait_timeout" || reasons["T5"] != "wait_timeout" {
			t.Fatalf("v%d timeout verdict mismatch: %v", version, reasons)
		}
		if version == 1 {
			reference = result.SemanticDigest()
		} else if result.SemanticDigest() != reference {
			t.Fatalf("v%d timeout semantic digest differs", version)
		}
	}
}

func commitsByTx(result *RunResult) map[string]uint64 {
	out := make(map[string]uint64)
	for _, commit := range result.Commits {
		out[commit.TxID] = commit.CommitSeq
	}
	return out
}

func rollbacksByTx(result *RunResult) map[string]string {
	out := make(map[string]string)
	for _, rollback := range result.Rollbacks {
		out[rollback.TxID] = rollback.Reason
	}
	return out
}
