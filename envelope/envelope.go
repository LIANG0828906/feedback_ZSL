package envelope

import (
	"math"

	"dialoguefit/model"
)

const AlgorithmVersion = "envelope-rms-centered-v1"

func FrameCount(sampleCount, frameLength, frameShift int) int {
	if sampleCount < frameLength || frameLength <= 0 || frameShift <= 0 {
		return 0
	}
	return (sampleCount-frameLength)/frameShift + 1
}

// Extract returns the globally normalized, centered-smoothed RMS envelope.
// The calculation has no map-order or goroutine-dependent reduction: callers
// may compute raw frames concurrently and then invoke Reduce deterministically.
func Extract(d model.Dialogue, cfg model.EnvelopeConfig) ([]model.Frame, error) {
	count := FrameCount(len(d.PCM.Samples), cfg.FrameLength, cfg.FrameShift)
	if count == 0 {
		return nil, model.DecodeError(d.ID, "envelope", "PCM is shorter than one frame or frame parameters are invalid")
	}
	raw := RawFrames(d.PCM.Samples, count, cfg.FrameLength, cfg.FrameShift)
	smoothed := Smooth(raw, cfg.SmoothWindow)
	values := Normalize(smoothed)
	frames := make([]model.Frame, count)
	for i := 0; i < count; i++ {
		start := i * cfg.FrameShift
		frames[i] = model.Frame{
			Index:        i,
			StartSample:  start,
			CenterSample: start + cfg.FrameLength/2,
			EndSample:    start + cfg.FrameLength,
			Value:        values[i],
		}
	}
	return frames, nil
}

func RawFrames(samples []int16, count, length, shift int) []float64 {
	raw := make([]float64, count)
	for i := 0; i < count; i++ {
		raw[i] = RMS(samples[i*shift : min(i*shift+length, len(samples))])
	}
	return raw
}

func RMS(chunk []int16) float64 {
	var sumSquares int64
	for _, sample := range chunk {
		v := int64(sample)
		sumSquares += v * v
	}
	return math.Sqrt(float64(sumSquares)/float64(len(chunk))) / 32768.0
}

func Smooth(raw []float64, window int) []float64 {
	if window <= 1 || len(raw) == 0 {
		return append([]float64(nil), raw...)
	}
	leftRadius := (window - 1) / 2
	rightRadius := window / 2
	out := make([]float64, len(raw))
	for i := range raw {
		lo := max(0, i-leftRadius)
		hi := min(len(raw)-1, i+rightRadius)
		sum := 0.0
		n := 0
		for j := lo; j <= hi; j++ {
			sum += raw[j]
			n++
		}
		out[i] = sum / float64(n)
	}
	return out
}

func Normalize(values []float64) []float64 {
	out := make([]float64, len(values))
	maxValue := 0.0
	for _, v := range values {
		if v > maxValue {
			maxValue = v
		}
	}
	if maxValue == 0 {
		return out
	}
	for i, v := range values {
		out[i] = v / maxValue
	}
	return out
}

func FramesForPhoneme(frames []model.Frame, ph model.Phoneme) []model.Frame {
	out := make([]model.Frame, 0)
	for _, f := range frames {
		if f.CenterSample >= ph.StartSample && f.CenterSample < ph.EndSample {
			out = append(out, f)
		}
	}
	return out
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
