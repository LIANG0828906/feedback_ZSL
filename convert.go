package vpcompose

import "math"

// DivideRounded performs integer division with an explicit, non-floating
// rounding rule. Negative inputs are handled symmetrically.
func DivideRounded(numerator, denominator int64, rounding Rounding) (int64, error) {
	if denominator == 0 {
		return 0, &EngineError{
			Code: CodeOverflow, Path: "division", Message: "zero denominator",
			Operands: []int64{numerator, denominator},
		}
	}
	if !rounding.Valid() {
		return 0, schemaError("rounding", "unsupported rounding rule")
	}
	q := numerator / denominator
	r := numerator % denominator
	if r == 0 {
		return q, nil
	}
	negative := numerator < 0
	if negative {
		r = -r
	}
	up := false
	switch rounding {
	case RoundUp, RoundCeil:
		up = true
	case RoundDown:
		up = false
	case RoundFloor:
		if negative {
			up = true
		}
	case RoundHalfUp, RoundNearest:
		up = r >= denominator-r
	}
	if up {
		if negative {
			q--
		} else {
			q++
		}
	}
	return q, nil
}

func checkedMul(a, b int64, path string) (int64, error) {
	if a == 0 || b == 0 {
		return 0, nil
	}
	if a > 0 && b > 0 && a > math.MaxInt64/b {
		return 0, overflowError(path, a, b)
	}
	if a < 0 && b < 0 && a < math.MaxInt64/b {
		return 0, overflowError(path, a, b)
	}
	if a > 0 && b < 0 && b < math.MinInt64/a {
		return 0, overflowError(path, a, b)
	}
	if a < 0 && b > 0 && a < math.MinInt64/b {
		return 0, overflowError(path, a, b)
	}
	return a * b, nil
}

// MillisecondsToTicks converts a millisecond time to the internal tick base.
func MillisecondsToTicks(ms, tickRate int64, rounding Rounding, path string) (int64, error) {
	if tickRate <= 0 {
		return 0, schemaError(path, "tick rate must be positive")
	}
	product, err := checkedMul(ms, tickRate, path)
	if err != nil {
		return 0, err
	}
	if rounding == "" {
		rounding = RoundHalfUp
	}
	result, err := DivideRounded(product, 1000, rounding)
	if err != nil {
		var ee *EngineError
		if AsEngineError(err, &ee) {
			ee.Path = path
		}
		return 0, err
	}
	return result, nil
}

// TicksToMilliseconds converts an internal tick time back to milliseconds.
func TicksToMilliseconds(ticks, tickRate int64, rounding Rounding, path string) (int64, error) {
	if tickRate <= 0 {
		return 0, schemaError(path, "tick rate must be positive")
	}
	if rounding == "" {
		rounding = RoundHalfUp
	}
	scaled, err := checkedMul(ticks, 1000, path)
	if err != nil {
		return 0, err
	}
	result, err := DivideRounded(scaled, tickRate, rounding)
	if err != nil {
		var ee *EngineError
		if AsEngineError(err, &ee) {
			ee.Path = path
		}
		return 0, err
	}
	return result, nil
}

// AsEngineError is a small local wrapper that keeps callers from importing
// errors solely for conversion error decoration.
func AsEngineError(err error, target **EngineError) bool {
	if err == nil {
		return false
	}
	if ee, ok := err.(*EngineError); ok {
		*target = ee
		return true
	}
	return false
}

// PremultiplyChannel converts a straight 16-bit channel to premultiplied form.
func PremultiplyChannel(channel, alpha uint16) uint16 {
	if channel == 0 || alpha == 0 {
		return 0
	}
	if alpha == 0xffff {
		return channel
	}
	return uint16((uint32(channel)*uint32(alpha) + 32767) / 65535)
}

// UnpremultiplyChannel converts a premultiplied 16-bit channel to straight form.
func UnpremultiplyChannel(channel, alpha uint16) uint16 {
	if channel == 0 || alpha == 0 {
		return 0
	}
	if alpha == 0xffff {
		return channel
	}
	value := (uint32(channel)*65535 + uint32(alpha)/2) / uint32(alpha)
	if value > 65535 {
		return 65535
	}
	return uint16(value)
}

// ConvertAlphaMode performs the declared conversion and returns a trace,
// including a no-op trace for already-canonical data.
func ConvertAlphaMode(rgba [4]uint16, from, to AlphaMode) ([4]uint16, ConversionTrace) {
	out := rgba
	trace := ConversionTrace{
		Path: "rgba", OldValue: string(from), NewValue: string(to),
		Rule: "integer-rounded",
	}
	if from == to {
		trace.Detail = "already canonical"
		return out, trace
	}
	if from == AlphaStraight && to == AlphaPremultiplied {
		out[0] = PremultiplyChannel(rgba[0], rgba[3])
		out[1] = PremultiplyChannel(rgba[1], rgba[3])
		out[2] = PremultiplyChannel(rgba[2], rgba[3])
		trace.Detail = "straight RGB multiplied by alpha"
		return out, trace
	}
	if from == AlphaPremultiplied && to == AlphaStraight {
		out[0] = UnpremultiplyChannel(rgba[0], rgba[3])
		out[1] = UnpremultiplyChannel(rgba[1], rgba[3])
		out[2] = UnpremultiplyChannel(rgba[2], rgba[3])
		trace.Detail = "premultiplied RGB divided by alpha"
		return out, trace
	}
	trace.Detail = "unknown alpha convention; value unchanged"
	return out, trace
}
