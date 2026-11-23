package orchestrator

import "fmt"

// ValidateProject runs structural validation over the whole project
// and returns the first violation found.
func ValidateProject(p *Project) *Error {
	if p.ID == "" {
		return newError(ERef, "", "id", "project id missing")
	}
	if p.SampleRate <= 0 {
		return newError(EPara, "", "sample_rate", "sample rate must be positive")
	}
	if p.ToleranceSamples < 0 {
		return newError(EPara, "", "tolerance_samples", "tolerance must be non-negative")
	}
	seen := map[string]bool{}
	for i := range p.Dialogues {
		d := &p.Dialogues[i]
		if seen[d.ID] {
			return newError(ERef, d.ID, "id", "duplicate dialogue id")
		}
		seen[d.ID] = true
		if err := ValidateDialogue(d); err != nil {
			return err
		}
	}
	if err := ValidatePolicy(p); err != nil {
		return err
	}
	for _, t := range p.Truth {
		if p.DialogueByID(t.DialogueID) == nil {
			return newError(ERef, t.DialogueID, "truth", "truth entry references unknown dialogue")
		}
	}
	return nil
}

// ValidateDialogue checks one dialogue's references, rhythm weights
// and optional manual range.
func ValidateDialogue(d *Dialogue) *Error {
	if d.ID == "" {
		return newError(ERef, "", "id", "dialogue id missing")
	}
	if d.Text == "" {
		return newError(ERef, d.ID, "text", "dialogue text missing")
	}
	if d.Segment.ID == "" {
		return newError(ERef, d.ID, "segment.id", "voice segment reference missing")
	}
	if d.Segment.EndSample <= d.Segment.StartSample {
		return newError(EPara, d.ID, "segment", "segment end must be after start")
	}
	nChars := len(d.Chars())
	if len(d.Rhythm) != nChars {
		e := newError(ERhy, d.ID, "rhythm",
			fmt.Sprintf("rhythm weight count mismatch: want %d got %d", nChars, len(d.Rhythm)))
		e.Expected = int64(nChars)
		e.Actual = int64(len(d.Rhythm))
		return e
	}
	if d.Manual != nil {
		m := d.Manual
		if m.End <= m.Start {
			return newError(EPara, d.ID, "manual", "manual range end must be after start")
		}
		if m.Start < d.Segment.StartSample || m.End > d.Segment.EndSample {
			return newError(EPara, d.ID, "manual",
				"manual range must lie within the voice segment time range")
		}
	}
	if d.InterruptAt != nil {
		t := *d.InterruptAt
		if t < d.Segment.StartSample || t >= d.Segment.EndSample {
			return newError(EPara, d.ID, "interrupt_at",
				"interrupt point must lie within the voice segment")
		}
	}
	return nil
}

// ValidatePolicy checks playback / interruption strategy values.
func ValidatePolicy(p *Project) *Error {
	if p.Policy.CutoffLimitSamples <= 0 {
		e := newError(EPara, "", "cutoff_limit_samples", "cutoff limit must be positive")
		e.Actual = p.Policy.CutoffLimitSamples
		return e
	}
	return nil
}
