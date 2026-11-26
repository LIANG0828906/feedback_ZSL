package downsample

import (
	"fmt"
	"math"
	"math/bits"
)

// WeightConversionReport records every fixed-point conversion boundary.
type WeightConversionReport struct {
	MetricID              string  `json:"metric_id,omitempty"`
	PointSeq              int64   `json:"point_seq,omitempty"`
	InputRaw              int64   `json:"input_raw"`
	SourceScale           int64   `json:"source_scale"`
	TargetScale           int64   `json:"target_scale"`
	SourceBits            int     `json:"source_bits"`
	TargetBits            int     `json:"target_bits"`
	IntermediateNumerator string  `json:"intermediate_numerator"`
	OutputRaw             int64   `json:"output_raw"`
	ExactTarget           float64 `json:"exact_target"`
	Delta                 float64 `json:"delta"`
	Tolerance             float64 `json:"tolerance"`
	WithinTolerance       bool    `json:"within_tolerance"`
	SourceOverflow        bool    `json:"source_overflow"`
	ArithmeticOverflow    bool    `json:"arithmetic_overflow"`
	TargetOverflow        bool    `json:"target_overflow"`
	Saturated             bool    `json:"saturated"`
	Code                  string  `json:"code"`
}

func maxSigned(bits int) int64 {
	if bits >= 64 {
		return math.MaxInt64
	}
	if bits <= 1 {
		return 0
	}
	return int64(1)<<(bits-1) - 1
}

func validateWeightBits(bits int, fieldPath string) *Error {
	if bits < 8 || bits > 64 {
		return NewError(CodeWeightScaleOutOfRange, "fixed weight bit width must be in [8,64]").
			WithFieldPath(fieldPath).
			WithContext("bits", bits)
	}
	return nil
}

// ConvertWeight converts a fixed-point integer between 1000 and 10000 scales.
// It always returns a report. When overflow makes the strict result invalid,
// the report contains E_WEIGHT_FIXED_OVERFLOW and the saturated value.
func ConvertWeight(raw int64, srcScale WeightScale, srcBits int, dstScale WeightScale, dstBits int) (int64, WeightConversionReport, *Error) {
	rep := WeightConversionReport{
		InputRaw:    raw,
		SourceScale: int64(srcScale),
		TargetScale: int64(dstScale),
		SourceBits:  srcBits,
		TargetBits:  dstBits,
		Tolerance:   0.5,
		Code:        CodeOK,
	}

	if !srcScale.Valid() || !dstScale.Valid() {
		err := NewError(CodeWeightScaleOutOfRange, "weight scale must be 1000 or 10000").
			WithFieldPath("weight_scale").
			WithContext("source_scale", int64(srcScale)).
			WithContext("target_scale", int64(dstScale))
		rep.Code = err.Code
		return 0, rep, err
	}
	if err := validateWeightBits(srcBits, "weight_bits"); err != nil {
		rep.Code = err.Code
		return 0, rep, err
	}
	if err := validateWeightBits(dstBits, "target_weight_bits"); err != nil {
		rep.Code = err.Code
		return 0, rep, err
	}
	if raw < 0 {
		err := NewError(CodeWeightScaleOutOfRange, "weight raw value must be non-negative").
			WithFieldPath("weight").
			WithContext("raw", raw)
		rep.Code = err.Code
		return 0, rep, err
	}

	srcMax := maxSigned(srcBits)
	if raw > srcMax {
		rep.SourceOverflow = true
		rep.Saturated = true
		rep.OutputRaw = srcMax
		rep.IntermediateNumerator = "source_out_of_range"
		rep.ExactTarget = float64(raw) * float64(dstScale) / float64(srcScale)
		rep.Delta = float64(rep.OutputRaw) - rep.ExactTarget
		rep.WithinTolerance = false
		rep.Code = CodeWeightOverflow
		err := NewError(CodeWeightOverflow, "source weight exceeds declared fixed-point width").
			WithFieldPath("weight").
			WithContext("raw", raw).
			WithContext("max", srcMax).
			WithContext("bits", srcBits)
		return rep.OutputRaw, rep, err
	}

	rep.ExactTarget = float64(raw) * float64(dstScale) / float64(srcScale)
	hi, lo := bits.Mul64(uint64(raw), uint64(dstScale))
	if hi != 0 || lo > uint64(math.MaxInt64) {
		rep.ArithmeticOverflow = true
		rep.TargetOverflow = true
		rep.Saturated = true
		rep.OutputRaw = maxSigned(dstBits)
		rep.IntermediateNumerator = fmt.Sprintf("overflow(raw=%d*scale=%d)", raw, dstScale)
		rep.Delta = float64(rep.OutputRaw) - rep.ExactTarget
		rep.WithinTolerance = false
		rep.Code = CodeWeightOverflow
		err := NewError(CodeWeightOverflow, "fixed-point conversion intermediate overflow").
			WithFieldPath("weight").
			WithContext("raw", raw).
			WithContext("target_scale", int64(dstScale)).
			WithContext("saturated_to", rep.OutputRaw)
		return rep.OutputRaw, rep, err
	}

	numerator := int64(lo)
	rep.IntermediateNumerator = fmt.Sprintf("%d", numerator)
	quotient := numerator / int64(srcScale)
	remainder := numerator % int64(srcScale)
	if remainder*2 >= int64(srcScale) {
		quotient++
	}

	dstMax := maxSigned(dstBits)
	if quotient > dstMax {
		rep.TargetOverflow = true
		rep.Saturated = true
		rep.OutputRaw = dstMax
	} else {
		rep.OutputRaw = quotient
	}
	rep.Delta = float64(rep.OutputRaw) - rep.ExactTarget
	rep.WithinTolerance = math.Abs(rep.Delta) <= rep.Tolerance+1e-12 && !rep.Saturated
	if rep.Saturated {
		rep.WithinTolerance = false
		rep.Code = CodeWeightOverflow
		err := NewError(CodeWeightOverflow, "converted weight exceeds target fixed-point width; saturated").
			WithFieldPath("weight").
			WithContext("raw", raw).
			WithContext("output", rep.OutputRaw).
			WithContext("target_max", dstMax).
			WithContext("delta", rep.Delta)
		return rep.OutputRaw, rep, err
	}
	return rep.OutputRaw, rep, nil
}

// CanonicalWeight converts to the canonical 1/10000 fixed-point scale.
func CanonicalWeight(raw int64, scale WeightScale, bits int) (int64, WeightConversionReport, *Error) {
	return ConvertWeight(raw, scale, bits, ScalePermyriad, bits)
}

// StableRoundTrip demonstrates that rounding reaches a fixed point and does
// not amplify across repeated scale conversions.
func StableRoundTrip(raw int64, scale WeightScale, bits, iterations int) ([]WeightConversionReport, int64, error) {
	reports := make([]WeightConversionReport, 0, iterations*2)
	current := raw
	currentScale := scale
	var coded *Error
	for i := 0; i < iterations; i++ {
		other := ScalePermyriad
		if currentScale == ScalePermyriad {
			other = ScalePermille
		}
		out, rep, err := ConvertWeight(current, currentScale, bits, other, bits)
		rep.MetricID = fmt.Sprintf("roundtrip-%d", i)
		reports = append(reports, rep)
		if err != nil {
			coded = err
		}
		current = out
		currentScale = other

		back, rep2, err2 := ConvertWeight(current, currentScale, bits, scale, bits)
		rep2.MetricID = fmt.Sprintf("roundtrip-back-%d", i)
		reports = append(reports, rep2)
		if err2 != nil && coded == nil {
			coded = err2
		}
		current = back
		currentScale = scale
	}
	return reports, current, coded
}
