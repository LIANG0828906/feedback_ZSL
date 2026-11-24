package audiocal

import (
	"math"
	"sort"
)

// ChannelFit 是单通道的拟合结果。
type ChannelFit struct {
	ChannelID int       `json:"channel_id"`
	Beta      []float64 `json:"beta"`         // 对数频三角基函数权重（滤波器参数）
	Predicted []float64 `json:"predicted_db"` // 网格各频点预测补偿增益 dB
	Residuals []float64 `json:"residuals_db"` // 网格各频点残差 |pred-target| dB
	FIRTaps   []float64 `json:"fir_taps"`     // 合成 FIR 系数（固件滤波器系数源）
}

// CalibrationModel 是拟合产出的校准模型。
type CalibrationModel struct {
	SampleID    string        `json:"sample_id"`
	SampleRate  int           `json:"sample_rate_hz"`
	Grid        []float64     `json:"grid_hz"`
	Target      []TargetPoint `json:"target"`
	Channels    []ChannelFit  `json:"channels"`
	Config      FitConfig     `json:"config"`
	AlgorithmID string        `json:"algorithm_id"`
}

// AlgorithmID 是拟合/编码算法版本，检查点据此作废旧快照。
const AlgorithmID = "audiocal-ridge-v1"

// FitViolation 标识首个超差的通道与频点。
type FitViolation struct {
	ChannelIndex int
	PointIndex   int
	Got          float64
	Upper        float64
}

// dftMagnitudeDB 在指定频率处对实序列做单 bin DFT，返回归一化幅度 dB。
func dftMagnitudeDB(ir []float64, sampleRate int, freq float64) float64 {
	omega := 2 * math.Pi * freq / float64(sampleRate)
	var re, im float64
	for n, x := range ir {
		ph := omega * float64(n)
		re += x * math.Cos(ph)
		im -= x * math.Sin(ph)
	}
	mag := math.Hypot(re, im) / float64(len(ir))
	if mag <= 0 {
		mag = 1e-12
	}
	return 20 * math.Log10(mag)
}

// measuredResponseDB 逐网格频点测量通道频响。
func measuredResponseDB(ch ChannelIR, sampleRate int, grid []float64) []float64 {
	out := make([]float64, len(grid))
	for i, f := range grid {
		out[i] = dftMagnitudeDB(ch.IR, sampleRate, f)
	}
	return out
}

// designMatrix 构造对数频三角基设计矩阵，行=频点、列=基函数。
func designMatrix(grid []float64, k int) [][]float64 {
	if len(grid) == 0 {
		return nil
	}
	x0 := math.Log2(grid[0])
	x1 := math.Log2(grid[len(grid)-1])
	m := make([][]float64, len(grid))
	for r, f := range grid {
		x := math.Log2(f)
		row := make([]float64, k)
		for j := 0; j < k; j++ {
			center := x0 + (x1-x0)*float64(j)/float64(k-1)
			row[j] = triangleBasis(x, center, j, k, x0, x1)
		}
		m[r] = row
	}
	return m
}

// triangleBasis 是带边界半支撑的线性三角基。
func triangleBasis(x, center float64, j, k int, x0, x1 float64) float64 {
	step := (x1 - x0) / float64(k-1)
	d := math.Abs(x-center) / step
	if d >= 1 {
		return 0
	}
	return 1 - d
}

// ridgeSolve 解 (A'A + λI)β = A'y，高斯消元（带部分主元）。
func ridgeSolve(a [][]float64, y []float64, lambda float64) []float64 {
	n := len(a[0])
	m := len(a)
	ata := make([][]float64, n)
	for i := range ata {
		ata[i] = make([]float64, n+1)
	}
	for i := 0; i < n; i++ {
		for j := 0; j < n; j++ {
			var s float64
			for r := 0; r < m; r++ {
				s += a[r][i] * a[r][j]
			}
			ata[i][j] = s
		}
		var s float64
		for r := 0; r < m; r++ {
			s += a[r][i] * y[r]
		}
		ata[i][n] = s
		ata[i][i] += lambda
	}
	return gaussianSolve(ata)
}

func gaussianSolve(m [][]float64) []float64 {
	n := len(m)
	for col := 0; col < n; col++ {
		pivot := col
		for r := col + 1; r < n; r++ {
			if math.Abs(m[r][col]) > math.Abs(m[pivot][col]) {
				pivot = r
			}
		}
		m[col], m[pivot] = m[pivot], m[col]
		pv := m[col][col]
		if math.Abs(pv) < 1e-15 {
			pv = 1e-15
		}
		for r := 0; r < n; r++ {
			if r == col {
				continue
			}
			factor := m[r][col] / pv
			for c := col; c <= n; c++ {
				m[r][c] -= factor * m[col][c]
			}
		}
	}
	out := make([]float64, n)
	for i := 0; i < n; i++ {
		out[i] = m[i][n] / m[i][i]
	}
	return out
}

func matVec(a [][]float64, v []float64) []float64 {
	out := make([]float64, len(a))
	for r := range a {
		var s float64
		for c := range a[r] {
			s += a[r][c] * v[c]
		}
		out[r] = s
	}
	return out
}

// synthesizeFIR 由网格增益曲线经 Hermitian 对称谱做 IDFT 合成实 FIR。
func synthesizeFIR(grid, gainDB []float64, sampleRate, taps int) []float64 {
	nyquist := float64(sampleRate) / 2
	spec := make([]complex128, taps)
	for k := 0; k < taps; k++ {
		f := nyquist * float64(k) / float64(taps-1)
		g := interpGain(grid, gainDB, f)
		amp := math.Pow(10, g/20.0)
		spec[k] = complex(amp, 0)
	}
	tapsOut := make([]float64, taps)
	for n := 0; n < taps; n++ {
		var re float64
		for k := 0; k < taps; k++ {
			re += real(spec[k]) * math.Cos(2*math.Pi*float64(k)*float64(n)/float64(taps))
		}
		tapsOut[n] = re / float64(taps)
	}
	// 循环移位使滤波器因果（峰值居中），保持确定性。
	shift := taps / 2
	shifted := make([]float64, taps)
	for i := range tapsOut {
		shifted[(i+shift)%taps] = tapsOut[i]
	}
	return shifted
}

// interpGain 在对数频率轴上做分段线性插值，越界取端点。
func interpGain(grid, gain []float64, freq float64) float64 {
	if freq <= grid[0] {
		return gain[0]
	}
	last := grid[len(grid)-1]
	if freq >= last {
		return gain[len(gain)-1]
	}
	idx := sort.Search(len(grid), func(i int) bool { return grid[i] >= freq })
	if idx == 0 {
		return gain[0]
	}
	x0, x1 := math.Log2(grid[idx-1]), math.Log2(grid[idx])
	y0, y1 := gain[idx-1], gain[idx]
	x := math.Log2(freq)
	t := (x - x0) / (x1 - x0)
	return y0 + t*(y1-y0)
}

// FIRResponseDB 计算 FIR 在给定频点的幅度响应 dB（本地与设备端共用同一确定性算法）。
func FIRResponseDB(taps []float64, sampleRate int, freqs []float64) []float64 {
	out := make([]float64, len(freqs))
	for i, f := range freqs {
		out[i] = dftMagnitudeDB(taps, sampleRate, f)
	}
	return out
}

// Fit 执行全通道拟合并校验解析真值上界（业务要求 2）。
func Fit(s *Sample, cfg FitConfig) (*CalibrationModel, error) {
	if err := ValidateSample(s); err != nil {
		return nil, err
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	grid := cfg.GridFrequencies
	if len(grid) == 0 {
		grid = make([]float64, len(s.Target))
		for i, p := range s.Target {
			grid[i] = p.FrequencyHz
		}
	}
	targetOnGrid := interpolateTarget(s.Target, grid)
	a := designMatrix(grid, cfg.BasisCount)

	model := &CalibrationModel{
		SampleID:    s.ID,
		SampleRate:  s.SampleRateHz,
		Grid:        append([]float64(nil), grid...),
		Target:      append([]TargetPoint(nil), s.Target...),
		Config:      cfg,
		AlgorithmID: AlgorithmID,
	}

	var firstViolation *FitViolation
	for ci, ch := range s.Channels {
		measured := measuredResponseDB(ch, s.SampleRateHz, grid)
		// 需要的补偿 = 目标 - 测量（把通道频响拉到目标曲线上）。
		y := make([]float64, len(grid))
		for i := range y {
			y[i] = targetOnGrid[i] - measured[i]
		}
		beta := ridgeSolve(a, y, cfg.Regularization)
		pred := matVec(a, beta)
		res := make([]float64, len(grid))
		for i := range res {
			res[i] = math.Abs(pred[i] - y[i])
		}
		fir := synthesizeFIR(grid, pred, s.SampleRateHz, cfg.FIRTaps)
		model.Channels = append(model.Channels, ChannelFit{
			ChannelID: ch.ChannelID,
			Beta:      beta,
			Predicted: pred,
			Residuals: res,
			FIRTaps:   fir,
		})
		// 与解析真值上界比对（真值按目标频点给出，网格默认与目标频点对齐）。
		for pi, r := range res {
			if pi >= len(ch.Truth.ResidualUpperDB) {
				break
			}
			if r > ch.Truth.ResidualUpperDB[pi] && firstViolation == nil {
				firstViolation = &FitViolation{
					ChannelIndex: ci, PointIndex: pi,
					Got: r, Upper: ch.Truth.ResidualUpperDB[pi],
				}
			}
		}
	}

	if firstViolation != nil {
		v := firstViolation
		return nil, &BizError{
			Code:    CodeFit,
			Message: "拟合残差超出解析真值上界",
			Field:   "channel=" + itoa(v.ChannelIndex) + ",point=" + itoa(v.PointIndex),
			Detail: map[string]any{
				"channel_index": v.ChannelIndex,
				"point_index":   v.PointIndex,
				"got_db":        v.Got,
				"upper_db":      v.Upper,
			},
		}
	}
	return model, nil
}

// interpolateTarget 把目标曲线重采样到给定网格。
func interpolateTarget(target []TargetPoint, grid []float64) []float64 {
	f := make([]float64, len(target))
	g := make([]float64, len(target))
	for i, p := range target {
		f[i] = p.FrequencyHz
		g[i] = p.GainDB
	}
	out := make([]float64, len(grid))
	for i, hz := range grid {
		out[i] = interpGain(f, g, hz)
	}
	return out
}

// TotalSSE 返回所有通道残差平方和，用于正则单调性验证。
func (m *CalibrationModel) TotalSSE() float64 {
	var sse float64
	for _, ch := range m.Channels {
		for _, r := range ch.Residuals {
			sse += r * r
		}
	}
	return sse
}
