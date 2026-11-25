package lipsync

import "fmt"

type LipMap struct {
	Frames      []FrameResult
	Switches    []LipSwitch
	Transitions []LipTransition
}

// MapLips maps quantized phoneme spans to base visemes and then applies the
// transition constraint. A family crossing consumes one frame inside the new
// phoneme and is recorded as an inserted transition frame. The subtitle
// schedule is intentionally not an input here, so character/line changes can
// never reset mouth continuity.
func MapLips(p DialogueProject, timing Timing, schedule SubtitleSchedule) (LipMap, error) {
	if err := ValidateProject(p); err != nil {
		return LipMap{}, err
	}
	if len(schedule.Frames) != timing.FrameCount {
		return LipMap{}, &DomainError{
			Code:       CodeLip,
			DialogueID: p.DialogueID,
			Frame:      intPtr(0),
			Field:      "frame",
			Reason:     "schedule/lip frame count mismatch",
			Message:    "cannot map lips for a mismatched frame count",
		}
	}
	frames := make([]FrameResult, timing.FrameCount)
	for f := range frames {
		frames[f] = FrameResult{
			Frame:     f,
			CharStart: schedule.Frames[f].CharStart,
			CharEnd:   schedule.Frames[f].CharEnd,
			BaseLip:   LipSilent,
			Lip:       LipSilent,
		}
	}
	for _, ph := range timing.Phonemes {
		if ph.StartFrame < 0 || ph.EndFrame > int64(timing.FrameCount) {
			return LipMap{}, &DomainError{
				Code:       CodeLip,
				DialogueID: p.DialogueID,
				Frame:      intPtr(int(ph.StartFrame)),
				Field:      "phoneme.interval",
				Reason:     "quantized phoneme outside frame range",
				Message:    "quantized phoneme interval is outside the dialogue",
			}
		}
		segmentID := p.Segments[ph.SegmentIndex].ID
		for f := ph.StartFrame; f < ph.EndFrame; f++ {
			frames[f].BaseLip = ph.Lip
			frames[f].Lip = ph.Lip
			frames[f].PhonemeID = ph.ID
			frames[f].SegmentID = segmentID
		}
	}
	switches := make([]LipSwitch, 0)
	transitions := make([]LipTransition, 0)
	for f := 1; f < len(frames); f++ {
		previous := frames[f-1].BaseLip
		current := frames[f].BaseLip
		if previous == current {
			continue
		}
		switches = append(switches, LipSwitch{Frame: f, From: previous, To: current})
		if !previous.IsValid() || !current.IsValid() {
			return LipMap{}, &DomainError{
				Code:       CodeLip,
				DialogueID: p.DialogueID,
				Frame:      intPtr(f),
				Left:       string(previous),
				Right:      string(current),
				Reason:     "unknown lip category",
				Message:    "unknown lip category during transition",
			}
		}
		leftFamily := lipFamily(previous)
		rightFamily := lipFamily(current)
		distance := leftFamily - rightFamily
		if distance < 0 {
			distance = -distance
		}
		if leftFamily == rightFamily {
			continue
		}
		if p.LipTransitionTolerance < 1 {
			return LipMap{}, &DomainError{
				Code:       CodeLip,
				DialogueID: p.DialogueID,
				Field:      "lip_transition_tolerance",
				Frame:      intPtr(f),
				Left:       string(previous),
				Right:      string(current),
				Reason:     "discontinuous family jump exceeds tolerance",
				Message:    fmt.Sprintf("transition %s -> %s at frame %d exceeds tolerance", previous, current, f),
			}
		}
		frames[f].Lip = LipClosed
		frames[f].Transition = true
		transitions = append(transitions, LipTransition{
			Frame:          f,
			From:           previous,
			To:             current,
			Inserted:       []LipCategory{LipClosed},
			InsertedFrames: []int{f},
			Distance:       distance + 1,
		})
	}
	return LipMap{Frames: frames, Switches: switches, Transitions: transitions}, nil
}
