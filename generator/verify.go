package generator

import (
	"fmt"
	"sort"

	"dialogueaudio/engine"
)

// TruthMismatch localizes the first oracle deviation.
type TruthMismatch struct {
	ClipID     string `json:"clip_id"`
	FrameIndex int    `json:"frame_index"`
	Field      string `json:"field"`
	Want       int64  `json:"want"`
	Got        int64  `json:"got"`
}

// TruthComparison is finite and independent of the acceptance tolerance.
type TruthComparison struct {
	Pass            bool           `json:"pass"`
	ToleranceQ16    int64          `json:"tolerance_q16"`
	MaxDeviationQ16 int64          `json:"max_deviation_q16"`
	ComparedFrames  int            `json:"compared_frames"`
	FirstOutside    *TruthMismatch `json:"first_outside,omitempty"`
}

// CompareEnvelopes compares measured integer envelopes with generator truth.
// The observed MaxDeviationQ16 is computed before applying the tolerance, so
// tightening tolerance cannot change or increase the deviation value.
func CompareEnvelopes(truth TruthBundle, envelopes map[string]engine.ClipEnvelope, toleranceQ16 int64) TruthComparison {
	comp := TruthComparison{Pass: true, ToleranceQ16: toleranceQ16}
	truthClips := append([]TruthClip(nil), truth.Clips...)
	sort.Slice(truthClips, func(i, j int) bool { return truthClips[i].ClipID < truthClips[j].ClipID })
	for _, truthClip := range truthClips {
		env, ok := envelopes[truthClip.ClipID]
		if !ok {
			if comp.FirstOutside == nil {
				comp.FirstOutside = &TruthMismatch{
					ClipID: truthClip.ClipID, FrameIndex: -1, Field: "clip_missing",
				}
			}
			comp.Pass = false
			continue
		}
		if len(env.Frames) != len(truthClip.Frames) {
			if comp.FirstOutside == nil {
				comp.FirstOutside = &TruthMismatch{
					ClipID: truthClip.ClipID, FrameIndex: -1, Field: "frame_count",
					Want: int64(len(truthClip.Frames)), Got: int64(len(env.Frames)),
				}
			}
			comp.Pass = false
			continue
		}
		for i, frame := range truthClip.Frames {
			got := env.Frames[i]
			comp.ComparedFrames++
			dev := engine.AbsInt64(got.EnvelopeQ16 - frame.EnvelopeQ16)
			if dev > comp.MaxDeviationQ16 {
				comp.MaxDeviationQ16 = dev
			}
			if dev > toleranceQ16 {
				comp.Pass = false
				if comp.FirstOutside == nil {
					comp.FirstOutside = &TruthMismatch{
						ClipID: truthClip.ClipID, FrameIndex: frame.FrameIndex,
						Field: "envelope_q16", Want: frame.EnvelopeQ16, Got: got.EnvelopeQ16,
					}
				}
			}
			if got.WeightedEnergy != frame.WeightedEnergy {
				comp.Pass = false
				if comp.FirstOutside == nil {
					comp.FirstOutside = &TruthMismatch{
						ClipID: truthClip.ClipID, FrameIndex: frame.FrameIndex,
						Field: "weighted_energy", Want: frame.WeightedEnergy, Got: got.WeightedEnergy,
					}
				}
			}
		}
	}
	return comp
}

// ValidateTruth checks structural invariants.
func ValidateTruth(truth TruthBundle) error {
	if truth.Seed < 0 || truth.SampleRate <= 0 || truth.FrameLenSamples <= 0 || truth.HopSamples <= 0 {
		return fmt.Errorf("invalid truth header")
	}
	if len(truth.Clips) == 0 {
		return fmt.Errorf("truth has no clips")
	}
	for _, clip := range truth.Clips {
		if len(clip.Frames) == 0 {
			return fmt.Errorf("truth clip %s has no frames", clip.ClipID)
		}
	}
	return nil
}
