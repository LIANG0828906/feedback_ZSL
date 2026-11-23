package orchestrator

import "testing"

func TestIncrementalMatchesFullRecompute(t *testing.T) {
	p := genProject(t)
	o, err := NewOrchestrator(p)
	if err != nil {
		t.Fatalf("new orchestrator: %v", err)
	}
	order := o.Queue.Order()
	// Submit the first two dialogues; they stay locked and reused.
	for _, id := range order[:2] {
		if oerr := o.OrchestrateOne(id); oerr != nil {
			t.Fatalf("orchestrate %s: %v", id, oerr)
		}
	}
	editedID := order[2]
	if eerr := o.EditDialogue(editedID, func(d *Dialogue) {
		for i := range d.Rhythm {
			d.Rhythm[i] *= 1.5
		}
	}); eerr != nil {
		t.Fatalf("edit: %v", eerr)
	}
	manifest, rerr := o.RecomputeIncremental(editedID)
	if rerr != nil {
		t.Fatalf("incremental recompute: %v", rerr)
	}
	for i, entry := range manifest {
		if i < 2 && entry.Action != "reused" {
			t.Fatalf("entry %d (%s) should be reused, got %s", i, entry.DialogueID, entry.Action)
		}
		if i >= 2 && entry.Action != "recomputed" {
			t.Fatalf("entry %d (%s) should be recomputed, got %s", i, entry.DialogueID, entry.Action)
		}
		if entry.Reason == "" {
			t.Fatalf("entry %s missing invalidation reason", entry.DialogueID)
		}
	}
	// Incremental path must equal a from-scratch full recompute.
	full, _, ferr := FullRecompute(p)
	if ferr != nil {
		t.Fatalf("full recompute: %v", ferr)
	}
	if derr := CompareResults(o.Results, full, order); derr != nil {
		t.Fatalf("incremental vs full mismatch: %v", derr)
	}
}

func TestCompareResultsDetectsDiff(t *testing.T) {
	p := genProject(t)
	a, order, err := FullRecompute(p)
	if err != nil {
		t.Fatalf("full recompute: %v", err)
	}
	b, _, _ := FullRecompute(p)
	// Tamper one character interval in b.
	victim := order[1]
	b[victim].Schedule.Chars[0].End++
	derr := CompareResults(a, b, order)
	if derr == nil || derr.Code != EDiff {
		t.Fatalf("want E_DIFF, got %v", derr)
	}
	if derr.DialogueID != victim || derr.Field != "chars" {
		t.Fatalf("E_DIFF must locate dialogue and field, got %v", derr)
	}
}
