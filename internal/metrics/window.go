package metrics

import (
	"vqa/internal/fixed"
	"vqa/internal/sample"
)

// C1/C2/C3 are the standard SSIM stability constants expressed in the 0..255
// pixel domain, converted to Q16.16. All SSIM intermediates stay in fixed
// point; only the frame-mean reporting step converts to float64.
var (
	qC1 = fixed.FromInt(6 * 6)   // (0.01*255)^2 rounded to 7
	qC2 = fixed.FromInt(58 * 58) // (0.03*255)^2 = 58.5225 -> 58
)

// WindowResult is one SSIM window in the deterministic global grid.
type WindowResult struct {
	TopX    int   `json:"top_x"`
	TopY    int   `json:"top_y"`
	OwnerBX int   `json:"owner_bx"`
	OwnerBY int   `json:"owner_by"`
	SSIMR   int64 `json:"ssim_q16"`
}

// WindowGrid enumerates the frame-wide window positions. The same grid is used
// by full-frame evaluation, block evaluation and the verifier, which is what
// makes merged block results identical to single-pass frame results.
type WindowGrid struct {
	Params Params
	Tops   []WindowResult
}

// BuildGrid creates all window top-left positions in raster order and assigns
// each window deterministically to the block containing its top-left pixel.
func BuildGrid(p Params) *WindowGrid {
	g := &WindowGrid{Params: p}
	for ty := 0; ty+p.Window <= p.FrameHeight; ty += p.Stride {
		for tx := 0; tx+p.Window <= p.FrameWidth; tx += p.Stride {
			g.Tops = append(g.Tops, WindowResult{
				TopX:    tx,
				TopY:    ty,
				OwnerBX: tx / p.BlockSize,
				OwnerBY: ty / p.BlockSize,
			})
		}
	}
	return g
}

// CheckBand proves every window that intersects a block lies inside that
// block's halo. The furthest window top-left before a block origin is
// origin-(window-1), so halo must be at least RequiredHalo (already validated)
// and the grid positions are rechecked here for defense in depth.
func (g *WindowGrid) CheckBand() error {
	p := g.Params
	for _, w := range g.Tops {
		originX := w.OwnerBX * p.BlockSize
		originY := w.OwnerBY * p.BlockSize
		if originX-w.TopX > p.Halo || originY-w.TopY > p.Halo {
			return bandError(p)
		}
	}
	return nil
}

func bandError(p Params) error {
	return errBand(p.RequiredHalo(), p.Halo)
}

// EvaluateWindow computes the fixed-point SSIM of one window, channel 0..ch-1
// averaged per pixel location. Truncations inside the fixed point math are
// recorded at stable symbolic locations.
func EvaluateWindow(orig, rec *sample.Frame, tx, ty int, p Params, tracer *fixed.Tracer) fixed.Q {
	k := p.Window
	n := k * k
	var sumO, sumR, sumOO, sumRR, sumOR int64
	for y := ty; y < ty+k; y++ {
		for x := tx; x < tx+k; x++ {
			for c := 0; c < p.Channels; c++ {
				o := int64(orig.At(x, y, c))
				r := int64(rec.At(x, y, c))
				sumO += o
				sumR += r
				sumOO += o * o
				sumRR += r * r
				sumOR += o * r
			}
		}
	}
	samples := int64(n * p.Channels)
	loc := "window.ssim"
	muO := fixed.Div(fixed.FromInt(sumO), fixed.FromInt(samples), loc+".mu_o", tracer)
	muR := fixed.Div(fixed.FromInt(sumR), fixed.FromInt(samples), loc+".mu_r", tracer)
	// Variance = E[x^2] - mu^2 in fixed point.
	eOO := fixed.Div(fixed.FromInt(sumOO), fixed.FromInt(samples), loc+".e_oo", tracer)
	eRR := fixed.Div(fixed.FromInt(sumRR), fixed.FromInt(samples), loc+".e_rr", tracer)
	eOR := fixed.Div(fixed.FromInt(sumOR), fixed.FromInt(samples), loc+".e_or", tracer)
	varO := fixed.Sub(eOO, fixed.Mul(muO, muO, loc+".var_o", tracer))
	varR := fixed.Sub(eRR, fixed.Mul(muR, muR, loc+".var_r", tracer))
	cov := fixed.Sub(eOR, fixed.Mul(muO, muR, loc+".cov", tracer))
	if varO < 0 {
		varO = 0
	}
	if varR < 0 {
		varR = 0
	}
	num1 := fixed.Add(fixed.Mul(fixed.FromInt(2), fixed.Mul(muO, muR, loc+".num1.mu", tracer), loc+".num1.2mu", tracer), qC1)
	den1 := fixed.Add(fixed.Add(fixed.Mul(muO, muO, loc+".den1.mu", tracer), fixed.Mul(muR, muR, loc+".den1.mur", tracer)), qC1)
	num2 := fixed.Add(fixed.Mul(fixed.FromInt(2), cov, loc+".num2.2cov", tracer), qC2)
	den2 := fixed.Add(fixed.Add(varO, varR), qC2)
	num := fixed.Mul(num1, num2, loc+".num", tracer)
	den := fixed.Mul(den1, den2, loc+".den", tracer)
	if den == 0 {
		return fixed.One
	}
	return fixed.Div(num, den, loc+".final", tracer)
}

// EvaluateGrid evaluates every window in raster order. This is the single
// source of truth; block and full-frame paths both call it with the same grid.
func EvaluateGrid(orig, rec *sample.Frame, p Params, tracer *fixed.Tracer) []WindowResult {
	grid := BuildGrid(p)
	out := make([]WindowResult, len(grid.Tops))
	for i, w := range grid.Tops {
		s := EvaluateWindow(orig, rec, w.TopX, w.TopY, p, tracer)
		w.SSIMR = int64(s)
		out[i] = w
	}
	return out
}

// MeanSSIM averages window SSIM values in grid order. Equal window sets yield
// bit-identical means regardless of which block "produced" them.
func MeanSSIM(windows []WindowResult, tracer *fixed.Tracer) fixed.Q {
	if len(windows) == 0 {
		return 0
	}
	var acc fixed.Q
	for _, w := range windows {
		acc = fixed.Add(acc, fixed.Q(w.SSIMR))
	}
	return fixed.Div(acc, fixed.FromInt(int64(len(windows))), "frame.ssim.mean", tracer)
}
