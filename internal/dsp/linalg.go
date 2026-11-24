package dsp

import (
	"math"
)

// SolveNormalEquations 解 (AᵀA) x = Aᵀb，并返回参数协方差矩阵（按残差方差缩放前）。
// a 为行优先的 n×p 设计矩阵，b 为长度 n 的观测向量。
// 返回 x（长度 p）、ATA 的逆（行优先 p×p）与残差平方和；退化时返回错误。
func SolveNormalEquations(a []float64, b []float64, n, p int) (x, ataInv []float64, rss float64, ok bool) {
	if n <= p || len(a) != n*p || len(b) != n {
		return nil, nil, 0, false
	}
	ata := make([]float64, p*p)
	atb := make([]float64, p)
	for i := 0; i < n; i++ {
		ri := i * p
		for u := 0; u < p; u++ {
			aiu := a[ri+u]
			atb[u] += aiu * b[i]
			for v := 0; v < p; v++ {
				ata[u*p+v] += aiu * a[ri+v]
			}
		}
	}
	ataInv, ok = InvertSymmetric(ata, p)
	if !ok {
		return nil, nil, 0, false
	}
	x = make([]float64, p)
	for u := 0; u < p; u++ {
		var s float64
		for v := 0; v < p; v++ {
			s += ataInv[u*p+v] * atb[v]
		}
		x[u] = s
	}
	for i := 0; i < n; i++ {
		var pred float64
		ri := i * p
		for u := 0; u < p; u++ {
			pred += a[ri+u] * x[u]
		}
		d := b[i] - pred
		rss += d * d
	}
	return x, ataInv, rss, true
}

// InvertSymmetric 用 Cholesky 分解求对称正定矩阵 m（行优先 p×p）的逆。
func InvertSymmetric(m []float64, p int) ([]float64, bool) {
	// 轻度对角加载，抑制数值退化（量级远小于物理量，不改变可解释性）。
	var tr float64
	for i := 0; i < p; i++ {
		tr += m[i*p+i]
	}
	ridge := 1e-12 * tr / float64(p)
	if ridge == 0 {
		ridge = 1e-12
	}
	mm := make([]float64, len(m))
	copy(mm, m)
	for i := 0; i < p; i++ {
		mm[i*p+i] += ridge
	}
	L := make([]float64, p*p)
	for i := 0; i < p; i++ {
		for j := 0; j <= i; j++ {
			var s float64
			for k := 0; k < j; k++ {
				s += L[i*p+k] * L[j*p+k]
			}
			if i == j {
				v := mm[i*p+i] - s
				if v <= 1e-18 {
					return nil, false
				}
				L[i*p+j] = math.Sqrt(v)
			} else {
				L[i*p+j] = (mm[i*p+j] - s) / L[j*p+j]
			}
		}
	}
	// 求逆：对每个单位向量做前代+回代。
	inv := make([]float64, p*p)
	y := make([]float64, p)
	x := make([]float64, p)
	for col := 0; col < p; col++ {
		for i := 0; i < p; i++ {
			var s float64
			for k := 0; k < i; k++ {
				s += L[i*p+k] * y[k]
			}
			rhs := 0.0
			if i == col {
				rhs = 1
			}
			y[i] = (rhs - s) / L[i*p+i]
		}
		for i := p - 1; i >= 0; i-- {
			var s float64
			for k := i + 1; k < p; k++ {
				s += L[k*p+i] * x[k]
			}
			x[i] = (y[i] - s) / L[i*p+i]
		}
		for i := 0; i < p; i++ {
			inv[i*p+col] = x[i]
		}
	}
	return inv, true
}
