package lipsync

import "fmt"

// ConstructTruth is the generator's analytic truth path. It does not consult
// the engine, state machine, cache, or SQLite.
func ConstructTruth(p DialogueProject) (Truth, error) {
	timeline, err := BuildTimeline(p)
	if err != nil {
		return Truth{}, err
	}
	contentHash, err := ContentHash(p)
	if err != nil {
		return Truth{}, fmt.Errorf("hash truth: %w", err)
	}
	frames := make([]FrameTruth, len(timeline.Frames))
	for i, frame := range timeline.Frames {
		frames[i] = frameTruth(frame)
	}
	return Truth{
		DialogueID:          p.DialogueID,
		FPS:                 p.FPS,
		FrameCount:          timeline.Timing.FrameCount,
		Frames:              makeFrameResults(frames),
		LipSwitches:         timeline.Switches,
		Transitions:         timeline.Transitions,
		ContentHash:         contentHash,
		QuantizationVersion: p.Quantization.Version,
		AlgorithmVersion:    p.AlgorithmVersion,
	}, nil
}

// makeFrameResults converts public truth fields back into the common frame
// shape while keeping the constructor independent of runtime bookkeeping.
func makeFrameResults(in []FrameTruth) []FrameResult {
	out := make([]FrameResult, len(in))
	for i, f := range in {
		out[i] = FrameResult{
			Frame:      f.Frame,
			CharStart:  f.CharStart,
			CharEnd:    f.CharEnd,
			BaseLip:    f.BaseLip,
			Lip:        f.Lip,
			Transition: f.Transition,
			PhonemeID:  f.PhonemeID,
			SegmentID:  f.SegmentID,
		}
	}
	return out
}
