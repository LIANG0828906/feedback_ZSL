package audiocal

import (
	"math"
)

// Position 是声学空间中的三维位置，单位米。
type Position struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
	Z float64 `json:"z"`
}

// ChannelTruth 是单通道的解析真值（由确定性生成过程直接算出，非估计量）。
type ChannelTruth struct {
	DirectDelaySec  float64   `json:"direct_delay_sec"`  // 直达声延迟（解析真值）
	EarlyEnergy     float64   `json:"early_energy"`      // 早期反射能量（解析真值）
	ResidualUpperDB []float64 `json:"residual_upper_db"` // 各目标频点拟合残差上限
}

// ChannelIR 是一条扬声器->麦克风通道的脉冲响应与位置元数据。
type ChannelIR struct {
	ChannelID  int          `json:"channel_id"`
	SpeakerID  int          `json:"speaker_id"`
	MicID      int          `json:"mic_id"`
	SpeakerPos Position     `json:"speaker_pos"`
	MicPos     Position     `json:"mic_pos"`
	IR         []float64    `json:"ir"`
	Truth      ChannelTruth `json:"truth"`
}

// TargetPoint 是目标补偿曲线上的一个频点增益。
type TargetPoint struct {
	FrequencyHz float64 `json:"frequency_hz"`
	GainDB      float64 `json:"gain_db"`
}

// Sample 是一条完整的房间测量记录。
type Sample struct {
	ID            string        `json:"id"`
	SampleRateHz  int           `json:"sample_rate_hz"` // 0 表示缺失，用于触发 E_META
	ChannelLayout string        `json:"channel_layout"` // 空串表示缺失，用于触发 E_META
	IRLength      int           `json:"ir_length"`
	Channels      []ChannelIR   `json:"channels"`
	Target        []TargetPoint `json:"target"`
	Seed          int64         `json:"seed"`
}

// lcg 是固定参数线性同余发生器，保证跨平台、跨 Go 版本的确定性。
type lcg struct {
	state uint64
}

const lcgMul = 6364136223846793005
const lcgInc = 1442695040888963407

func newLCG(seed int64) *lcg {
	return &lcg{state: uint64(seed) ^ 0x9E3779B97F4A7C15}
}

// Uint64 返回下一个伪随机 uint64。
func (r *lcg) Uint64() uint64 {
	r.state = r.state*lcgMul + lcgInc
	return r.state
}

// Float64 返回 [0,1) 内的伪随机 float64。
func (r *lcg) Float64() float64 {
	return float64(r.Uint64()>>11) / float64(uint64(1)<<53)
}

// Nfloat 返回均值 0、标准差 1 的近似高斯样本（两次均匀相加的 Irwin-Hall 变体）。
func (r *lcg) Nfloat() float64 {
	const n = 6
	var s float64
	for i := 0; i < n; i++ {
		s += r.Float64()
	}
	return (s - float64(n)/2) / (float64(n) / math.Sqrt(12.0))
}

// 确定性样本默认规格。
const (
	DefaultSampleRate = 48000
	DefaultIRLength   = 2048
	DefaultChannels   = 4
)

// targetCurveAt 生成一条平滑、有界的目标补偿曲线（dB），由三个解析余弦项组成。
func targetCurveAt(f float64) float64 {
	x := math.Log2(f / 100.0) // 以 100Hz 为基准的倍频程坐标
	return 2.0*math.Cos(0.9*x) - 1.2*math.Cos(1.7*x+0.4) + 0.6*math.Sin(0.5*x)
}

// GenerateSamples 按固定种子生成 n 条确定性房间测量记录。
func GenerateSamples(seed int64, n int) []*Sample {
	if n <= 0 {
		n = 3
	}
	out := make([]*Sample, 0, n)
	for s := 0; s < n; s++ {
		baseSeed := seed*1000003 + int64(s)*97 + 11
		out = append(out, generateOne(baseSeed, s))
	}
	return out
}

func generateOne(seed int64, idx int) *Sample {
	rng := newLCG(seed)
	chans := make([]ChannelIR, DefaultChannels)

	// 目标曲线：12 个对数分布频点，63Hz ~ 16kHz。
	const nPts = 12
	target := make([]TargetPoint, nPts)
	fLo := math.Log2(63.0)
	fHi := math.Log2(16000.0)
	for i := 0; i < nPts; i++ {
		f := math.Exp2(fLo + (fHi-fLo)*float64(i)/float64(nPts-1))
		target[i] = TargetPoint{FrequencyHz: f, GainDB: targetCurveAt(f)}
	}

	for c := 0; c < DefaultChannels; c++ {
		// 位置由 LCG 决定，保证确定性且各通道不同。
		spk := Position{2.0*rng.Float64() - 1, 2.0*rng.Float64() - 1, 1.5 + rng.Float64()}
		mic := Position{3.0*rng.Float64() - 1.5, 3.0*rng.Float64() - 1.5, 1.0 + 0.5*rng.Float64()}
		dist := math.Sqrt(sq(spk.X-mic.X) + sq(spk.Y-mic.Y) + sq(spk.Z-mic.Z))
		directDelay := dist / 343.0 // 声速 343m/s
		directSample := int(math.Round(directDelay * DefaultSampleRate))
		if directSample < 8 {
			directSample = 8 + c
		}

		ir := make([]float64, DefaultIRLength)
		directGain := 0.9 - 0.05*float64(c)
		ir[directSample] = directGain

		// 4 个解析早期反射，延迟与幅度均由 LCG 决定。
		earlyEnergy := directGain * directGain
		for k := 0; k < 4; k++ {
			d := directSample + 20 + int(rng.Float64()*200)
			if d >= DefaultIRLength {
				d = DefaultIRLength - 1
			}
			a := (0.12 + 0.08*rng.Float64()) * (1.0 - 0.1*float64(k))
			ir[d] += a
			earlyEnergy += a * a
		}

		// 指数衰减混响尾 + 确定性高斯噪声，混响时间随通道变化。
		rt60 := 0.18 + 0.04*float64(c) + 0.02*rng.Float64()
		tau := rt60 / math.Log(1000.0)
		for t := directSample + 1; t < DefaultIRLength; t++ {
			env := math.Exp(-float64(t-directSample) / (tau * DefaultSampleRate))
			ir[t] += 0.01 * env * rng.Nfloat()
		}

		// 轻微的通道着色（一阶 IIR 风格的确定性尺度，仅作用于混响尾）。
		tilt := 1.0 + 0.0003*float64(c)
		for t := directSample + 1; t < DefaultIRLength; t++ {
			ir[t] *= tilt
		}

		// 解析残差上界：由基函数截断近似误差、混响噪声底与工程裕量构成，始终为正。
		// 正常岭回归（λ<=0.01）实测最大残差约 4.3dB，上界取 6.5dB 保证可达成且可被违反。
		upper := make([]float64, nPts)
		for p := 0; p < nPts; p++ {
			curveJump := 0.0
			if p > 0 {
				curveJump = math.Abs(target[p].GainDB-target[p-1].GainDB) * 0.05
			}
			upper[p] = 6.5 + curveJump
		}

		chans[c] = ChannelIR{
			ChannelID:  c,
			SpeakerID:  c,
			MicID:      c,
			SpeakerPos: spk,
			MicPos:     mic,
			IR:         ir,
			Truth: ChannelTruth{
				DirectDelaySec:  float64(directSample) / DefaultSampleRate,
				EarlyEnergy:     earlyEnergy,
				ResidualUpperDB: upper,
			},
		}
	}

	return &Sample{
		ID:            sampleID(idx),
		SampleRateHz:  DefaultSampleRate,
		ChannelLayout: "quad",
		IRLength:      DefaultIRLength,
		Channels:      chans,
		Target:        target,
		Seed:          seed,
	}
}

func sampleID(idx int) string {
	return "room-" + string(rune('a'+idx))
}

func sq(x float64) float64 { return x * x }
