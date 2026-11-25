package audiofp

import "math"

// SynthesizeToneSample renders one deterministic characteristic-tone sample.
// The same timbre is used by the fixture generator and category templates.
func SynthesizeToneSample(tone ToneParams, tSeconds float64) float64 {
	amplitude := tone.Amplitude
	if amplitude == 0 {
		amplitude = 0.7
	}
	attack := math.Min(1.0, tSeconds/0.005)
	envelope := math.Exp(-tone.Decay * tSeconds)
	phase := 2.0 * math.Pi * tone.FrequencyHz * tSeconds
	wave := 0.72*math.Sin(phase) +
		0.20*math.Sin(2.0*phase) +
		0.08*math.Sin(3.0*phase)
	return amplitude * attack * envelope * wave
}
