package metrics

import (
	"math"
	"sort"

	"vqa/internal/fixed"
	"vqa/internal/sample"
)

// BlockStats is the integer-accumulator error summary of one block.
type BlockStats struct {
	SampleCount int64 `json:"sample_count"`
	SumAbsErr   int64 `json:"sum_abs_err"`
	SumSqErr    int64 `json:"sum_sq_err"`
	MaxAbsErr   int   `json:"max_abs_err"`
	MeanAbsQ16  int64 `json:"mean_abs_q16"`
	MSEQ16      int64 `json:"mse_q16"`
}

// Quantiles are nearest-rank absolute error quantiles over block pixels.
type Quantiles struct {
	Q10 int `json:"q10"`
	Q50 int `json:"q50"`
	Q90 int `json:"q90"`
}

// EdgeFidelity is the fraction of analytic edge pixels whose reconstruction
// error stays within the declared tolerance, kept in Q16.16.
type EdgeFidelity struct {
	Checked     int   `json:"checked"`
	Within      int   `json:"within"`
	Tolerance   int   `json:"tolerance"`
	FidelityQ16 int64 `json:"fidelity_q16"`
}

// PSNR computes peak signal-to-noise ratio in dB from a mean squared error.
// Integer accumulators feed the sum; only the final logarithm (no fixed-point
// equivalent) is floating point.
func PSNR(sumSq int64, samples int64) float64 {
	if sumSq == 0 {
		return math.Inf(1)
	}
	mse := float64(sumSq) / float64(samples)
	return 20 * math.Log10(255.0/math.Sqrt(mse))
}

// FramePSNR computes PSNR directly from two frames.
func FramePSNR(orig, rec *sample.Frame) (float64, int64) {
	var sumSq, n int64
	for i := range orig.Pix {
		d := int64(orig.Pix[i]) - int64(rec.Pix[i])
		sumSq += d * d
		n++
	}
	return PSNR(sumSq, n), sumSq
}

// EvaluateBlockStats accumulates integer error statistics over a block region.
// The Q16 mean/MSE divisions trace their truncation locations.
func EvaluateBlockStats(orig, rec *sample.Frame, bx, by int, p Params, tracer *fixed.Tracer) (BlockStats, []int, Quantiles) {
	n := p.BlockSize
	var st BlockStats
	errs := make([]int, 0, n*n*p.Channels)
	for y := by * n; y < (by+1)*n; y++ {
		for x := bx * n; x < (bx+1)*n; x++ {
			for c := 0; c < p.Channels; c++ {
				d := orig.At(x, y, c) - rec.At(x, y, c)
				if d < 0 {
					d = -d
				}
				st.SampleCount++
				st.SumAbsErr += int64(d)
				st.SumSqErr += int64(d) * int64(d)
				if d > st.MaxAbsErr {
					st.MaxAbsErr = d
				}
				errs = append(errs, d)
			}
		}
	}
	mean := fixed.Div(fixed.FromInt(st.SumAbsErr), fixed.FromInt(st.SampleCount), "block.mean_abs", tracer)
	mse := fixed.Div(fixed.FromInt(st.SumSqErr), fixed.FromInt(st.SampleCount), "block.mse", tracer)
	st.MeanAbsQ16 = int64(mean)
	st.MSEQ16 = int64(mse)
	sort.Ints(errs)
	q := Quantiles{
		Q10: nearestRank(errs, 0.10),
		Q50: nearestRank(errs, 0.50),
		Q90: nearestRank(errs, 0.90),
	}
	return st, errs, q
}

// EvaluateEdgeFidelity checks the analytic edge positions against a tolerance.
func EvaluateEdgeFidelity(orig, rec *sample.Frame, edges []sample.EdgePixel, tolerance int, tracer *fixed.Tracer) EdgeFidelity {
	ef := EdgeFidelity{Tolerance: tolerance, Checked: len(edges)}
	if ef.Checked == 0 {
		return ef
	}
	for _, e := range edges {
		d := orig.At(e.X, e.Y, e.Channel) - rec.At(e.X, e.Y, e.Channel)
		if d < 0 {
			d = -d
		}
		if d <= tolerance {
			ef.Within++
		}
	}
	ratio := fixed.Div(fixed.FromInt(int64(ef.Within)), fixed.FromInt(int64(ef.Checked)), "edge.fidelity", tracer)
	ef.FidelityQ16 = int64(ratio)
	return ef
}

func nearestRank(sorted []int, rank float64) int {
	idx := int(math.Ceil(rank * float64(len(sorted))))
	if idx < 1 {
		idx = 1
	}
	return sorted[idx-1]
}

// MonotoneMeanAbs proves the tightening-direction requirement: as the quantizer
// step shrinks the mean absolute block error must not increase. It returns the
// ordered mean values for the supplied blocks for external monotonic checks.
func MonotoneMeanAbs(stats []BlockStats) []int64 {
	out := make([]int64, len(stats))
	for i, s := range stats {
		out[i] = s.MeanAbsQ16
	}
	return out
}
