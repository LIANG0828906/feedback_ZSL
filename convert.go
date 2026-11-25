package dialogue

import (
	"math"
	"math/bits"
)

// Fixed-point scales declared by the kernel. All acoustic and calibration
// arithmetic uses these integer units; floats are not used in the processing
// chain.
const (
	PermilleScale      int64 = 1_000
	BasisScale         int64 = 10_000
	LinearScale        int64 = 1_000_000
	EnergyScale        int64 = 1_000_000
	DBScale            int64 = 1_000 // milli-decibel
	MinLoudDBMilli     int64 = -120_000
	logQBits           uint  = 24
	logQ               int64 = 1 << logQBits
	ln2Q24             int64 = 11_629_080
	invLn2Q24          int64 = 24_204_406
	log2LinearScaleQ24 int64 = 334_396_231
	dbCoefficientNum   int64 = 60_205_999 // 6020.599913... * 10000
	dbCoefficientDen   int64 = 10_000
)

// RoundDir declares how an integer remainder is resolved.
type RoundDir string

const (
	RoundFloor   RoundDir = "floor"
	RoundCeil    RoundDir = "ceil"
	RoundNearest RoundDir = "nearest_ties_up"
	RoundTrunc   RoundDir = "truncate_toward_zero"
)

// Truncation records one integer division/quantization remainder. Position is
// the source coordinate (sample, frame or fixed-point index) when meaningful.
type Truncation struct {
	Field       string   `json:"field"`
	Operation   string   `json:"operation"`
	Numerator   int64    `json:"numerator"`
	Denominator int64    `json:"denominator"`
	Quotient    int64    `json:"quotient"`
	Remainder   int64    `json:"remainder"`
	Direction   RoundDir `json:"direction"`
	Position    int64    `json:"position,omitempty"`
}

func checkedMulPositive(a, b int64) (int64, bool) {
	if a < 0 || b < 0 {
		return 0, false
	}
	hi, lo := bits.Mul64(uint64(a), uint64(b))
	if hi != 0 || lo > math.MaxInt64 {
		return 0, false
	}
	return int64(lo), true
}

func resolveRounding(n, d int64, dir RoundDir, field, op string, position int64) (int64, Truncation) {
	q := n / d
	r := n % d
	rawQ, rawR := q, r
	switch dir {
	case RoundFloor:
		if r < 0 {
			q--
		}
	case RoundCeil:
		if r > 0 {
			q++
		} else if r < 0 {
			q--
		}
	case RoundNearest:
		ar := r
		if ar < 0 {
			ar = -ar
		}
		ad := d
		if ad < 0 {
			ad = -ad
		}
		if ar*2 >= ad {
			if r >= 0 {
				q++
			} else {
				q--
			}
		}
	case RoundTrunc:
		// Go quotient already truncates toward zero.
	}
	return q, Truncation{
		Field:       field,
		Operation:   op,
		Numerator:   n,
		Denominator: d,
		Quotient:    rawQ,
		Remainder:   rawR,
		Direction:   dir,
		Position:    position,
	}
}

// SamplesToMillis converts a sample coordinate using integer arithmetic.
func SamplesToMillis(samples, sampleRate int64, dir RoundDir, field string) (int64, Truncation, error) {
	if sampleRate <= 0 {
		return 0, Truncation{}, parameterError("", field, "sample rate must be positive")
	}
	n, ok := checkedMulPositive(samples, 1000)
	if !ok {
		return 0, Truncation{}, overflowError(field, samples, 1000)
	}
	v, tr := resolveRounding(n, sampleRate, dir, field, "samples*1000/sample_rate", samples)
	return v, tr, nil
}

// MillisToSamples converts milliseconds to samples using integer arithmetic.
func MillisToSamples(ms, sampleRate int64, dir RoundDir, field string) (int64, Truncation, error) {
	if sampleRate <= 0 {
		return 0, Truncation{}, parameterError("", field, "sample rate must be positive")
	}
	n, ok := checkedMulPositive(ms, sampleRate)
	if !ok {
		return 0, Truncation{}, overflowError(field, ms, sampleRate)
	}
	v, tr := resolveRounding(n, 1000, dir, field, "ms*sample_rate/1000", ms)
	return v, tr, nil
}

// PermilleToBasis converts x/1000 to x/10000.
func PermilleToBasis(pm int64, field string) (int64, error) {
	v, ok := checkedMulPositive(pm, 10)
	if !ok {
		return 0, overflowError(field, pm, 10)
	}
	return v, nil
}

// BasisToPermille reduces basis points to permille with declared rounding.
func BasisToPermille(bp int64, dir RoundDir, field string) (int64, Truncation, error) {
	if bp < 0 {
		return 0, Truncation{}, parameterError("", field, "basis value must be non-negative")
	}
	v, tr := resolveRounding(bp, 10, dir, field, "bp/10", 0)
	return v, tr, nil
}

// BasisToLinearMicro converts basis points (10000=unity) to linear micro units.
func BasisToLinearMicro(bp int64, field string) (int64, error) {
	v, ok := checkedMulPositive(bp, LinearScale/BasisScale)
	if !ok {
		return 0, overflowError(field, bp, LinearScale/BasisScale)
	}
	return v, nil
}

// LinearMicroToBasis converts linear micro units to basis points.
func LinearMicroToBasis(linearMicro int64, dir RoundDir, field string) (int64, Truncation, error) {
	if linearMicro < 0 {
		return 0, Truncation{}, parameterError("", field, "linear amplitude must be non-negative")
	}
	n, ok := checkedMulPositive(linearMicro, BasisScale)
	if !ok {
		return 0, Truncation{}, overflowError(field, linearMicro, BasisScale)
	}
	v, tr := resolveRounding(n, LinearScale, dir, field, "linear_micro*10000/1e6", 0)
	return v, tr, nil
}

func log2FixedQ24(x int64, field string, traces *[]Truncation) (int64, error) {
	if x <= 0 {
		return 0, parameterError("", field, "log2 input must be positive")
	}
	exp := bits.Len64(uint64(x)) - 1
	den := int64(1) << uint(exp)
	n, ok := checkedMulPositive(x, logQ)
	if !ok {
		return 0, overflowError(field, x, logQ)
	}
	m, tr := resolveRounding(n, den, RoundFloor, field+".log2_mantissa", "x*Q/2^e", int64(exp))
	*traces = append(*traces, tr)

	num, ok := checkedMulPositive(m-logQ, logQ)
	if !ok {
		return 0, overflowError(field+".z", m-logQ, logQ)
	}
	z, tr := resolveRounding(num, m+logQ, RoundFloor, field+".z", "(m-Q)*Q/(m+Q)", 0)
	*traces = append(*traces, tr)

	z2, tr := resolveRounding(z*z, logQ, RoundFloor, field+".z2", "z*z/Q", 0)
	*traces = append(*traces, tr)
	sum := z
	term := z
	for _, odd := range []int64{3, 5, 7, 9} {
		term, tr = resolveRounding(term*z2, logQ, RoundFloor, field+".atanh_term", "term*z2/Q", odd)
		*traces = append(*traces, tr)
		part, tr := resolveRounding(term, odd, RoundFloor, field+".atanh_div", "term/odd", odd)
		*traces = append(*traces, tr)
		sum += part
	}
	lnQ := 2 * sum
	num, ok = checkedMulPositive(lnQ, invLn2Q24)
	if !ok {
		return 0, overflowError(field+".log2_frac", lnQ, invLn2Q24)
	}
	frac, tr := resolveRounding(num, logQ, RoundNearest, field+".log2_frac", "ln*inv_ln2/Q", int64(exp))
	*traces = append(*traces, tr)
	return (int64(exp) << logQBits) + frac, nil
}

// LinearToDBMilli converts linear amplitude in parts per 1e6 to milli-dB.
func LinearToDBMilli(linearMicro int64, field string) (int64, []Truncation, error) {
	traces := make([]Truncation, 0, 12)
	if linearMicro == 0 {
		return MinLoudDBMilli, traces, nil
	}
	if linearMicro < 0 {
		linearMicro = -linearMicro
	}
	lx, err := log2FixedQ24(linearMicro, field, &traces)
	if err != nil {
		return 0, traces, err
	}
	delta := lx - log2LinearScaleQ24
	n, ok := checkedMulPositive(delta, dbCoefficientNum)
	if !ok {
		// delta may be negative; checkedMulPositive rejects it. Handle signed
		// through absolute magnitude and restore sign.
		if delta < 0 {
			absDelta := -delta
			n2, ok2 := checkedMulPositive(absDelta, dbCoefficientNum)
			if !ok2 {
				return 0, traces, overflowError(field, absDelta, dbCoefficientNum)
			}
			n = -n2
			ok = true
		}
		if !ok {
			return 0, traces, overflowError(field, delta, dbCoefficientNum)
		}
	}
	v, tr := resolveRounding(n, logQ*dbCoefficientDen, RoundNearest, field, "delta_log2*6020.5999", 0)
	traces = append(traces, tr)
	return v, traces, nil
}

func expFixedQ24(yQ int64, traces *[]Truncation, field string) (int64, error) {
	result := logQ
	term := logQ
	for n := int64(1); n <= 12; n++ {
		v, tr := resolveRounding(term*yQ, logQ, RoundFloor, field+".exp_mul", "term*y/Q", n)
		*traces = append(*traces, tr)
		term = v
		term, tr = resolveRounding(term, n, RoundFloor, field+".exp_div", "term/n", n)
		*traces = append(*traces, tr)
		result += term
		if term == 0 {
			break
		}
	}
	return result, nil
}

// DBMilliToLinear converts milli-dB back to linear micro units using integer
// fixed-point exponentiation.
func DBMilliToLinear(dbMilli int64, field string) (int64, []Truncation, error) {
	traces := make([]Truncation, 0, 16)
	if dbMilli <= MinLoudDBMilli {
		return 1, traces, nil
	}
	// deltaQ = db * Q * 10000 / 60205999
	n1, ok := checkedMulPositive(dbMilli, logQ*dbCoefficientDen)
	if !ok {
		if dbMilli < 0 {
			absDB := -dbMilli
			nAbs, okAbs := checkedMulPositive(absDB, logQ*dbCoefficientDen)
			if !okAbs {
				return 0, traces, overflowError(field, absDB, logQ*dbCoefficientDen)
			}
			n1 = -nAbs
		} else {
			return 0, traces, overflowError(field, dbMilli, logQ*dbCoefficientDen)
		}
	}
	deltaQ, tr := resolveRounding(n1, dbCoefficientNum, RoundNearest, field, "db*Q*10000/60205999", 0)
	traces = append(traces, tr)
	xQ := log2LinearScaleQ24 + deltaQ
	k, rem := resolveRounding(xQ, logQ, RoundFloor, field+".pow2_floor", "xQ/Q", 0)
	traces = append(traces, rem)
	fracQ := xQ - k*logQ
	yQ, tr2 := resolveRounding(fracQ*ln2Q24, logQ, RoundFloor, field+".ln2_frac", "frac*ln2/Q", k)
	traces = append(traces, tr2)
	powQ, err := expFixedQ24(yQ, &traces, field)
	if err != nil {
		return 0, traces, err
	}

	var linear int64
	if k >= 0 {
		shift := uint(k)
		if shift > 40 {
			return 0, traces, overflowError(field, k, 40)
		}
		scaled, okMul := checkedMulPositive(powQ, int64(1)<<shift)
		if !okMul {
			return 0, traces, overflowError(field+".pow2", powQ, int64(1)<<shift)
		}
		linear, tr = resolveRounding(scaled, logQ, RoundNearest, field, "powQ*2^k/Q", k)
		traces = append(traces, tr)
	} else {
		shift := uint(-k)
		den := logQ * (int64(1) << shift)
		linear, tr = resolveRounding(powQ, den, RoundNearest, field, "powQ/(Q*2^-k)", k)
		traces = append(traces, tr)
	}
	return linear, traces, nil
}

// IntSqrt returns floor(sqrt(n)) using integer arithmetic.
func IntSqrt(n int64) int64 {
	if n <= 0 {
		return 0
	}
	// math/bits gives a safe initial estimate; Newton iterations stay integer.
	x := int64(1) << ((uint(bits.Len64(uint64(n))) + 1) / 2)
	for {
		y := (x + n/x) / 2
		if y >= x {
			return x
		}
		x = y
	}
}

// SampleToAmpMicro maps a signed S16 sample to linear amplitude parts per 1e6
// of full scale. It truncates toward zero and records the remainder.
func SampleToAmpMicro(sample int16, field string) (int64, Truncation) {
	n := int64(sample) * LinearScale
	v, tr := resolveRounding(n, 32768, RoundTrunc, field, "sample*1e6/32768", int64(sample))
	return v, tr
}
