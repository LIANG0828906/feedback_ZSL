package orchestrator

import "fmt"

// InterruptResult describes cutoff and resume data after a player
// interruption at sample point T.
type InterruptResult struct {
	InterruptAt int64 `json:"interrupt_at"`
	CutoffEnd   int64 `json:"cutoff_end"`
	ResumeIndex int   `json:"resume_index"`
	ResumeStart int64 `json:"resume_start"`
	Progress    int   `json:"progress"` // characters fully presented before T
}

func isSentenceEnd(r rune) bool {
	switch r {
	case '。', '！', '？', '.', '!', '?':
		return true
	}
	return false
}

// ComputeInterrupt calculates the tail-sentence cutoff point and the
// resume position for an interruption at sample t. The tail (the
// remainder of the sentence containing the resume character) must
// finish within limit samples after t, otherwise E_CUT is returned
// with the required duration and the allowed limit.
func ComputeInterrupt(d *Dialogue, s *Schedule, t int64, limit int64) (*InterruptResult, *Error) {
	if t < s.PresentStart || t >= s.PresentEnd {
		return nil, newError(EPara, d.ID, "interrupt_at",
			"interrupt point must lie within the presentation window")
	}
	if limit <= 0 {
		e := newError(EPara, d.ID, "cutoff_limit_samples", "cutoff limit must be positive")
		e.Actual = limit
		return nil, e
	}

	progress := -1
	for i, c := range s.Chars {
		if c.End > t {
			progress = i
			break
		}
	}
	if progress < 0 {
		progress = len(s.Chars)
	}

	runes := d.Chars()
	tailEnd := len(runes) - 1
	for i := progress; i < len(runes); i++ {
		if isSentenceEnd(runes[i]) {
			tailEnd = i
			break
		}
	}

	required := s.Chars[tailEnd].End - t
	if required > limit {
		e := newError(ECut, d.ID, "cutoff_limit_samples",
			fmt.Sprintf("tail sentence needs %d samples to cut off, limit is %d", required, limit))
		e.Expected = required
		e.Actual = limit
		return nil, e
	}

	return &InterruptResult{
		InterruptAt: t,
		CutoffEnd:   t + required,
		ResumeIndex: tailEnd + 1,
		ResumeStart: t + required,
		Progress:    progress,
	}, nil
}

// ResumeView returns the remaining character intervals for a resumed
// presentation. No character before ResumeIndex is advanced again and
// the first resumed character starts exactly at ResumeStart, so the
// total progress matches the uninterrupted schedule.
func ResumeView(s *Schedule, ir *InterruptResult) ([]CharInterval, *Error) {
	if ir.ResumeIndex < 0 || ir.ResumeIndex > len(s.Chars) {
		return nil, newError(EQueue, s.DialogueID, "resume_index",
			"resume position is unreachable")
	}
	if ir.ResumeIndex == len(s.Chars) {
		return []CharInterval{}, nil
	}
	if s.Chars[ir.ResumeIndex].Start != ir.ResumeStart {
		return nil, newError(EQueue, s.DialogueID, "resume_start",
			"resume start does not align with the saved progress")
	}
	out := make([]CharInterval, len(s.Chars)-ir.ResumeIndex)
	copy(out, s.Chars[ir.ResumeIndex:])
	return out, nil
}
