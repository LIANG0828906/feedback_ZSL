// Package propagation models how quantizer coefficient error spreads through
// the inverse transform into pixel error, records block-boundary jumps and
// derives an analytic per-frame upper bound strictly from observed parameters
// (quantizer step, block size, transform order).
package propagation

import (
	"math"
	"strconv"

	"vqa/internal/errs"
	"vqa/internal/sample"
)

// BoundaryJump is the pixel error discontinuity across one block edge.
type BoundaryJump struct {
	FromBX  int    `json:"from_bx"`
	FromBY  int    `json:"from_by"`
	ToBX    int    `json:"to_bx"`
	ToBY    int    `json:"to_by"`
	Edge    string `json:"edge"`
	MaxJump int    `json:"max_jump"`
}

// BlockPropagation is the propagation report of one block.
type BlockPropagation struct {
	BX           int      `json:"bx"`
	BY           int      `json:"by"`
	MaxCoeffErr  float64  `json:"max_coeff_err"`
	MeanCoeffErr float64  `json:"mean_coeff_err"`
	MaxPixelErr  int      `json:"max_pixel_err"`
	ObservedStep int      `json:"observed_step"`
	PixelBound   int      `json:"pixel_bound"`
	Neighbors    []string `json:"neighbors"`
}

// FrameBound is the analytic frame-level bound.
type FrameBound struct {
	Frame          int     `json:"frame"`
	Step           int     `json:"step"`
	BlockSize      int     `json:"transform_order"`
	PixelBound     int     `json:"pixel_bound"`
	MSEBound       float64 `json:"mse_bound"`
	PSNRLowerBound float64 `json:"psnr_lower_bound_db"`
}

// Report bundles propagation data for one frame.
type Report struct {
	Frame  int                `json:"frame"`
	Blocks []BlockPropagation `json:"blocks"`
	Jumps  []BoundaryJump     `json:"jumps"`
	Bound  FrameBound         `json:"bound"`
}

// Analyze recomputes the block transform errors from the original frame and
// the codec parameters, derives pixel-domain jumps over spatial adjacency and
// returns the frame propagation report.
func Analyze(frame int, orig, rec *sample.Frame, blockSize, quality int) *Report {
	codec := sample.NewCodec(blockSize, quality)
	cols := orig.Width / blockSize
	rows := orig.Height / blockSize
	rep := &Report{Frame: frame}
	for by := 0; by < rows; by++ {
		for bx := 0; bx < cols; bx++ {
			tile := extractTile(orig, bx, by, blockSize)
			br := codec.EncodeBlock(tile, orig.Channels)
			bp := BlockPropagation{
				BX:           bx,
				BY:           by,
				ObservedStep: codec.Step,
				PixelBound:   sample.PixelBound(blockSize, codec.Step),
				Neighbors:    neighbors(bx, by, cols, rows),
			}
			var sumCoeff float64
			for _, e := range br.CoeffErr {
				a := math.Abs(e)
				if a > bp.MaxCoeffErr {
					bp.MaxCoeffErr = a
				}
				sumCoeff += a
			}
			bp.MeanCoeffErr = sumCoeff / float64(len(br.CoeffErr))
			for _, e := range br.PixelErr {
				if a := absInt(e); a > bp.MaxPixelErr {
					bp.MaxPixelErr = a
				}
			}
			rep.Blocks = append(rep.Blocks, bp)
		}
	}
	rep.Jumps = boundaryJumps(orig, rec, blockSize)
	step := sample.QuantStep(quality)
	pb := sample.PixelBound(blockSize, step)
	mseBound := float64(pb * pb)
	rep.Bound = FrameBound{
		Frame:          frame,
		Step:           step,
		BlockSize:      blockSize,
		PixelBound:     pb,
		MSEBound:       mseBound,
		PSNRLowerBound: 20 * math.Log10(255.0/math.Sqrt(mseBound)),
	}
	return rep
}

// VerifyBound checks every observed pixel error against the analytic bound and
// every measured PSNR against the frame lower bound. A breach returns E_BOUND
// with frame, block coordinates and the observed value.
func VerifyBound(r *Report, observedPSNR float64) error {
	for _, bp := range r.Blocks {
		if bp.MaxPixelErr > bp.PixelBound {
			return errs.New(errs.EBound, "pixel error exceeded analytic bound").
				WithInt("frame", int64(r.Frame)).
				WithInt("bx", int64(bp.BX)).WithInt("by", int64(bp.BY)).
				WithInt("observed", int64(bp.MaxPixelErr)).
				WithInt("bound", int64(bp.PixelBound))
		}
	}
	if observedPSNR < r.Bound.PSNRLowerBound {
		return errs.New(errs.EBound, "measured PSNR below analytic lower bound").
			WithInt("frame", int64(r.Frame)).
			With("observed_psnr", formatFloat(observedPSNR)).
			With("psnr_lower_bound", formatFloat(r.Bound.PSNRLowerBound))
	}
	return nil
}

func boundaryJumps(orig, rec *sample.Frame, blockSize int) []BoundaryJump {
	cols := orig.Width / blockSize
	rows := orig.Height / blockSize
	var jumps []BoundaryJump
	errAt := func(x, y, c int) int {
		return absInt(rec.At(x, y, c) - orig.At(x, y, c))
	}
	for by := 0; by < rows; by++ {
		for bx := 0; bx < cols; bx++ {
			if bx+1 < cols {
				mj := 0
				for y := by * blockSize; y < (by+1)*blockSize; y++ {
					for c := 0; c < orig.Channels; c++ {
						d := absInt(errAt((bx+1)*blockSize, y, c) - errAt(bx*blockSize+blockSize-1, y, c))
						if d > mj {
							mj = d
						}
					}
				}
				jumps = append(jumps, BoundaryJump{bx, by, bx + 1, by, "vertical", mj})
			}
			if by+1 < rows {
				mj := 0
				for x := bx * blockSize; x < (bx+1)*blockSize; x++ {
					for c := 0; c < orig.Channels; c++ {
						d := absInt(errAt(x, (by+1)*blockSize, c) - errAt(x, by*blockSize+blockSize-1, c))
						if d > mj {
							mj = d
						}
					}
				}
				jumps = append(jumps, BoundaryJump{bx, by, bx, by + 1, "horizontal", mj})
			}
		}
	}
	return jumps
}

func neighbors(bx, by, cols, rows int) []string {
	var out []string
	add := func(x, y int) {
		if x >= 0 && y >= 0 && x < cols && y < rows {
			out = append(out, coord(x, y))
		}
	}
	add(bx-1, by)
	add(bx+1, by)
	add(bx, by-1)
	add(bx, by+1)
	return out
}

func coord(x, y int) string {
	return "(" + itoa(x) + "," + itoa(y) + ")"
}

func extractTile(f *sample.Frame, bx, by, n int) []uint8 {
	tile := make([]uint8, n*n*f.Channels)
	for y := 0; y < n; y++ {
		for x := 0; x < n; x++ {
			for c := 0; c < f.Channels; c++ {
				tile[(y*n+x)*f.Channels+c] = f.Pix[((by*n+y)*f.Width+(bx*n+x))*f.Channels+c]
			}
		}
	}
	return tile
}

func absInt(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	var b [12]byte
	i := len(b)
	for v > 0 {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
	}
	return string(b[i:])
}

func formatFloat(v float64) string {
	return strconv.FormatFloat(v, 'f', 4, 64)
}

// FrameBoundPSNR returns the analytic PSNR lower bound in dB given the
// per-pixel error bound. It lets the verifier re-derive bounds from nothing
// but the observed quantizer step and block size.
func FrameBoundPSNR(pixelBound int) float64 {
	mseBound := float64(pixelBound * pixelBound)
	return 20 * math.Log10(255.0/math.Sqrt(mseBound))
}
