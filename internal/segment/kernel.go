package segment

import (
	"fmt"

	"maskpreview/internal/core"
)

type prepared struct {
	width, height int
	r, g, b       []uint8
}

type ExactResult struct {
	GrowMask       []uint8       `json:"-"`
	Mask           []uint8       `json:"-"`
	LabelMask      []int         `json:"-"`
	Width          int           `json:"width"`
	Height         int           `json:"height"`
	Regions        []RegionStat  `json:"regions"`
	Snapshot       core.Snapshot `json:"snapshot"`
	IterationsUsed int           `json:"iterations_used"`
}

func Exact(img *core.Image, snap core.Snapshot) (*ExactResult, error) {
	return runKernel(img, snap, snap.Params.GrowthIterations)
}

func runKernel(img *core.Image, snap core.Snapshot, iterationLimit int) (*ExactResult, error) {
	if err := img.Validate(); err != nil {
		return nil, err
	}
	if err := snap.Validate(); err != nil {
		return nil, err
	}
	if snap.ImageID != "" && img.ID != "" && snap.ImageID != img.ID {
		return nil, core.MetaError("segment.exact", "image_id", img.ID, snap.ImageID)
	}
	if iterationLimit <= 0 {
		iterationLimit = snap.Params.GrowthIterations
	}
	prep := prepare(img, snap.Quantization)
	grow, used, err := grow(prep, snap.Params, snap.Seeds, iterationLimit)
	if err != nil {
		return nil, err
	}
	thresholded := threshold(grow)
	mask := morphologyOpenClose(thresholded, img.Width, img.Height, snap.Params.KernelSize)
	regions, labels := analyze(mask, img.Width, img.Height)
	return &ExactResult{
		GrowMask:       grow,
		Mask:           mask,
		LabelMask:      labels,
		Width:          img.Width,
		Height:         img.Height,
		Regions:        regions,
		Snapshot:       snap.Clone(),
		IterationsUsed: used,
	}, nil
}

func prepare(img *core.Image, quant core.QuantizationRule) prepared {
	out := prepared{
		width:  img.Width,
		height: img.Height,
		r:      make([]uint8, img.Width*img.Height),
		g:      make([]uint8, img.Width*img.Height),
		b:      make([]uint8, img.Width*img.Height),
	}
	for i := 0; i < img.Width*img.Height; i++ {
		off := i * 4
		out.r[i] = quantize(img.Pix[off], quant.ChannelBits)
		out.g[i] = quantize(img.Pix[off+1], quant.ChannelBits)
		out.b[i] = quantize(img.Pix[off+2], quant.ChannelBits)
	}
	return out
}

func quantize(v uint8, bits int) uint8 {
	if bits >= 8 {
		return v
	}
	step := 1 << (8 - bits)
	return uint8((int(v) / step) * step)
}

func grow(p prepared, params core.Params, seeds []core.Point, iterationLimit int) ([]uint8, int, error) {
	if len(seeds) == 0 {
		return nil, 0, core.ParamError("segment.grow", "seeds", 0, 1, "unbounded")
	}
	size := p.width * p.height
	mask := make([]uint8, size)
	visited := make([]bool, size)
	type queueItem struct {
		x, y int
	}
	queue := make([]queueItem, 0, size)
	seedColors := make([]core.RGB, 0, len(seeds))
	for _, seed := range seeds {
		if seed.X < 0 || seed.Y < 0 || seed.X >= p.width || seed.Y >= p.height {
			return nil, 0, &core.Error{
				Code:    core.CodeParam,
				Op:      "segment.grow",
				Field:   "seeds",
				Got:     fmt.Sprintf("(%d,%d)", seed.X, seed.Y),
				Want:    fmt.Sprintf("[0,%d)x[0,%d)", p.width, p.height),
				Message: "seed point outside image",
			}
		}
		seedColors = append(seedColors, seedReference(p, seed))
	}

	iterationsUsed := 0
	frontier := make([]queueItem, 0, len(seeds))
	for i, seed := range seeds {
		idx := seed.Y*p.width + seed.X
		if visited[idx] {
			continue
		}
		visited[idx] = true
		mask[idx] = 1
		frontier = append(frontier, queueItem{x: seed.X, y: seed.Y})
		_ = i
	}
	for len(frontier) > 0 && iterationsUsed < iterationLimit {
		iterationsUsed++
		queue = append(queue[:0], frontier...)
		frontier = frontier[:0]
		for _, item := range queue {
			curIdx := item.y*p.width + item.x
			cur := core.RGB{R: p.r[curIdx], G: p.g[curIdx], B: p.b[curIdx]}
			for _, d := range [][2]int{{1, 0}, {-1, 0}, {0, 1}, {0, -1}} {
				nx, ny := item.x+d[0], item.y+d[1]
				if nx < 0 || ny < 0 || nx >= p.width || ny >= p.height {
					continue
				}
				ni := ny*p.width + nx
				if visited[ni] {
					continue
				}
				candidate := core.RGB{R: p.r[ni], G: p.g[ni], B: p.b[ni]}
				if edgeGradient(cur, candidate) > params.GradientThreshold {
					continue
				}
				if nearestColorDistance(candidate, seedColors) > params.ColorTolerance {
					continue
				}
				visited[ni] = true
				mask[ni] = 1
				frontier = append(frontier, queueItem{x: nx, y: ny})
			}
		}
	}
	return mask, iterationsUsed, nil
}

func seedReference(p prepared, seed core.Point) core.RGB {
	var sr, sg, sb, count int
	for yy := seed.Y - 1; yy <= seed.Y+1; yy++ {
		if yy < 0 || yy >= p.height {
			continue
		}
		for xx := seed.X - 1; xx <= seed.X+1; xx++ {
			if xx < 0 || xx >= p.width {
				continue
			}
			i := yy*p.width + xx
			sr += int(p.r[i])
			sg += int(p.g[i])
			sb += int(p.b[i])
			count++
		}
	}
	if count == 0 {
		return core.RGB{}
	}
	return core.RGB{R: uint8(sr / count), G: uint8(sg / count), B: uint8(sb / count)}
}

func nearestColorDistance(c core.RGB, seedColors []core.RGB) int {
	best := 1 << 30
	for _, sc := range seedColors {
		d := c.Distance(sc)
		if d < best {
			best = d
		}
	}
	return best
}

func edgeGradient(a, b core.RGB) int {
	dr := abs(int(a.R) - int(b.R))
	dg := abs(int(a.G) - int(b.G))
	db := abs(int(a.B) - int(b.B))
	if dr > dg {
		if dr > db {
			return dr
		}
		return db
	}
	if dg > db {
		return dg
	}
	return db
}

func threshold(mask []uint8) []uint8 {
	out := make([]uint8, len(mask))
	for i, v := range mask {
		if v > 0 {
			out[i] = 1
		}
	}
	return out
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}
