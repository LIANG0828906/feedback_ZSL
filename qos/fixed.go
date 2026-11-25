package qos

import "math"

const TokenScale int64 = 10000

// FixedCheck describes integer conversion/refill behavior for audit output.
type FixedCheck struct {
	DeclaredWidth int   `json:"declared_width"`
	Overflow      bool  `json:"overflow"`
	Saturated     bool  `json:"saturated"`
	Tolerance     int64 `json:"tolerance"`
	WithinTol     bool  `json:"within_tolerance"`
}

func widthMax(width int) int64 {
	if width == 32 {
		return 1<<31 - 1
	}
	return math.MaxInt64
}

func MulChecked(a, b int64, width int) (int64, bool) {
	if a == 0 || b == 0 {
		return 0, false
	}
	if a > 0 && b > 0 && a > math.MaxInt64/b {
		return widthMax(width), true
	}
	if a < 0 && b < 0 && a < math.MaxInt64/b {
		return widthMax(width), true
	}
	if a > 0 && b < 0 && b < math.MinInt64/a {
		return math.MinInt64, true
	}
	if a < 0 && b > 0 && a < math.MinInt64/b {
		return math.MinInt64, true
	}
	v := a * b
	if v > widthMax(width) || v < -widthMax(width)-1 {
		return v, true
	}
	return v, false
}

func AddChecked(a, b int64, width int) (int64, bool) {
	if b > 0 && a > math.MaxInt64-b {
		return widthMax(width), true
	}
	if b < 0 && a < math.MinInt64-b {
		return math.MinInt64, true
	}
	v := a + b
	if v > widthMax(width) || v < -widthMax(width)-1 {
		return v, true
	}
	return v, false
}

// ConvertRateToPermyriad converts a per-mille or per-myriad rate into the
// canonical 1/10000 bytes-per-millisecond representation.
func ConvertRateToPermyriad(value, scale int64, width int) (int64, error) {
	if value < 0 {
		return 0, NewError(ERateNegative, "rate", "rate must not be negative", map[string]any{"value": value, "scale": scale})
	}
	var factor int64
	switch scale {
	case RateScalePermille:
		factor = 10
	case RateScalePermyriad:
		factor = 1
	default:
		return 0, NewError(ERateScale, "rate.scale", "rate scale must be 1000 or 10000", map[string]any{"scale": scale})
	}
	v, overflow := MulChecked(value, factor, width)
	if overflow || v > widthMax(width) {
		return 0, NewError(ERateOverflow, "rate", "rate conversion overflow", map[string]any{"value": value, "scale": scale, "width": width})
	}
	return v, nil
}

func ConvertWeightToPermyriad(value, scale int64, width int) (int64, error) {
	if value <= 0 {
		return 0, NewError(EWeightRange, "weight", "weight must be positive", map[string]any{"value": value, "scale": scale})
	}
	var factor int64
	switch scale {
	case RateScalePermille:
		factor = 10
	case RateScalePermyriad:
		factor = 1
	default:
		return 0, NewError(EWeightRange, "weight.scale", "weight scale must be 1000 or 10000", map[string]any{"scale": scale})
	}
	v, overflow := MulChecked(value, factor, width)
	if overflow || v > 1<<40 {
		return 0, NewError(EWeightRange, "weight", "weight conversion overflow", map[string]any{"value": value, "scale": scale, "width": width})
	}
	return v, nil
}

func ConvertPacketsToBytes(packets, nominal int64, width int) (int64, error) {
	if packets < 0 {
		return 0, NewError(ECapacityNegative, "capacity.packets", "packet capacity must not be negative", map[string]any{"packets": packets})
	}
	v, overflow := MulChecked(packets, nominal, width)
	if overflow {
		return 0, NewError(EOverflow, "capacity.packets", "packet-to-byte capacity conversion overflow", map[string]any{"packets": packets, "nominal_packet_bytes": nominal, "width": width})
	}
	return v, nil
}

// PacketsFromBytes returns packet-equivalent thousandths without floating point.
func PacketsFromBytes(bytes, nominal int64) (int64, error) {
	if nominal <= 0 {
		return 0, NewError(EOverflow, "nominal_packet_bytes", "nominal packet size must be positive", nil)
	}
	v, overflow := MulChecked(bytes, 1000, 64)
	if overflow {
		return math.MaxInt64, NewError(EOverflow, "tokens", "packet-equivalent conversion overflow", map[string]any{"bytes": bytes})
	}
	return v / nominal, nil
}

// RefillResult is exact to 1/10000 token. The remainder is carried to the next
// step, so serialized ticks cannot accumulate conversion drift.
type RefillResult struct {
	Added         int64
	Numerator     int64
	Remainder     int64
	Saturated     bool
	Overflow      bool
	Tokens        int64
	IntegerDenom  int64
	DeclaredWidth int
}

func Refill(tokens, remainder, ratePermyriad, elapsedMS, capacity int64, width int) (RefillResult, error) {
	res := RefillResult{Remainder: remainder, Tokens: tokens, IntegerDenom: TokenScale, DeclaredWidth: width}
	if elapsedMS < 0 {
		return res, NewError(ETimeNegative, "elapsed_ms", "elapsed time must not be negative", map[string]any{"elapsed_ms": elapsedMS})
	}
	if remainder < 0 || remainder >= TokenScale {
		return res, NewError(EInternal, "refill.remainder", "refill remainder out of range", map[string]any{"remainder": remainder})
	}
	product, overflow := MulChecked(ratePermyriad, elapsedMS, width)
	if overflow {
		res.Overflow = true
		return res, NewError(EOverflow, "refill", "rate * elapsed overflow", map[string]any{"rate_permyriad": ratePermyriad, "elapsed_ms": elapsedMS, "width": width})
	}
	num, overflow := AddChecked(product, remainder, width)
	if overflow {
		res.Overflow = true
		return res, NewError(EOverflow, "refill", "refill numerator overflow", map[string]any{"product": product, "remainder": remainder, "width": width})
	}
	res.Numerator = num
	res.Added = num / TokenScale
	res.Remainder = num % TokenScale
	next, overflow := AddChecked(tokens, res.Added, width)
	if overflow {
		res.Overflow = true
		return res, NewError(EOverflow, "refill.tokens", "token balance overflow", map[string]any{"tokens": tokens, "added": res.Added, "width": width})
	}
	if next > capacity {
		next = capacity
		res.Saturated = true
	}
	res.Tokens = next
	return res, nil
}

// DelayForDeficit computes ceil(deficit*10000/rate), rounds up to the queue
// age quantum, and returns the unquantized value and quantization error.
func DelayForDeficit(deficitBytes, ratePermyriad, quantumMS int64, width int) (delayMS, exactMS, errorMS int64, check FixedCheck, err error) {
	check = FixedCheck{DeclaredWidth: width, Tolerance: quantumMS - 1, WithinTol: true}
	if deficitBytes < 0 {
		return 0, 0, 0, check, NewError(EOverflow, "delay.deficit", "token deficit must not be negative", map[string]any{"deficit_bytes": deficitBytes})
	}
	if deficitBytes == 0 {
		return quantumMS, 0, quantumMS, check, nil
	}
	if ratePermyriad <= 0 {
		return 0, 0, 0, check, NewError(ERateZero, "delay.rate", "positive rate is required to defer a packet", map[string]any{"rate_permyriad": ratePermyriad})
	}
	if quantumMS <= 0 {
		return 0, 0, 0, check, NewError(EDelayQueue, "delay.quantum", "queue age quantum must be positive", map[string]any{"quantum_ms": quantumMS})
	}
	num, overflow := MulChecked(deficitBytes, TokenScale, width)
	if overflow {
		check.Overflow = true
		check.WithinTol = false
		return 0, 0, 0, check, NewError(EOverflow, "delay", "deficit conversion overflow", map[string]any{"deficit_bytes": deficitBytes, "width": width})
	}
	if num > math.MaxInt64-(ratePermyriad-1) {
		check.Overflow = true
		check.WithinTol = false
		return 0, 0, 0, check, NewError(EOverflow, "delay", "delay ceiling addition overflow", map[string]any{"numerator": num, "rate_permyriad": ratePermyriad})
	}
	exactMS = (num + ratePermyriad - 1) / ratePermyriad
	rem := exactMS % quantumMS
	if rem != 0 {
		delayMS = exactMS + (quantumMS - rem)
	} else {
		delayMS = exactMS
	}
	errorMS = delayMS - exactMS
	check.WithinTol = errorMS >= 0 && errorMS < quantumMS
	if !check.WithinTol {
		return 0, exactMS, errorMS, check, NewError(EOverflow, "delay", "delay quantization exceeded declared tolerance", map[string]any{"exact_ms": exactMS, "quantum_ms": quantumMS, "error_ms": errorMS})
	}
	return delayMS, exactMS, errorMS, check, nil
}

func RateValueForPermyriad(ratePermyriad, scale int64) (int64, error) {
	switch scale {
	case RateScalePermyriad:
		return ratePermyriad, nil
	case RateScalePermille:
		if ratePermyriad%10 != 0 {
			return 0, NewError(ERateScale, "rate.scale", "canonical rate is not exactly representable at per-mille scale", map[string]any{"rate_permyriad": ratePermyriad, "scale": scale})
		}
		return ratePermyriad / 10, nil
	default:
		return 0, NewError(ERateScale, "rate.scale", "unsupported scale", map[string]any{"scale": scale})
	}
}
