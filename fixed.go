package fogkernel

import (
	"math"
	"strconv"
	"strings"
)

// Declared fixed-point widths. All schema scale/distance conversions stay in
// int64 until geometry needs floating-point ray division/exp.
const (
	CoeffScale         int64 = 1_000_000
	DistanceScale      int64 = 1_000_000 // fixed micrometres per metre
	MicroPerMM         int64 = 1_000
	TauScale           int64 = 1_000_000_000
	TransmittanceQ     int64 = 1_000_000_000
	DefaultTolQ        int64 = 1_000 // 1e-6 transmittance
	PreviewBoundQ      int64 = 50_000_000
	MaxDeclaredWorkers       = 64
	MaxDeclaredBuffer        = 4096
)

type ScaleKind string

const (
	ScalePermille  ScaleKind = "permille"
	ScalePermyriad ScaleKind = "permyriad"
)

type ConversionAudit struct {
	ID              string  `json:"id"`
	Kind            string  `json:"kind"`
	SourceVersion   string  `json:"source_version"`
	RawValue        string  `json:"raw_value"`
	Intermediate    string  `json:"intermediate"`
	CanonicalQ      int64   `json:"canonical_q"`
	CanonicalFloat  float64 `json:"canonical_float"`
	TauQ            int64   `json:"tau_q,omitempty"`
	Tau             float64 `json:"tau,omitempty"`
	DifferenceQ     int64   `json:"difference_q,omitempty"`
	ToleranceQ      int64   `json:"tolerance_q"`
	WithinTolerance bool    `json:"within_tolerance"`
	Overflow        bool    `json:"overflow"`
	Saturated       bool    `json:"saturated"`
	ExpectedProbe   bool    `json:"expected_probe,omitempty"`
	StatusCode      string  `json:"status_code"`
	Message         string  `json:"message,omitempty"`
}

func checkedMul(a, b int64) (int64, bool) {
	if a == 0 || b == 0 {
		return 0, true
	}
	result := a * b
	if result/b != a {
		return 0, false
	}
	return result, true
}

// CoefficientToQ converts a non-negative scaled coefficient to CoeffScale Q.
// Permille raw n means n/1000; permyriad raw n means n/10000.
func CoefficientToQ(raw int64, scale ScaleKind, path string) (int64, error) {
	if raw < 0 {
		return 0, NewBizError(CodeCoeffNegative, path, "coefficient must be non-negative, got %d", raw).
			WithContext("raw", raw, "scale", string(scale))
	}
	multiplier := int64(0)
	switch scale {
	case ScalePermille:
		multiplier = CoeffScale / 1000
	case ScalePermyriad:
		multiplier = CoeffScale / 10000
	default:
		return 0, NewBizError(CodeSchemaCombination, path, "unknown coefficient scale %q", scale)
	}
	value, ok := checkedMul(raw, multiplier)
	if !ok {
		return 0, NewBizError(CodeFixedOverflow, path, "coefficient conversion overflows int64").
			WithContext("raw", raw, "multiplier", multiplier, "scale", string(scale))
	}
	return value, nil
}

func CoefficientToQSaturated(raw int64, scale ScaleKind) (value int64, overflow bool, saturated bool) {
	value, err := CoefficientToQ(raw, scale, "$.audit")
	if err == nil {
		return value, false, false
	}
	if raw < 0 {
		return 0, false, true
	}
	return math.MaxInt64, true, true
}

func QToPermille(q int64) int64  { return q / (CoeffScale / 1000) }
func QToPermyriad(q int64) int64 { return q / (CoeffScale / 10000) }
func QToFloat(q int64, scale int64) float64 {
	return float64(q) / float64(scale)
}

func MMToMicro(mm int64, path string) (int64, error) {
	v, ok := checkedMul(mm, MicroPerMM)
	if !ok {
		return 0, NewBizError(CodeFixedOverflow, path, "millimetre to micrometre conversion overflows").
			WithContext("millimetres", mm)
	}
	return v, nil
}

func MMToMicroSaturated(mm int64) (micro int64, overflow bool, saturated bool) {
	v, ok := checkedMul(mm, MicroPerMM)
	if ok {
		return v, false, false
	}
	if mm < 0 {
		return math.MinInt64, true, true
	}
	return math.MaxInt64, true, true
}

func MicroToMM(micro int64, path string) (int64, error) {
	if micro%MicroPerMM != 0 {
		return 0, NewBizError(CodeFixedPrecision, path, "distance is not an integral number of millimetres").
			WithContext("micrometres", micro)
	}
	return micro / MicroPerMM, nil
}

// DecimalMetersToMicro parses a JSON meter literal as an exact decimal. The
// declared internal width is micrometres; generated documents use at most
// three fractional digits.
func DecimalMetersToMicro(literal string, path string) (int64, error) {
	s := strings.TrimSpace(literal)
	if s == "" {
		return 0, NewBizError(CodeSchemaField, path, "empty meter literal")
	}
	negative := false
	if s[0] == '+' || s[0] == '-' {
		negative = s[0] == '-'
		s = s[1:]
	}
	parts := strings.SplitN(s, ".", 2)
	wholeText := parts[0]
	if wholeText == "" {
		wholeText = "0"
	}
	whole := int64(0)
	for _, r := range wholeText {
		if r < '0' || r > '9' {
			return 0, NewBizError(CodeSchemaField, path, "invalid meter literal %q", literal)
		}
		digit := int64(r - '0')
		if whole > (math.MaxInt64-digit)/10 {
			return 0, NewBizError(CodeFixedOverflow, path, "meter whole part overflows int64").
				WithContext("literal", literal)
		}
		whole = whole*10 + digit
	}
	fracText := ""
	if len(parts) == 2 {
		fracText = parts[1]
	}
	if len(fracText) > 6 {
		return 0, NewBizError(CodeFixedPrecision, path, "meter literal has more than six fractional digits").
			WithContext("literal", literal, "fractional_digits", len(fracText))
	}
	frac, err := strconv.ParseInt(fracText+"000000"[:6-len(fracText)], 10, 64)
	if err != nil && fracText != "" {
		return 0, NewBizError(CodeSchemaField, path, "invalid meter fraction %q", fracText)
	}
	microWhole, ok := checkedMul(whole, DistanceScale)
	if !ok {
		return 0, NewBizError(CodeFixedOverflow, path, "meter to micrometre conversion overflows").
			WithContext("metres", whole)
	}
	total, ok := checkedAdd(microWhole, frac)
	if !ok {
		return 0, NewBizError(CodeFixedOverflow, path, "meter fixed-point addition overflows")
	}
	if negative {
		total = -total
	}
	return total, nil
}

func checkedAdd(a, b int64) (int64, bool) {
	if b > 0 && a > math.MaxInt64-b {
		return 0, false
	}
	if b < 0 && a < math.MinInt64-b {
		return 0, false
	}
	return a + b, true
}

func FloatMetersToMicroRounded(v float64, path string) (int64, error) {
	if math.IsNaN(v) || math.IsInf(v, 0) {
		return 0, NewBizError(CodeSchemaField, path, "meter value is not finite")
	}
	rounded := math.Round(v * float64(DistanceScale))
	if rounded > float64(math.MaxInt64) || rounded < float64(math.MinInt64) {
		return 0, NewBizError(CodeFixedOverflow, path, "meter value overflows fixed-point distance")
	}
	return int64(rounded), nil
}

func MicroToMetersString(micro int64) string {
	sign := ""
	if micro < 0 {
		sign = "-"
		micro = -micro
	}
	return sign + strconv.FormatInt(micro/DistanceScale, 10) + "." +
		pad6(micro%DistanceScale)
}

func pad6(v int64) string {
	s := strconv.FormatInt(v, 10)
	return strings.Repeat("0", 6-len(s)) + s
}

func QuantizeTau(tau float64) int64 {
	if math.IsNaN(tau) || tau < 0 {
		return 0
	}
	q := math.Round(tau * float64(TauScale))
	if q >= float64(math.MaxInt64) {
		return math.MaxInt64
	}
	return int64(q)
}

func TransmittanceWithinTolerance(a, b float64, toleranceQ int64) bool {
	tol := float64(toleranceQ) / float64(TransmittanceQ)
	return math.Abs(a-b) <= tol
}

func FixedDifferenceQ(a, b float64, scale int64) int64 {
	return int64(math.Round(math.Abs(a-b) * float64(scale)))
}
