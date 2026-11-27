package stage

import (
	"fmt"
	"math/big"
	"strconv"
)

// ScaleInt performs value*numerator/denominator without floating point. The
// returned remainder is the signed remainder of the scaled numerator. Rounding
// is explicit so bounds and residual accounting never depend on host defaults.
func ScaleInt(value, numerator, denominator int64, rounding Rounding, field string) (int64, int64, error) {
	if denominator == 0 {
		return 0, 0, &Error{Code: CodeValue, Message: "zero denominator", Field: field, FieldName: field}
	}
	n := new(big.Int).Mul(big.NewInt(value), big.NewInt(numerator))
	d := big.NewInt(denominator)
	q, r := new(big.Int), new(big.Int)
	q.QuoRem(n, d, r)

	switch rounding {
	case RoundFloor:
		if r.Sign() != 0 && r.Sign() != d.Sign() {
			q.Sub(q, big.NewInt(1))
		}
	case RoundCeil:
		if r.Sign() != 0 && r.Sign() == d.Sign() {
			q.Add(q, big.NewInt(1))
		}
	case RoundNearest:
		twiceAbsR := new(big.Int).Abs(r)
		twiceAbsR.Lsh(twiceAbsR, 1)
		absD := new(big.Int).Abs(d)
		if twiceAbsR.Cmp(absD) >= 0 {
			if r.Sign() == d.Sign() {
				q.Add(q, big.NewInt(1))
			} else {
				q.Sub(q, big.NewInt(1))
			}
		}
	case RoundTowardZero, RoundExact:
		// big.Int.QuoRem is already toward zero.
	default:
		return 0, 0, &Error{Code: CodeValue, Message: "unknown rounding mode", Field: field}
	}

	if !q.IsInt64() {
		return 0, 0, &Error{
			Code:      CodeOverflow,
			Message:   "integer conversion overflow",
			Field:     field,
			FieldName: field,
			Operands:  []int64{value, numerator},
			Rounding:  string(rounding),
			Details:   []string{fmt.Sprintf("denominator=%d", denominator)},
		}
	}
	if !r.IsInt64() {
		return 0, 0, &Error{Code: CodeOverflow, Message: "remainder overflow", Field: field, Operands: []int64{value, numerator}}
	}
	return q.Int64(), r.Int64(), nil
}

func MicroDegToDeciDeg(v int64) (int64, int64, error) {
	return ScaleInt(v, 1, 100_000, RoundNearest, "angle.deci_deg")
}

func DeciDegToMicroDeg(v int64) (int64, int64, error) {
	return ScaleInt(v, 100_000, 1, RoundExact, "angle.microdeg")
}

func MicroDegToMilliDeg(v int64) (int64, int64, error) {
	return ScaleInt(v, 1, 1_000, RoundNearest, "angle.milli_deg")
}

func MilliDegToMicroDeg(v int64) (int64, int64, error) {
	return ScaleInt(v, 1_000, 1, RoundExact, "angle.microdeg")
}

const (
	// 103993/33102 is a continued-fraction approximation of pi with sub-1e-9
	// relative error. Both conversion directions use the same declaration.
	piNumerator   int64 = 103993
	piDenominator int64 = 33102
	angleScale    int64 = 180 * piDenominator
)

// MicroDegToMicroRad uses the declared pi rational 355/113. Both directions use
// that same declaration, with nearest rounding and residual recording.
func MicroDegToMicroRad(v int64) (int64, int64, error) {
	return ScaleInt(v, piNumerator, angleScale, RoundNearest, "angle.microrad")
}

func MicroRadToMicroDeg(v int64) (int64, int64, error) {
	return ScaleInt(v, angleScale, piNumerator, RoundNearest, "angle.microdeg")
}

func TickToMS(tick, tickHz int64) (int64, int64, error) {
	return ScaleInt(tick, 1000, tickHz, RoundNearest, "time.ms")
}

func MSToTick(ms, tickHz int64) (int64, int64, error) {
	return ScaleInt(ms, tickHz, 1000, RoundNearest, "time.tick")
}

func MicrosecondToMS(v int64) (int64, int64, error) {
	return ScaleInt(v, 1, 1000, RoundNearest, "time.ms")
}

func MSToMicrosecond(v int64) (int64, int64, error) {
	return ScaleInt(v, 1000, 1, RoundExact, "time.us")
}

func Tick10KToMS(v int64) (int64, int64, error) {
	return ScaleInt(v, 1, 10, RoundNearest, "time.ms")
}

func MSToTick10K(v int64) (int64, int64, error) {
	return ScaleInt(v, 10, 1, RoundExact, "time.tick_10k")
}

func PermilleToPermyriad(v int64) (int64, int64, error) {
	return ScaleInt(v, 10, 1, RoundExact, "scale.permyriad")
}

func PermyriadToPermille(v int64) (int64, int64, error) {
	return ScaleInt(v, 1, 10, RoundNearest, "scale.permille")
}

func CCTPermilleToKelvin(v int64) (int64, int64, error) {
	q, r, err := ScaleInt(v, CCTMaxKelvin-CCTMinKelvin, 1000, RoundNearest, "color.cct_kelvin")
	if err != nil {
		return 0, 0, err
	}
	return CCTMinKelvin + q, r, nil
}

func CCTKelvinToPermille(v int64) (int64, int64, error) {
	return ScaleInt(v-CCTMinKelvin, 1000, CCTMaxKelvin-CCTMinKelvin, RoundNearest, "color.cct_permille")
}

func CCTPermyriadToKelvin(v int64) (int64, int64, error) {
	q, r, err := ScaleInt(v, CCTMaxKelvin-CCTMinKelvin, 10_000, RoundNearest, "color.cct_kelvin")
	if err != nil {
		return 0, 0, err
	}
	return CCTMinKelvin + q, r, nil
}

func CCTKelvinToPermyriad(v int64) (int64, int64, error) {
	return ScaleInt(v-CCTMinKelvin, 10_000, CCTMaxKelvin-CCTMinKelvin, RoundNearest, "color.cct_permyriad")
}

func ByteToPermyriad(v int64) (int64, int64, error) {
	return ScaleInt(v, 10_000, 255, RoundNearest, "color.rgb_permyriad")
}

func PermyriadToByte(v int64) (int64, int64, error) {
	return ScaleInt(v, 255, 10_000, RoundNearest, "color.rgb_byte")
}

func formatInt(v int64) string {
	return strconv.FormatInt(v, 10)
}

func formatVec(v Vec3) string {
	return fmt.Sprintf("[%d,%d,%d]", v[0], v[1], v[2])
}

func formatRGB(v [3]int64) string {
	return fmt.Sprintf("[%d,%d,%d]", v[0], v[1], v[2])
}
