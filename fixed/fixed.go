// Package fixed implements the only arithmetic used across the domain:
// checked integer rescaling, sample/time conversions, and fixed-point
// decibel/linear-amplitude conversion. No floating point is used.
package fixed

import (
	"fmt"
	"math/big"

	"dialogueaudio/audioerr"
)

const (
	// Q16 is the internal scale for linear gains/weights and decibels.
	Q16 int64 = 1 << 16
)

// Rounding declares how a non-zero remainder is handled.
type Rounding string

const (
	RoundTrunc   Rounding = "trunc_zero"
	RoundFloor   Rounding = "floor"
	RoundCeil    Rounding = "ceil"
	RoundNearest Rounding = "nearest_ties_away_from_zero"
)

// Trace is an auditable integer-conversion step.
type Trace struct {
	Operation    string   `json:"operation"`
	Field        string   `json:"field"`
	FromScale    string   `json:"from_scale,omitempty"`
	ToScale      string   `json:"to_scale,omitempty"`
	Input        int64    `json:"input"`
	Output       int64    `json:"output"`
	Numerator    string   `json:"numerator,omitempty"`
	Denominator  int64    `json:"denominator,omitempty"`
	Quotient     int64    `json:"quotient"`
	Remainder    int64    `json:"remainder"`
	Residual     int64    `json:"residual"`
	Rounding     Rounding `json:"rounding"`
	Truncated    bool     `json:"truncated"`
	TruncationAt string   `json:"truncation_at,omitempty"`
}

// CheckedAdd returns a+b or E_OVF.
func CheckedAdd(a, b int64, field string) (int64, error) {
	if (b > 0 && a > int64(^uint64(0)>>1)-b) || (b < 0 && a < -int64(^uint64(0)>>1)-1-b) {
		return 0, audioerr.Overflow(field, a, b)
	}
	return a + b, nil
}

// CheckedMul returns a*b or E_OVF.
func CheckedMul(a, b int64, field string) (int64, error) {
	if a == 0 || b == 0 {
		return 0, nil
	}
	product := new(big.Int).Mul(big.NewInt(a), big.NewInt(b))
	if !product.IsInt64() {
		return 0, audioerr.Overflow(field, a, b)
	}
	return product.Int64(), nil
}

// MulDiv computes a*b/c exactly before checking the int64 result.
func MulDiv(a, b, c int64, field string) (int64, error) {
	if c == 0 {
		return 0, audioerr.New(audioerr.CodeOverflow, field, "division by zero")
	}
	numerator := new(big.Int).Mul(big.NewInt(a), big.NewInt(b))
	quotient := new(big.Int).Quo(numerator, big.NewInt(c))
	if !quotient.IsInt64() {
		return 0, audioerr.Overflow(field, a, b)
	}
	return quotient.Int64(), nil
}

// Rescale converts value from one integer scale to another. The residual is
// (output*fromScale - value*toScale), i.e. the error left in denominator
// units. Every call returns a trace, including exact conversions.
func Rescale(value, fromScale, toScale int64, rounding Rounding, field string) (int64, Trace, error) {
	if fromScale <= 0 || toScale <= 0 {
		return 0, Trace{}, audioerr.New(audioerr.CodeOverflow, field, "scales must be positive")
	}
	numeratorInt := new(big.Int).Mul(big.NewInt(value), big.NewInt(toScale))
	denominator := new(big.Int).SetInt64(fromScale)
	truncatedQuotient := new(big.Int)
	remainder := new(big.Int)
	truncatedQuotient.QuoRem(numeratorInt, denominator, remainder)
	quotient := new(big.Int).Set(truncatedQuotient)
	remSign := remainder.Sign()
	residual := int64(0)
	truncated := remSign != 0
	if remSign != 0 {
		adjust := false
		switch rounding {
		case RoundFloor:
			adjust = value < 0
		case RoundCeil:
			adjust = value > 0
		case RoundNearest:
			twiceRem := new(big.Int).Abs(new(big.Int).Lsh(new(big.Int).Set(remainder), 1))
			adjust = twiceRem.Cmp(denominator) >= 0
		case RoundTrunc, "":
			adjust = false
		default:
			return 0, Trace{}, audioerr.New(audioerr.CodeSchema, field, "unknown rounding direction")
		}
		if adjust {
			if value < 0 {
				quotient.Sub(quotient, big.NewInt(1))
			} else {
				quotient.Add(quotient, big.NewInt(1))
			}
		}
	}
	if !quotient.IsInt64() {
		return 0, Trace{}, audioerr.Overflow(field, value, toScale)
	}
	numeratorForResidual := new(big.Int).Mul(quotient, denominator)
	numeratorForResidual.Sub(numeratorForResidual, numeratorInt)
	if !numeratorForResidual.IsInt64() {
		// The residual is diagnostic; clamp only when it cannot even fit. The
		// conversion result itself remains valid.
		residual = 0
	} else {
		residual = numeratorForResidual.Int64()
	}
	out := quotient.Int64()
	tr := Trace{
		Operation:    "rescale",
		Field:        field,
		FromScale:    fmt.Sprintf("1/%d", fromScale),
		ToScale:      fmt.Sprintf("1/%d", toScale),
		Input:        value,
		Output:       out,
		Numerator:    numeratorInt.String(),
		Denominator:  fromScale,
		Quotient:     truncatedQuotient.Int64(),
		Remainder:    remainder.Int64(),
		Residual:     residual,
		Rounding:     rounding,
		Truncated:    truncated,
		TruncationAt: "quotient_remainder",
	}
	return out, tr, nil
}

// SamplesToMillis converts samples using the given rounding direction.
func SamplesToMillis(samples int64, sampleRate int, rounding Rounding, field string) (int64, Trace, error) {
	if sampleRate <= 0 {
		return 0, Trace{}, audioerr.New(audioerr.CodeSchema, field, "sample rate must be positive")
	}
	return Rescale(samples, int64(sampleRate), 1000, rounding, field)
}

// MillisToSamples converts milliseconds to samples.
func MillisToSamples(ms int64, sampleRate int, rounding Rounding, field string) (int64, Trace, error) {
	if sampleRate <= 0 {
		return 0, Trace{}, audioerr.New(audioerr.CodeSchema, field, "sample rate must be positive")
	}
	return Rescale(ms, 1000, int64(sampleRate), rounding, field)
}

// SamplesToMicros converts samples to microseconds.
func SamplesToMicros(samples int64, sampleRate int, rounding Rounding, field string) (int64, Trace, error) {
	if sampleRate <= 0 {
		return 0, Trace{}, audioerr.New(audioerr.CodeSchema, field, "sample rate must be positive")
	}
	return Rescale(samples, int64(sampleRate), 1_000_000, rounding, field)
}

// MicrosToSamples converts microseconds to samples.
func MicrosToSamples(us int64, sampleRate int, rounding Rounding, field string) (int64, Trace, error) {
	if sampleRate <= 0 {
		return 0, Trace{}, audioerr.New(audioerr.CodeSchema, field, "sample rate must be positive")
	}
	return Rescale(us, 1_000_000, int64(sampleRate), rounding, field)
}

// Log2Q16 computes floor(log2(x)*Q16) using integer squaring. x must be > 0.
func Log2Q16(x int64, field string) (int64, error) {
	if x <= 0 {
		return 0, audioerr.New(audioerr.CodeSchema, field, "logarithm input must be positive")
	}
	exponent := int64(0)
	normalized := x
	for normalized >= 2*Q16 {
		normalized >>= 1
		exponent++
	}
	for normalized < Q16 {
		normalized <<= 1
		exponent--
	}
	// We intentionally normalize to [Q16, 2*Q16) and then use repeated
	// squaring to recover 16 fractional bits.
	// normalized is a Q16 mantissa. Recover the missing Q16 factor of the
	// integer argument: x = (normalized/Q16) * 2^(exponent+16).
	result := (exponent + 16) * Q16
	for bit := int64(Q16 >> 1); bit > 0; bit >>= 1 {
		normalized = (normalized * normalized) / Q16
		if normalized >= 2*Q16 {
			normalized >>= 1
			result += bit
		}
	}
	return result, nil
}

const (
	log10Of2Q16  int64 = 19728 // 0.30102999566 * 65536
	log2Of10Q16  int64 = 217706
	ln2ApproxQ16 int64 = 45426
)

// EnergyToDBQ16 converts a fixed-point mean-square energy to 10*log10(energy)
// in Q16 decibels. The result is absolute dBFS-style loudness.
func EnergyToDBQ16(energy int64, field string) (int64, Trace, error) {
	if energy <= 0 {
		return 0, Trace{}, audioerr.New(audioerr.CodeSchema, field, "energy must be positive")
	}
	log2, err := Log2Q16(energy, field)
	if err != nil {
		return 0, Trace{}, err
	}
	log10, err := MulDiv(log2, log10Of2Q16, Q16, field)
	if err != nil {
		return 0, Trace{}, err
	}
	db, err := CheckedMul(log10, 10, field)
	if err != nil {
		return 0, Trace{}, err
	}
	return db, Trace{
		Operation:    "energy_to_db_q16",
		Field:        field,
		Input:        energy,
		Output:       db,
		Quotient:     db,
		Remainder:    0,
		Residual:     0,
		Rounding:     RoundTrunc,
		Truncated:    true,
		TruncationAt: "log2_16bit_fraction_then_log10",
	}, nil
}

// GainToDBQ16 converts a linear amplitude Q16 value to 20*log10(gain) in Q16.
func GainToDBQ16(gainQ16 int64, field string) (int64, Trace, error) {
	if gainQ16 <= 0 {
		return 0, Trace{}, audioerr.New(audioerr.CodeSchema, field, "gain must be positive")
	}
	log2, err := Log2Q16(gainQ16, field)
	if err != nil {
		return 0, Trace{}, err
	}
	log2 -= 16 * Q16 // normalize 1.0 == Q16 to 0 dB
	log10, err := MulDiv(log2, log10Of2Q16, Q16, field)
	if err != nil {
		return 0, Trace{}, err
	}
	db, err := CheckedMul(log10, 20, field)
	if err != nil {
		return 0, Trace{}, err
	}
	return db, Trace{
		Operation:    "linear_gain_to_db_q16",
		Field:        field,
		Input:        gainQ16,
		Output:       db,
		Quotient:     db,
		Remainder:    0,
		Residual:     0,
		Rounding:     RoundTrunc,
		Truncated:    true,
		TruncationAt: "log2_16bit_fraction_then_log10",
	}, nil
}

var exp2Factors = [...]int64{
	92681, // 2^(1/2)
	77935, // 2^(1/4)
	71467,
	68437,
	66970,
	66249,
	65891,
	65713,
	65624,
	65579,
	65557,
	65546,
	65540,
	65537,
	65536,
	65536,
}

// Exp2Q16 computes 2^(exponentQ16/65536)*65536 with checked arithmetic.
func Exp2Q16(exponentQ16 int64, field string) (int64, Trace, error) {
	integerPart := exponentQ16 / Q16
	fraction := exponentQ16 % Q16
	if fraction < 0 {
		fraction += Q16
		integerPart--
	}
	if integerPart > 30 || integerPart < -30 {
		return 0, Trace{}, audioerr.Overflow(field, exponentQ16, Q16)
	}
	result := Q16
	var err error
	for i, factor := range exp2Factors {
		if fraction&(1<<uint(15-i)) != 0 {
			result, err = MulDiv(result, factor, Q16, field)
			if err != nil {
				return 0, Trace{}, err
			}
		}
	}
	if integerPart >= 0 {
		result, err = CheckedMul(result, int64(1)<<uint(integerPart), field)
	} else {
		divisor := int64(1) << uint(-integerPart)
		result /= divisor
	}
	if err != nil {
		return 0, Trace{}, err
	}
	return result, Trace{
		Operation:    "exp2_q16",
		Field:        field,
		Input:        exponentQ16,
		Output:       result,
		Quotient:     result,
		Remainder:    fraction,
		Residual:     0,
		Rounding:     RoundTrunc,
		Truncated:    true,
		TruncationAt: "16_factor_fraction",
	}, nil
}

// DBToGainQ16 converts a dB Q16 adjustment to a linear amplitude Q16 gain.
func DBToGainQ16(dbQ16 int64, field string) (int64, Trace, error) {
	// exponent = db * log2(10) / 20
	exponent, err := MulDiv(dbQ16, log2Of10Q16, 20*Q16, field)
	if err != nil {
		return 0, Trace{}, err
	}
	gain, trace, err := Exp2Q16(exponent, field)
	if err != nil {
		return 0, Trace{}, err
	}
	trace.Operation = "db_q16_to_linear_gain"
	return gain, trace, nil
}

// ApplyGainToEnergy applies a Q16 linear gain to a Q16 energy and returns the
// resulting energy. Intermediate products are checked.
func ApplyGainToEnergy(energy, gainQ16 int64, field string) (int64, error) {
	scaled, err := MulDiv(energy, gainQ16, Q16, field)
	if err != nil {
		return 0, err
	}
	return MulDiv(scaled, gainQ16, Q16, field)
}
