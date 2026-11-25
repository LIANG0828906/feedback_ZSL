package lipsync

import "fmt"

// Timeline is the pure, deterministic full construction of one dialogue.
type Timeline struct {
	Project      DialogueProject
	Timing       Timing
	Schedule     SubtitleSchedule
	Frames       []FrameResult
	Switches     []LipSwitch
	Transitions  []LipTransition
	Dependencies []FrameDependency
}

func BuildTimeline(p DialogueProject) (*Timeline, error) {
	if err := ValidateProject(p); err != nil {
		return nil, err
	}
	timing, err := QuantizeProject(p)
	if err != nil {
		return nil, err
	}
	schedule, err := ScheduleSubtitle(p, timing.FrameCount)
	if err != nil {
		return nil, err
	}
	lipMap, err := MapLips(p, timing, schedule)
	if err != nil {
		return nil, err
	}
	deps, err := BuildFrameDependencies(p, timing, schedule, lipMap.Frames)
	if err != nil {
		return nil, err
	}
	return &Timeline{
		Project:      p,
		Timing:       timing,
		Schedule:     schedule,
		Frames:       lipMap.Frames,
		Switches:     lipMap.Switches,
		Transitions:  lipMap.Transitions,
		Dependencies: deps,
	}, nil
}

func frameAt(timing Timing, frame int) (*QuantizedPhoneme, int, bool) {
	var selected *QuantizedPhoneme
	for i := range timing.Phonemes {
		ph := &timing.Phonemes[i]
		if int64(frame) >= ph.StartFrame && int64(frame) < ph.EndFrame {
			selected = ph
		}
		if ph.StartFrame > int64(frame) && selected == nil {
			break
		}
	}
	if selected == nil {
		return nil, -1, false
	}
	for i := range timing.Phonemes {
		if timing.Phonemes[i].ID == selected.ID && timing.Phonemes[i].SegmentIndex == selected.SegmentIndex {
			return selected, i, true
		}
	}
	return nil, -1, false
}

func consumedPhonemeOffset(timing Timing, frameEnd int) int {
	count := 0
	for _, ph := range timing.Phonemes {
		if ph.EndFrame <= int64(frameEnd) {
			count++
		}
	}
	return count
}

func validateFrameRange(dialogueID string, frame, frameCount int) error {
	if frame < 0 || frame >= frameCount {
		return &DomainError{
			Code:       CodeStage,
			DialogueID: dialogueID,
			Frame:      intPtr(frame),
			Field:      "frame",
			Reason:     "frame outside dialogue",
			Message:    fmt.Sprintf("frame %d is outside 0..%d", frame, frameCount-1),
		}
	}
	return nil
}
