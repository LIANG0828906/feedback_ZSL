// Package fixedint contains checked, fixed-width integer time arithmetic.
//
// The diagnostics model uses 64-bit signed millisecond timestamps. Seconds are
// converted with an explicit 1000x scale. Arithmetic never relies on wall time
// for cache expiry or upstream timeout decisions.
package fixedint

import (
	"math"

	"dnslab/codes"
)

const (
	TimeBits        = 64
	SecondScaleMS   = int64(1000)
	MaxTTLSeconds   = int64(math.MaxInt32)
	MaxTTLMS        = int64(24 * 60 * 60 * 1000) // workbench declaration: 24h
	MaxTimeoutMS    = int64(30_000)
	MinWorkerBudget = 1
	MaxWorkerBudget = 64
	MinHopBudget    = 1
	MaxHopBudget    = 16
	SaturatedMaxMS  = int64(math.MaxInt64)
	SaturatedMinMS  = int64(math.MinInt64)
)

// Result records both the computed value and whether saturation occurred.
type Result struct {
	Value     int64 `json:"value"`
	Saturated bool  `json:"saturated"`
}

// Mul64 performs checked signed multiplication.
func Mul64(a, b int64) Result {
	if a == 0 || b == 0 {
		return Result{Value: 0}
	}
	if a == math.MinInt64 && b == -1 || b == math.MinInt64 && a == -1 {
		return Result{Value: math.MaxInt64, Saturated: true}
	}
	q := a * b
	if q/a != b {
		if (a < 0) == (b < 0) {
			return Result{Value: math.MaxInt64, Saturated: true}
		}
		return Result{Value: math.MinInt64, Saturated: true}
	}
	return Result{Value: q}
}

// Add64 performs checked signed addition.
func Add64(a, b int64) Result {
	if b > 0 && a > math.MaxInt64-b {
		return Result{Value: math.MaxInt64, Saturated: true}
	}
	if b < 0 && a < math.MinInt64-b {
		return Result{Value: math.MinInt64, Saturated: true}
	}
	return Result{Value: a + b}
}

// SecondsToMS converts seconds to fixed-point milliseconds.
func SecondsToMS(seconds int64) (int64, bool) {
	r := Mul64(seconds, SecondScaleMS)
	return r.Value, r.Saturated
}

// RemainingToAbsolute adds reference time and remaining TTL with saturation.
func RemainingToAbsolute(nowMS, remainingMS int64) (int64, bool) {
	r := Add64(nowMS, remainingMS)
	return r.Value, r.Saturated
}

// Remaining computes expiresAtMS-nowMS and saturates negative/positive edge
// values. Expired entries return zero/negative remaining without saturation.
func Remaining(expiresAtMS, nowMS int64) Result {
	if expiresAtMS > 0 && nowMS < 0 && expiresAtMS > math.MaxInt64+nowMS {
		return Result{Value: math.MaxInt64, Saturated: true}
	}
	if expiresAtMS < 0 && nowMS > 0 && expiresAtMS < math.MinInt64+nowMS {
		return Result{Value: math.MinInt64, Saturated: true}
	}
	return Result{Value: expiresAtMS - nowMS}
}

// ValidateTTLMS enforces the declared TTL domain.
func ValidateTTLMS(ttlMS int64) error {
	if ttlMS < 0 {
		return codes.New(codes.ETTLRange, "ttl must be non-negative").
			WithContext("value_ms", ttlMS).
			WithContext("min_ms", int64(0))
	}
	if ttlMS > MaxTTLMS {
		return codes.New(codes.ETTLRange, "ttl exceeds declared fixed-width domain").
			WithContext("value_ms", ttlMS).
			WithContext("max_ms", MaxTTLMS).
			WithContext("bits", TimeBits)
	}
	return nil
}

// ValidateTimeoutMS enforces the declared timeout domain.
func ValidateTimeoutMS(timeoutMS int64) error {
	if timeoutMS <= 0 {
		return codes.New(codes.ETimeoutRange, "timeout must be positive").
			WithContext("value_ms", timeoutMS).
			WithContext("min_ms", int64(1))
	}
	if timeoutMS > MaxTimeoutMS {
		return codes.New(codes.ETimeoutRange, "timeout exceeds declared domain").
			WithContext("value_ms", timeoutMS).
			WithContext("max_ms", MaxTimeoutMS)
	}
	return nil
}

// ValidateWorkerBudget validates the concurrency budget.
func ValidateWorkerBudget(n int) error {
	if n < MinWorkerBudget || n > MaxWorkerBudget {
		return codes.New(codes.EBudgetRange, "worker budget out of range").
			WithContext("value", n).
			WithContext("min", MinWorkerBudget).
			WithContext("max", MaxWorkerBudget)
	}
	return nil
}

// ValidateHopBudget validates the per-query forwarding hop budget.
func ValidateHopBudget(n int) error {
	if n < MinHopBudget || n > MaxHopBudget {
		return codes.New(codes.EHopsRange, "hop budget out of range").
			WithContext("value", n).
			WithContext("min", MinHopBudget).
			WithContext("max", MaxHopBudget)
	}
	return nil
}
