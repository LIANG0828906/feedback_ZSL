package locklab

import (
	"reflect"
	"testing"
)

func TestIncrementalLockModeReconvergesSuffix(t *testing.T) {
	oldProject := GenerateProject(20261127, DefaultGenerateOptions())
	newProject := oldProject.Clone()
	newProject.Transactions[7].Ops[3].Mode = ModeS
	full, err := Run(newProject, RunOptions{})
	if err != nil {
		t.Fatalf("full: %v", err)
	}
	incremental, err := RunIncremental(oldProject, newProject, "T8")
	if err != nil {
		t.Fatalf("incremental: %v", err)
	}
	if incremental.Result.SemanticDigest() != full.SemanticDigest() {
		t.Fatalf("incremental digest differs from full")
	}
	if incremental.Report.ReusedPrefixSteps == 0 {
		t.Fatalf("expected prefix cache reuse: %+v", incremental.Report)
	}
	if incremental.Report.ResyncStep == 0 || incremental.Report.ReusedSuffixSteps == 0 {
		t.Fatalf("expected state reconvergence and suffix reuse: %+v", incremental.Report)
	}
	if !containsString(incremental.Report.PropagationClosure, "T8") {
		t.Fatalf("closure misses edited transaction: %+v", incremental.Report.PropagationClosure)
	}
}

func TestIncrementalWriteTargetRecomputeMatchesFull(t *testing.T) {
	oldProject := GenerateProject(20261127, DefaultGenerateOptions())
	newProject := oldProject.Clone()
	newProject.Transactions[8].Ops[4].ItemID = "inventory/I1"
	full, err := Run(newProject, RunOptions{})
	if err != nil {
		t.Fatalf("full: %v", err)
	}
	incremental, err := RunIncremental(oldProject, newProject, "T9")
	if err != nil {
		t.Fatalf("incremental: %v", err)
	}
	if incremental.Result.SemanticDigest() != full.SemanticDigest() {
		t.Fatalf("incremental digest differs from full")
	}
	if incremental.Report.ReusedPrefixSteps == 0 || len(incremental.Report.RecomputedSteps) == 0 {
		t.Fatalf("expected cache reuse plus recomputation: %+v", incremental.Report)
	}
}

func TestIncrementalIsolationChangesReadVisibility(t *testing.T) {
	oldProject := GenerateProject(20261127, DefaultGenerateOptions())
	newProject := oldProject.Clone()
	newProject.Transactions[1].Isolation = RepeatableRead
	full, err := Run(newProject, RunOptions{})
	if err != nil {
		t.Fatalf("full: %v", err)
	}
	incremental, err := RunIncremental(oldProject, newProject, "T2")
	if err != nil {
		t.Fatalf("incremental: %v", err)
	}
	if incremental.Result.SemanticDigest() != full.SemanticDigest() {
		t.Fatalf("incremental digest differs from full")
	}
	if !reflect.DeepEqual(incremental.Result.Reads, full.Reads) {
		t.Fatalf("incremental read visibility differs from full")
	}
	if !containsString(incremental.Report.EditedFields, "isolation") {
		t.Fatalf("isolation edit not reported: %+v", incremental.Report.EditedFields)
	}
	if len(incremental.Report.AffectedQueueIntervals) == 0 {
		t.Fatalf("isolation edit did not report affected wait-queue intervals")
	}
}

func TestIncrementalRejectsMultiTransactionEdit(t *testing.T) {
	oldProject := GenerateProject(20261127, DefaultGenerateOptions())
	newProject := oldProject.Clone()
	newProject.Transactions[0].Ops[0].Mode = ModeIS
	newProject.Transactions[1].Isolation = Serializable
	if _, err := RunIncremental(oldProject, newProject, "T1"); ErrorCodeOf(err) != EInvalidEdit {
		t.Fatalf("want invalid edit, got %v", err)
	}
}

func TestIncrementalOperationReorderAcrossTransactions(t *testing.T) {
	oldProject := GenerateProject(20261127, DefaultGenerateOptions())
	newProject := oldProject.Clone()
	readOne := newProject.Transactions[7].Ops[1]
	readTwo := newProject.Transactions[7].Ops[2]
	newProject.Transactions[7].Ops[1] = readTwo
	newProject.Transactions[7].Ops[2] = readOne
	newProject.Transactions[7].Ops[1].LocalSeq = 2
	newProject.Transactions[7].Ops[1].AtMs = 216
	newProject.Transactions[7].Ops[2].LocalSeq = 3
	newProject.Transactions[7].Ops[2].AtMs = 217
	if err := ValidateProject(newProject); err != nil {
		t.Fatalf("edited project invalid: %v", err)
	}
	full, err := Run(newProject, RunOptions{})
	if err != nil {
		t.Fatalf("full: %v", err)
	}
	incremental, err := RunIncremental(oldProject, newProject, "T8")
	if err != nil {
		t.Fatalf("incremental: %v", err)
	}
	if incremental.Result.SemanticDigest() != full.SemanticDigest() {
		t.Fatalf("incremental reorder digest differs from full")
	}
	if incremental.Report.FirstInvalidatedStep == 0 {
		t.Fatalf("reorder was not invalidated")
	}
}

func TestCachedBaselineIncremental(t *testing.T) {
	oldProject := GenerateProject(20261127, DefaultGenerateOptions())
	baseline, err := BuildBaseline(oldProject)
	if err != nil {
		t.Fatalf("baseline: %v", err)
	}
	newProject := oldProject.Clone()
	newProject.Transactions[1].Isolation = RepeatableRead
	incremental, err := RunIncrementalWithBaseline(baseline, newProject, "T2")
	if err != nil {
		t.Fatalf("cached incremental: %v", err)
	}
	full, err := Run(newProject, RunOptions{})
	if err != nil {
		t.Fatalf("full: %v", err)
	}
	if incremental.Result.SemanticDigest() != full.SemanticDigest() {
		t.Fatalf("cached baseline result differs from full")
	}
}
