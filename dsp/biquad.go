package dsp

import (
	"math"

	"loudnessworkbench/apperr"
)

type BiquadCoefficients struct {
	B0 float64
	B1 float64
	B2 float64
	A1 float64
	A2 float64
}

// KWeightingCoefficients implements the two BS.1770 shelving/high-pass stages.
func KWeightingCoefficients(sampleRate int) (BiquadCoefficients, BiquadCoefficients, error) {
	if !SupportedSampleRate(sampleRate) {
		return BiquadCoefficients{}, BiquadCoefficients{}, apperr.New(apperr.CodeParameter, "unsupported sample rate").
			WithField("sample_rate_hz").WithActual(sampleRate)
	}
	fs := float64(sampleRate)

	const (
		shelfF0 = 1681.974450955533
		shelfQ  = 0.7071752369554196
		shelfG  = 3.999843853973347
	)
	k := math.Tan(math.Pi * shelfF0 / fs)
	vh := math.Pow(10, shelfG/20)
	vb := math.Pow(vh, 0.4996667741545416)
	den := 1 + k/shelfQ + k*k
	shelf := BiquadCoefficients{
		B0: (vh + vb*k/shelfQ + k*k) / den,
		B1: 2 * (k*k - vh) / den,
		B2: (vh - vb*k/shelfQ + k*k) / den,
		A1: 2 * (k*k - 1) / den,
		A2: (1 - k/shelfQ + k*k) / den,
	}

	const (
		highPassF0 = 38.13547087602444
		highPassQ  = 0.5003270373238773
	)
	k = math.Tan(math.Pi * highPassF0 / fs)
	den = 1 + k/highPassQ + k*k
	highPass := BiquadCoefficients{
		B0: 1,
		B1: -2,
		B2: 1,
		A1: 2 * (k*k - 1) / den,
		A2: (1 - k/highPassQ + k*k) / den,
	}
	return shelf, highPass, nil
}

type biquad struct {
	c  BiquadCoefficients
	z1 float64
	z2 float64
}

func (b *biquad) Process(x float64) float64 {
	y := b.c.B0*x + b.z1
	b.z1 = b.c.B1*x - b.c.A1*y + b.z2
	b.z2 = b.c.B2*x - b.c.A2*y
	return y
}

type kFilter struct {
	shelf    biquad
	highPass biquad
}

func newKFilter(sampleRate int) (kFilter, error) {
	shelf, highPass, err := KWeightingCoefficients(sampleRate)
	if err != nil {
		return kFilter{}, err
	}
	return kFilter{shelf: biquad{c: shelf}, highPass: biquad{c: highPass}}, nil
}

func (f *kFilter) Process(x float64) float64 {
	return f.highPass.Process(f.shelf.Process(x))
}
