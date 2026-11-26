package dialogue

import (
	"fmt"
	"math/big"
)

// RoundingMode declares how a discarded remainder is handled.
type RoundingMode string

const (
	// RoundHalfAwayFromZero is the kernel's single declared rounding mode.
	RoundHalfAwayFromZero RoundingMode = "half-away-from-zero"
)

// Truncation records one integer conversion that discarded a remainder.
type Truncation struct {
	Location     string `json:"location"`
	Field        string `json:"field"`
	Operation    string `json:"operation"`
	OperandA     int64  `json:"operand_a"`
	OperandB     int64  `json:"operand_b"`
	RoundedValue int64  `json:"rounded_value"`
	Remainder    int64  `json:"remainder"`
	Residue      int64  `json:"residue"`
	Mode         string `json:"mode"`
}

// ScaleSpec names a native fixed-point scale.
type ScaleSpec struct {
	Name  string
	Scale int64
}

// ConvertScale converts value from one fixed-point scale to another using
// integer arithmetic only. Positive and negative values use half-away-from-zero.
func ConvertScale(value int64, from, to int64, field, location string) (int64, []Truncation, error) {
	if from <= 0 || to <= 0 {
		return 0, nil, &Error{
			Code:     CodeOVF,
			Field:    field,
			Path:     field,
			Operands: []int64{from, to},
			Message:  "scale must be positive",
		}
	}
	if from == to {
		return value, nil, nil
	}
	rounded, remainder, residue, err := roundMulDiv(value, to, from, field, from, to)
	if err != nil {
		return 0, nil, err
	}
	if remainder != 0 {
		return rounded, []Truncation{{
			Location:     location,
			Field:        field,
			Operation:    fmt.Sprintf("*%d/%d", to, from),
			OperandA:     value,
			OperandB:     from,
			RoundedValue: rounded,
			Remainder:    remainder,
			Residue:      residue,
			Mode:         string(RoundHalfAwayFromZero),
		}}, nil
	}
	return rounded, nil, nil
}

// MillisecondsToSamples converts a millisecond duration to samples.
func MillisecondsToSamples(ms, sampleRate int64, field, location string) (int64, []Truncation, error) {
	if sampleRate <= 0 {
		return 0, nil, &Error{Code: CodeSchema, Field: field, Path: field, Message: "sample rate must be positive"}
	}
	rounded, remainder, residue, err := roundMulDiv(ms, sampleRate, 1000, field, ms, 1000)
	if err != nil {
		return 0, nil, err
	}
	if remainder == 0 {
		return rounded, nil, nil
	}
	return rounded, []Truncation{{
		Location:     location,
		Field:        field,
		Operation:    fmt.Sprintf("*%d/1000", sampleRate),
		OperandA:     ms,
		OperandB:     1000,
		RoundedValue: rounded,
		Remainder:    remainder,
		Residue:      residue,
		Mode:         string(RoundHalfAwayFromZero),
	}}, nil
}

// SamplesToMilliseconds converts a sample count to milliseconds.
func SamplesToMilliseconds(samples, sampleRate int64, field, location string) (int64, []Truncation, error) {
	if sampleRate <= 0 {
		return 0, nil, &Error{Code: CodeOVF, Field: field, Path: field, Operands: []int64{samples, sampleRate}, Message: "sample rate must be positive"}
	}
	rounded, remainder, residue, err := roundMulDiv(samples, 1000, sampleRate, field, samples, sampleRate)
	if err != nil {
		return 0, nil, err
	}
	if remainder == 0 {
		return rounded, nil, nil
	}
	return rounded, []Truncation{{
		Location:     location,
		Field:        field,
		Operation:    fmt.Sprintf("*1000/%d", sampleRate),
		OperandA:     samples,
		OperandB:     sampleRate,
		RoundedValue: rounded,
		Remainder:    remainder,
		Residue:      residue,
		Mode:         string(RoundHalfAwayFromZero),
	}}, nil
}

func roundMulDiv(value, mul, div int64, field string, operands ...int64) (int64, int64, int64, error) {
	if div == 0 {
		return 0, 0, 0, &Error{Code: CodeOVF, Field: field, Path: field, Operands: append([]int64(nil), operands...), Message: "division by zero"}
	}
	a := big.NewInt(value)
	b := big.NewInt(mul)
	a.Mul(a, b)
	den := big.NewInt(div)
	q := new(big.Int)
	r := new(big.Int)
	q.QuoRem(a, den, r)

	twice := new(big.Int).Lsh(new(big.Int).Abs(new(big.Int).Set(r)), 1)
	absDen := new(big.Int).Abs(new(big.Int).Set(den))
	if twice.Cmp(absDen) >= 0 {
		if a.Sign() < 0 {
			q.Sub(q, big.NewInt(1))
		} else {
			q.Add(q, big.NewInt(1))
		}
	}
	if !q.IsInt64() {
		return 0, 0, 0, &Error{
			Code:     CodeOVF,
			Field:    field,
			Path:     field,
			Operands: append([]int64(nil), operands...),
			Message:  "integer conversion result overflows int64",
		}
	}
	rounded := q.Int64()
	rawRemainder := new(big.Int).Set(r)
	if !rawRemainder.IsInt64() {
		return 0, 0, 0, &Error{Code: CodeOVF, Field: field, Path: field, Operands: append([]int64(nil), operands...), Message: "remainder overflows int64"}
	}
	// Residue is rounded-minus-exact, expressed in the destination numerator.
	exactNumerator := new(big.Int).Mul(den, big.NewInt(rounded))
	residue := new(big.Int).Sub(exactNumerator, a)
	if !residue.IsInt64() {
		return 0, 0, 0, &Error{Code: CodeOVF, Field: field, Path: field, Operands: append([]int64(nil), operands...), Message: "rounding residue overflows int64"}
	}
	return rounded, rawRemainder.Int64(), residue.Int64(), nil
}

func checkedMul(a, b int64, field string) (int64, error) {
	product := new(big.Int).Mul(big.NewInt(a), big.NewInt(b))
	if !product.IsInt64() {
		return 0, &Error{Code: CodeOVF, Field: field, Path: field, Operands: []int64{a, b}, Message: "multiplication overflows int64"}
	}
	return product.Int64(), nil
}

func truncDivMul(value, mul, div int64, field, location string) (int64, []Truncation, error) {
	if div == 0 {
		return 0, nil, &Error{Code: CodeOVF, Field: field, Path: field, Operands: []int64{value, 0}, Message: "division by zero"}
	}
	product := new(big.Int).Mul(big.NewInt(value), big.NewInt(mul))
	q := new(big.Int)
	r := new(big.Int)
	q.QuoRem(product, big.NewInt(div), r)
	if !q.IsInt64() || !r.IsInt64() {
		return 0, nil, &Error{Code: CodeOVF, Field: field, Path: field, Operands: []int64{value, mul}, Message: "integer conversion overflow"}
	}
	if r.Sign() == 0 {
		return q.Int64(), nil, nil
	}
	return q.Int64(), []Truncation{{
		Location:     location,
		Field:        field,
		Operation:    fmt.Sprintf("*%d/%d", mul, div),
		OperandA:     value,
		OperandB:     div,
		RoundedValue: q.Int64(),
		Remainder:    r.Int64(),
		Residue:      r.Int64(),
		Mode:         "toward-zero",
	}}, nil
}
