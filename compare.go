package lipsync

import "fmt"

type Mismatch struct {
	DialogueID string `json:"dialogue_id"`
	Frame      int    `json:"frame"`
	Field      string `json:"field"`
	Left       string `json:"left"`
	Right      string `json:"right"`
}

func (m *Mismatch) Error() string {
	if m == nil {
		return ""
	}
	return fmt.Sprintf("first mismatch dialogue=%s frame=%d field=%s left=%s right=%s", m.DialogueID, m.Frame, m.Field, m.Left, m.Right)
}

type Comparison struct {
	Name       string    `json:"name"`
	DialogueID string    `json:"dialogue_id"`
	Consistent bool      `json:"consistent"`
	Mismatch   *Mismatch `json:"mismatch,omitempty"`
}

func CompareFrames(dialogueID, name string, left, right []FrameResult) Comparison {
	comparison := Comparison{Name: name, DialogueID: dialogueID, Consistent: true}
	limit := len(left)
	if len(right) > limit {
		limit = len(right)
	}
	for f := 0; f < limit; f++ {
		switch {
		case f >= len(left):
			comparison.Consistent = false
			comparison.Mismatch = &Mismatch{DialogueID: dialogueID, Frame: f, Field: "frame", Left: "missing", Right: "present"}
			return comparison
		case f >= len(right):
			comparison.Consistent = false
			comparison.Mismatch = &Mismatch{DialogueID: dialogueID, Frame: f, Field: "frame", Left: "present", Right: "missing"}
			return comparison
		}
		l, r := left[f], right[f]
		fields := []struct {
			name string
			l    any
			r    any
		}{
			{"frame", l.Frame, r.Frame},
			{"char_start", l.CharStart, r.CharStart},
			{"char_end", l.CharEnd, r.CharEnd},
			{"base_lip", l.BaseLip, r.BaseLip},
			{"lip", l.Lip, r.Lip},
			{"transition", l.Transition, r.Transition},
			{"phoneme_id", l.PhonemeID, r.PhonemeID},
			{"segment_id", l.SegmentID, r.SegmentID},
		}
		for _, field := range fields {
			if field.l != field.r {
				comparison.Consistent = false
				comparison.Mismatch = &Mismatch{
					DialogueID: dialogueID,
					Frame:      f,
					Field:      field.name,
					Left:       fmt.Sprint(field.l),
					Right:      fmt.Sprint(field.r),
				}
				return comparison
			}
		}
	}
	return comparison
}

func CompareTransitions(dialogueID, name string, left, right []LipTransition) Comparison {
	comparison := Comparison{Name: name, DialogueID: dialogueID, Consistent: true}
	limit := len(left)
	if len(right) > limit {
		limit = len(right)
	}
	for i := 0; i < limit; i++ {
		if i >= len(left) || i >= len(right) {
			comparison.Consistent = false
			comparison.Mismatch = &Mismatch{DialogueID: dialogueID, Frame: -1, Field: "transitions", Left: fmt.Sprint(len(left)), Right: fmt.Sprint(len(right))}
			return comparison
		}
		l, r := left[i], right[i]
		fields := []struct {
			name string
			l    any
			r    any
		}{
			{"transition.frame", l.Frame, r.Frame},
			{"transition.from", l.From, r.From},
			{"transition.to", l.To, r.To},
			{"transition.inserted", fmt.Sprint(l.Inserted), fmt.Sprint(r.Inserted)},
			{"transition.inserted_frames", fmt.Sprint(l.InsertedFrames), fmt.Sprint(r.InsertedFrames)},
			{"transition.distance", l.Distance, r.Distance},
		}
		for _, field := range fields {
			if field.l != field.r {
				comparison.Consistent = false
				comparison.Mismatch = &Mismatch{
					DialogueID: dialogueID,
					Frame:      l.Frame,
					Field:      field.name,
					Left:       fmt.Sprint(field.l),
					Right:      fmt.Sprint(field.r),
				}
				return comparison
			}
		}
	}
	return comparison
}

func CompareTruth(dialogueID string, runtime []FrameResult, truth Truth, transitions []LipTransition) []Comparison {
	comparisons := []Comparison{CompareFrames(dialogueID, "runtime_vs_truth", runtime, truth.Frames)}
	comparisons = append(comparisons, CompareTransitions(dialogueID, "runtime_transitions_vs_truth", transitions, truth.Transitions))
	return comparisons
}
