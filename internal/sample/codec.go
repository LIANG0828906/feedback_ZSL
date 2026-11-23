package sample

import "math"

// SupportedQualities is the closed set of accepted quality levels. Any other
// level is rejected with E_SCHEMA at load time.
var SupportedQualities = []int{90, 70, 50}

// QuantStep derives the deterministic quantizer step from a quality level.
// Lower quality means a coarser step; the mapping is part of the algorithm
// contract and is versioned through AlgorithmVersion.
func QuantStep(quality int) int {
	return 1 + (100-quality)/5
}

// QualitySupported reports whether quality is an accepted level.
func QualitySupported(quality int) bool {
	for _, q := range SupportedQualities {
		if q == quality {
			return true
		}
	}
	return false
}

// dctBasis precomputes the orthonormal DCT-II basis for the block size so both
// forward and inverse transforms share one exact coefficient table.
type dctBasis struct {
	n      int
	cosTbl [][]float64 // cosTbl[k][i]
	alpha  []float64
}

func newDCTBasis(n int) *dctBasis {
	b := &dctBasis{n: n, cosTbl: make([][]float64, n), alpha: make([]float64, n)}
	for k := 0; k < n; k++ {
		b.cosTbl[k] = make([]float64, n)
		if k == 0 {
			b.alpha[k] = math.Sqrt(1.0 / float64(n))
		} else {
			b.alpha[k] = math.Sqrt(2.0 / float64(n))
		}
		for i := 0; i < n; i++ {
			b.cosTbl[k][i] = math.Cos(math.Pi * float64(2*i+1) * float64(k) / float64(2*n))
		}
	}
	return b
}

// forward applies the 2-D orthonormal DCT-II to an n*n tile.
func (b *dctBasis) forward(tile []float64) []float64 {
	n := b.n
	tmp := make([]float64, n*n)
	out := make([]float64, n*n)
	// Transform columns then rows (separable, same basis).
	for y := 0; y < n; y++ {
		for k := 0; k < n; k++ {
			s := 0.0
			for i := 0; i < n; i++ {
				s += tile[y*n+i] * b.cosTbl[k][i]
			}
			tmp[y*n+k] = b.alpha[k] * s
		}
	}
	for x := 0; x < n; x++ {
		for k := 0; k < n; k++ {
			s := 0.0
			for i := 0; i < n; i++ {
				s += tmp[i*n+x] * b.cosTbl[k][i]
			}
			out[k*n+x] = b.alpha[k] * s
		}
	}
	return out
}

// inverse is the matching DCT-III reconstruction.
func (b *dctBasis) inverse(coeff []float64) []float64 {
	n := b.n
	tmp := make([]float64, n*n)
	out := make([]float64, n*n)
	for x := 0; x < n; x++ {
		for i := 0; i < n; i++ {
			s := 0.0
			for k := 0; k < n; k++ {
				s += b.alpha[k] * coeff[k*n+x] * b.cosTbl[k][i]
			}
			tmp[i*n+x] = s
		}
	}
	for y := 0; y < n; y++ {
		for i := 0; i < n; i++ {
			s := 0.0
			for k := 0; k < n; k++ {
				s += b.alpha[k] * tmp[y*n+k] * b.cosTbl[k][i]
			}
			out[y*n+i] = s
		}
	}
	return out
}

// quantize rounds coefficients to the quantizer lattice and returns both the
// dequantized coefficient and the coefficient-domain error (before -> after).
func quantize(c float64, step int) (dequant float64, errCoeff float64) {
	if step <= 0 {
		step = 1
	}
	idx := math.Round(c / float64(step))
	dequant = idx * float64(step)
	return dequant, dequant - c
}

// Codec is a configured block frequency codec.
type Codec struct {
	Basis *dctBasis
	Step  int
	N     int
}

// NewCodec builds a codec for block size n and quality level.
func NewCodec(n, quality int) *Codec {
	return &Codec{Basis: newDCTBasis(n), Step: QuantStep(quality), N: n}
}

// BlockReport captures the deterministic quantization error of one block.
type BlockReport struct {
	CoeffErr  []float64 // per-coefficient dequantized - original
	PixelErr  []int     // per-pixel reconstructed - original, clamped domain
	Recon     []uint8   // reconstructed block pixels
	MaxAbsErr int
}

// EncodeBlock runs forward DCT, lattice quantization and inverse DCT for one
// n*n*channels tile laid out as (y*n+x)*ch+c.
func (cd *Codec) EncodeBlock(tile []uint8, channels int) BlockReport {
	n := cd.N
	rep := BlockReport{
		CoeffErr: make([]float64, n*n*channels),
		PixelErr: make([]int, n*n*channels),
		Recon:    make([]uint8, n*n*channels),
	}
	plane := make([]float64, n*n)
	for c := 0; c < channels; c++ {
		for y := 0; y < n; y++ {
			for x := 0; x < n; x++ {
				plane[y*n+x] = float64(tile[(y*n+x)*channels+c])
			}
		}
		coeff := cd.Basis.forward(plane)
		deq := make([]float64, n*n)
		for k := 0; k < n*n; k++ {
			d, e := quantize(coeff[k], cd.Step)
			deq[k] = d
			rep.CoeffErr[k*channels+c] = e
		}
		recon := cd.Basis.inverse(deq)
		for y := 0; y < n; y++ {
			for x := 0; x < n; x++ {
				idx := y*n + x
				rv := clamp255(int(math.Round(recon[idx])))
				rep.Recon[idx*channels+c] = uint8(rv)
				orig := int(plane[idx])
				err := rv - orig
				rep.PixelErr[idx*channels+c] = err
				if a := absInt(err); a > rep.MaxAbsErr {
					rep.MaxAbsErr = a
				}
			}
		}
	}
	return rep
}

func absInt(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

// PixelBound is the analytic per-pixel reconstruction error upper bound derived
// only from observed codec parameters: quantizer step, block size and the
// transform order (= block size for this square DCT).
//
// Each coefficient error is bounded by step/2; the orthonormal DCT spreads it
// over N^2 basis contributions, each no larger than alphaMax^2 = 2/N:
//
//	|pixel error before rounding| <= N^2 * (step/2) * (2/N) = N*step.
//
// Rounding to the uint8 lattice adds at most 1, hence N*step + 1.
func PixelBound(blockSize, step int) int {
	return blockSize*step + 1
}
