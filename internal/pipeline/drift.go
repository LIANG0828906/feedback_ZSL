package pipeline

import (
	"math"
	"mcapture/internal/dsp"
	"mcapture/internal/session"
)

// DriftParams 是参数化漂移模型：初相位 phi0 + 分段线性速率节点。
type DriftParams struct {
	Phi0        float64        `json:"phi0"`
	Knots       []session.Knot `json:"knots"`
	Phi0CI      float64        `json:"phi0_ci"`
	RateCI      []float64      `json:"rate_ci"`
	MarkersUsed int            `json:"markers_used"`
}

// PhiAt 与生成端同构：模型在核心采样 n 处的累计漂移（设备采样点）。
func (p DriftParams) PhiAt(n int64) float64 {
	knots := p.Knots
	if n <= knots[0].At {
		return p.Phi0
	}
	phi := p.Phi0
	for k := 0; k < len(knots)-1; k++ {
		a, b := knots[k], knots[k+1]
		hi := b.At
		if n < hi {
			hi = n
		}
		if hi > a.At {
			span := float64(b.At - a.At)
			for j := a.At; j < hi; j++ {
				t := float64(j-a.At) / span
				phi += a.Rate + (b.Rate-a.Rate)*t
			}
		}
		if hi == n {
			return phi
		}
	}
	return phi
}

// markerObs 是某通道上检测到的一个标记观测。
type markerObs struct {
	index   int
	coreRef float64 // 参考核心位置（来自参考通道检测，不读真值）
	arrival float64 // 本通道含裕量坐标下的分数到达
}

// EstimateDrift 以参考标记为基准估计分段线性漂移。
//
// 采用良态的两步最小二乘：
//  1. 每个标记处的局部偏移 d_m = arrival_m - margin - coreRef_m；
//  2. 局部速率 r_m 由相邻偏移的加权中心差分得到；
//  3. 在分段边界节点上对 (coreRef_m, r_m) 做帐篷基加权最小二乘，
//     得到分段线性速率曲线；phi0 由 d_m - 积分速率 的均值估计。
//
// 标记不足或退化返回 E_DRIFT（带通道与所用标记数，由调用方补通道）。
func EstimateDrift(obs []markerObs, core, margin, segLen int64) (DriftParams, error) {
	numSeg := int((core + segLen - 1) / segLen)
	knotCount := numSeg + 1
	// 至少需要：phi0 + 速率节点数 个观测，且中心差分至少 2 个标记。
	if len(obs) < knotCount+1 || len(obs) < 3 {
		return DriftParams{}, session.NewError(session.EDrift,
			"insufficient markers for drift estimation").
			WithMarker(len(obs))
	}
	knotAt := make([]int64, knotCount)
	for k := range knotAt {
		at := int64(k) * segLen
		if at > core {
			at = core
		}
		knotAt[k] = at
	}

	// 第 1 步：局部偏移（相对参考核心轴）。
	type pt struct {
		n float64 // 核心位置
		d float64 // 局部偏移（设备点）
		r float64 // 局部速率
	}
	pts := make([]pt, len(obs))
	for i, o := range obs {
		pts[i] = pt{n: o.coreRef, d: o.arrival - float64(margin) - o.coreRef}
	}
	// 第 2 步：中心差分求局部速率，端点用前/后向差分。
	for i := range pts {
		switch {
		case i == 0:
			pts[i].r = (pts[1].d - pts[0].d) / (pts[1].n - pts[0].n)
		case i == len(pts)-1:
			pts[i].r = (pts[i].d - pts[i-1].d) / (pts[i].n - pts[i-1].n)
		default:
			pts[i].r = (pts[i+1].d - pts[i-1].d) / (pts[i+1].n - pts[i-1].n)
		}
	}

	// 第 3 步：速率节点加权最小二乘。设计列为帐篷基，尺度天然 O(1)。
	n := len(pts)
	A := make([]float64, n*knotCount)
	y := make([]float64, n)
	for i, q := range pts {
		for k := 0; k < knotCount; k++ {
			A[i*knotCount+k] = tentBasis(knotAt, k, int64(math.Round(q.n)))
		}
		y[i] = q.r
	}
	rateX, ataInv, rss, ok := dsp.SolveNormalEquations(A, y, n, knotCount)
	if !ok {
		return DriftParams{}, session.NewError(session.EDrift,
			"degenerate drift design matrix").WithMarker(n)
	}
	rateSigma := math.Sqrt(rss / float64(n-knotCount))
	const z95 = 1.96

	params := DriftParams{
		Knots:       make([]session.Knot, knotCount),
		RateCI:      make([]float64, knotCount),
		MarkersUsed: n,
	}
	for k := 0; k < knotCount; k++ {
		params.Knots[k] = session.Knot{At: knotAt[k], Rate: rateX[k]}
		params.RateCI[k] = z95 * rateSigma * math.Sqrt(math.Max(ataInv[k*knotCount+k], 0))
	}

	// phi0：令 d_m = phi0 + ∫_0^n rate，取残差均值（带其样本标准差置信区间）。
	var residSum, residSq float64
	for _, q := range pts {
		integ := params.integral(int64(math.Round(q.n)))
		residSum += q.d - integ
	}
	params.Phi0 = residSum / float64(n)
	for _, q := range pts {
		integ := params.integral(int64(math.Round(q.n)))
		r := q.d - integ - params.Phi0
		residSq += r * r
	}
	phiSigma := math.Sqrt(residSq/float64(n-1)) / math.Sqrt(float64(n))
	params.Phi0CI = z95 * phiSigma
	return params, nil
}

// integral 计算速率曲线在 [0,n) 上的积分。
func (p DriftParams) integral(n int64) float64 {
	var acc float64
	for k := 0; k < len(p.Knots)-1; k++ {
		a, b := p.Knots[k], p.Knots[k+1]
		hi := b.At
		if n < hi {
			hi = n
		}
		if hi > a.At {
			span := float64(b.At - a.At)
			for j := a.At; j < hi; j++ {
				t := float64(j-a.At) / span
				acc += a.Rate + (b.Rate-a.Rate)*t
			}
		}
		if hi == n {
			break
		}
	}
	return acc
}

// phiPrefix 一次性计算 PhiAt(0..core)，供重采样热路径 O(1) 查询。
func (p DriftParams) phiPrefix(core int64) []float64 {
	out := make([]float64, core+1)
	out[0] = p.Phi0
	phi := p.Phi0
	for n := int64(0); n < core; n++ {
		phi += p.rateAt(n)
		out[n+1] = phi
	}
	return out
}

// rateAt 返回核心采样 n 处的分段线性速率。
func (p DriftParams) rateAt(n int64) float64 {
	knots := p.Knots
	if n <= knots[0].At {
		return knots[0].Rate
	}
	for k := 0; k < len(knots)-1; k++ {
		a, b := knots[k], knots[k+1]
		if n <= b.At {
			t := float64(n-a.At) / float64(b.At-a.At)
			return a.Rate + (b.Rate-a.Rate)*t
		}
	}
	return knots[len(knots)-1].Rate
}

// tentBasis 是速率曲线在节点 k 上的分段线性（帐篷）基函数。
func tentBasis(knotAt []int64, k int, j int64) float64 {
	if j <= knotAt[0] {
		if k == 0 {
			return 1
		}
		return 0
	}
	if j >= knotAt[len(knotAt)-1] {
		if k == len(knotAt)-1 {
			return 1
		}
		return 0
	}
	for s := 0; s < len(knotAt)-1; s++ {
		a, b := knotAt[s], knotAt[s+1]
		if j >= a && j <= b {
			t := float64(j-a) / float64(b-a)
			if k == s {
				return 1 - t
			}
			if k == s+1 {
				return t
			}
			return 0
		}
	}
	return 0
}
