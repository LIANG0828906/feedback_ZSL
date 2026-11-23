package orchestrator

import "testing"

func TestParallelMatchesSerial(t *testing.T) {
	p := genProject(t)
	order := p.Policy.QueueOrder

	serial, serialOrder, serr := OrchestrateParallel(p, ResourceLimits{MaxWorkers: 1})
	if serr != nil {
		t.Fatalf("serial run: %v", serr)
	}
	par, parOrder, perr := OrchestrateParallel(p, ResourceLimits{MaxWorkers: 4})
	if perr != nil {
		t.Fatalf("parallel run: %v", perr)
	}
	if derr := CompareResults(serial, par, order); derr != nil {
		t.Fatalf("parallel vs serial mismatch: %v", derr)
	}
	if len(parOrder) != len(serialOrder) {
		t.Fatalf("commit sequence length %d != %d", len(parOrder), len(serialOrder))
	}
	for i := range serialOrder {
		if parOrder[i] != serialOrder[i] {
			t.Fatalf("commit sequence %d: %s != %s", i, parOrder[i], serialOrder[i])
		}
	}
}

func TestResourceLimitsEnforced(t *testing.T) {
	p := genProject(t)
	_, _, err := OrchestrateParallel(p, ResourceLimits{MaxWorkers: 2, MaxCharsPerDialogue: 1})
	if err == nil || err.Code != EPara || err.Field != "text_buffer" {
		t.Fatalf("want E_PARA on text buffer limit, got %v", err)
	}
	_, _, err = OrchestrateParallel(p, ResourceLimits{MaxWorkers: 2, MaxSamplesPerDialogue: 10})
	if err == nil || err.Code != EPara || err.Field != "sample_buffer" {
		t.Fatalf("want E_PARA on sample buffer limit, got %v", err)
	}
}

func TestErrorIsolationAndRetry(t *testing.T) {
	p := genProject(t)
	// Inject a zero-sum rhythm fault into one dialogue.
	victim := &p.Dialogues[0]
	saved := make([]float64, len(victim.Rhythm))
	copy(saved, victim.Rhythm)
	for i := range victim.Rhythm {
		victim.Rhythm[i] = 0
	}

	o, err := NewOrchestrator(p)
	if err != nil {
		t.Fatalf("new orchestrator: %v", err)
	}
	runErr := o.OrchestrateAll()
	if runErr == nil || runErr.Code != ERhy || runErr.DialogueID != victim.ID {
		t.Fatalf("want E_RHY for injected fault, got %v", runErr)
	}
	if o.States[victim.ID].State != StateFailed {
		t.Fatalf("faulted dialogue must be failed, got %s", o.States[victim.ID].State)
	}
	// The fault does not pollute other dialogues.
	done, total := o.Progress()
	if done != total-1 {
		t.Fatalf("progress %d/%d, want %d isolated failures", done, total, 1)
	}
	for id, st := range o.States {
		if id != victim.ID && st.State != StateSubmitted {
			t.Fatalf("dialogue %s polluted by fault: %s", id, st.State)
		}
	}
	// Fix the fault and retry only that dialogue.
	copy(victim.Rhythm, saved)
	if rerr := o.OrchestrateOne(victim.ID); rerr != nil {
		t.Fatalf("retry: %v", rerr)
	}
	if o.States[victim.ID].State != StateSubmitted {
		t.Fatalf("retried dialogue must be submitted, got %s", o.States[victim.ID].State)
	}
	done, total = o.Progress()
	if done != total {
		t.Fatalf("progress %d/%d after retry", done, total)
	}
}
