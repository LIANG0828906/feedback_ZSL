package units

import (
	"math"
	"math/big"
	"strings"

	"lipsync/domain"
)

type Rounding string

const (
	RoundFloor   Rounding = "floor"
	RoundCeil    Rounding = "ceil"
	RoundNearest Rounding = "nearest"
)

func ParseRounding(value string) (Rounding, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "floor", "down", "toward_zero":
		return RoundFloor, nil
	case "ceil", "up":
		return RoundCeil, nil
	case "nearest", "round":
		return RoundNearest, nil
	default:
		return "", domain.NewError(domain.CodeSchema, "rounding", "unsupported rounding direction")
	}
}

// Conversion describes one checked integer conversion. A non-zero Remainder is
// also emitted as a domain.Truncation by the caller.
type Conversion struct {
	Input       int64
	Output      int64
	Numerator   int64
	Denominator int64
	Remainder   int64
	Direction   Rounding
}

func (c Conversion) Truncated() bool { return c.Remainder != 0 }

// ConvertFrames converts an absolute source position/duration to frame units.
// Samples use samples*frameRate/sampleRate; milliseconds use ms*frameRate/1000.
func ConvertFrames(input, sourceRate, frameRate int64, rounding Rounding, field string) (Conversion, error) {
	if input < 0 || sourceRate <= 0 || frameRate <= 0 {
		return Conversion{}, &domain.Error{
			Code:     domain.CodeSchema,
			Path:     field,
			OperandA: input,
			OperandB: sourceRate,
			Message:  "conversion inputs must be non-negative and rates must be positive",
		}
	}
	num := new(big.Int).Mul(big.NewInt(input), big.NewInt(frameRate))
	den := big.NewInt(sourceRate)
	if !num.IsInt64() {
		return Conversion{}, &domain.Error{
			Code:     domain.CodeOverflow,
			Path:     field,
			Field:    field,
			OperandA: input,
			OperandB: frameRate,
			Message:  "multiplication overflows int64 during frame conversion",
		}
	}
	n := num.Int64()
	q, r := n/den.Int64(), n%den.Int64()
	out, err := applyRounding(q, r, den.Int64(), rounding)
	if err != nil {
		return Conversion{}, &domain.Error{
			Code:     domain.CodeOverflow,
			Path:     field,
			Field:    field,
			OperandA: q,
			OperandB: 1,
			Message:  "rounded frame conversion overflows int64",
		}
	}
	return Conversion{
		Input:       input,
		Output:      out,
		Numerator:   n,
		Denominator: den.Int64(),
		Remainder:   r,
		Direction:   rounding,
	}, nil
}

func SamplesToFrames(samples, sampleRate, frameRate int64, rounding Rounding, field string) (Conversion, error) {
	return ConvertFrames(samples, sampleRate, frameRate, rounding, field)
}

func MillisToFrames(ms, frameRate int64, rounding Rounding, field string) (Conversion, error) {
	return ConvertFrames(ms, 1000, frameRate, rounding, field)
}

// FramesToSamples and FramesToMillis are used by lower-version exporters.
func FramesToSamples(frames, sampleRate, frameRate int64, rounding Rounding, field string) (Conversion, error) {
	if frames < 0 || sampleRate <= 0 || frameRate <= 0 {
		return Conversion{}, &domain.Error{Code: domain.CodeSchema, Path: field, Message: "invalid frame conversion input"}
	}
	num := new(big.Int).Mul(big.NewInt(frames), big.NewInt(sampleRate))
	if !num.IsInt64() {
		return Conversion{}, &domain.Error{
			Code: domain.CodeOverflow, Path: field, Field: field,
			OperandA: frames, OperandB: sampleRate,
			Message: "multiplication overflows int64 during sample conversion",
		}
	}
	n := num.Int64()
	q, r := n/frameRate, n%frameRate
	out, err := applyRounding(q, r, frameRate, rounding)
	if err != nil {
		return Conversion{}, &domain.Error{Code: domain.CodeOverflow, Path: field, Field: field, OperandA: q, OperandB: 1}
	}
	return Conversion{Input: frames, Output: out, Numerator: n, Denominator: frameRate, Remainder: r, Direction: rounding}, nil
}

func FramesToMillis(frames, frameRate int64, rounding Rounding, field string) (Conversion, error) {
	if frames < 0 || frameRate <= 0 {
		return Conversion{}, &domain.Error{Code: domain.CodeSchema, Path: field, Message: "invalid millisecond conversion input"}
	}
	num := new(big.Int).Mul(big.NewInt(frames), big.NewInt(1000))
	if !num.IsInt64() {
		return Conversion{}, &domain.Error{
			Code: domain.CodeOverflow, Path: field, Field: field,
			OperandA: frames, OperandB: 1000,
			Message: "multiplication overflows int64 during millisecond conversion",
		}
	}
	n := num.Int64()
	q, r := n/frameRate, n%frameRate
	out, err := applyRounding(q, r, frameRate, rounding)
	if err != nil {
		return Conversion{}, &domain.Error{Code: domain.CodeOverflow, Path: field, Field: field, OperandA: q, OperandB: 1}
	}
	return Conversion{Input: frames, Output: out, Numerator: n, Denominator: frameRate, Remainder: r, Direction: rounding}, nil
}

func PermilleToBasisPoints(value int64, rounding Rounding, field string) (Conversion, error) {
	return scaleChecked(value, 10000, rounding, field, 1000)
}

func BasisPointsToPermille(value int64, rounding Rounding, field string) (Conversion, error) {
	return scaleChecked(value, 1, rounding, field, 10)
}

// scaleChecked computes value*factor/divisor without float accumulation.
func scaleChecked(value, factor int64, rounding Rounding, field string, divisor int64) (Conversion, error) {
	if value < 0 || factor <= 0 || divisor <= 0 {
		return Conversion{}, &domain.Error{Code: domain.CodeSchema, Path: field, Message: "invalid scale conversion input"}
	}
	num := new(big.Int).Mul(big.NewInt(value), big.NewInt(factor))
	if !num.IsInt64() {
		return Conversion{}, &domain.Error{
			Code: domain.CodeOverflow, Path: field, Field: field,
			OperandA: value, OperandB: factor,
			Message: "multiplication overflows int64 during budget conversion",
		}
	}
	n := num.Int64()
	q, r := n/divisor, n%divisor
	out, err := applyRounding(q, r, divisor, rounding)
	if err != nil {
		return Conversion{}, &domain.Error{Code: domain.CodeOverflow, Path: field, Field: field, OperandA: q, OperandB: 1}
	}
	return Conversion{Input: value, Output: out, Numerator: n, Denominator: divisor, Remainder: r, Direction: rounding}, nil
}

func applyRounding(q, remainder, denominator int64, rounding Rounding) (int64, error) {
	if remainder == 0 {
		return q, nil
	}
	increment := int64(0)
	switch rounding {
	case RoundFloor:
	case RoundCeil:
		increment = 1
	case RoundNearest:
		if remainder >= (denominator+1)/2 {
			increment = 1
		}
	default:
		return 0, domain.NewError(domain.CodeSchema, "rounding", "unsupported rounding direction")
	}
	if increment > 0 && q == math.MaxInt64 {
		return 0, domain.NewError(domain.CodeOverflow, "rounding", "rounded conversion overflows int64")
	}
	return q + increment, nil
}

func RecordTruncation(list *[]domain.Truncation, path, from, to string, c Conversion, channel, packet string) {
	if !c.Truncated() {
		return
	}
	*list = append(*list, domain.Truncation{
		Path:        path,
		FromUnit:    from,
		ToUnit:      to,
		Input:       c.Input,
		Output:      c.Output,
		Remainder:   c.Remainder,
		Numerator:   c.Numerator,
		Denominator: c.Denominator,
		Direction:   string(c.Direction),
		ChannelID:   channel,
		PacketID:    packet,
	})
}
