package spatialpick

import (
	"fmt"
	"math"
)

// FixedMatrix stores a row-major affine matrix in integer units of
// 10^-precision. All published world transforms use this representation.
type FixedMatrix [16]int64

func fixedScale(precision int) float64 { return math.Pow10(precision) }

func validPrecision(precision int) bool { return precision >= 0 && precision <= 9 }

func quantizeFloat(v float64, precision int) (int64, bool) {
	if !finite(v) {
		return 0, false
	}
	s := fixedScale(precision)
	x := math.Round(v * s)
	if x < math.MinInt64 || x > math.MaxInt64 {
		return 0, false
	}
	return int64(x), true
}

func QuantizeMatrix(m Mat4, precision int) (FixedMatrix, error) {
	if !validPrecision(precision) {
		return FixedMatrix{}, coded(CodeSchema, "quantize", "precision must be in [0,9]")
	}
	var out FixedMatrix
	for i, v := range m {
		q, ok := quantizeFloat(v, precision)
		if !ok {
			return FixedMatrix{}, &CodedError{Code: CodeSchema, Op: "quantize", Field: fmt.Sprintf("matrix[%d]", i), Message: "matrix value is non-finite or out of range"}
		}
		out[i] = q
	}
	if !out.IsAffine(precision) {
		return FixedMatrix{}, &CodedError{Code: CodeSchema, Op: "quantize", Field: "matrix", Message: "matrix is not affine"}
	}
	return out, nil
}

func (m FixedMatrix) Float(precision int) Mat4 {
	s := fixedScale(precision)
	var out Mat4
	for i, v := range m {
		out[i] = float64(v) / s
	}
	return out
}

func (m FixedMatrix) IsAffine(precision int) bool {
	scale, ok := quantizeFloat(1, precision)
	return ok && m[12] == 0 && m[13] == 0 && m[14] == 0 && m[15] == scale
}

func MultiplyFixed(a, b FixedMatrix, precision int) (FixedMatrix, error) {
	if !validPrecision(precision) {
		return FixedMatrix{}, coded(CodeNumeric, "multiply", "invalid precision")
	}
	var out FixedMatrix
	scale := int64(1)
	for i := 0; i < precision; i++ {
		scale *= 10
	}
	for i := 0; i < 4; i++ {
		for j := 0; j < 4; j++ {
			var sum int64
			for k := 0; k < 4; k++ {
				p, ok := mulInt64(a[i*4+k], b[k*4+j])
				if !ok {
					return FixedMatrix{}, &CodedError{Code: CodeNumeric, Op: "multiply", Field: fmt.Sprintf("matrix[%d][%d]", i, j), Message: "fixed-point multiplication overflow"}
				}
				sum, ok = addInt64(sum, p)
				if !ok {
					return FixedMatrix{}, &CodedError{Code: CodeNumeric, Op: "multiply", Field: fmt.Sprintf("matrix[%d][%d]", i, j), Message: "fixed-point accumulation overflow"}
				}
			}
			out[i*4+j] = divRound(sum, scale)
		}
	}
	return out, nil
}

func mulInt64(a, b int64) (int64, bool) {
	if a == 0 || b == 0 {
		return 0, true
	}
	if (a == -1 && b == math.MinInt64) || (b == -1 && a == math.MinInt64) {
		return 0, false
	}
	p := a * b
	if p/b != a {
		return 0, false
	}
	return p, true
}

func addInt64(a, b int64) (int64, bool) {
	s := a + b
	if (b > 0 && s < a) || (b < 0 && s > a) {
		return 0, false
	}
	return s, true
}

func divRound(a, b int64) int64 {
	if b < 0 {
		a, b = -a, -b
	}
	q := a / b
	r := a % b
	if r == 0 {
		return q
	}
	if a >= 0 {
		if r >= (b+1)/2 {
			q++
		}
	} else if -r >= (b+1)/2 {
		q--
	}
	return q
}

func InverseFixedAffine(m FixedMatrix, precision int) (FixedMatrix, error) {
	inv, ok := Mat4InverseAffine(m.Float(precision))
	if !ok {
		return FixedMatrix{}, &CodedError{Code: CodeNumeric, Op: "inverse", Message: "world matrix is singular"}
	}
	out, err := QuantizeMatrix(inv, precision)
	if err != nil {
		return FixedMatrix{}, err
	}
	return out, nil
}
