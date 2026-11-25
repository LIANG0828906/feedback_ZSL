package samplegen

import (
	"math"
	"math/rand"

	"soundlab/audiofp"
)

type profile struct {
	amp [audiofp.MaxBands]float64
}

func bandCenterBin(band, bands, frameLen int) int {
	n := nextPowerOfTwo(frameLen)
	positive := n/2 - 1
	bin := 1 + (band*positive)/bands + positive/(2*bands)
	if bin < 1 {
		bin = 1
	}
	if bin > positive {
		bin = positive
	}
	return bin
}

func makeProfiles(rng *rand.Rand, blocks, bands int) []profile {
	out := make([]profile, blocks)
	for i := range out {
		activeA := rng.Intn(bands)
		activeB := rng.Intn(bands)
		out[i].amp[activeA] += 0.22 + rng.Float64()*0.16
		out[i].amp[activeB] += 0.15 + rng.Float64()*0.13
		for band := 0; band < bands; band++ {
			if rng.Intn(5) == 0 {
				out[i].amp[band] += rng.Float64() * 0.06
			}
		}
	}
	return out
}

func synthAsset(id string, sampleRate, totalSamples int, r audiofp.Recipe, rng *rand.Rand,
	markerBlocks map[int]bool, silentTail int) audiofp.PCMAsset {
	hop := r.FrameShift
	bands := r.Bands
	blocks := totalSamples/hop + 3
	profiles := makeProfiles(rng, blocks, bands)
	for block := range markerBlocks {
		if block >= 0 && block < len(profiles) {
			band := bands - 1
			profiles[block].amp[band] += 0.38
		}
	}
	phases := make([]float64, bands)
	freqs := make([]float64, bands)
	for band := 0; band < bands; band++ {
		phases[band] = rng.Float64() * 2 * math.Pi
		bin := bandCenterBin(band, bands, r.FrameLen)
		freqs[band] = float64(bin) * float64(sampleRate) / float64(nextPowerOfTwo(r.FrameLen))
	}
	samples := make([]int16, totalSamples)
	silentFrom := totalSamples - silentTail
	for n := 0; n < totalSamples; n++ {
		if silentTail > 0 && n >= silentFrom {
			continue
		}
		block := n / hop
		next := block + 1
		if next >= len(profiles) {
			next = block
		}
		t := float64(n%hop) / float64(hop)
		v := 0.0
		for band := 0; band < bands; band++ {
			amp := profiles[block].amp[band]*(1-t) + profiles[next].amp[band]*t
			if amp == 0 {
				continue
			}
			v += amp * math.Sin(2*math.Pi*freqs[band]*float64(n)/float64(sampleRate)+phases[band])
		}
		// A fixed low-level shaped component prevents accidental digital silence
		// in non-silent regions while keeping band ratios profile-driven.
		v += 0.002 * math.Sin(2*math.Pi*37*float64(n)/float64(sampleRate))
		if v > 0.98 {
			v = 0.98
		}
		if v < -0.98 {
			v = -0.98
		}
		samples[n] = int16(math.Round(v * 32767))
	}
	return audiofp.PCMAsset{ID: id, SampleRate: sampleRate, Samples: samples}
}

func synthSegment(totalSamples int, sampleRate int, r audiofp.Recipe, rng *rand.Rand) []int16 {
	asset := synthAsset("replacement", sampleRate, totalSamples, r, rng, nil, 0)
	return asset.Samples
}
