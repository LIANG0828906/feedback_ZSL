package quantforensics

import (
	"fmt"
)

type QuantOutput struct {
	BlockID         string   `json:"blockId"`
	ContentHash     string   `json:"contentHash"`
	Indices         []int    `json:"indices"`
	PixelErrors     [][3]int `json:"pixelErrors"`
	WorkedPixels    []RGB    `json:"workedPixels,omitempty"`
	DitherOffsets   [][3]int `json:"ditherOffsets,omitempty"`
	DitherAmplitude int      `json:"ditherAmplitude"`
	DitherPPM       int64    `json:"ditherPPM"`
	AlgoVersion     string   `json:"algoVersion"`
	ParamVersion    string   `json:"paramVersion"`
}

func metricWeights(metric Metric) [3]int64 {
	switch metric {
	case MetricEuclidean:
		return [3]int64{1, 1, 1}
	case MetricMaxRGB:
		return [3]int64{1, 1, 1}
	default:
		// Slightly favors green sensitivity while remaining integral.
		return [3]int64{2, 4, 3}
	}
}

func ColorDistanceSq(a, b RGB, metric Metric) int64 {
	da := [3]int64{int64(a.R) - int64(b.R), int64(a.G) - int64(b.G), int64(a.B) - int64(b.B)}
	if metric == MetricMaxRGB {
		m := int64(0)
		for _, d := range da {
			if d < 0 {
				d = -d
			}
			if d > m {
				m = d
			}
		}
		return m * m
	}
	w := metricWeights(metric)
	return w[0]*da[0]*da[0] + w[1]*da[1]*da[1] + w[2]*da[2]*da[2]
}

func NearestPaletteIndex(p Palette, c RGB, metric Metric) (int, error) {
	if len(p.Entries) == 0 {
		return 0, schemaError("$.palette.entries", "palette is empty")
	}
	best := 0
	bestDist := int64(1<<62 - 1)
	for i, e := range p.Entries {
		d := ColorDistanceSq(c, e.EffectiveColor(), metric)
		// Strict comparison makes ties resolve to the lower stable index.
		if d < bestDist {
			bestDist = d
			best = i
		}
	}
	return best, nil
}

func splitMix64(x uint64) uint64 {
	x += 0x9e3779b97f4a7c15
	z := x
	z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9
	z = (z ^ (z >> 27)) * 0x94d049bb133111eb
	return z ^ (z >> 31)
}

func ditherOffset(seed uint64, pixel, channel, amplitude int) int {
	if amplitude <= 0 {
		return 0
	}
	h := splitMix64(seed ^ uint64(pixel)*0x9e3779b185ebca87 ^ uint64(channel+1)*0xc2b2ae3d27d4eb4f)
	span := uint64(2*amplitude + 1)
	return int(h%span) - amplitude
}

func clampRGB(r, g, b int) RGB {
	clamp := func(v int) uint8 {
		if v < 0 {
			return 0
		}
		if v > 255 {
			return 255
		}
		return uint8(v)
	}
	return RGB{R: clamp(r), G: clamp(g), B: clamp(b)}
}

// QuantizeBlock is a pure function of image pixels, block coordinates, palette
// and parameters. It deliberately has no global rand or map-order dependency.
func QuantizeBlock(im Image, block Block, palette Palette, params Params) (QuantOutput, error) {
	return QuantizeBlockWithBuffer(im, block, palette, params, nil)
}

// QuantizeBlockWithBuffer accepts a reusable per-worker error buffer. The
// buffer is only used as scratch and is never retained in QuantOutput.
func QuantizeBlockWithBuffer(im Image, block Block, palette Palette, params Params, providedErrBuf [][3]int) (QuantOutput, error) {
	if block.W <= 0 || block.H <= 0 {
		return QuantOutput{}, schemaError("$.blocks."+block.ID, "block has invalid dimensions")
	}
	if block.X < 0 || block.Y < 0 || block.X+block.W > im.Width || block.Y+block.H > im.Height {
		return QuantOutput{}, schemaError("$.blocks."+block.ID, "block %+v is outside image %s %dx%d", block.Coordinate(), im.ID, im.Width, im.Height)
	}
	if len(palette.Entries) == 0 {
		return QuantOutput{}, schemaError("$.palette.entries", "palette is empty")
	}
	metric := params.Metric
	if metric == "" {
		metric = MetricWeightedRGB
	}
	ditherPPM := params.GlobalDitherPPM
	if block.HasDitherOverride {
		ditherPPM = block.DitherPPM
	}
	if ditherPPM < 0 {
		return QuantOutput{}, schemaError("$.params.ditherPPM", "dither must be non-negative")
	}
	amplitude := int(ditherPPM * 255 / 1_000_000)
	contentHash := BlockContentHash(im, block)
	seed := StableHash(contentHash, AlgoVersion, params.ParamVersion, string(metric), fmt.Sprint(ditherPPM))

	n := block.W * block.H
	out := QuantOutput{
		BlockID:         block.ID,
		ContentHash:     contentHash,
		Indices:         make([]int, n),
		PixelErrors:     make([][3]int, n),
		WorkedPixels:    make([]RGB, n),
		DitherOffsets:   make([][3]int, n),
		DitherAmplitude: amplitude,
		DitherPPM:       ditherPPM,
		AlgoVersion:     AlgoVersion,
		ParamVersion:    params.ParamVersion,
	}
	var errBuf [][3]int
	if cap(providedErrBuf) >= n {
		errBuf = providedErrBuf[:n]
		for i := range errBuf {
			errBuf[i] = [3]int{}
		}
	} else {
		errBuf = make([][3]int, n)
	}

	for y := 0; y < block.H; y++ {
		for x := 0; x < block.W; x++ {
			local := y*block.W + x
			src := im.At(block.X+x, block.Y+y)
			original := [3]int{int(src.R), int(src.G), int(src.B)}
			offsets := [3]int{
				ditherOffset(seed, local, 0, amplitude),
				ditherOffset(seed, local, 1, amplitude),
				ditherOffset(seed, local, 2, amplitude),
			}
			out.DitherOffsets[local] = offsets
			worked := clampRGB(
				original[0]+errBuf[local][0]+offsets[0],
				original[1]+errBuf[local][1]+offsets[1],
				original[2]+errBuf[local][2]+offsets[2],
			)
			out.WorkedPixels[local] = worked
			idx, err := NearestPaletteIndex(palette, worked, metric)
			if err != nil {
				return QuantOutput{}, err
			}
			selected := palette.Entries[idx].EffectiveColor()
			selectedInts := [3]int{int(selected.R), int(selected.G), int(selected.B)}
			out.Indices[local] = idx
			for c := 0; c < 3; c++ {
				out.PixelErrors[local][c] = original[c] - selectedInts[c]
				spreadError := int(worked.Component(componentName(c))) - selectedInts[c]
				distributeError(errBuf, block.W, block.H, x, y, c, spreadError)
			}
		}
	}
	return out, nil
}

func componentName(c int) string {
	switch c {
	case 0:
		return "R"
	case 1:
		return "G"
	default:
		return "B"
	}
}

// distributeError uses Floyd-Steinberg weights with denominator 16. Processing
// order is always raster order inside one isolated block buffer.
func distributeError(buf [][3]int, w, h, x, y, channel, qerr int) {
	add := func(dx, dy, weight int) {
		nx, ny := x+dx, y+dy
		if nx < 0 || nx >= w || ny >= h {
			return
		}
		buf[ny*w+nx][channel] += qerr * weight / 16
	}
	add(1, 0, 7)
	add(-1, 1, 3)
	add(0, 1, 5)
	add(1, 1, 1)
}
