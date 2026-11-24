package fixed

import (
	"math"
	"math/big"
)

const Scale int64 = 1_000_000

func FromMilli(v int64) int64 { return v * 1000 }
func ToMilli(v int64) int64   { return v / 1000 }

// MulDiv returns floor(a*b/div) and the non-negative truncation remainder.
// All values are interpreted in the same fixed-point scale.
func MulDiv(a, b, div int64) (int64, int64) {
	if div <= 0 {
		return 0, 0
	}
	x := new(big.Int).Mul(big.NewInt(a), big.NewInt(b))
	q := new(big.Int)
	r := new(big.Int)
	q.QuoRem(x, big.NewInt(div), r)
	return q.Int64(), r.Int64()
}

// SegmentLength returns floor(sqrt(dx^2+dy^2) * Scale) and the squared
// remainder after truncation. The remainder is a proof artifact: it shows
// exactly what was discarded before the integer path sum was formed.
func SegmentLength(dx, dy int64) (int64, int64) {
	sq := new(big.Int).Mul(big.NewInt(dx), big.NewInt(dx))
	yy := new(big.Int).Mul(big.NewInt(dy), big.NewInt(dy))
	sq.Add(sq, yy)
	sq.Mul(sq, big.NewInt(Scale*Scale))
	root := new(big.Int).Sqrt(sq)
	rem := new(big.Int).Sub(sq, new(big.Int).Mul(root, root))
	return root.Int64(), rem.Int64()
}

// Log10Ratio returns floor(log10(num/den) * Scale). The transcendental
// operation is converted once at this boundary, after which every dB sum is
// integer fixed-point.
func Log10Ratio(num, den int64) (int64, int64) {
	if num <= 0 || den <= 0 {
		return 0, 0
	}
	v := math.Log10(float64(num) / float64(den))
	scaled := v * float64(Scale)
	truncated := int64(math.Trunc(scaled))
	remainder := int64(math.Round((scaled - float64(truncated)) * 1_000_000))
	return truncated, remainder
}

func DecayDropDBMicro(sampleTimeMicros, rt60Micros int64) (int64, int64) {
	if rt60Micros <= 0 {
		return 0, 0
	}
	return MulDiv(60*Scale, sampleTimeMicros, rt60Micros)
}

func DistanceLossDBMicro(pathLengthMicro int64) (int64, int64) {
	const referenceDistanceMicro = 100 * Scale
	if pathLengthMicro <= referenceDistanceMicro {
		return 0, 0
	}
	logv, rem := Log10Ratio(pathLengthMicro, referenceDistanceMicro)
	return logv * 20, rem
}

func GainDBMicro(gainMilli int64) (int64, int64) {
	if gainMilli <= 0 {
		return 0, 0
	}
	logv, rem := Log10Ratio(gainMilli*1000, Scale)
	return logv * 20, rem
}

func DensityBoostDBMicro(densityMilli int64) (int64, int64) {
	if densityMilli < 0 {
		return 0, 0
	}
	num := Scale + densityMilli*1000
	logv, rem := Log10Ratio(num, Scale)
	return logv * 10, rem
}

func Average(values []int64) (int64, int64) {
	if len(values) == 0 {
		return 0, 0
	}
	var total int64
	for _, v := range values {
		total += v
	}
	return MulDiv(total, 1, int64(len(values)))
}

func Clamp(v, lo, hi int64) int64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func ToFloat(v int64) float64 { return float64(v) / float64(Scale) }

func FixedLogRemainderName(stage string) string {
	return stage + ".truncation"
}
