package samplegen

import (
	"math"

	"soundlab/audiofp"
)

// This file deliberately contains a generator-local spectral reference. It is
// not the production extractor: truth is derived from the generator's own
// synthesis and reference transform.

func nextPowerOfTwo(n int) int {
	p := 1
	for p < n {
		p <<= 1
	}
	return p
}

func fft(re, im []float64) {
	n := len(re)
	for i, j := 1, 0; i < n; i++ {
		bit := n >> 1
		for ; j&bit != 0; bit >>= 1 {
			j ^= bit
		}
		j ^= bit
		if i < j {
			re[i], re[j] = re[j], re[i]
			im[i], im[j] = im[j], im[i]
		}
	}
	for length := 2; length <= n; length <<= 1 {
		angle := -2 * math.Pi / float64(length)
		wr0, wi0 := math.Cos(angle), math.Sin(angle)
		for i := 0; i < n; i += length {
			wr, wi := 1.0, 0.0
			for j := 0; j < length/2; j++ {
				ur, ui := re[i+j], im[i+j]
				vr := re[i+j+length/2]*wr - im[i+j+length/2]*wi
				vi := re[i+j+length/2]*wi + im[i+j+length/2]*wr
				re[i+j], im[i+j] = ur+vr, ui+vi
				re[i+j+length/2], im[i+j+length/2] = ur-vr, ui-vi
				wr, wi = wr*wr0-wi*wi0, wr*wi0+wi*wr0
			}
		}
	}
}

type referenceResult struct {
	frames    []audiofp.FrameTruth
	minMargin float64
}

func referenceFrames(asset audiofp.PCMAsset, r audiofp.Recipe) referenceResult {
	count := 0
	if len(asset.Samples) >= r.FrameLen {
		count = (len(asset.Samples)-r.FrameLen)/r.FrameShift + 1
	}
	out := make([]audiofp.FrameTruth, 0, count)
	minMargin := 1.0
	nFFT := nextPowerOfTwo(r.FrameLen)
	positive := nFFT/2 - 1
	limit := float64((int(1) << uint(r.QuantBits)) - 1)
	if r.QuantBits >= 16 {
		limit = 65535
	}
	for frameNo := 0; frameNo < count; frameNo++ {
		start := frameNo * r.FrameShift
		re := make([]float64, nFFT)
		im := make([]float64, nFFT)
		for i := 0; i < r.FrameLen; i++ {
			re[i] = float64(asset.Samples[start+i]) / 32768.0
		}
		fft(re, im)
		energies := make([]float64, r.Bands)
		total := 0.0
		for bin := 1; bin <= positive; bin++ {
			band := (bin - 1) * r.Bands / positive
			p := re[bin]*re[bin] + im[bin]*im[bin]
			energies[band] += p
			total += p
		}
		levels := make([]uint8, r.Bands)
		zero := total <= 1e-18
		if !zero {
			for band, energy := range energies {
				scaled := energy / total * limit
				nearest := math.Round(scaled)
				margin := math.Abs(scaled - nearest)
				if margin < minMargin {
					minMargin = margin
				}
				v := int(math.Floor(scaled + 0.5))
				if v < 0 {
					v = 0
				}
				if v > int(limit) {
					v = int(limit)
				}
				levels[band] = uint8(v)
			}
		}
		packed := audiofp.PackLevels(levels, r.QuantBits)
		out = append(out, audiofp.FrameTruth{
			FrameNo: frameNo, StartSample: start, Bands: r.Bands, BitWidth: r.QuantBits,
			Levels: levels, PackedHex: packed, Zero: zero,
			Summary: audiofp.FrameSummary(start, levels, r.QuantBits, zero),
		})
	}
	return referenceResult{frames: out, minMargin: minMargin}
}

func similarityFromLevels(a, b []uint8, bits int) float64 {
	denom := float64(len(a) * ((1 << bits) - 1))
	if bits >= 16 {
		denom = float64(len(a) * 65535)
	}
	if denom == 0 {
		return 0
	}
	dist := 0
	for i := range a {
		d := int(a[i]) - int(b[i])
		if d < 0 {
			d = -d
		}
		dist += d
	}
	v := 1 - float64(dist)/denom
	return math.Round(v*1e12) / 1e12
}

func rankTruth(query []audiofp.FrameTruth, assets []audiofp.AssetTruth, r audiofp.Recipe,
	threshold float64, limit int) []audiofp.RankedTruth {
	type candidate struct {
		asset string
		start int
		end   int
		sim   float64
	}
	var candidates []candidate
	for _, asset := range assets {
		if len(asset.Frames) < len(query) {
			continue
		}
		for start := 0; start+len(query) <= len(asset.Frames); start++ {
			dist := 0
			for i := range query {
				for band := range query[i].Levels {
					d := int(query[i].Levels[band]) - int(asset.Frames[start+i].Levels[band])
					if d < 0 {
						d = -d
					}
					dist += d
				}
			}
			denom := float64(len(query) * r.Bands * ((1 << r.QuantBits) - 1))
			if r.QuantBits >= 16 {
				denom = float64(len(query) * r.Bands * 65535)
			}
			sim := math.Round((1-float64(dist)/denom)*1e12) / 1e12
			if sim+1e-15 < threshold {
				continue
			}
			sampleStart := asset.Frames[start].StartSample
			sampleEnd := sampleStart + (len(query)-1)*r.FrameShift + r.FrameLen
			candidates = append(candidates, candidate{
				asset: asset.AssetID, start: sampleStart, end: sampleEnd, sim: sim,
			})
		}
	}
	for i := 0; i < len(candidates); i++ {
		for j := i + 1; j < len(candidates); j++ {
			swap := false
			if candidates[j].sim > candidates[i].sim {
				swap = true
			} else if candidates[j].sim == candidates[i].sim {
				if candidates[j].asset < candidates[i].asset ||
					(candidates[j].asset == candidates[i].asset && candidates[j].start < candidates[i].start) {
					swap = true
				}
			}
			if swap {
				candidates[i], candidates[j] = candidates[j], candidates[i]
			}
		}
	}
	if len(candidates) > limit {
		candidates = candidates[:limit]
	}
	out := make([]audiofp.RankedTruth, len(candidates))
	for i, c := range candidates {
		out[i] = audiofp.RankedTruth{
			Rank: i + 1, AssetID: c.asset, StartSample: c.start,
			EndSample: c.end, Similarity: c.sim,
		}
	}
	return out
}
