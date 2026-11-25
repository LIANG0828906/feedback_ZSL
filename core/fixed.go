package dialoguecal

import (
	"math"
	"math/bits"
)

// All shipping kernels use signed 32-bit containers. Q15 carries linear
// amplitudes/energies and Q8 carries decibel values. Intermediate products
// use int64 and are checked before narrowing.
const (
	FixedWidthBits = 32
	Q15Shift       = 15
	Q15Scale       = int64(1) << Q15Shift
	Q8Shift        = 8
	Q8Scale        = int64(1) << Q8Shift

	Int32Max = int64(math.MaxInt32)
	Int32Min = int64(math.MinInt32)

	MinDBQ8 Q8 = -96 * 256

	ln2Q31          int64 = 1488522236
	ln10Over20Q23   int64 = 965774
	lnToDBFactorQ16 int64 = 569238 // round((20*256/ln(10))*256)
)

// Q15 is a signed linear fixed-point value with 15 fractional bits.
type Q15 int32

// Q8 is a signed decibel fixed-point value with 8 fractional bits.
type Q8 int32

// Saturation reports a narrowed fixed-point operation.
type Saturation struct {
	Operation string `json:"operation"`
	Min       bool   `json:"min,omitempty"`
	Max       bool   `json:"max,omitempty"`
}

func saturateInt32(value int64, operation string) (int32, *Saturation) {
	if value > Int32Max {
		return math.MaxInt32, &Saturation{Operation: operation, Max: true}
	}
	if value < Int32Min {
		return math.MinInt32, &Saturation{Operation: operation, Min: true}
	}
	return int32(value), nil
}

func strictInt32(value int64, path string) (int32, error) {
	if value > Int32Max || value < Int32Min {
		return 0, newError(CodeFixedOverflow, path, "value %d exceeds int32 fixed-point container", value)
	}
	return int32(value), nil
}

// MulQ15 multiplies two Q15 values with nearest-even-ish integer rounding and
// explicit saturation. The returned marker is non-nil when narrowing saturated.
func MulQ15(a, b Q15) (Q15, *Saturation) {
	product := int64(a) * int64(b)
	if product >= 0 {
		product += Q15Scale / 2
	} else {
		product -= Q15Scale / 2
	}
	v, sat := saturateInt32(product>>Q15Shift, "mul_q15")
	if sat != nil {
		return Q15(v), sat
	}
	return Q15(v), nil
}

// CheckedMulQ15 is the strict variant used by critical-case verification.
func CheckedMulQ15(a, b Q15, path string) (Q15, error) {
	product := int64(a) * int64(b)
	v, err := strictInt32(product>>Q15Shift, path)
	if err != nil {
		return 0, err
	}
	return Q15(v), nil
}

func PermilleToQ15(permille int) Q15 {
	return Q15((int64(permille)*Q15Scale + 500) / 1000)
}

func Per10kToQ15(per10k int) Q15 {
	return Q15((int64(per10k)*Q15Scale + 5000) / 10000)
}

func Q15ToPermilleRound(v Q15) int {
	return int((int64(v)*1000 + Q15Scale/2) / Q15Scale)
}

func Q15ToPer10kRound(v Q15) int {
	return int((int64(v)*10000 + Q15Scale/2) / Q15Scale)
}

// mulQ31 multiplies two signed Q31 values. Inputs/intermediates stay within
// int64 for the reduced-range series used below.
func mulQ31(a, b int64) int64 {
	return (a * b) >> 31
}

// lnQ15Normalized returns ln(v/32768) in Q31. v must be positive.
func lnQ15Normalized(v Q15) int64 {
	u := uint32(v)
	exponent := bits.Len32(u) - 1
	one := uint64(1) << 31
	mantissa := uint64(u) << uint(31-exponent)
	numerator := (mantissa - one) << 31
	y := int64(numerator / (mantissa + one))

	y2 := mulQ31(y, y)
	power := y
	sum := power
	for n := 3; n <= 11; n += 2 {
		power = mulQ31(power, y2)
		sum += power / int64(n)
	}
	lnMantissa := 2 * sum
	return lnMantissa + int64(exponent-15)*ln2Q31
}

// LinearToDBQ8 converts a Q15 linear amplitude to Q8 decibels. Zero and
// negative inputs use the declared -96 dB floor.
func LinearToDBQ8(v Q15) Q8 {
	if v <= 0 {
		return MinDBQ8
	}
	lnNorm := lnQ15Normalized(v)
	// lnNorm is Q31; lnToDBFactorQ16 is round(20*256/ln10*256).
	db := (lnNorm * lnToDBFactorQ16) >> 39
	if db > Int32Max {
		return Q8(math.MaxInt32)
	}
	if db < Int32Min {
		return Q8(math.MinInt32)
	}
	return Q8(db)
}

func floorDiv(a, b int64) (quotient, remainder int64) {
	quotient = a / b
	remainder = a % b
	if remainder < 0 {
		quotient--
		remainder += b
	}
	return quotient, remainder
}

// expQ31 returns exp(x) in Q31-like precision. The returned value is int64
// because positive decibel gains can exceed signed Q31's 1.999 maximum.
func expQ31(x int64) int64 {
	k, reduced := floorDiv(x, ln2Q31)
	one := int64(1) << 31
	term := one
	sum := one
	for n := int64(1); n <= 12; n++ {
		term = mulQ31(term, reduced) / n
		sum += term
	}

	if k >= 0 {
		if k >= 31 {
			return math.MaxInt64
		}
		shift := uint(k)
		return sum << shift
	}
	shift := uint(-k)
	if -k >= 62 {
		return 0
	}
	return sum >> shift
}

// DBQ8ToLinearQ15 converts Q8 dB to saturated Q15 linear amplitude.
func DBQ8ToLinearQ15(db Q8) (Q15, *Saturation) {
	lnAmplitudeQ31 := int64(db) * ln10Over20Q23
	expanded := expQ31(lnAmplitudeQ31)
	// expQ31 is Q31; Q15 needs 16 fractional bits fewer.
	const shift = 16
	rounded := expanded >> shift
	if expanded&(1<<(shift-1)) != 0 {
		rounded++
	}
	v, sat := saturateInt32(rounded, "db_to_linear_q15")
	if sat != nil {
		return Q15(v), sat
	}
	return Q15(v), nil
}

// DBMicroToPermille converts schema micro-decibels (dB*100000) to rounded
// linear permille. It keeps the loader conversion in integer arithmetic.
func DBMicroToPermille(dbMicro int64) int {
	// ln amplitude Q31 = dbMicro * (ln10/20) * 2^31 / 1e5.
	const factorQ100 = int64(247263) // round(ln(10)/20 * 2^31 / 100000 * 100)
	lnAmplitudeQ31 := dbMicro * factorQ100 / 100
	linearQ31 := expQ31(lnAmplitudeQ31)
	permille := (linearQ31*1000 + (1 << 30)) >> 31
	if permille < 0 {
		permille = 0
	}
	if permille > math.MaxInt32 {
		permille = math.MaxInt32
	}
	return int(permille)
}

// Q15EnergyDifference is exposed for truth tolerance reports.
func absInt64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}
