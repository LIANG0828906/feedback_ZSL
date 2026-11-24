package codec

import (
	"fmt"
	"math"
	"sync"

	"audioeval/internal/model"
)

const cosScale int64 = 1 << 14

var (
	cosCacheMu sync.Mutex
	cosCache   = map[int][]int64{}
)

type FrameEncoding struct {
	Reconstruction []int16
	CoeffErrorsQ14 []int64
	InverseSSE     int64
}

func EncodeFrame(samples []int16, step int64) (FrameEncoding, error) {
	if len(samples) == 0 {
		return FrameEncoding{}, model.NewError(model.CodePara, "frame_length", -1, "frame length must be positive")
	}
	if step <= 0 || step > 1<<20 {
		return FrameEncoding{}, model.NewError(model.CodePara, "quant_step", -1, "quantization step is outside the supported range")
	}
	n := len(samples)
	table := cosineTable(n)
	scaledStep := step * cosScale
	coeffs := make([]int64, n)
	quantized := make([]int64, n)
	errors := make([]int64, n)

	for k := 0; k < n; k++ {
		var sum int64
		for i := 0; i < n; i++ {
			sum += int64(samples[i]) * table[i*n+k]
		}
		coeffs[k] = sum
		index := roundDiv(sum, scaledStep)
		quantized[k] = index * scaledStep
		errors[k] = sum - quantized[k]
	}

	den := int64(n) * cosScale * cosScale
	recon := make([]int16, n)
	var sse int64
	for i := 0; i < n; i++ {
		sum := quantized[0] * table[i*n]
		for k := 1; k < n; k++ {
			sum += 2 * quantized[k] * table[i*n+k]
		}
		value := roundDiv(sum, den)
		if value < math.MinInt16 {
			value = math.MinInt16
		}
		if value > math.MaxInt16 {
			value = math.MaxInt16
		}
		recon[i] = int16(value)
		diff := int64(samples[i]) - value
		sse += diff * diff
	}

	return FrameEncoding{
		Reconstruction: recon,
		CoeffErrorsQ14: errors,
		InverseSSE:     sse,
	}, nil
}

func CoefficientErrorSummary(errors []int64) (absSum int64, squareSum int64) {
	for _, err := range errors {
		if err < 0 {
			absSum -= err
		} else {
			absSum += err
		}
		squareSum += err * err
	}
	return absSum, squareSum
}

func cosineTable(n int) []int64 {
	cosCacheMu.Lock()
	defer cosCacheMu.Unlock()
	if table, ok := cosCache[n]; ok {
		return table
	}
	table := make([]int64, n*n)
	for i := 0; i < n; i++ {
		for k := 0; k < n; k++ {
			angle := math.Pi * (float64(i) + 0.5) * float64(k) / float64(n)
			table[i*n+k] = int64(math.Round(math.Cos(angle) * float64(cosScale)))
		}
	}
	cosCache[n] = table
	return table
}

func roundDiv(value, divisor int64) int64 {
	if divisor == 0 {
		panic(fmt.Sprintf("codec: zero divisor for value %d", value))
	}
	if divisor < 0 {
		value = -value
		divisor = -divisor
	}
	if value >= 0 {
		return (value + divisor/2) / divisor
	}
	return -((-value + divisor/2) / divisor)
}
