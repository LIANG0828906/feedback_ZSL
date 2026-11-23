package orchestrator

import (
	"fmt"
	"math"
)

// weightScale converts floating rhythm weights to fixed-point integers
// so scheduling uses exact integer sample arithmetic.
const weightScale = 1 << 20

// CharInterval is the presentation interval of one character.
type CharInterval struct {
	Index int   `json:"index"`
	Start int64 `json:"start"`
	End   int64 `json:"end"`
}

// Schedule is the character-level presentation plan of one dialogue.
type Schedule struct {
	DialogueID   string         `json:"dialogue_id"`
	PresentStart int64          `json:"present_start"`
	PresentEnd   int64          `json:"present_end"`
	Chars        []CharInterval `json:"chars"`
}

// presentRange resolves the effective presentation window: the manual
// range when declared, otherwise the full voice segment.
func presentRange(d *Dialogue) (int64, int64) {
	if d.Manual != nil {
		return d.Manual.Start, d.Manual.End
	}
	return d.Segment.StartSample, d.Segment.EndSample
}

// ScheduleDialogue splits the presentation window into contiguous,
// non-overlapping per-character intervals proportional to the rhythm
// weights, using cumulative integer rounding.
func ScheduleDialogue(d *Dialogue) (*Schedule, *Error) {
	if err := ValidateDialogue(d); err != nil {
		return nil, err
	}
	start, end := presentRange(d)
	duration := end - start
	n := len(d.Chars())

	weights := make([]int64, n)
	var total int64
	for i, w := range d.Rhythm {
		if w < 0 || math.IsNaN(w) || math.IsInf(w, 0) {
			e := newError(ERhy, d.ID, "rhythm", "rhythm weight must be a finite non-negative number")
			e.Position = i
			return nil, e
		}
		fi := int64(math.Round(w * weightScale))
		if fi < 0 || total > math.MaxInt64-fi {
			e := newError(ERhy, d.ID, "rhythm", "rhythm weight unit conversion overflow")
			e.Position = i
			return nil, e
		}
		weights[i] = fi
		total += fi
	}
	if total == 0 {
		e := newError(ERhy, d.ID, "rhythm", "rhythm weight sum is zero")
		return nil, e
	}

	s := &Schedule{DialogueID: d.ID, PresentStart: start, PresentEnd: end, Chars: make([]CharInterval, n)}
	var cum int64
	prev := start
	for i := 0; i < n; i++ {
		cum += weights[i]
		if duration != 0 && cum > math.MaxInt64/duration {
			e := newError(ERhy, d.ID, "rhythm", "sample unit conversion overflow")
			e.Position = i
			return nil, e
		}
		cur := start + cum*duration/total
		s.Chars[i] = CharInterval{Index: i, Start: prev, End: cur}
		prev = cur
	}
	return s, nil
}

// CheckContiguous verifies the character intervals tile the whole
// presentation window without gaps or overlaps.
func (s *Schedule) CheckContiguous() *Error {
	if len(s.Chars) == 0 {
		return newError(ERhy, s.DialogueID, "chars", "schedule has no character intervals")
	}
	if s.Chars[0].Start != s.PresentStart {
		return newError(ERhy, s.DialogueID, "chars", "first character does not start at presentation start")
	}
	for i, c := range s.Chars {
		if c.End < c.Start {
			e := newError(ERhy, s.DialogueID, "chars", "character interval is reversed")
			e.Position = i
			return e
		}
		if i > 0 && c.Start != s.Chars[i-1].End {
			e := newError(ERhy, s.DialogueID, "chars",
				fmt.Sprintf("gap or overlap before character %d", i))
			e.Position = i
			return e
		}
	}
	if s.Chars[len(s.Chars)-1].End != s.PresentEnd {
		return newError(ERhy, s.DialogueID, "chars", "last character does not end at presentation end")
	}
	return nil
}
