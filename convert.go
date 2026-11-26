package vpf

import (
	"math"
)

type RoundingDirection string

const (
	RoundFloor   RoundingDirection = "floor_toward_zero_for_non_negative"
	RoundNearest RoundingDirection = "nearest_half_up"
)

// ScaleFloor computes value*num/den using int64 arithmetic. A non-zero
// remainder is recorded as a deterministic truncation site.
func ScaleFloor(field, at string, value, num, den int64, traces *TraceBook) (int64, error) {
	if den <= 0 {
		return 0, schemaError(0, field, "scale denominator must be positive, got %d", den)
	}
	if value < 0 || num < 0 {
		return 0, schemaError(0, field, "only non-negative conversion operands are declared, got value=%d num=%d", value, num)
	}
	if num != 0 && value > math.MaxInt64/num {
		return 0, overflowError(field, value, num)
	}
	product := value * num
	out := product / den
	rem := product % den
	if rem != 0 && traces != nil {
		traces.AddTruncation(TruncationRecord{
			Field:       field,
			Operation:   "floor_scale",
			Input:       value,
			Operand:     num,
			Denominator: den,
			Output:      out,
			Remainder:   rem,
			Direction:   string(RoundFloor),
			At:          at,
		})
	}
	return out, nil
}

// ScaleNearest computes nearest-half-up integer scaling and records the
// discarded remainder whenever the exact rational result is non-integral.
func ScaleNearest(field, at string, value, num, den int64, traces *TraceBook) (int64, error) {
	if den <= 0 {
		return 0, schemaError(0, field, "scale denominator must be positive, got %d", den)
	}
	if value < 0 || num < 0 {
		return 0, schemaError(0, field, "only non-negative conversion operands are declared, got value=%d num=%d", value, num)
	}
	if num != 0 && value > math.MaxInt64/(2*num) {
		// The rounding addition can itself overflow; report the two source operands.
		return 0, overflowError(field, value, num)
	}
	product := value * num
	rounded := product + den/2
	if rounded < product {
		return 0, overflowError(field, product, den/2)
	}
	out := rounded / den
	rem := product % den
	if rem != 0 && traces != nil {
		traces.AddTruncation(TruncationRecord{
			Field:       field,
			Operation:   "nearest_half_up_scale",
			Input:       value,
			Operand:     num,
			Denominator: den,
			Output:      out,
			Remainder:   rem,
			Direction:   string(RoundNearest),
			At:          at,
		})
	}
	return out, nil
}

func TicksToNanoseconds(field string, ticks int64, traces *TraceBook) (int64, error) {
	out, err := ScaleFloor(field, "TicksToNanoseconds", ticks, TickNanoseconds, 1, traces)
	if err != nil {
		return 0, err
	}
	return out, nil
}

func NanosecondsToTicks(field string, ns int64, traces *TraceBook) (int64, error) {
	return ScaleFloor(field, "NanosecondsToTicks", ns, 1, TickNanoseconds, traces)
}

func MillisecondsToNanoseconds(field string, ms int64, traces *TraceBook) (int64, error) {
	return ScaleFloor(field, "MillisecondsToNanoseconds", ms, MillisecondNanoseconds, 1, traces)
}

func NanosecondsToMilliseconds(field string, ns int64, traces *TraceBook) (int64, error) {
	return ScaleFloor(field, "NanosecondsToMilliseconds", ns, 1, MillisecondNanoseconds, traces)
}

func MicrosecondsToNanoseconds(field string, us int64, traces *TraceBook) (int64, error) {
	return ScaleFloor(field, "MicrosecondsToNanoseconds", us, MicrosecondNanoseconds, 1, traces)
}

func NanosecondsToMicroseconds(field string, ns int64, traces *TraceBook) (int64, error) {
	return ScaleFloor(field, "NanosecondsToMicroseconds", ns, 1, MicrosecondNanoseconds, traces)
}

// TimeToSchemaUnit converts the canonical nanosecond timeline to a schema's
// declared integer time unit.
func TimeToSchemaUnit(field string, ns int64, version int, traces *TraceBook) (int64, error) {
	switch version {
	case SchemaV1:
		return NanosecondsToTicks(field, ns, traces)
	case SchemaV2:
		return NanosecondsToMilliseconds(field, ns, traces)
	case SchemaV3:
		return NanosecondsToMicroseconds(field, ns, traces)
	default:
		return 0, schemaError(version, field, "unknown schema version")
	}
}

// TimeFromSchemaUnit converts a schema time value into canonical nanoseconds.
func TimeFromSchemaUnit(field string, value int64, version int, traces *TraceBook) (int64, error) {
	switch version {
	case SchemaV1:
		return TicksToNanoseconds(field, value, traces)
	case SchemaV2:
		return MillisecondsToNanoseconds(field, value, traces)
	case SchemaV3:
		return MicrosecondsToNanoseconds(field, value, traces)
	default:
		return 0, schemaError(version, field, "unknown schema version")
	}
}

// PremultiplyChannel converts a straight-alpha color channel to a
// premultiplied channel using floor rounding.
func PremultiplyChannel(field string, channel, alpha uint8, traces *TraceBook) (uint8, error) {
	out, err := ScaleFloor(field, "PremultiplyChannel", int64(channel), int64(alpha), ColorMax, traces)
	if err != nil {
		return 0, err
	}
	return uint8(out), nil
}

// UnpremultiplyChannel recovers a straight channel using floor division.
// Alpha zero is declared to carry zero color in these schemas.
func UnpremultiplyChannel(field string, premultiplied, alpha uint8, traces *TraceBook) (uint8, error) {
	if alpha == 0 {
		if premultiplied != 0 && traces != nil {
			traces.AddTruncation(TruncationRecord{
				Field:     field,
				Operation: "UnpremultiplyZeroAlpha",
				Input:     int64(premultiplied),
				Output:    0,
				Remainder: int64(premultiplied),
				Direction: "clamp_zero_alpha_color",
				At:        "UnpremultiplyChannel",
			})
		}
		return 0, nil
	}
	out, err := ScaleFloor(field, "UnpremultiplyChannel", int64(premultiplied), ColorMax, int64(alpha), traces)
	if err != nil {
		return 0, err
	}
	if out > ColorMax {
		return ColorMax, nil
	}
	return uint8(out), nil
}
