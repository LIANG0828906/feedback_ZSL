package loudness

import (
	"fmt"
	"math/bits"
)

// MulDiv computes a*b/c with an explicit rounding mode. It uses 128-bit
// intermediate arithmetic and reports overflow as E_OVF with both operands.
func MulDiv(a, b, c int64, mode Rounding) (int64, Truncation, error) {
	return MulDivAt("", a, b, c, mode)
}

// MulDivAt is MulDiv with a diagnostic path.
func MulDivAt(path string, a, b, c int64, mode Rounding) (int64, Truncation, error) {
	tr := Truncation{Path: path, Operation: "muldiv", Numerator: a, Denominator: c, Rounding: mode}
	if c == 0 {
		return 0, tr, &DomainError{Code: EOvF, Path: path, Field: "denominator", Operands: []int64{a, b}, Detail: "division by zero"}
	}
	if !mode.Valid() {
		return 0, tr, &DomainError{Code: EPara, Path: path, Field: "rounding", Detail: "unknown rounding mode"}
	}

	negative := (a < 0) != (b < 0)
	if c < 0 {
		negative = !negative
	}
	ua := abs64(a)
	ub := abs64(b)
	uc := abs64(c)
	hi, lo := bits.Mul64(ua, ub)
	if hi >= uc {
		return 0, tr, &DomainError{Code: EOvF, Path: path, Field: "muldiv", Operands: []int64{a, b}, Detail: "product/divisor exceeds int64"}
	}
	q, r := bits.Div64(hi, lo, uc)
	rounded := false
	if mode == RoundHalfUp && r != 0 && r >= (uc-r) {
		if q == uint64(^uint64(0)>>1) && negative {
			// -2^63 is representable.
		} else if q == uint64(^uint64(0)>>1) {
			return 0, tr, &DomainError{Code: EOvF, Path: path, Field: "rounded quotient", Operands: []int64{a, b}}
		}
		q++
		rounded = true
	}
	if q > uint64(^uint64(0)>>1) {
		if !(negative && q == uint64(1)<<63) {
			return 0, tr, &DomainError{Code: EOvF, Path: path, Field: "quotient", Operands: []int64{a, b}}
		}
	}
	var result int64
	if negative {
		if q == uint64(1)<<63 {
			result = -1 << 63
		} else {
			result = -int64(q)
		}
	} else {
		result = int64(q)
	}
	signedRemainder := int64(r)
	if negative {
		signedRemainder = -signedRemainder
	}
	if rounded {
		if negative {
			signedRemainder += int64(uc)
		} else {
			signedRemainder -= int64(uc)
		}
	}
	tr.Quotient = result
	tr.Remainder = signedRemainder
	tr.Residual = signedRemainder
	return result, tr, nil
}

func abs64(v int64) uint64 {
	if v < 0 {
		return uint64(-(v + 1)) + 1
	}
	return uint64(v)
}

// ConvertScale converts an integer between declared fixed-point scales.
func ConvertScale(value, fromScale, toScale int64, path string, mode Rounding) (int64, Truncation, error) {
	if fromScale <= 0 || toScale <= 0 {
		return 0, Truncation{}, &DomainError{Code: EPara, Path: path, Field: "scale", Detail: "scales must be positive"}
	}
	return MulDivAt(path, value, toScale, fromScale, mode)
}

// SamplesToMillis converts an internal sample position to milliseconds.
func SamplesToMillis(samples, sampleRate int64, mode Rounding) (int64, Truncation, error) {
	if sampleRate <= 0 {
		return 0, Truncation{}, &DomainError{Code: EPara, Path: "sampleRate", LegalMin: 1, LegalMax: MaxSampleRate}
	}
	return MulDivAt("time.samples->millis", samples, 1000, sampleRate, mode)
}

// MillisToSamples converts milliseconds into internal sample positions.
func MillisToSamples(millis, sampleRate int64, mode Rounding) (int64, Truncation, error) {
	if sampleRate <= 0 {
		return 0, Truncation{}, &DomainError{Code: EPara, Path: "sampleRate", LegalMin: 1, LegalMax: MaxSampleRate}
	}
	return MulDivAt("time.millis->samples", millis, sampleRate, 1000, mode)
}

// DBPerOctaveQ16 is 20*log10(2) in Q16.
const DBPerOctaveQ16 int64 = 394566

// DBRoundTripToleranceQ8 is the declared worst-case round-trip tolerance for
// the Q16 linear amplitude / Q8 dB representation.
const DBRoundTripToleranceQ8 int64 = 12

// LinearToDBQ16 converts linear amplitude Q16 to loudness Q8 dB using integer
// log2 extraction and a Q16 octave constant.
func LinearToDBQ16(linearQ16 int64, path string) (int64, Truncation, error) {
	tr := Truncation{Path: path, Operation: "linear->db", Numerator: linearQ16, Denominator: 1, Rounding: RoundHalfUp}
	if linearQ16 <= 0 {
		return 0, tr, &DomainError{Code: EPara, Path: path, Field: "linearQ16", Operands: []int64{linearQ16}, Detail: "linear amplitude must be positive"}
	}
	log2Q16, err := log2Fixed(linearQ16)
	if err != nil {
		return 0, tr, err
	}
	// Full-scale linear Q16 is 1.0, i.e. 2^16 relative amplitude.
	log2Q16 -= 16 << 16
	dbQ16, _, err := MulDivAt(path+".log2->db", log2Q16, DBPerOctaveQ16, InternalLinearScale, RoundHalfUp)
	if err != nil {
		return 0, tr, err
	}
	dbQ8, divTrace, err := MulDivAt(path+".db-q16->q8", dbQ16, 1, InternalLoudnessScale, RoundHalfUp)
	if err != nil {
		return 0, tr, err
	}
	tr.Quotient = dbQ8
	tr.Remainder = divTrace.Remainder
	tr.Residual = divTrace.Residual
	return dbQ8, tr, nil
}

// DBQ8ToLinearQ16 converts loudness Q8 dB to linear amplitude Q16. The
// fractional octave is accumulated with a fixed set of integer sqrt factors.
func DBQ8ToLinearQ16(dbQ8 int64, path string) (int64, Truncation, error) {
	tr := Truncation{Path: path, Operation: "db->linear", Numerator: dbQ8, Denominator: 1, Rounding: RoundHalfUp}
	octavesQ16, _, err := MulDivAt(path+".db->log2", dbQ8, 1<<24, DBPerOctaveQ16, RoundDown)
	if err != nil {
		return 0, tr, err
	}
	whole := octavesQ16 / InternalLinearScale
	rem := octavesQ16 % InternalLinearScale
	if rem < 0 {
		whole--
		rem += InternalLinearScale
	}
	factorQ16 := exp2FractionQ16(uint32(rem))
	var result int64
	if whole >= 0 {
		if whole >= 47 {
			return 0, tr, &DomainError{Code: EOvF, Path: path, Field: "wholeOctaves", Operands: []int64{dbQ8, whole}}
		}
		result = factorQ16 << uint(whole)
	} else {
		shift := -whole
		if shift >= 63 {
			result = 0
		} else {
			result = factorQ16 >> uint(shift)
		}
	}
	tr.Quotient = result
	tr.Remainder = rem
	tr.Residual = rem
	return result, tr, nil
}

func log2Fixed(x int64) (int64, error) {
	if x <= 0 {
		return 0, &DomainError{Code: EPara, Field: "log2", Operands: []int64{x}, Detail: "argument must be positive"}
	}
	n := int64(bits.Len64(uint64(x)) - 1)
	m, _, err := MulDivAt("fixed.log2.normalize", x, InternalLinearScale, int64(1)<<uint(n), RoundDown)
	if err != nil {
		return 0, err
	}
	result := n << 16
	for bit := int64(1 << 15); bit > 0; bit >>= 1 {
		m = (m * m) >> 16
		if m >= 2*InternalLinearScale {
			m >>= 1
			result |= bit
		}
	}
	return result, nil
}

var exp2FactorsQ16 = [16]int64{
	92682, 77936, 71468, 68438, 66971, 66250, 65892, 65714,
	65625, 65580, 65558, 65547, 65542, 65539, 65537, 65537,
}

func exp2FractionQ16(frac uint32) int64 {
	result := InternalLinearScale
	for i, factor := range exp2FactorsQ16 {
		if frac&(1<<uint(15-i)) != 0 {
			result = (result * factor) >> 16
		}
	}
	return result
}

// DBQ8ToGainQ12 converts a dB change to a linear gain factor.
func DBQ8ToGainQ12(dbQ8 int64, path string) (int64, Truncation, error) {
	linear, tr, err := DBQ8ToLinearQ16(dbQ8, path)
	if err != nil {
		return 0, tr, err
	}
	gain, scaleTrace, err := MulDivAt(path+".linear-q16->gain-q12", linear, InternalGainScale, InternalLinearScale, RoundHalfUp)
	if err != nil {
		return 0, tr, err
	}
	tr.Quotient = gain
	tr.Remainder = scaleTrace.Remainder
	tr.Residual = scaleTrace.Residual
	return gain, tr, nil
}

// GainQ12ToDBQ8 converts a linear gain factor to its dB change.
func GainQ12ToDBQ8(gainQ12 int64, path string) (int64, Truncation, error) {
	linear, tr, err := MulDivAt(path+".gain-q12->linear-q16", gainQ12, InternalLinearScale, InternalGainScale, RoundHalfUp)
	if err != nil {
		return 0, tr, err
	}
	return LinearToDBQ16(linear, path+".linear->db")
}

// CheckedMul returns a*b or E_OVF.
func CheckedMul(a, b int64, path string) (int64, error) {
	hi, lo := bits.Mul64(abs64(a), abs64(b))
	if hi != 0 || lo > uint64(^uint64(0)>>1) {
		return 0, &DomainError{Code: EOvF, Path: path, Field: "multiplication", Operands: []int64{a, b}}
	}
	result := int64(lo)
	if (a < 0) != (b < 0) {
		result = -result
	}
	return result, nil
}

// CheckedAdd returns a+b or E_OVF.
func CheckedAdd(a, b int64, path string) (int64, error) {
	if (b > 0 && a > mathMaxInt64-b) || (b < 0 && a < mathMinInt64-b) {
		return 0, &DomainError{Code: EOvF, Path: path, Field: "addition", Operands: []int64{a, b}}
	}
	return a + b, nil
}

// CheckedSub returns a-b or E_OVF.
func CheckedSub(a, b int64, path string) (int64, error) {
	if (b < 0 && a > mathMaxInt64+b) || (b > 0 && a < mathMinInt64+b) {
		return 0, &DomainError{Code: EOvF, Path: path, Field: "subtraction", Operands: []int64{a, b}}
	}
	return a - b, nil
}

const (
	mathMaxInt64 = int64(^uint64(0) >> 1)
	mathMinInt64 = -mathMaxInt64 - 1
)

func fixedRatioQ16(value, scale int64, path string) (int64, error) {
	result, _, err := MulDivAt(path, value, InternalLinearScale, scale, RoundHalfUp)
	return result, err
}

func formatTrace(tr Truncation) string {
	return fmt.Sprintf("q=%d r=%d residual=%d", tr.Quotient, tr.Remainder, tr.Residual)
}
