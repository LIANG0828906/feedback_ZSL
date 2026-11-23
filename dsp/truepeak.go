package dsp

import (
	"math"

	"loudnessworkbench/audio"
)

const (
	truePeakOversample = 4
	truePeakRadius     = 8
)

var truePeakWeights [truePeakOversample][truePeakRadius * 2]float64

func init() {
	for phase := 1; phase < truePeakOversample; phase++ {
		fraction := float64(phase) / float64(truePeakOversample)
		sum := 0.0
		for tap := -truePeakRadius + 1; tap <= truePeakRadius; tap++ {
			distance := fraction - float64(tap)
			weight := sinc(distance) * blackman(distance/truePeakRadius)
			truePeakWeights[phase][tap+truePeakRadius-1] = weight
			sum += weight
		}
		if math.Abs(sum) > 1e-12 {
			for tap := range truePeakWeights[phase] {
				truePeakWeights[phase][tap] /= sum
			}
		}
	}
}

// TruePeakLinear estimates the inter-sample peak with 4x windowed-sinc
// interpolation. The same estimator is used for samples, truth and checks.
func TruePeakLinear(s audio.Signal) float64 {
	peak := audio.MaxAbs(s)
	if s.Frames() < 2 {
		return peak
	}
	frames := s.Frames()
	channels := s.Channels
	for channel := 0; channel < s.Channels; channel++ {
		for n := 0; n < frames; n++ {
			for phase := 1; phase < truePeakOversample; phase++ {
				sum := 0.0
				for tap := 0; tap < truePeakRadius*2; tap++ {
					source := reflectIndex(n+tap-truePeakRadius+1, frames)
					sum += s.Data[source*channels+channel] * truePeakWeights[phase][tap]
				}
				if value := math.Abs(sum); value > peak {
					peak = value
				}
			}
		}
	}
	return peak
}

func TruePeakDBTP(s audio.Signal) float64 {
	return LinearToDB(TruePeakLinear(s))
}

func SamplePeakDBFS(s audio.Signal) float64 {
	return LinearToDB(audio.MaxAbs(s))
}

func reflectIndex(index, frames int) int {
	if frames <= 1 {
		return 0
	}
	for index < 0 || index >= frames {
		if index < 0 {
			index = -index - 1
		}
		if index >= frames {
			index = 2*frames - index - 1
		}
	}
	return index
}

func sinc(x float64) float64 {
	if math.Abs(x) < 1e-12 {
		return 1
	}
	px := math.Pi * x
	return math.Sin(px) / px
}

func blackman(x float64) float64 {
	if math.Abs(x) > 1 {
		return 0
	}
	return 0.42 + 0.5*math.Cos(math.Pi*x) + 0.08*math.Cos(2*math.Pi*x)
}
