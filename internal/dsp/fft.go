package dsp

import "math"

// FFT 原地迭代 radix-2 Cooley-Tukey 快速傅里叶变换，要求 n 为 2 的幂。
// 纯 float64 标量运算、固定顺序，跨 goroutine/分帧顺序结果逐位一致。
func FFT(real, imag []float64) {
	n := len(real)
	if n != len(imag) || n&(n-1) != 0 {
		panic("fft: 长度必须为相等的 2 的幂")
	}
	// 位反转置换。
	for i, j := 1, 0; i < n; i++ {
		bit := n >> 1
		for ; j&bit != 0; bit >>= 1 {
			j ^= bit
		}
		j ^= bit
		if i < j {
			real[i], real[j] = real[j], real[i]
			imag[i], imag[j] = imag[j], imag[i]
		}
	}
	for length := 2; length <= n; length <<= 1 {
		ang := -2 * math.Pi / float64(length)
		wReal, wImag := math.Cos(ang), math.Sin(ang)
		half := length >> 1
		for start := 0; start < n; start += length {
			curReal, curImag := 1.0, 0.0
			for k := 0; k < half; k++ {
				aReal, aImag := real[start+k], imag[start+k]
				bReal := curReal*real[start+k+half] - curImag*imag[start+k+half]
				bImag := curReal*imag[start+k+half] + curImag*real[start+k+half]
				real[start+k] = aReal + bReal
				imag[start+k] = aImag + bImag
				real[start+k+half] = aReal - bReal
				imag[start+k+half] = aImag - bImag
				nextReal := curReal*wReal - curImag*wImag
				curImag = curReal*wImag + curImag*wReal
				curReal = nextReal
			}
		}
	}
}
