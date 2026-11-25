package model

import "math"

func CheckedAdd(field string, a, b int64) (int64, *KernelError) {
	if b > 0 && a > math.MaxInt64-b {
		return 0, Overflow(field, a, b)
	}
	if b < 0 && a < math.MinInt64-b {
		return 0, Overflow(field, a, b)
	}
	return a + b, nil
}

func CheckedSub(field string, a, b int64) (int64, *KernelError) {
	if b == math.MinInt64 {
		return 0, Overflow(field, a, b)
	}
	return CheckedAdd(field, a, -b)
}

func CheckedMul(field string, a, b int64) (int64, *KernelError) {
	if a == 0 || b == 0 {
		return 0, nil
	}
	if (a == -1 && b == math.MinInt64) || (b == -1 && a == math.MinInt64) {
		return 0, Overflow(field, a, b)
	}
	p := a * b
	if p/b != a {
		return 0, Overflow(field, a, b)
	}
	return p, nil
}

// FloorDiv truncates toward negative infinity. All schema transforms use this
// declared direction.
func FloorDiv(a, b int64) int64 {
	q := a / b
	if a%b != 0 && ((a < 0) != (b < 0)) {
		q--
	}
	return q
}

func FloorMod(a, b int64) int64 {
	m := a % b
	if m != 0 && ((m < 0) != (b < 0)) {
		m += b
	}
	return m
}

func GCD(a, b int64) int64 {
	if a < 0 {
		a = -a
	}
	if b < 0 {
		b = -b
	}
	for b != 0 {
		a, b = b, a%b
	}
	return a
}

func InternalTickRate(tickRate int64) (int64, *KernelError) {
	if tickRate <= 0 {
		return 0, &KernelError{Code: CodeSchema, Field: "tick_rate", Message: "tick rate must be positive"}
	}
	g := GCD(1000, tickRate)
	scaled, err := CheckedMul("tick_rate.lcm", tickRate/g, 1000)
	if err != nil {
		return 0, err
	}
	return scaled, nil
}

// MillisToInternal is exact because the internal rate is a multiple of 1000.
func MillisToInternal(field string, ms, internalRate int64) (int64, *KernelError) {
	if ms < 0 {
		return 0, &KernelError{Code: CodeSchema, Field: field, Message: "time must not be negative"}
	}
	return CheckedMul(field, ms, internalRate/1000)
}

// NativeTicksToInternal is exact because the internal rate is a multiple of
// the declared native tick rate.
func NativeTicksToInternal(field string, ticks, tickRate, internalRate int64) (int64, *KernelError) {
	if ticks < 0 {
		return 0, &KernelError{Code: CodeSchema, Field: field, Message: "time must not be negative"}
	}
	return CheckedMul(field, ticks, internalRate/tickRate)
}

// InternalToMillis floors any remainder and reports the truncated remainder in
// internal ticks.
func InternalToMillis(internal, internalRate int64) (int64, int64, *KernelError) {
	if internal < 0 {
		return 0, 0, &KernelError{Code: CodeSchema, Field: "time", Message: "time must not be negative"}
	}
	perMS := internalRate / 1000
	ms := FloorDiv(internal, perMS)
	residual := FloorMod(internal, perMS)
	return ms, residual, nil
}

func InternalToNativeTicks(internal, internalRate, tickRate int64) (int64, int64, *KernelError) {
	if internal < 0 {
		return 0, 0, &KernelError{Code: CodeSchema, Field: "time", Message: "time must not be negative"}
	}
	perTick := internalRate / tickRate
	ticks := FloorDiv(internal, perTick)
	residual := FloorMod(internal, perTick)
	return ticks, residual, nil
}

func UnitsToInternal(field string, units int64) (int64, *KernelError) {
	return CheckedMul(field, units, InternalMilliPerUnit)
}

func InternalToUnits(field string, internal int64) (int64, int64, *KernelError) {
	units := FloorDiv(internal, InternalMilliPerUnit)
	residual := FloorMod(internal, InternalMilliPerUnit)
	return units, residual, nil
}

func AbsoluteFromOffset(field string, origin, offsetUnits int64) (int64, *KernelError) {
	offset, err := UnitsToInternal(field, offsetUnits)
	if err != nil {
		return 0, err
	}
	return CheckedAdd(field, origin, offset)
}

func OffsetFromAbsolute(field string, absolute, origin int64) (int64, int64, *KernelError) {
	delta, err := CheckedSub(field, absolute, origin)
	if err != nil {
		return 0, 0, err
	}
	units, residual, err := InternalToUnits(field, delta)
	return units, residual, err
}
