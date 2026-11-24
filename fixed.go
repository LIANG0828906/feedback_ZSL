package balance

import (
	"math/big"
	"math/bits"
)

// Fixed is an integer with three decimal places. No float is used by battle
// settlement or by budget statistics.
type Fixed int64

const (
	FixedScale     Fixed = 1000
	BPSDenominator       = 10000
	PPMSize              = 1_000_000
	MaxFixed       Fixed = 1_000_000_000_000
	maxPercentBPS        = 5000
)

func (f Fixed) String() string {
	return big.NewInt(int64(f)).String()
}

func inFixedRange(v Fixed) bool {
	return v >= -MaxFixed && v <= MaxFixed
}

const (
	maxInt64Value = Fixed(1<<63 - 1)
	minInt64Value = Fixed(-1 << 63)
)

func addFixed(a, b Fixed) (Fixed, bool) {
	if b > 0 && a > maxInt64Value-b {
		return 0, false
	}
	if b < 0 && a < minInt64Value-b {
		return 0, false
	}
	return a + b, true
}

func subFixed(a, b Fixed) (Fixed, bool) {
	if b == Fixed(-1<<63) {
		return 0, false
	}
	return addFixed(a, -b)
}

// MulDiv performs (a*b)/den with integer truncation and an overflow check.
func MulDiv(a, b Fixed, den int64) (Fixed, bool) {
	if den == 0 {
		return 0, false
	}
	z := new(big.Int).Mul(big.NewInt(int64(a)), big.NewInt(int64(b)))
	z.Quo(z, big.NewInt(den))
	if !z.IsInt64() {
		return 0, false
	}
	return Fixed(z.Int64()), true
}

func applyBPS(base Fixed, bps int) (Fixed, bool) {
	return MulDiv(base, Fixed(bps), BPSDenominator)
}

func damageWithVariance(base Fixed, varianceBPS int) (Fixed, bool) {
	delta, ok := applyBPS(base, varianceBPS)
	if !ok {
		return 0, false
	}
	out, ok := addFixed(base, delta)
	if !ok || out < 0 {
		return 0, false
	}
	return out, true
}

func ratePPM(numerator, denominator int64) int64 {
	if denominator <= 0 {
		return 0
	}
	if numerator <= 0 {
		return 0
	}
	if numerator >= denominator {
		return PPMSize
	}
	return (numerator*PPMSize + denominator/2) / denominator
}

// ConfidenceInterval is a conservative Hoeffding interval for a Bernoulli
// mean. Its width depends only on the sample count, which makes monotonicity
// with respect to increased sampling explicit and testable.
type ConfidenceInterval struct {
	LowerPPM int64 `json:"lowerPpm"`
	UpperPPM int64 `json:"upperPpm"`
	WidthPPM int64 `json:"widthPpm"`
}

func confidenceInterval(wins, samples int64) ConfidenceInterval {
	if samples <= 0 {
		return ConfidenceInterval{LowerPPM: 0, UpperPPM: PPMSize, WidthPPM: PPMSize}
	}
	rate := ratePPM(wins, samples)
	half := hoeffdingHalfWidthPPM(samples)
	width := half * 2
	if width > PPMSize {
		width = PPMSize
	}
	lower := rate - half
	if lower < 0 {
		lower = 0
	}
	upper := rate + half
	if upper > PPMSize {
		upper = PPMSize
	}
	return ConfidenceInterval{LowerPPM: lower, UpperPPM: upper, WidthPPM: width}
}

func hoeffdingHalfWidthPPM(samples int64) int64 {
	if samples <= 0 {
		return PPMSize / 2
	}
	// ln(2/alpha) for alpha=0.05 is approximately 9.903, represented
	// as 9,903,000 ppm. Multiplying by 1e6 yields the ppm numerator.
	const lnTermMicro = int64(9_903_000)
	numerator := lnTermMicro * int64(PPMSize)
	denominator := 2 * samples
	value := uint64(numerator / denominator)
	half := int64(isqrt64(value))
	if half > PPMSize/2 {
		half = PPMSize / 2
	}
	return half
}

func isqrt64(n uint64) uint64 {
	if n == 0 {
		return 0
	}
	x := uint64(1) << ((bits.Len64(n) + 1) / 2)
	for {
		y := (x + n/x) / 2
		if y >= x {
			return x
		}
		x = y
	}
}

func safeMulInt64(a, b int64) (int64, bool) {
	if a < 0 || b < 0 {
		return 0, false
	}
	if a == 0 || b == 0 {
		return 0, true
	}
	if a > (1<<63-1)/b {
		return 0, false
	}
	return a * b, true
}
