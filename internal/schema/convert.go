package schema

import (
	"math"

	"prosodyalign/internal/errs"
)

type Truncation struct {
	FieldPath string `json:"field_path"`
	Operation string `json:"operation"`
	Input     int64  `json:"input"`
	Output    int64  `json:"output"`
	Remainder int64  `json:"remainder"`
	Direction string `json:"direction"`
	AtSample  int64  `json:"at_sample,omitempty"`
}

func mulChecked(fieldPath string, a, b int64) (int64, error) {
	if a == 0 || b == 0 {
		return 0, nil
	}
	if a > math.MaxInt64/b || a < math.MinInt64/b {
		return 0, errs.Ovf(fieldPath, a, b)
	}
	return a * b, nil
}

func SamplesToMillis(samples int64, sampleRate int, fieldPath string) (int64, Truncation, error) {
	if sampleRate <= 0 {
		return 0, Truncation{}, errs.Schema("", fieldPath, "sample rate must be positive")
	}
	scaled, err := mulChecked(fieldPath, samples, 1000)
	if err != nil {
		return 0, Truncation{}, err
	}
	q := scaled / int64(sampleRate)
	r := scaled % int64(sampleRate)
	tr := Truncation{FieldPath: fieldPath, Operation: "samples_to_millis_floor", Input: samples, Output: q, Remainder: r, Direction: "floor_nonnegative"}
	if r != 0 {
		tr.AtSample = samples
	}
	return q, tr, nil
}

func MillisToSamples(ms int64, sampleRate int, fieldPath string) (int64, Truncation, error) {
	if sampleRate <= 0 {
		return 0, Truncation{}, errs.Schema("", fieldPath, "sample rate must be positive")
	}
	scaled, err := mulChecked(fieldPath, ms, int64(sampleRate))
	if err != nil {
		return 0, Truncation{}, err
	}
	q := scaled / 1000
	r := scaled % 1000
	tr := Truncation{FieldPath: fieldPath, Operation: "millis_to_samples_floor", Input: ms, Output: q, Remainder: r, Direction: "floor_nonnegative"}
	return q, tr, nil
}

func PermilleToPer10k(v int64) int64 { return v * 10 }

func Per10kToPermilleFloor(v int64, fieldPath string) (int64, Truncation) {
	q := v / 10
	r := v % 10
	return q, Truncation{FieldPath: fieldPath, Operation: "per_10k_to_permille_floor", Input: v, Output: q, Remainder: r, Direction: "floor_nonnegative"}
}

func MilliHzToCentiHzFloor(v int64, fieldPath string) (int64, Truncation) {
	q := v / 10
	r := v % 10
	return q, Truncation{FieldPath: fieldPath, Operation: "milli_hz_to_centi_hz_floor", Input: v, Output: q, Remainder: r, Direction: "floor_nonnegative"}
}

func CentiHzToMilliHz(v int64, fieldPath string) (int64, error) {
	return mulChecked(fieldPath, v, 10)
}
