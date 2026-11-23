package orchestrator

import "testing"

func genProject(t *testing.T) *Project {
	t.Helper()
	p, err := Generate(DefaultSeed, 6)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if verr := ValidateProject(p); verr != nil {
		t.Fatalf("generated project invalid: %v", verr)
	}
	return p
}

func TestValidateMissingReference(t *testing.T) {
	p := genProject(t)
	p.Dialogues[0].Segment.ID = ""
	err := ValidateProject(p)
	if err == nil || err.Code != ERef {
		t.Fatalf("want E_REF, got %v", err)
	}
	if err.DialogueID != p.Dialogues[0].ID {
		t.Fatalf("E_REF must name the dialogue, got %q", err.DialogueID)
	}

	p = genProject(t)
	p.Dialogues[1].Text = ""
	if err := ValidateProject(p); err == nil || err.Code != ERef {
		t.Fatalf("want E_REF for missing text, got %v", err)
	}

	p = genProject(t)
	p.Truth = append(p.Truth, TruthEntry{DialogueID: "dlg-ghost"})
	if err := ValidateProject(p); err == nil || err.Code != ERef || err.DialogueID != "dlg-ghost" {
		t.Fatalf("want E_REF for dangling truth reference, got %v", err)
	}
}

func TestValidateRhythmCountMismatch(t *testing.T) {
	p := genProject(t)
	p.Dialogues[2].Rhythm = p.Dialogues[2].Rhythm[:len(p.Dialogues[2].Rhythm)-1]
	err := ValidateProject(p)
	if err == nil || err.Code != ERhy {
		t.Fatalf("want E_RHY, got %v", err)
	}
	want := int64(len(p.Dialogues[2].Chars()))
	if err.Expected != want || err.Actual != want-1 {
		t.Fatalf("E_RHY must carry expected/actual counts, got %d/%d", err.Expected, err.Actual)
	}
}

func TestValidateIllegalParams(t *testing.T) {
	p := genProject(t)
	p.Policy.CutoffLimitSamples = 0
	err := ValidateProject(p)
	if err == nil || err.Code != EPara || err.Field != "cutoff_limit_samples" {
		t.Fatalf("want E_PARA on cutoff_limit_samples, got %v", err)
	}

	p = genProject(t)
	d := &p.Dialogues[0]
	d.Manual = &SampleRange{Start: d.Segment.StartSample - 10, End: d.Segment.EndSample}
	if err := ValidateProject(p); err == nil || err.Code != EPara || err.Field != "manual" {
		t.Fatalf("want E_PARA on manual range, got %v", err)
	}

	p = genProject(t)
	d = &p.Dialogues[0]
	d.Segment.EndSample = d.Segment.StartSample
	if err := ValidateProject(p); err == nil || err.Code != EPara || err.Field != "segment" {
		t.Fatalf("want E_PARA on segment, got %v", err)
	}
}
