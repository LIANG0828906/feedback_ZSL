package orchestrator

import "testing"

func TestQueueOrderPreservedUnderInterrupt(t *testing.T) {
	p := genProject(t)
	o, err := NewOrchestrator(p)
	if err != nil {
		t.Fatalf("new orchestrator: %v", err)
	}
	if oerr := o.OrchestrateAll(); oerr != nil {
		t.Fatalf("orchestrate: %v", oerr)
	}
	// Commit order equals the declared queue order even though some
	// dialogues were interrupted and resumed in place.
	want := p.Policy.QueueOrder
	got := o.CommitOrder()
	if len(got) != len(want) {
		t.Fatalf("commit order length %d != %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("commit order position %d: got %s want %s", i, got[i], want[i])
		}
	}
	done, total := o.Progress()
	if done != total {
		t.Fatalf("progress %d/%d after full run", done, total)
	}
}

func TestQueueConflicts(t *testing.T) {
	p := genProject(t)
	p.Policy.QueueOrder[1] = p.Policy.QueueOrder[0]
	_, err := BuildQueue(p)
	if err == nil || err.Code != EQueue {
		t.Fatalf("want E_QUEUE for duplicate, got %v", err)
	}
	if err.Position != 1 {
		t.Fatalf("E_QUEUE must carry conflict position, got %d", err.Position)
	}

	p = genProject(t)
	p.Policy.QueueOrder[0] = "dlg-ghost"
	if _, err := BuildQueue(p); err == nil || err.Code != EQueue || err.DialogueID != "dlg-ghost" {
		t.Fatalf("want E_QUEUE for unknown dialogue, got %v", err)
	}

	p = genProject(t)
	q, qerr := BuildQueue(p)
	if qerr != nil {
		t.Fatalf("build queue: %v", qerr)
	}
	// Resume requested for a dialogue that is not resume-pending.
	id := q.At(0)
	if rerr := q.CheckResumePosition(id, map[string]bool{}); rerr == nil || rerr.Code != EQueue {
		t.Fatalf("want E_QUEUE for missing resume position, got %v", rerr)
	}
}

func TestStateMachine(t *testing.T) {
	st := &DialogueState{DialogueID: "dlg-x", State: StatePending}
	if err := st.Transition(StateSubmitted); err == nil || err.Code != EState {
		t.Fatalf("want E_STATE for pending->submitted, got %v", err)
	}
	if st.State != StatePending {
		t.Fatalf("illegal transition mutated state to %s", st.State)
	}
	// Legal arc: pending -> orchestrating -> interrupted -> resume
	// pending -> orchestrating -> submitted.
	for _, to := range []State{StateOrchestrating, StateInterrupted, StateResumePending, StateOrchestrating, StateSubmitted} {
		if err := st.Transition(to); err != nil {
			t.Fatalf("legal transition to %s rejected: %v", to, err)
		}
	}
	if err := st.Transition(StateOrchestrating); err == nil || err.Code != EState {
		t.Fatalf("submitted dialogue must be locked, got %v", err)
	}
	// Failed dialogues may retry.
	f := &DialogueState{DialogueID: "dlg-y", State: StateFailed}
	if err := f.Transition(StateOrchestrating); err != nil {
		t.Fatalf("retry from failed must be legal: %v", err)
	}
}

func TestSubmittedDialogueLocked(t *testing.T) {
	p := genProject(t)
	o, err := NewOrchestrator(p)
	if err != nil {
		t.Fatalf("new orchestrator: %v", err)
	}
	id := o.Queue.At(0)
	if oerr := o.OrchestrateOne(id); oerr != nil {
		t.Fatalf("orchestrate: %v", oerr)
	}
	before := o.Results[id].Schedule.PresentEnd
	if eerr := o.EditDialogue(id, func(d *Dialogue) { d.Rhythm[0] = 99 }); eerr == nil || eerr.Code != EState {
		t.Fatalf("want E_STATE editing submitted dialogue, got %v", eerr)
	}
	if o.Results[id].Schedule.PresentEnd != before {
		t.Fatal("locked result changed after rejected edit")
	}
	if oerr := o.OrchestrateOne(id); oerr == nil || oerr.Code != EState {
		t.Fatalf("want E_STATE re-orchestrating submitted dialogue, got %v", oerr)
	}
}
