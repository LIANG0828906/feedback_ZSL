package metrics

import (
	"math"

	"qkernel/internal/errs"
	"qkernel/internal/imagex"
)

type Block struct {
	Index         int         `json:"index"`
	X             int         `json:"x"`
	Y             int         `json:"y"`
	Rect          imagex.Rect `json:"rect"`
	NoiseVariance float64     `json:"noise_variance"`
	EdgeClarity   float64     `json:"edge_clarity"`
	ChromaCbMean  float64     `json:"chroma_cb_mean"`
	ChromaCrMean  float64     `json:"chroma_cr_mean"`
	DeltaNoise    float64     `json:"delta_noise,omitempty"`
	DeltaEdge     float64     `json:"delta_edge,omitempty"`
	DeltaChroma   float64     `json:"delta_chroma,omitempty"`
}

type Report struct {
	BlockSize int     `json:"block_size"`
	Width     int     `json:"width"`
	Height    int     `json:"height"`
	Blocks    []Block `json:"blocks"`
}

func Compute(f *imagex.Frame, blockSize int) (*Report, error) {
	if blockSize <= 0 {
		return nil, &errs.Error{Code: errs.EPara, Message: "block_size must be positive", Field: "metric_block_size", StepIndex: -1, BlockX: -1, BlockY: -1}
	}
	if f.Width%blockSize != 0 || f.Height%blockSize != 0 {
		return nil, &errs.Error{Code: errs.EPara, Message: "block_size must divide frame dimensions", Field: "metric_block_size", StepIndex: -1, BlockX: -1, BlockY: -1}
	}
	luma := make([]float64, f.Width*f.Height)
	cb := make([]float64, f.Width*f.Height)
	cr := make([]float64, f.Width*f.Height)
	for y := 0; y < f.Height; y++ {
		for x := 0; x < f.Width; x++ {
			r, g, b := f.RGBAtClamped(x, y)
			rf, gf, bf := float64(r), float64(g), float64(b)
			i := y*f.Width + x
			luma[i] = 0.299*rf + 0.587*gf + 0.114*bf
			cb[i] = 128 - 0.168736*rf - 0.331264*gf + 0.5*bf
			cr[i] = 128 + 0.5*rf - 0.418688*gf - 0.081312*bf
		}
	}

	rep := &Report{BlockSize: blockSize, Width: f.Width, Height: f.Height}
	idx := 0
	for by := 0; by < f.Height; by += blockSize {
		for bx := 0; bx < f.Width; bx += blockSize {
			b := Block{Index: idx, X: bx / blockSize, Y: by / blockSize, Rect: imagex.R(bx, by, bx+blockSize, by+blockSize)}
			var residualSum, residualSq, edgeSum, cbSum, crSum, n float64
			for y := by; y < by+blockSize; y++ {
				for x := bx; x < bx+blockSize; x++ {
					center := luma[y*f.Width+x]
					var blurSum float64
					var blurN float64
					for dy := -1; dy <= 1; dy++ {
						for dx := -1; dx <= 1; dx++ {
							xx, yy := f.ClampXY(x+dx, y+dy)
							blurSum += luma[yy*f.Width+xx]
							blurN++
						}
					}
					res := center - blurSum/blurN
					residualSum += res
					residualSq += res * res

					tl := lumaAt(luma, f, x-1, y-1)
					tc := lumaAt(luma, f, x, y-1)
					tr := lumaAt(luma, f, x+1, y-1)
					ml := lumaAt(luma, f, x-1, y)
					mr := lumaAt(luma, f, x+1, y)
					bl := lumaAt(luma, f, x-1, y+1)
					bc := lumaAt(luma, f, x, y+1)
					br := lumaAt(luma, f, x+1, y+1)
					gx := -tl - 2*ml - bl + tr + 2*mr + br
					gy := -tl - 2*tc - tr + bl + 2*bc + br
					edgeSum += math.Hypot(gx, gy)

					cbSum += cb[y*f.Width+x]
					crSum += cr[y*f.Width+x]
					n++
				}
			}
			meanRes := residualSum / n
			b.NoiseVariance = residualSq/n - meanRes*meanRes
			if b.NoiseVariance < 0 && b.NoiseVariance > -1e-9 {
				b.NoiseVariance = 0
			}
			b.EdgeClarity = edgeSum / n
			b.ChromaCbMean = cbSum / n
			b.ChromaCrMean = crSum / n
			rep.Blocks = append(rep.Blocks, b)
			idx++
		}
	}
	return rep, nil
}

func lumaAt(luma []float64, f *imagex.Frame, x, y int) float64 {
	x, y = f.ClampXY(x, y)
	return luma[y*f.Width+x]
}

func Compare(base, degraded *Report) (*Report, error) {
	if base.BlockSize != degraded.BlockSize || len(base.Blocks) != len(degraded.Blocks) {
		return nil, errs.New(errs.EMeta, "metric reports are not comparable")
	}
	out := *degraded
	out.Blocks = append([]Block(nil), degraded.Blocks...)
	for i := range out.Blocks {
		b := base.Blocks[i]
		d := &out.Blocks[i]
		d.DeltaNoise = d.NoiseVariance - b.NoiseVariance
		d.DeltaEdge = d.EdgeClarity - b.EdgeClarity
		dcb := d.ChromaCbMean - b.ChromaCbMean
		dcr := d.ChromaCrMean - b.ChromaCrMean
		d.DeltaChroma = math.Hypot(dcb, dcr)
	}
	return &out, nil
}
