package quota

import (
	"fmt"
	"math"
	"math/bits"
)

// Conversion records every fixed-point operation used to obtain Result. The
// result is never silently accepted when Overflow or Saturated is true.
type Conversion struct {
	CaseID        string  `json:"case_id,omitempty"`
	Path          string  `json:"path"`
	Input         int64   `json:"input"`
	FromScale     int64   `json:"from_scale,omitempty"`
	ToScale       int64   `json:"to_scale,omitempty"`
	Numerator     uint64  `json:"intermediate_numerator"`
	Denominator   int64   `json:"denominator"`
	Quotient      int64   `json:"intermediate_quotient"`
	Intermediates []int64 `json:"intermediate_values,omitempty"`
	Result        int64   `json:"result"`
	Overflow      bool    `json:"overflow"`
	Saturated     bool    `json:"saturated"`
	Formula       string  `json:"formula"`
	ErrorCode     string  `json:"error_code,omitempty"`
}

type PrecisionReport struct {
	Conversion       Conversion `json:"conversion"`
	Truth            int64      `json:"truth"`
	Delta            int64      `json:"delta"`
	Tolerance        int64      `json:"tolerance"`
	ExceedsTolerance bool       `json:"exceeds_tolerance"`
	Accepted         bool       `json:"accepted"`
	Note             string     `json:"note"`
}

func checkedMul(a, b int64) (uint64, int64, bool, bool) {
	if a < 0 || b < 0 {
		return 0, 0, false, false
	}
	hi, lo := bits.Mul64(uint64(a), uint64(b))
	if hi != 0 || lo > math.MaxInt64 {
		return lo, math.MaxInt64, true, true
	}
	return lo, int64(lo), false, false
}

// ScaleRatio converts a ratio between permille and basis points. Negative
// ratios are rejected; positive overflow is saturated and flagged.
func ScaleRatio(value, fromScale, toScale int64, path string) (Conversion, error) {
	c := Conversion{
		Path:      path,
		Input:     value,
		FromScale: fromScale,
		ToScale:   toScale,
		Formula:   "value * toScale / fromScale",
	}
	if value < 0 {
		c.ErrorCode = CodeNegativeQuota
		return c, NewError(CodeNegativeQuota, "convert_ratio", path, "negative ratio")
	}
	if fromScale <= 0 || toScale <= 0 {
		c.ErrorCode = CodeSchemaField
		return c, NewError(CodeSchemaField, "convert_ratio", path, "non-positive scale")
	}
	num, product, overflow, saturated := checkedMul(value, toScale)
	c.Numerator = num
	c.Denominator = fromScale
	if overflow {
		c.Overflow = true
		c.Saturated = saturated
		c.Result = math.MaxInt64
		c.Quotient = math.MaxInt64
		c.ErrorCode = CodeOverflow
		return c, &BusinessError{
			Code:   CodeOverflow,
			Op:     "convert_ratio",
			Path:   path,
			Detail: fmt.Sprintf("numerator overflow: %d*%d", value, toScale),
		}
	}
	c.Quotient = product / fromScale
	c.Result = c.Quotient
	return c, nil
}

// ConvertUnit applies a checked integer multiplier with saturation details.
func ConvertUnit(value, multiplier int64, path, formula string) (Conversion, error) {
	c := Conversion{Path: path, Input: value, Formula: formula}
	if value < 0 {
		c.ErrorCode = CodeNegativeQuota
		return c, NewError(CodeNegativeQuota, "convert_unit", path, "negative value")
	}
	if multiplier <= 0 {
		c.ErrorCode = CodeSchemaField
		return c, NewError(CodeSchemaField, "convert_unit", path, "non-positive multiplier")
	}
	num, product, overflow, saturated := checkedMul(value, multiplier)
	c.Numerator = num
	c.Denominator = 1
	if overflow {
		c.Overflow = true
		c.Saturated = saturated
		c.Result = math.MaxInt64
		c.Quotient = math.MaxInt64
		c.ErrorCode = CodeOverflow
		return c, &BusinessError{Code: CodeOverflow, Op: "convert_unit", Path: path, Detail: "multiplication overflow"}
	}
	c.Quotient = product
	c.Result = product
	return c, nil
}

// QuotaForNode computes floor(capacity * quotaRatio / 10000 * weight / 1000)
// in two explicitly checked steps. The second multiplication is performed on
// the quotient, matching the documented calculation path.
func QuotaForNode(capacityMilli, ratioBP, weightPermille int64, path string) (Conversion, error) {
	c := Conversion{
		Path:      path,
		FromScale: RatioScaleBasisPoints,
		ToScale:   RatioScalePermille,
		Formula:   "floor(floor(capacity * ratio_bp / 10000) * weight_permille / 1000)",
	}
	if capacityMilli < 0 || ratioBP < 0 || weightPermille < 0 {
		c.ErrorCode = CodeNegativeQuota
		return c, NewError(CodeNegativeQuota, "quota_for_node", path, "negative quota operand")
	}
	firstNum, firstQuotient, overflow, saturated := checkedMul(capacityMilli, ratioBP)
	c.Input = capacityMilli
	c.Numerator = firstNum
	c.Denominator = RatioScaleBasisPoints
	if overflow {
		c.Overflow = true
		c.Saturated = saturated
		c.Result = math.MaxInt64
		c.Quotient = math.MaxInt64
		c.ErrorCode = CodeOverflow
		return c, &BusinessError{Code: CodeOverflow, Op: "quota_for_node", Path: path,
			Detail: fmt.Sprintf("capacity*ratio overflow: %d*%d", capacityMilli, ratioBP)}
	}
	base := firstQuotient / RatioScaleBasisPoints
	c.Intermediates = []int64{firstQuotient, base}
	secondNum, weighted, overflow, saturated := checkedMul(base, weightPermille)
	c.Numerator = secondNum
	c.Denominator = RatioScalePermille
	if overflow {
		c.Overflow = true
		c.Saturated = saturated
		c.Result = math.MaxInt64
		c.Quotient = math.MaxInt64
		c.ErrorCode = CodeOverflow
		return c, &BusinessError{Code: CodeOverflow, Op: "quota_for_node", Path: path,
			Detail: fmt.Sprintf("base*weight overflow: %d*%d", base, weightPermille)}
	}
	c.Quotient = weighted
	c.Intermediates = append(c.Intermediates, weighted)
	c.Result = weighted / RatioScalePermille
	return c, nil
}

// CheckPrecision annotates a conversion against the generator truth. A result
// with an overflow or a delta outside tolerance is explicitly not accepted.
func CheckPrecision(c Conversion, truth, tolerance int64, caseID string) PrecisionReport {
	c.CaseID = caseID
	delta := c.Result - truth
	if delta < 0 {
		delta = -delta
	}
	exceeds := delta > tolerance
	accepted := !c.Overflow && !c.Saturated && !exceeds
	note := "accepted"
	if c.Overflow {
		note = "overflow: saturated value must not be used"
	} else if exceeds {
		note = "difference exceeds declared fixed-point tolerance"
	}
	return PrecisionReport{
		Conversion:       c,
		Truth:            truth,
		Delta:            delta,
		Tolerance:        tolerance,
		ExceedsTolerance: exceeds,
		Accepted:         accepted,
		Note:             note,
	}
}
