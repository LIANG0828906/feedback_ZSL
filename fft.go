package audiofp

import (
	"math"
	"math/bits"
)

func nextPow2(v int) int {
	if v <= 1 {
		return 1
	}
	return 1 << bits.Len(uint(v-1))
}

// fft performs an in-place iterative radix-2 DFT. Input length must be a power
// of two. It is local to the caller and therefore safe for concurrent use.
func fft(re, im []float64) {
	n := len(re)
	if n != len(im) || n == 0 || n&(n-1) != 0 {
		panic("fft: slices must have equal power-of-two length")
	}

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
		half := length / 2
		step := -2.0 * math.Pi / float64(length)
		for start := 0; start < n; start += length {
			angle := 0.0
			for j := 0; j < half; j++ {
				wr := math.Cos(angle)
				wi := math.Sin(angle)
				idx := start + j
				other := idx + half
				tr := wr*re[other] - wi*im[other]
				ti := wr*im[other] + wi*re[other]
				re[other] = re[idx] - tr
				im[other] = im[idx] - ti
				re[idx] += tr
				im[idx] += ti
				angle += step
			}
		}
	}
}
