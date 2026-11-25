package gateway

import (
	"math"
	"sort"
)

const (
	DurationFractionBits = 16
	DurationScale        = 1 << DurationFractionBits
	DurationBitWidth     = 64
)

// FixedDuration is a signed 64-bit Q16.16 count of microseconds.
type FixedDuration int64

type DurationTrace struct {
	RecordID           string  `json:"record_id"`
	SourceUnit         string  `json:"source_unit"`
	SourceValue        string  `json:"source_value"`
	ConvertedUS        int64   `json:"converted_us"`
	FixedRaw           int64   `json:"fixed_q16_16_raw"`
	BucketIndex        int     `json:"bucket_index"`
	Increment          int64   `json:"increment"`
	Applied            bool    `json:"applied"`
	ConversionErrorUS  float64 `json:"conversion_error_us"`
	ToleranceUS        int64   `json:"tolerance_us"`
	WithinTolerance    bool    `json:"within_tolerance"`
	Overflowed         bool    `json:"overflowed"`
	Saturated          bool    `json:"saturated"`
	RecomputeIteration int     `json:"recompute_iteration,omitempty"`
}

type Histogram struct {
	BoundariesUS    []int64         `json:"boundaries_us"`
	BoundariesFixed []FixedDuration `json:"boundaries_fixed_raw"`
	ToleranceUS     int64           `json:"tolerance_us"`
}

func NewHistogram(boundariesUS []int64, toleranceUS int64) (*Histogram, error) {
	if len(boundariesUS) == 0 {
		return nil, NewError(CodeBucketBoundaryInvalid, "histogram.boundaries_us", "at least one boundary is required")
	}
	fixed := make([]FixedDuration, len(boundariesUS))
	for i, boundary := range boundariesUS {
		if boundary <= 0 {
			return nil, NewError(CodeBucketBoundaryInvalid, "histogram.boundaries_us", "boundary must be positive").
				WithContext("index", i).WithContext("value", boundary)
		}
		if i > 0 && boundary <= boundariesUS[i-1] {
			return nil, NewError(CodeBucketBoundaryInvalid, "histogram.boundaries_us", "boundaries must be strictly increasing").
				WithContext("index", i).WithContext("value", boundary).WithContext("previous", boundariesUS[i-1])
		}
		converted, err := USSmallToFixed(boundary)
		if err != nil {
			return nil, NewError(CodeBucketScaleOutOfRange, "histogram.boundaries_us", "boundary cannot fit Q16.16").
				WithContext("index", i).WithContext("value", boundary)
		}
		fixed[i] = converted
	}
	if toleranceUS < 0 {
		return nil, NewError(CodeBucketBoundaryInvalid, "histogram.tolerance_us", "tolerance must be non-negative").
			WithContext("value", toleranceUS)
	}
	return &Histogram{BoundariesUS: append([]int64(nil), boundariesUS...), BoundariesFixed: fixed, ToleranceUS: toleranceUS}, nil
}

func USSmallToFixed(us int64) (FixedDuration, error) {
	if us < 0 {
		return 0, NewError(CodeDurationNegative, "duration_us", "duration is negative").WithContext("value", us)
	}
	if us > math.MaxInt64/DurationScale {
		return 0, NewError(CodeDurationOverflow, "duration_us", "duration overflows Q16.16").
			WithContext("bit_width", DurationBitWidth).WithContext("value", us)
	}
	return FixedDuration(us * DurationScale), nil
}

func MillisecondsToFixed(ms int64) (FixedDuration, error) {
	if ms < 0 {
		return 0, NewError(CodeDurationNegative, "duration_ms", "duration is negative").WithContext("value", ms)
	}
	us, err := mulChecked(ms, 1000)
	if err != nil {
		return 0, err
	}
	return USSmallToFixed(us)
}

// DecimalMillisecondsToFixed converts a decimal millisecond value to Q16.16
// microseconds using integer arithmetic only. It retains up to nine decimal
// digits and rounds the final Q16 bit.
func DecimalMillisecondsToFixed(wholeMS int64, fractional string) (FixedDuration, error) {
	if wholeMS < 0 {
		return 0, NewError(CodeDurationNegative, "duration_ms", "negative duration").WithContext("value", wholeMS)
	}
	wholePart, err := mulChecked(wholeMS, 1000*DurationScale)
	if err != nil {
		return 0, err
	}
	if fractional == "" {
		return FixedDuration(wholePart), nil
	}
	if len(fractional) > 9 {
		return 0, NewError(CodeFieldType, "duration_ms", "too many decimal digits").WithContext("digits", len(fractional))
	}
	var numerator int64
	for _, r := range fractional {
		if r < '0' || r > '9' {
			return 0, NewError(CodeFieldType, "duration_ms", "invalid decimal fraction").WithContext("value", fractional)
		}
		numerator = numerator*10 + int64(r-'0')
	}
	denominator := int64Pow10(len(fractional))
	// fractional_ms * 1000 us/ms * 65536 Q units/us.
	scaledNumerator, err := mulChecked(numerator, 1000*DurationScale)
	if err != nil {
		return 0, err
	}
	fracPart := scaledNumerator / denominator
	if rem := scaledNumerator % denominator; rem*2 >= denominator {
		fracPart++
	}
	raw, err := addChecked(wholePart, fracPart)
	if err != nil {
		return 0, err
	}
	if raw > math.MaxInt64 {
		return 0, NewError(CodeDurationOverflow, "duration_ms", "duration overflows Q16.16")
	}
	return FixedDuration(raw), nil
}

func int64Pow10(digits int) int64 {
	value := int64(1)
	for i := 0; i < digits; i++ {
		value *= 10
	}
	return value
}

func ClassifyFixed(d FixedDuration, boundaries []FixedDuration) int {
	return sort.Search(len(boundaries), func(i int) bool { return d < boundaries[i] })
}

func (h *Histogram) ClassifyUS(us int64) (int, FixedDuration, error) {
	fixed, err := USSmallToFixed(us)
	if err != nil {
		return 0, 0, err
	}
	return ClassifyFixed(fixed, h.BoundariesFixed), fixed, nil
}

func BucketCount(boundaryCount int) int {
	return boundaryCount + 1
}

func mulChecked(a, b int64) (int64, error) {
	if a == 0 || b == 0 {
		return 0, nil
	}
	if a > math.MaxInt64/b || a < math.MinInt64/b {
		return 0, NewError(CodeDurationOverflow, "duration", "integer multiplication overflow").
			WithContext("a", a).WithContext("b", b).WithContext("bit_width", 64)
	}
	return a * b, nil
}

func addChecked(a, b int64) (int64, error) {
	if b > 0 && a > math.MaxInt64-b {
		return 0, NewError(CodeDurationOverflow, "duration", "integer addition overflow").WithContext("a", a).WithContext("b", b)
	}
	if b < 0 && a < math.MinInt64-b {
		return 0, NewError(CodeDurationOverflow, "duration", "integer addition underflow").WithContext("a", a).WithContext("b", b)
	}
	return a + b, nil
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// SaturatingFromUS is used by ingestion paths whose declared policy is clamp.
func SaturatingFromUS(us int64) (FixedDuration, bool) {
	if us < 0 {
		return 0, false
	}
	if us > math.MaxInt64/DurationScale {
		return FixedDuration(math.MaxInt64), true
	}
	return FixedDuration(us * DurationScale), false
}
