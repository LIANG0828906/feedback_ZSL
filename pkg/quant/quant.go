package quant

import (
	"fmt"
	"math"
	"sort"

	"lipsync.local/prototype/pkg/domain"
)

// SampleToFrame converts one sample boundary with checked integer arithmetic.
// It returns the quantized frame and a truncation record. No floating point is
// used anywhere in the conversion.
func SampleToFrame(dialogueID, phonemeID, boundary string, sample int64, sampleRate, frameRate int, direction string) (int, domain.Truncation, error) {
	if sample < 0 || sampleRate <= 0 || frameRate <= 0 {
		return 0, domain.Truncation{}, &domain.Error{
			Code: domain.ErrPara, DialogueID: dialogueID, PhonemeID: phonemeID,
			Field: "sample_to_frame", Reason: "arguments must be non-negative/positive",
		}
	}
	if !domain.QuantizeAllowed(direction) {
		return 0, domain.Truncation{}, &domain.Error{
			Code: domain.ErrSchema, DialogueID: dialogueID, PhonemeID: phonemeID,
			Field: "quantize", Reason: "unsupported quantize direction",
		}
	}
	numerator, ok := checkedMul(sample, int64(frameRate))
	if !ok {
		return 0, domain.Truncation{}, &domain.Error{
			Code: domain.ErrOverflow, DialogueID: dialogueID, PhonemeID: phonemeID,
			Field: "sample*frame_rate", OperandA: sample, OperandB: int64(frameRate),
			Reason: "sample to frame multiplication overflow",
		}
	}
	denominator := int64(sampleRate)
	floor := numerator / denominator
	remainder := numerator % denominator
	frame := floor
	adjusted := false
	switch direction {
	case domain.QuantizeCeil:
		if remainder != 0 {
			frame++
			adjusted = true
		}
	case domain.QuantizeNearest:
		half := denominator / 2
		if remainder >= half && remainder != 0 {
			frame++
			adjusted = true
		}
	}
	if frame > math.MaxInt {
		return 0, domain.Truncation{}, &domain.Error{
			Code: domain.ErrOverflow, DialogueID: dialogueID, PhonemeID: phonemeID,
			Field: "frame", OperandA: frame, Reason: "frame exceeds int range",
		}
	}
	tr := domain.Truncation{
		DialogueID: dialogueID, PhonemeID: phonemeID, Boundary: boundary,
		Sample: sample, Numerator: numerator, Denominator: denominator,
		FloorFrame: int(floor), Frame: int(frame), Remainder: remainder,
		Direction: direction, Adjusted: adjusted,
	}
	return int(frame), tr, nil
}

// QuantizeDialogue quantizes all boundaries and converts duration weights to
// basis points. It normalizes input order by sample position, making the result
// independent of map/goroutine completion order.
func QuantizeDialogue(d domain.Dialogue, frameRate int, direction string) (domain.QuantizeResult, error) {
	if d.PCM.SampleRate <= 0 {
		return domain.QuantizeResult{}, &domain.Error{
			Code: domain.ErrDecode, DialogueID: d.ID, Field: "pcm.sample_rate", Reason: "invalid sample rate",
		}
	}
	if _, ok := domain.SupportedFrameRates[frameRate]; !ok {
		return domain.QuantizeResult{}, &domain.Error{
			Code: domain.ErrRate, DialogueID: d.ID, Field: "frame_rate", Reason: "unsupported frame rate",
		}
	}
	phonemes := append([]domain.Phoneme(nil), d.Phonemes...)
	sort.SliceStable(phonemes, func(i, j int) bool {
		if phonemes[i].StartSample == phonemes[j].StartSample {
			return phonemes[i].ID < phonemes[j].ID
		}
		return phonemes[i].StartSample < phonemes[j].StartSample
	})
	out := make([]domain.QuantizedPhoneme, 0, len(phonemes))
	truncations := make([]domain.Truncation, 0, len(phonemes)*2)
	for i, ph := range phonemes {
		start, trStart, err := SampleToFrame(d.ID, ph.ID, "start", ph.StartSample, d.PCM.SampleRate, frameRate, direction)
		if err != nil {
			return domain.QuantizeResult{}, err
		}
		end, trEnd, err := SampleToFrame(d.ID, ph.ID, "end", ph.EndSample, d.PCM.SampleRate, frameRate, direction)
		if err != nil {
			return domain.QuantizeResult{}, err
		}
		if end <= start {
			return domain.QuantizeResult{}, &domain.Error{
				Code: domain.ErrPara, DialogueID: d.ID, PhonemeID: ph.ID,
				Field: "quantized_interval", Reason: "phoneme collapses to zero frames",
			}
		}
		weightBasis, err := PermilleToBasisPoints(ph.WeightPermille, d.ID, ph.ID)
		if err != nil {
			return domain.QuantizeResult{}, err
		}
		out = append(out, domain.QuantizedPhoneme{
			DialogueID: d.ID, PhonemeID: ph.ID, Index: i,
			StartSample: ph.StartSample, EndSample: ph.EndSample,
			StartFrame: start, EndFrame: end,
			Category: ph.Category, WeightPermille: ph.WeightPermille,
			WeightBasis: weightBasis, AffinityToNext: ph.AffinityToNext,
		})
		truncations = append(truncations, trStart, trEnd)
	}
	if len(out) > 1 {
		for i := 1; i < len(out); i++ {
			if out[i].StartFrame < out[i-1].EndFrame {
				return domain.QuantizeResult{}, &domain.Error{
					Code: domain.ErrOrder, DialogueID: d.ID,
					PhonemeA: out[i-1].PhonemeID, PhonemeB: out[i].PhonemeID,
					Field: "frame_interval", Reason: "quantized phoneme frames overlap",
				}
			}
		}
	}
	return domain.QuantizeResult{
		FrameRate: frameRate, Direction: direction,
		Phonemes: out, Truncations: truncations,
	}, nil
}

// PermilleToBasisPoints converts a per-mille weight to basis points (0.01%).
func PermilleToBasisPoints(permille int64, dialogueID, phonemeID string) (int64, error) {
	if permille < 0 {
		return 0, &domain.Error{
			Code: domain.ErrPara, DialogueID: dialogueID, PhonemeID: phonemeID,
			Field: "weight_permille", Reason: "weight must not be negative",
		}
	}
	value, ok := checkedMul(permille, 10)
	if !ok {
		return 0, &domain.Error{
			Code: domain.ErrOverflow, DialogueID: dialogueID, PhonemeID: phonemeID,
			Field: "weight_permille*basis_factor", OperandA: permille, OperandB: 10,
			Reason: "weight conversion overflow",
		}
	}
	return value, nil
}

// BasisPointsToPermille converts basis points back to per-mille with deterministic
// floor division.
func BasisPointsToPermille(basisPoints int64, dialogueID, phonemeID string) (int64, error) {
	if basisPoints < 0 {
		return 0, &domain.Error{
			Code: domain.ErrPara, DialogueID: dialogueID, PhonemeID: phonemeID,
			Field: "weight_basis_points", Reason: "weight must not be negative",
		}
	}
	return basisPoints / 10, nil
}

func checkedMul(a, b int64) (int64, bool) {
	if a == 0 || b == 0 {
		return 0, true
	}
	if a < 0 || b < 0 {
		return 0, false
	}
	if a > math.MaxInt64/b {
		return 0, false
	}
	return a * b, true
}

func CheckedAdd(a, b int64) (int64, bool) {
	if b > 0 && a > math.MaxInt64-b {
		return 0, false
	}
	if b < 0 && a < math.MinInt64-b {
		return 0, false
	}
	return a + b, true
}

func FormatDurationBasis(basis int64) string {
	return fmt.Sprintf("%d.%02d permille", basis/10, basis%10)
}
