package orchestrator

import (
	"math"
	"testing"
)

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}

func TestScheduleMatchesTruth(t *testing.T) {
	p := genProject(t)
	for i := range p.Dialogues {
		d := &p.Dialogues[i]
		s, err := ScheduleDialogue(d)
		if err != nil {
			t.Fatalf("schedule %s: %v", d.ID, err)
		}
		truth := p.TruthByID(d.ID)
		if truth == nil {
			t.Fatalf("missing truth for %s", d.ID)
		}
		if abs64(s.PresentStart-truth.PresentStart) > p.ToleranceSamples {
			t.Fatalf("%s present start %d deviates from truth %d beyond tolerance",
				d.ID, s.PresentStart, truth.PresentStart)
		}
		if abs64(s.PresentEnd-truth.PresentEnd) > p.ToleranceSamples {
			t.Fatalf("%s present end %d deviates from truth %d beyond tolerance",
				d.ID, s.PresentEnd, truth.PresentEnd)
		}
		if cerr := s.CheckContiguous(); cerr != nil {
			t.Fatalf("%s intervals not contiguous: %v", d.ID, cerr)
		}
	}
}

func TestScheduleZeroSumWeights(t *testing.T) {
	p := genProject(t)
	d := &p.Dialogues[0]
	for i := range d.Rhythm {
		d.Rhythm[i] = 0
	}
	_, err := ScheduleDialogue(d)
	if err == nil || err.Code != ERhy || err.DialogueID != d.ID {
		t.Fatalf("want E_RHY for zero-sum weights, got %v", err)
	}
}

func TestScheduleOverflow(t *testing.T) {
	p := genProject(t)
	d := &p.Dialogues[0]
	d.Rhythm[3] = math.MaxFloat64
	_, err := ScheduleDialogue(d)
	if err == nil || err.Code != ERhy {
		t.Fatalf("want E_RHY for overflow, got %v", err)
	}
	if err.DialogueID != d.ID || err.Position != 3 {
		t.Fatalf("E_RHY must name dialogue and overflow position, got %v", err)
	}
}

func TestScheduleManualRange(t *testing.T) {
	p := genProject(t)
	for i := range p.Dialogues {
		d := &p.Dialogues[i]
		if d.Manual == nil {
			continue
		}
		s, err := ScheduleDialogue(d)
		if err != nil {
			t.Fatalf("schedule %s: %v", d.ID, err)
		}
		if s.PresentStart != d.Manual.Start || s.PresentEnd != d.Manual.End {
			t.Fatalf("%s manual range not honored: %d-%d", d.ID, s.PresentStart, s.PresentEnd)
		}
	}
}
