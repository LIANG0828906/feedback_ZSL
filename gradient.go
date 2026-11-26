package maskqa

import "fmt"

type convolutionKernel struct {
	Size int
	GX   [][]int
	GY   [][]int
}

func gradientKernel(operator string, size int) (convolutionKernel, error) {
	switch {
	case operator == "sobel3" && size == 3:
		gx := [][]int{{-1, 0, 1}, {-2, 0, 2}, {-1, 0, 1}}
		return convolutionKernel{Size: 3, GX: gx, GY: transposeKernel(gx)}, nil
	case operator == "prewitt3" && size == 3:
		gx := [][]int{{-1, 0, 1}, {-1, 0, 1}, {-1, 0, 1}}
		return convolutionKernel{Size: 3, GX: gx, GY: transposeKernel(gx)}, nil
	case operator == "sobel5" && size == 5:
		gx := [][]int{
			{-5, -4, 0, 4, 5},
			{-8, -10, 0, 10, 8},
			{-10, -20, 0, 20, 10},
			{-8, -10, 0, 10, 8},
			{-5, -4, 0, 4, 5},
		}
		return convolutionKernel{Size: 5, GX: gx, GY: transposeKernel(gx)}, nil
	default:
		return convolutionKernel{}, paraError("gradient_operator", "sobel3/prewitt3 size=3 or sobel5 size=5", "declared operator/size", fmt.Sprintf("%s@%d", operator, size), "unsupported gradient operator")
	}
}

func transposeKernel(k [][]int) [][]int {
	out := make([][]int, len(k))
	for i := range k {
		out[i] = make([]int, len(k[i]))
		for j := range k[i] {
			out[i][j] = k[j][i]
		}
	}
	return out
}

func ComputeGradientBlock(s *Sample, p Params, block BlockCoord) (*GradientOutput, error) {
	if err := p.ValidateGradient(); err != nil {
		return nil, err
	}
	kernel, err := gradientKernel(p.Operator, p.OperatorSize)
	if err != nil {
		return nil, err
	}
	out := &GradientOutput{Block: block, Width: block.Width, Height: block.Height, Magnitude: make([]int, block.Width*block.Height)}
	half := kernel.Size / 2
	for ly := 0; ly < block.Height; ly++ {
		for lx := 0; lx < block.Width; lx++ {
			x := block.OriginX + lx
			y := block.OriginY + ly
			luma, lumaRem, lumaNum := lumaAt(s.Raster, p, x, y)
			var gxQ, gyQ int64
			for ky := 0; ky < kernel.Size; ky++ {
				for kx := 0; kx < kernel.Size; kx++ {
					nx := x + kx - half
					ny := y + ky - half
					neighborLuma, _, _ := lumaAt(s.Raster, p, nx, ny)
					lumaQ := int64(neighborLuma) * p.FixedScale
					gxQ += int64(kernel.GX[ky][kx]) * lumaQ
					gyQ += int64(kernel.GY[ky][kx]) * lumaQ
				}
			}
			sumSq := gxQ*gxQ + gyQ*gyQ
			magQ := isqrt64(sumSq)
			magnitude := int(magQ / p.FixedScale)
			magRem := magQ % p.FixedScale
			idx := ly*block.Width + lx
			out.Magnitude[idx] = magnitude
			lumaMicro := lumaRem * p.TraceScale / 1000
			magMicro := magRem * p.TraceScale / p.FixedScale
			trace := PixelTrace{
				X: x, Y: y, BlockX: block.IndexX, BlockY: block.IndexY,
				GradientMicro:     lumaMicro + magMicro,
				GradientMagnitude: magnitude,
				Truncations: []Truncation{
					{Level: "gradient", X: x, Y: y, Operation: "luma_permille_division", RawNumerator: lumaNum, Quotient: int64(luma), Remainder: lumaRem, Denominator: 1000, ContributionMicro: lumaMicro, TruncatedAt: "weighted_rgb_sum/1000"},
					{Level: "gradient", X: x, Y: y, Operation: "euclidean_fixed_sqrt_scale_floor", RawNumerator: sumSq, Quotient: int64(magnitude), Remainder: magRem, Denominator: p.FixedScale, ContributionMicro: magMicro, TruncatedAt: "isqrt(gx_q^2+gy_q^2)/fixed_scale"},
				},
			}
			out.Traces = append(out.Traces, trace)
		}
	}
	return out, nil
}

func lumaAt(r *Raster, p Params, x, y int) (int, int64, int64) {
	x = clampInt(x, 0, r.Width-1)
	y = clampInt(y, 0, r.Height-1)
	rr, gg, bb, _ := r.RGBAAt(x, y)
	num := int64(rr)*int64(p.LumaWeightR) + int64(gg)*int64(p.LumaWeightG) + int64(bb)*int64(p.LumaWeightB)
	return int(num / 1000), num % 1000, num
}

func isqrt64(n int64) int64 {
	if n <= 0 {
		return 0
	}
	x := n/2 + 1
	for x > n/x {
		x = (x + n/x) / 2
	}
	for x+1 > 0 && x+1 <= n/(x+1) {
		x++
	}
	return x
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
