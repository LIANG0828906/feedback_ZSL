package convert

import (
	"fmt"
	"math"
	"math/bits"

	"lip-pipeline-prototype/internal/domain"
)

type Ledger struct {
	Records []domain.ConversionRecord `json:"records"`
}

func NewLedger() *Ledger {
	return &Ledger{Records: []domain.ConversionRecord{}}
}

func (l *Ledger) add(rec domain.ConversionRecord) {
	if l == nil {
		return
	}
	l.Records = append(l.Records, rec)
}

func checkedMul(a, b int64, field, path string) (int64, error) {
	if a < 0 || b < 0 {
		return 0, &domain.Error{
			Code: domain.CodeOvf, Message: "integer conversion operands must be non-negative",
			Path: path, Field: field, OperandA: fmt.Sprint(a), OperandB: fmt.Sprint(b),
		}
	}
	hi, lo := bits.Mul64(uint64(a), uint64(b))
	if hi != 0 || lo > uint64(math.MaxInt64) {
		return 0, &domain.Error{
			Code: domain.CodeOvf, Message: "integer multiplication overflow",
			Path: path, Field: field, OperandA: fmt.Sprint(a), OperandB: fmt.Sprint(b),
		}
	}
	return int64(lo), nil
}

func floorDiv(n, d int64) (int64, int64) {
	if d <= 0 {
		panic("convert: divisor must be positive")
	}
	return n / d, n % d
}

func SamplesToMS(samples int64, sampleRate int, path string, l *Ledger) (int64, error) {
	if sampleRate <= 0 {
		return 0, &domain.Error{Code: domain.CodeVal, Message: "sample_rate must be positive", Path: path + ".sample_rate", Field: "sample_rate", Actual: sampleRate}
	}
	if int64(sampleRate)%1000 == 0 {
		samplesPerMS := int64(sampleRate) / 1000
		ms, rem := floorDiv(samples, samplesPerMS)
		l.add(domain.ConversionRecord{
			Path: path, Field: "time", FromUnit: "samples", ToUnit: "milliseconds",
			Input: samples, Output: ms, Remainder: rem, Rounding: "floor_non_negative",
			TruncatedAt: path + ":floor(samples/(sample_rate/1000)); remainder_unit=samples",
		})
		return ms, nil
	}
	scaled, err := checkedMul(samples, 1000, path+".start_or_end_sample", path)
	if err != nil {
		return 0, err
	}
	ms, rem := floorDiv(scaled, int64(sampleRate))
	rec := domain.ConversionRecord{
		Path: path, Field: "time", FromUnit: "samples", ToUnit: "milliseconds",
		Input: samples, Output: ms, Remainder: rem, Rounding: "floor_non_negative",
		TruncatedAt: path + ":floor(sample*1000/sample_rate)",
	}
	l.add(rec)
	return ms, nil
}

func MSToSamples(ms int64, sampleRate int, path string, l *Ledger) (int64, error) {
	if sampleRate <= 0 {
		return 0, &domain.Error{Code: domain.CodeVal, Message: "sample_rate must be positive", Path: path + ".sample_rate", Field: "sample_rate", Actual: sampleRate}
	}
	scaled, err := checkedMul(ms, int64(sampleRate), path+".time_ms", path)
	if err != nil {
		return 0, err
	}
	samples, rem := floorDiv(scaled, 1000)
	rec := domain.ConversionRecord{
		Path: path, Field: "time", FromUnit: "milliseconds", ToUnit: "samples",
		Input: ms, Output: samples, Remainder: rem, Rounding: "floor_non_negative",
		TruncatedAt: path + ":floor(ms*sample_rate/1000)",
	}
	l.add(rec)
	return samples, nil
}

func PermilleToPermyriad(permille int, path string, l *Ledger) int {
	out := permille * 10
	l.add(domain.ConversionRecord{
		Path: path, Field: "weight", FromUnit: "permille", ToUnit: "permyriad",
		Input: int64(permille), Output: int64(out), Remainder: 0, Rounding: "exact_scale",
		TruncatedAt: path + ":permille*10",
	})
	return out
}

func PermyriadToPermille(permyriad int, path string, l *Ledger) (int, int) {
	out, rem := floorDiv(int64(permyriad), 10)
	l.add(domain.ConversionRecord{
		Path: path, Field: "weight", FromUnit: "permyriad", ToUnit: "permille",
		Input: int64(permyriad), Output: out, Remainder: rem, Rounding: "floor_non_negative",
		TruncatedAt: path + ":floor(weight_permyriad/10)",
	})
	return int(out), int(rem)
}

func QuantizeFrame(sample int64, fps, sampleRate int, path string) (int64, error) {
	if fps <= 0 || sampleRate <= 0 {
		return 0, &domain.Error{Code: domain.CodeVal, Message: "fps and sample_rate must be positive", Path: path, Field: "fps/sample_rate"}
	}
	scaled, err := checkedMul(sample, int64(fps), path, path)
	if err != nil {
		return 0, err
	}
	frame, _ := floorDiv(scaled, int64(sampleRate))
	return frame, nil
}

func CheckWeightRange(weight int, max int, path string) error {
	if weight < 0 || weight > max {
		return &domain.Error{
			Code: domain.CodeVal, Message: fmt.Sprintf("weight must be in [0,%d]", max),
			Path: path, Field: "weight", Expected: fmt.Sprintf("[0,%d]", max), Actual: weight,
		}
	}
	return nil
}
