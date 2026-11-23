// Package verifier independently recomputes frame metrics, analytic bounds and
// boundary merge results using only persisted block records and the truth
// manifest. It never reads raw sequences and never writes to the store.
package verifier

import (
	"fmt"
	"sort"

	"vqa/internal/errs"
	"vqa/internal/fixed"
	"vqa/internal/metrics"
	"vqa/internal/propagation"
	"vqa/internal/sample"
	"vqa/internal/store"
)

// Violation is the first offending field located by verification.
type Violation struct {
	Frame  int    `json:"frame"`
	BX     int    `json:"bx"`
	BY     int    `json:"by"`
	Field  string `json:"field"`
	Detail string `json:"detail"`
}

// Report is the verification outcome for one checkpoint store.
type Report struct {
	FramesChecked int         `json:"frames_checked"`
	BlocksChecked int         `json:"blocks_checked"`
	Violations    []Violation `json:"violations"`
	FramePSNR     []float64   `json:"frame_psnr_db"`
	FrameSSIMQ16  []int64     `json:"frame_ssim_q16"`
}

// Verify recomputes everything derivable from the persisted records and the
// manifest, returning E_DIVERGE on the first mismatch.
func Verify(m *sample.Manifest, st *store.Store, quality int) (*Report, error) {
	rep := &Report{}
	blocks := st.Blocks()
	frames := st.Frames()
	rep.BlocksChecked = len(blocks)

	// Recompute the deterministic global window merge purely from records.
	grid := metrics.BuildGrid(st.Meta().Params)

	for f := 0; f < m.Frames; f++ {
		var merged []metrics.WindowResult
		for _, br := range blocks {
			if br.Frame == f {
				merged = append(merged, br.Windows...)
			}
		}
		sort.SliceStable(merged, func(i, j int) bool {
			if merged[i].TopY != merged[j].TopY {
				return merged[i].TopY < merged[j].TopY
			}
			return merged[i].TopX < merged[j].TopX
		})
		// Deduplicate: owner emits each window exactly once, so counts match.
		if len(merged) != len(grid.Tops) {
			return nil, firstViolation(f, 0, 0, "window_merge",
				fmt.Sprintf("merged windows %d != grid windows %d", len(merged), len(grid.Tops)))
		}
		tracer := fixed.NewTracer()
		ssim := metrics.MeanSSIM(merged, tracer)

		// Reconstruct per-frame PSNR from persisted block integer accumulators.
		var sumSq, samples int64
		for _, br := range blocks {
			if br.Frame != f {
				continue
			}
			sumSq += br.Stats.SumSqErr
			samples += br.Stats.SampleCount
			// Quantile truth check for sampled frames.
			if err := checkQuantiles(m, quality, br); err != nil {
				return nil, err
			}
			// Analytic bound check from observed codec parameters only.
			step := sample.QuantStep(quality)
			bound := sample.PixelBound(m.BlockSize, step)
			if br.Propagation.MaxPixelErr > bound {
				return nil, firstViolation(f, br.BX, br.BY, "pixel_bound",
					fmt.Sprintf("observed %d > bound %d", br.Propagation.MaxPixelErr, bound))
			}
			if br.Propagation.PixelBound != bound {
				return nil, firstViolation(f, br.BX, br.BY, "pixel_bound_record", "stored bound not reproducible")
			}
		}
		psnr := metrics.PSNR(sumSq, samples)
		rep.FramePSNR = append(rep.FramePSNR, psnr)
		rep.FrameSSIMQ16 = append(rep.FrameSSIMQ16, int64(ssim))
		rep.FramesChecked++

		// Compare with the frame summary produced by the main pipeline.
		var frRec *store.FrameRecord
		for i := range frames {
			if frames[i].Frame == f {
				frRec = &frames[i]
			}
		}
		if frRec == nil {
			return nil, firstViolation(f, 0, 0, "frame_record", "missing frame summary")
		}
		if !floatClose(frRec.PSNR, psnr, 1e-9) {
			return nil, firstViolation(f, 0, 0, "psnr",
				fmt.Sprintf("stored %.6f != recomputed %.6f", frRec.PSNR, psnr))
		}
		if frRec.SSIMQ16 != int64(ssim) {
			return nil, firstViolation(f, 0, 0, "ssim",
				fmt.Sprintf("stored %d != recomputed %d", frRec.SSIMQ16, int64(ssim)))
		}
		// Re-derive the frame analytic lower bound.
		step := sample.QuantStep(quality)
		pb := sample.PixelBound(m.BlockSize, step)
		lower := propagation.FrameBoundPSNR(pb)
		if !floatClose(frRec.PSNRLower, lower, 1e-9) {
			return nil, firstViolation(f, 0, 0, "psnr_lower_bound", "bound not reproducible from parameters")
		}
		if psnr < lower {
			return nil, firstViolation(f, 0, 0, "psnr_lower_bound",
				fmt.Sprintf("measured %.6f below bound %.6f", psnr, lower))
		}

		// Truth fingerprint interval check for sampled frames.
		if err := checkFingerprint(m, quality, f, psnr); err != nil {
			return nil, err
		}
	}
	return rep, nil
}

func checkQuantiles(m *sample.Manifest, quality int, br store.BlockRecord) error {
	fp, ok := m.Fingerprints[fmt.Sprintf("%d:%d", quality, br.Frame)]
	if !ok {
		return nil
	}
	for _, tq := range fp.BlockQuantiles {
		if tq.BX != br.BX || tq.BY != br.BY {
			continue
		}
		if tq.Q10 != br.Quantiles.Q10 || tq.Q50 != br.Quantiles.Q50 || tq.Q90 != br.Quantiles.Q90 {
			return firstViolation(br.Frame, br.BX, br.BY, "block_quantiles",
				fmt.Sprintf("truth (%d,%d,%d) != observed (%d,%d,%d)",
					tq.Q10, tq.Q50, tq.Q90, br.Quantiles.Q10, br.Quantiles.Q50, br.Quantiles.Q90))
		}
	}
	return nil
}

func checkFingerprint(m *sample.Manifest, quality, frame int, psnr float64) error {
	fp, ok := m.Fingerprints[fmt.Sprintf("%d:%d", quality, frame)]
	if !ok {
		return nil
	}
	if psnr < fp.PSNRMin || psnr > fp.PSNRMax {
		return firstViolation(frame, 0, 0, "psnr_interval",
			fmt.Sprintf("measured %.6f outside truth [%.6f,%.6f]", psnr, fp.PSNRMin, fp.PSNRMax))
	}
	return nil
}

func firstViolation(frame, bx, by int, field, detail string) error {
	return errs.New(errs.EDiverge, "verification mismatch").
		WithInt("frame", int64(frame)).
		WithInt("bx", int64(bx)).WithInt("by", int64(by)).
		With("field", field).With("detail", detail)
}

func floatClose(a, b, tol float64) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	return d <= tol
}
