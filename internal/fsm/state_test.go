package fsm

import (
	"testing"

	"roomreverb/internal/errs"
	"roomreverb/internal/solver"
)

func TestLegalTransitionsAndFrozenSnapshot(t *testing.T) {
	tr := New([]string{"r0"})
	if err := tr.Transition("r0", Connectivity, ""); err != nil {
		t.Fatal(err)
	}
	if err := tr.Transition("r0", Parameters, ""); err != nil {
		t.Fatal(err)
	}
	result := solver.RoomResult{RoomID: "r0", Pairs: []solver.PairResult{{SourceID: "s0", ListenerID: "l0"}}}
	if err := tr.Commit("r0", Frozen{Result: result, Pairs: result.Pairs, ContentHash: "abc"}); err != nil {
		t.Fatal(err)
	}
	result.Pairs[0].SourceID = "mutated"
	got, _ := tr.Get("r0")
	if got.State != Committed || got.Frozen == nil || got.Frozen.Result.Pairs[0].SourceID != "s0" {
		t.Fatalf("committed snapshot was not frozen: %+v", got)
	}
}

func TestIllegalTransitionKeepsOriginalState(t *testing.T) {
	tr := New([]string{"r0"})
	err := tr.Transition("r0", Committed, "")
	if !errs.IsCode(err, errs.EState) {
		t.Fatalf("want E_STATE got %v", err)
	}
	got, _ := tr.Get("r0")
	if got.State != Pending {
		t.Fatalf("state changed on illegal transition: %s", got.State)
	}
	if err := tr.Transition("r0", Connectivity, ""); err != nil {
		t.Fatal(err)
	}
	if err := tr.Transition("r0", Parameters, ""); err != nil {
		t.Fatal(err)
	}
	if err := tr.Commit("r0", Frozen{}); err != nil {
		t.Fatal(err)
	}
	if err := tr.Transition("r0", Parameters, ""); !errs.IsCode(err, errs.EState) {
		t.Fatalf("want E_STATE got %v", err)
	}
	got, _ = tr.Get("r0")
	if got.State != Committed {
		t.Fatal("committed state was not preserved")
	}
	if err := tr.Invalidate("r0", "params_changed"); err != nil {
		t.Fatal(err)
	}
	got, _ = tr.Get("r0")
	if got.State != Pending || got.Frozen != nil {
		t.Fatalf("invalidate did not unfreeze: %+v", got)
	}
}

func TestFailedRetry(t *testing.T) {
	tr := New([]string{"r0"})
	_ = tr.Transition("r0", Connectivity, "")
	tr.Fail("r0", errs.New(errs.EConn, "no route"))
	if err := tr.Retry("r0"); err != nil {
		t.Fatal(err)
	}
	got, _ := tr.Get("r0")
	if got.State != Pending {
		t.Fatalf("want pending got %s", got.State)
	}
}
