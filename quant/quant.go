package quant

import (
	"math"
	"strconv"

	"lipsync/domain"
)

type Conversion struct {
	Value     int64
	Truncated bool
	Remainder int64
	Divisor   int64
	Round     domain.Rounding
	Field     string
}

func mulOverflows(a, b int64) bool {
	if a == 0 || b == 0 {
		return false
	}
	return a > math.MaxInt64/b || a < math.MinInt64/b
}

func divRound(num, den int64, r domain.Rounding) (int64, int64, bool, error) {
	if den == 0 {
		return 0, 0, false, &domain.Error{
			Code: domain.CodeParam, Message: "division by zero", Field: "denominator",
			OperandA: num, OperandB: den,
		}
	}
	if num < 0 || den < 0 {
		return 0, 0, false, &domain.Error{
			Code: domain.CodeParam, Message: "negative time or scale is not supported",
			OperandA: num, OperandB: den,
		}
	}
	q := num / den
	rem := num % den
	truncated := rem != 0
	switch r {
	case domain.RoundFloor:
	case domain.RoundCeil:
		if rem != 0 {
			q++
		}
	case domain.RoundNearest:
		if rem != 0 && rem >= den-rem {
			q++
		}
	default:
		return 0, 0, false, &domain.Error{
			Code: domain.CodeParam, Message: "invalid rounding mode",
			Value: string(r),
		}
	}
	return q, rem, truncated, nil
}

func SampleToFrame(sample, sampleRate, fps int64, rounding domain.Rounding, field string) (Conversion, error) {
	if sampleRate <= 0 || fps <= 0 || sample < 0 {
		return Conversion{}, &domain.Error{
			Code: domain.CodeParam, Message: "invalid sample quantization operands",
			Field: field, OperandA: sample, OperandB: sampleRate,
		}
	}
	if mulOverflows(sample, fps) {
		return Conversion{}, &domain.Error{
			Code: domain.CodeOverflow, Message: "sample*fps overflows int64",
			Field: field, OperandA: sample, OperandB: fps,
		}
	}
	num := sample * fps
	v, rem, truncated, err := divRound(num, sampleRate, rounding)
	if err != nil {
		return Conversion{}, err
	}
	return Conversion{Value: v, Truncated: truncated, Remainder: rem, Divisor: sampleRate, Round: rounding, Field: field}, nil
}

func MillisToFrame(ms, fps int64, rounding domain.Rounding, field string) (Conversion, error) {
	if fps <= 0 || ms < 0 {
		return Conversion{}, &domain.Error{
			Code: domain.CodeParam, Message: "invalid millisecond quantization operands",
			Field: field, OperandA: ms, OperandB: fps,
		}
	}
	if mulOverflows(ms, fps) {
		return Conversion{}, &domain.Error{
			Code: domain.CodeOverflow, Message: "milliseconds*fps overflows int64",
			Field: field, OperandA: ms, OperandB: fps,
		}
	}
	v, rem, truncated, err := divRound(ms*fps, 1000, rounding)
	if err != nil {
		return Conversion{}, err
	}
	return Conversion{Value: v, Truncated: truncated, Remainder: rem, Divisor: 1000, Round: rounding, Field: field}, nil
}

func FrameToMillis(frame, fps int64, rounding domain.Rounding, field string) (Conversion, error) {
	if fps <= 0 || frame < 0 {
		return Conversion{}, &domain.Error{
			Code: domain.CodeParam, Message: "invalid frame-to-millisecond operands",
			Field: field, OperandA: frame, OperandB: fps,
		}
	}
	if mulOverflows(frame, 1000) {
		return Conversion{}, &domain.Error{
			Code: domain.CodeOverflow, Message: "frame*1000 overflows int64",
			Field: field, OperandA: frame, OperandB: 1000,
		}
	}
	v, rem, truncated, err := divRound(frame*1000, fps, rounding)
	if err != nil {
		return Conversion{}, err
	}
	return Conversion{Value: v, Truncated: truncated, Remainder: rem, Divisor: fps, Round: rounding, Field: field}, nil
}

func FrameToSample(frame, sampleRate, fps int64, rounding domain.Rounding, field string) (Conversion, error) {
	if sampleRate <= 0 || fps <= 0 || frame < 0 {
		return Conversion{}, &domain.Error{
			Code: domain.CodeParam, Message: "invalid frame-to-sample operands",
			Field: field, OperandA: frame, OperandB: sampleRate,
		}
	}
	if mulOverflows(frame, sampleRate) {
		return Conversion{}, &domain.Error{
			Code: domain.CodeOverflow, Message: "frame*sampleRate overflows int64",
			Field: field, OperandA: frame, OperandB: sampleRate,
		}
	}
	v, rem, truncated, err := divRound(frame*sampleRate, fps, rounding)
	if err != nil {
		return Conversion{}, err
	}
	return Conversion{Value: v, Truncated: truncated, Remainder: rem, Divisor: fps, Round: rounding, Field: field}, nil
}

func ToFrame(value int64, basis string, sampleRate, fps int64, rounding domain.Rounding, field string) (Conversion, error) {
	switch basis {
	case "frame":
		return Conversion{Value: value, Field: field, Round: rounding}, nil
	case "millisecond":
		return MillisToFrame(value, fps, rounding, field)
	case "sample":
		return SampleToFrame(value, sampleRate, fps, rounding, field)
	default:
		return Conversion{}, &domain.Error{
			Code: domain.CodeSchema, Message: "unsupported time basis", Field: field,
			Value: basis,
		}
	}
}

func FromFrame(frame int64, basis string, sampleRate, fps int64, rounding domain.Rounding, field string) (Conversion, error) {
	switch basis {
	case "frame":
		return Conversion{Value: frame, Field: field, Round: rounding}, nil
	case "millisecond":
		return FrameToMillis(frame, fps, rounding, field)
	case "sample":
		return FrameToSample(frame, sampleRate, fps, rounding, field)
	default:
		return Conversion{}, &domain.Error{
			Code: domain.CodeSchema, Message: "unsupported time basis", Field: field,
			Value: basis,
		}
	}
}

// WeightToPermyriad converts a declared duration weight into the canonical
// per-ten-thousand representation without floating point arithmetic.
func WeightToPermyriad(value, sourceScale int64, rounding domain.Rounding, field string) (Conversion, error) {
	if sourceScale <= 0 || value < 0 {
		return Conversion{}, &domain.Error{
			Code: domain.CodeParam, Message: "invalid weight operands",
			Field: field, OperandA: value, OperandB: sourceScale,
		}
	}
	if sourceScale == 10000 {
		return Conversion{Value: value, Field: field, Round: rounding}, nil
	}
	if mulOverflows(value, 10000) {
		return Conversion{}, &domain.Error{
			Code: domain.CodeOverflow, Message: "weight*10000 overflows int64",
			Field: field, OperandA: value, OperandB: 10000,
		}
	}
	v, rem, truncated, err := divRound(value*10000, sourceScale, rounding)
	if err != nil {
		return Conversion{}, err
	}
	return Conversion{Value: v, Truncated: truncated, Remainder: rem, Divisor: sourceScale, Round: rounding, Field: field}, nil
}

func WeightFromPermyriad(value, targetScale int64, rounding domain.Rounding, field string) (Conversion, error) {
	if targetScale <= 0 || value < 0 {
		return Conversion{}, &domain.Error{
			Code: domain.CodeParam, Message: "invalid weight operands",
			Field: field, OperandA: value, OperandB: targetScale,
		}
	}
	if targetScale == 10000 {
		return Conversion{Value: value, Field: field, Round: rounding}, nil
	}
	if mulOverflows(value, targetScale) {
		return Conversion{}, &domain.Error{
			Code: domain.CodeOverflow, Message: "weight*targetScale overflows int64",
			Field: field, OperandA: value, OperandB: targetScale,
		}
	}
	v, rem, truncated, err := divRound(value*targetScale, 10000, rounding)
	if err != nil {
		return Conversion{}, err
	}
	return Conversion{Value: v, Truncated: truncated, Remainder: rem, Divisor: 10000, Round: rounding, Field: field}, nil
}

func FormatInt(v int64) string { return strconv.FormatInt(v, 10) }
