package audiofp

import "math"

// fftInPlace is a deterministic radix-2 Cooley-Tukey FFT.
func fftInPlace(re, im []float64) {
	n := len(re)
	if n <= 1 {
		return
	}
	j := 0
	for i := 1; i < n; i++ {
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
		angle := -2 * math.Pi / float64(length)
		wlenRe := math.Cos(angle)
		wlenIm := math.Sin(angle)
		for i := 0; i < n; i += length {
			wr, wi := 1.0, 0.0
			half := length / 2
			for j := 0; j < half; j++ {
				uRe := re[i+j]
				uIm := im[i+j]
				vRe := re[i+j+half]*wr - im[i+j+half]*wi
				vIm := re[i+j+half]*wi + im[i+j+half]*wr
				re[i+j] = uRe + vRe
				im[i+j] = uIm + vIm
				re[i+j+half] = uRe - vRe
				im[i+j+half] = uIm - vIm
				wr, wi = wr*wlenRe-wi*wlenIm, wr*wlenIm+wi*wlenRe
			}
		}
	}
}

func bandEnergies(samples []int16, frameNo int, r Recipe) ([]float64, float64) {
	nFFT := nextPowerOfTwo(r.FrameLen)
	re := make([]float64, nFFT)
	im := make([]float64, nFFT)
	start := FrameStartOffset(frameNo, r)
	for i := 0; i < r.FrameLen; i++ {
		re[i] = float64(samples[start+i]) / 32768.0
	}
	fftInPlace(re, im)
	positive := nFFT/2 - 1
	energies := make([]float64, r.Bands)
	total := 0.0
	for bin := 1; bin <= positive; bin++ {
		band := (bin - 1) * r.Bands / positive
		p := re[bin]*re[bin] + im[bin]*im[bin]
		energies[band] += p
		total += p
	}
	return energies, total
}
