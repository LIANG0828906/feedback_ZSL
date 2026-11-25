package lipsync

import "fmt"

func QuantizeSampleToFrame(sample, sampleRate, fps int, direction QuantizeDirection) (frame int, numerator, denominator, remainder int64) {
	if sampleRate <= 0 || fps <= 0 {
		return 0, 0, 1, 0
	}
	den := int64(sampleRate)
	num := int64(sample) * int64(fps)
	base := num / den
	rem := num % den
	frame = int(base)
	switch direction {
	case QuantizeUp:
		if rem != 0 {
			frame++
		}
	case QuantizeNearest:
		if rem*2 >= den {
			frame++
		}
	case QuantizeDown:
		fallthrough
	default:
	}
	return frame, num, den, rem
}

func PermilleToPermyriad(value int) int {
	return value * 10
}

func PermyriadToPermille(value int) (truncated int, remainder int) {
	return value / 10, value % 10
}

func NormalizeWeight(weight int, unit WeightUnit) (permyriad, permille, remainder int, err error) {
	switch unit {
	case WeightPermille:
		if weight < 0 || weight > 1000 {
			return 0, 0, 0, fmt.Errorf("permille weight %d outside [0,1000]", weight)
		}
		return PermilleToPermyriad(weight), weight, 0, nil
	case WeightPermyriad:
		if weight < 0 || weight > 10000 {
			return 0, 0, 0, fmt.Errorf("permyriad weight %d outside [0,10000]", weight)
		}
		truncated, rem := PermyriadToPermille(weight)
		return weight, truncated, rem, nil
	default:
		return 0, 0, 0, fmt.Errorf("unknown weight unit %q", unit)
	}
}

func QuantizeDialogue(d *Dialogue, recipe Recipe) ([]QuantizedPhoneme, []Truncation, error) {
	if d == nil {
		return nil, nil, SchemaError("dialogue", "dialogue is nil")
	}
	out := make([]QuantizedPhoneme, 0, len(d.Phonemes))
	truncations := make([]Truncation, 0, len(d.Phonemes)*2)
	for _, ph := range d.Phonemes {
		startFrame, startNum, den, startRem := QuantizeSampleToFrame(
			ph.StartSample, d.PCM.SampleRate, recipe.FPS, recipe.Quantization,
		)
		endFrame, endNum, _, endRem := QuantizeSampleToFrame(
			ph.EndSample, d.PCM.SampleRate, recipe.FPS, recipe.Quantization,
		)
		weight10k, weight1k, weightRem, err := NormalizeWeight(ph.Weight, ph.WeightUnit)
		if err != nil {
			return nil, nil, DialogueError(ECSchema, d,
				fmt.Sprintf("phonemes[%d].weight", ph.Index), "%v", err)
		}
		qp := QuantizedPhoneme{
			Phoneme:         ph,
			StartFrame:      startFrame,
			EndFrame:        endFrame,
			VisibleFrames:   endFrame - startFrame,
			WeightPermyriad: weight10k,
			WeightPermille:  weight1k,
			WeightRemainder: weightRem,
		}
		out = append(out, qp)
		truncations = append(truncations,
			Truncation{
				DialogueID: d.ID, ShardIndex: d.ShardIndex, PhonemeID: ph.ID,
				Boundary: "startSample", Sample: ph.StartSample, Direction: recipe.Quantization,
				ScaledNumerator: startNum, Denominator: den, Remainder: startRem, Frame: startFrame,
			},
			Truncation{
				DialogueID: d.ID, ShardIndex: d.ShardIndex, PhonemeID: ph.ID,
				Boundary: "endSample", Sample: ph.EndSample, Direction: recipe.Quantization,
				ScaledNumerator: endNum, Denominator: den, Remainder: endRem, Frame: endFrame,
			},
		)
	}
	return out, truncations, nil
}
