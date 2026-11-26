package dialoguecal

import (
	"math/bits"
)

type RoundMode string

const (
	RoundFloor    RoundMode = "floor"
	RoundHalfAway RoundMode = "half-away-from-zero"
)

const (
	ScalePermille      int64 = 1000
	ScalePerMyriad     int64 = 10000
	LinearQ16FullScale int64 = 1 << 16
)

// divRound divides integers without ever converting to floating point.
func divRound(num, den int64, mode RoundMode) (q int64, rem int64) {
	if den == 0 {
		return 0, 0
	}
	switch mode {
	case RoundHalfAway:
		negative := (num < 0) != (den < 0)
		an, ad := num, den
		if an < 0 {
			an = -an
		}
		if ad < 0 {
			ad = -ad
		}
		q = an / ad
		rem = an % ad
		if rem*2 >= ad {
			q++
		}
		if negative {
			q = -q
			rem = -rem
		}
	default:
		q = num / den
		rem = num % den
		if rem != 0 && ((rem < 0) != (den < 0)) {
			q--
			rem += den
		}
	}
	return q, rem
}

func checkedMul(A, B int64, field string) (int64, error) {
	if A == 0 || B == 0 {
		return 0, nil
	}
	if A == -1 && B == -1<<63 {
		return 0, &Error{Code: CodeOverflow, Field: field, OperandA: A, OperandB: B, Message: "signed multiplication overflow"}
	}
	if B == -1 && A == -1<<63 {
		return 0, &Error{Code: CodeOverflow, Field: field, OperandA: A, OperandB: B, Message: "signed multiplication overflow"}
	}
	if A > 0 {
		if B > 0 && A > (1<<63-1)/B {
			return 0, &Error{Code: CodeOverflow, Field: field, OperandA: A, OperandB: B, Message: "signed multiplication overflow"}
		}
		if B < 0 && B < (-1<<63)/A {
			return 0, &Error{Code: CodeOverflow, Field: field, OperandA: A, OperandB: B, Message: "signed multiplication overflow"}
		}
	} else {
		if B > 0 && A < (-1<<63)/B {
			return 0, &Error{Code: CodeOverflow, Field: field, OperandA: A, OperandB: B, Message: "signed multiplication overflow"}
		}
		if B < 0 && A != 0 && B < (1<<63-1)/A {
			return 0, &Error{Code: CodeOverflow, Field: field, OperandA: A, OperandB: B, Message: "signed multiplication overflow"}
		}
	}
	return A * B, nil
}

func checkedAdd(A, B int64, field string) (int64, error) {
	if B > 0 && A > 1<<63-1-B {
		return 0, &Error{Code: CodeOverflow, Field: field, OperandA: A, OperandB: B, Message: "signed addition overflow"}
	}
	if B < 0 && A < -1<<63-B {
		return 0, &Error{Code: CodeOverflow, Field: field, OperandA: A, OperandB: B, Message: "signed addition overflow"}
	}
	return A + B, nil
}

// SamplesToMillis is an integer conversion with an explicit rounding policy.
// The returned trace includes the remainder and the exact truncation site.
func SamplesToMillis(samples, sampleRate int64, field string, mode RoundMode) (int64, ConversionTrace, error) {
	if sampleRate <= 0 {
		return 0, ConversionTrace{}, &Error{Code: CodeCal, Field: field, OperandA: samples, OperandB: sampleRate, Message: "sample rate must be positive"}
	}
	num, err := checkedMul(samples, 1000, field)
	if err != nil {
		return 0, ConversionTrace{}, err
	}
	q, rem := divRound(num, sampleRate, mode)
	tr := ConversionTrace{
		FieldPath: field, Stage: "samples_to_ms", FromUnit: "sample", ToUnit: "ms",
		Rounding: string(mode), OperandA: samples, OperandB: sampleRate, Result: q,
		Remainder: rem, TruncationSite: "samples*1000/sampleRate",
	}
	return q, tr, nil
}

func MillisToSamples(ms, sampleRate int64, field string, mode RoundMode) (int64, ConversionTrace, error) {
	if sampleRate <= 0 {
		return 0, ConversionTrace{}, &Error{Code: CodeCal, Field: field, OperandA: ms, OperandB: sampleRate, Message: "sample rate must be positive"}
	}
	num, err := checkedMul(ms, sampleRate, field)
	if err != nil {
		return 0, ConversionTrace{}, err
	}
	q, rem := divRound(num, 1000, mode)
	tr := ConversionTrace{
		FieldPath: field, Stage: "ms_to_samples", FromUnit: "ms", ToUnit: "sample",
		Rounding: string(mode), OperandA: ms, OperandB: sampleRate, Result: q,
		Remainder: rem, TruncationSite: "ms*sampleRate/1000",
	}
	return q, tr, nil
}

// ScaleToPerMyriad converts source fixed-point gain/loudness values to the
// internal 1/10000 scale. Permille values are exactly multiplied by ten;
// basis-point values are an identity conversion.
func ScaleToPerMyriad(value int64, sourceScale int64, field string, mode RoundMode) (int64, ConversionTrace, error) {
	if sourceScale <= 0 {
		return 0, ConversionTrace{}, &Error{Code: CodeCal, Field: field, OperandA: value, OperandB: sourceScale, Message: "source scale must be positive"}
	}
	num, err := checkedMul(value, ScalePerMyriad, field)
	if err != nil {
		return 0, ConversionTrace{}, err
	}
	q, rem := divRound(num, sourceScale, mode)
	tr := ConversionTrace{
		FieldPath: field, Stage: "scale_to_internal", FromUnit: "1/" + itoa(sourceScale), ToUnit: "1/10000",
		Rounding: string(mode), OperandA: value, OperandB: sourceScale, Result: q,
		Remainder: rem, TruncationSite: "value*10000/sourceScale",
	}
	return q, tr, nil
}

func PerMyriadToScale(value, targetScale int64, field string, mode RoundMode) (int64, ConversionTrace, error) {
	if targetScale <= 0 {
		return 0, ConversionTrace{}, &Error{Code: CodeCal, Field: field, OperandA: value, OperandB: targetScale, Message: "target scale must be positive"}
	}
	num, err := checkedMul(value, targetScale, field)
	if err != nil {
		return 0, ConversionTrace{}, err
	}
	q, rem := divRound(num, ScalePerMyriad, mode)
	tr := ConversionTrace{
		FieldPath: field, Stage: "scale_from_internal", FromUnit: "1/10000", ToUnit: "1/" + itoa(targetScale),
		Rounding: string(mode), OperandA: value, OperandB: targetScale, Result: q,
		Remainder: rem, TruncationSite: "value*targetScale/10000",
	}
	return q, tr, nil
}

// log2Q16 computes floor(log2(x) * 65536) using only integer arithmetic.
// The low bits discarded while normalizing the mantissa are returned as a
// residual for traceability.
func log2Q16(x int64) (int64, int64) {
	if x <= 0 {
		return 0, 0
	}
	exp := bits.Len64(uint64(x)) - 1
	var y int64
	var residual int64
	if exp > 15 {
		shift := uint(exp - 15)
		y = x >> shift
		if shift > 0 {
			residual = x & ((1 << shift) - 1)
		}
	} else {
		y = x << uint(15-exp)
	}
	frac := int64(0)
	for i := 0; i < 16; i++ {
		y = (y * y) >> 15
		if y >= 1<<16 {
			y >>= 1
			frac |= 1 << uint(15-i)
		}
	}
	return int64(exp)<<16 + frac, residual
}

// linearQ16ToDB10K converts a Q16 linear amplitude (1<<16 == full scale) to
// hundredths of a millibel (1/10000 dB).
func linearQ16ToDB10K(linearQ16 int64, field string, mode RoundMode) (int64, ConversionTrace, error) {
	if linearQ16 <= 0 {
		return 0, ConversionTrace{}, &Error{Code: CodeCal, Field: field, OperandA: linearQ16, Message: "linear amplitude must be positive"}
	}
	logQ16, residual := log2Q16(linearQ16)
	logQ16 -= 16 << 16
	num, err := checkedMul(logQ16, 60206, field)
	if err != nil {
		return 0, ConversionTrace{}, err
	}
	db, rem := divRound(num, 65536, mode)
	tr := ConversionTrace{
		FieldPath: field, Stage: "linear_to_db", FromUnit: "linear.q16", ToUnit: "db.1/10000",
		Rounding: string(mode), OperandA: linearQ16, OperandB: 1 << 16, Result: db,
		Remainder: rem, Residual: residual, TruncationSite: "log2(mantissa)>>15",
	}
	return db, tr, nil
}

// LinearToDB converts a Q16 linear amplitude to the internal 1/10000 dB scale.
func LinearToDB(linearQ16 int64, field string, mode RoundMode) (int64, ConversionTrace, error) {
	return linearQ16ToDB10K(linearQ16, field, mode)
}

var exp2FracQ30 = [32]int64{
	1518500250, 1276901417, 1170923762, 1121280436,
	1097253708, 1085434106, 1079572136, 1076653033,
	1075196443, 1074468888, 1074105294, 1073923544,
	1073832680, 1073787251, 1073764537, 1073753181,
	1073747502, 1073744663, 1073743244, 1073742534,
	1073742179, 1073742001, 1073741913, 1073741868,
	1073741846, 1073741835, 1073741830, 1073741827,
	1073741825, 1073741825, 1073741824, 1073741824,
}

func exp2Q16(logQ16 int64, mode RoundMode) (int64, int64) {
	expQ, rem := divRound(logQ16, 65536, RoundFloor)
	frac := logQ16 - expQ*65536
	result := int64(1 << 30)
	for i := 0; i < 16; i++ {
		if frac&(1<<uint(15-i)) != 0 {
			result = (result * exp2FracQ30[i]) >> 30
		}
	}
	numerator := result
	denominator := int64(1 << 30)
	if expQ >= 0 {
		if expQ >= 32 {
			return 0, rem
		}
		numerator <<= uint(expQ)
	} else {
		if -expQ >= 62 {
			return 0, rem
		}
		denominator <<= uint(-expQ)
	}
	linearQ16, _ := divRound(numerator, denominator, mode)
	return linearQ16, rem
}

func db10KToLinearQ16(db int64, field string, mode RoundMode) (int64, ConversionTrace, error) {
	num, err := checkedMul(db, 65536, field)
	if err != nil {
		return 0, ConversionTrace{}, err
	}
	logQ16, rem := divRound(num, 60206, mode)
	logQ16 += 16 << 16
	linear, expRem := exp2Q16(logQ16, mode)
	if linear <= 0 {
		return 0, ConversionTrace{}, &Error{Code: CodeOverflow, Field: field, OperandA: db, OperandB: 60206, Message: "dB to linear conversion overflow"}
	}
	tr := ConversionTrace{
		FieldPath: field, Stage: "db_to_linear", FromUnit: "db.1/10000", ToUnit: "linear.q16",
		Rounding: string(mode), OperandA: db, OperandB: 60206, Result: linear,
		Remainder: rem, Residual: expRem, TruncationSite: "db*65536/60206",
	}
	return linear, tr, nil
}

// DBToLinear converts internal 1/10000 dB back to a Q16 linear amplitude.
func DBToLinear(db10k int64, field string, mode RoundMode) (int64, ConversionTrace, error) {
	return db10KToLinearQ16(db10k, field, mode)
}

type LinearRoundTrip struct {
	DB       int64
	Linear   int64
	Residual int64
	Up       ConversionTrace
	Down     ConversionTrace
}

func LinearDBRoundTrip(linearQ16 int64, field string, mode RoundMode) (LinearRoundTrip, error) {
	db, up, err := linearQ16ToDB10K(linearQ16, field, mode)
	if err != nil {
		return LinearRoundTrip{}, err
	}
	back, down, err := db10KToLinearQ16(db, field, mode)
	if err != nil {
		return LinearRoundTrip{}, err
	}
	return LinearRoundTrip{DB: db, Linear: back, Residual: back - linearQ16, Up: up, Down: down}, nil
}

func itoa(v int64) string {
	if v == 0 {
		return "0"
	}
	negative := v < 0
	if negative {
		v = -v
	}
	var buf [24]byte
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	if negative {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
