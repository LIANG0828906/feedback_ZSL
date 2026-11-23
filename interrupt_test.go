package orchestrator

import "testing"

func TestInterruptCutoffAndResume(t *testing.T) {
	p := genProject(t)
	for i := range p.Dialogues {
		d := &p.Dialogues[i]
		if d.InterruptAt == nil {
			continue
		}
		s, err := ScheduleDialogue(d)
		if err != nil {
			t.Fatalf("schedule %s: %v", d.ID, err)
		}
		ir, ierr := ComputeInterrupt(d, s, *d.InterruptAt, p.Policy.CutoffLimitSamples)
		if ierr != nil {
			t.Fatalf("interrupt %s: %v", d.ID, ierr)
		}
		// Cutoff finishes within the declared limit.
		if ir.CutoffEnd-ir.InterruptAt > p.Policy.CutoffLimitSamples {
			t.Fatalf("%s cutoff tail exceeds limit", d.ID)
		}
		truth := p.TruthByID(d.ID)
		if abs64(ir.CutoffEnd-truth.CutoffEnd) > p.ToleranceSamples {
			t.Fatalf("%s cutoff %d deviates from truth %d", d.ID, ir.CutoffEnd, truth.CutoffEnd)
		}
		if ir.ResumeIndex != truth.ResumeIndex {
			t.Fatalf("%s resume index %d, truth %d", d.ID, ir.ResumeIndex, truth.ResumeIndex)
		}
		// Resume never re-advances earlier characters and the total
		// progress matches the uninterrupted schedule.
		rest, rerr := ResumeView(s, ir)
		if rerr != nil {
			t.Fatalf("resume %s: %v", d.ID, rerr)
		}
		for _, c := range rest {
			if c.Index < ir.ResumeIndex {
				t.Fatalf("%s resume re-advances character %d", d.ID, c.Index)
			}
		}
		if len(rest) > 0 && rest[len(rest)-1].End != s.PresentEnd {
			t.Fatalf("%s resumed end %d != uninterrupted end %d",
				d.ID, rest[len(rest)-1].End, s.PresentEnd)
		}
		if len(rest) > 0 && rest[0].Start != ir.ResumeStart {
			t.Fatalf("%s resume start misaligned", d.ID)
		}
	}
}

func TestInterruptCutoffDeadlineFails(t *testing.T) {
	p := genProject(t)
	var d *Dialogue
	for i := range p.Dialogues {
		if p.Dialogues[i].InterruptAt != nil {
			d = &p.Dialogues[i]
			break
		}
	}
	if d == nil {
		t.Fatal("no scripted interruption in sample")
	}
	s, err := ScheduleDialogue(d)
	if err != nil {
		t.Fatalf("schedule: %v", err)
	}
	_, ierr := ComputeInterrupt(d, s, *d.InterruptAt, 1)
	if ierr == nil || ierr.Code != ECut {
		t.Fatalf("want E_CUT, got %v", ierr)
	}
	if ierr.DialogueID != d.ID {
		t.Fatalf("E_CUT must name the dialogue, got %v", ierr)
	}
	if ierr.Expected <= ierr.Actual {
		t.Fatalf("E_CUT must carry required %d > allowed %d", ierr.Expected, ierr.Actual)
	}
}

func TestInterruptOutOfWindow(t *testing.T) {
	p := genProject(t)
	d := &p.Dialogues[0]
	s, err := ScheduleDialogue(d)
	if err != nil {
		t.Fatalf("schedule: %v", err)
	}
	if _, ierr := ComputeInterrupt(d, s, s.PresentEnd+1, p.Policy.CutoffLimitSamples); ierr == nil || ierr.Code != EPara {
		t.Fatalf("want E_PARA for out-of-window interrupt, got %v", ierr)
	}
}
