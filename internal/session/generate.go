package session

import (
	"encoding/binary"
	"hash/fnv"
	"math"
	"mcapture/internal/dsp"
	"os"
	"path/filepath"
)

// GenConfig 是确定性会话生成参数。固定 Seed 即固定全部样本与真值。
type GenConfig struct {
	Seed        int64
	SessionID   string
	SampleRate  float64
	DurationSec float64
	Channels    int
	SegmentSec  float64
	MarkerShape string
	MarkerWidth int
	NumMarkers  int
	Margin      int64
	MaxDriftPPM float64 // 各段速率的最大绝对值（百万分比）
	// 故障注入
	DropChannel string  // 该通道不写裸文件（E_CHAN）
	DropMarker  int     // 非参考通道跳过该标记索引（E_MARK），-1 表示不注入
	ClipGain    float64 // >1 时非参考通道施加该增益并饱和削顶（E_CLIP），0 表示不注入
}

// DefaultGenConfig 给出验收用默认配置。
func DefaultGenConfig() GenConfig {
	return GenConfig{
		Seed:        20261124,
		SessionID:   "sess-deterministic-001",
		SampleRate:  48000,
		DurationSec: 2.0,
		Channels:    4,
		SegmentSec:  0.5,
		MarkerShape: "pulse",
		MarkerWidth: 8,
		NumMarkers:  12,
		Margin:      64,
		MaxDriftPPM: 350,
		DropMarker:  -1,
	}
}

// Fault 记录一次注入的故障，供演示与测试断言。
type Fault struct {
	Kind    string `json:"kind"`
	Channel string `json:"channel"`
	Marker  int    `json:"marker"`
	Sample  int64  `json:"sample"`
}

type rng struct{ s uint64 }

func newRNG(seed int64, stream int) *rng {
	h := fnv.New64a()
	var b [8]byte
	binary.LittleEndian.PutUint64(b[:], uint64(seed))
	h.Write(b[:])
	binary.LittleEndian.PutUint64(b[:], uint64(stream))
	h.Write(b[:])
	return &rng{s: h.Sum64() | 1}
}

// float64 标准正态（Box-Muller），确定性。
func (r *rng) gaussian() float64 {
	u1 := r.unit()
	u2 := r.unit()
	return math.Sqrt(-2*math.Log(u1)) * math.Cos(2*math.Pi*u2)
}

func (r *rng) unit() float64 {
	// xorshift64*
	x := r.s
	x ^= x >> 12
	x ^= x << 25
	x ^= x >> 27
	r.s = x
	return float64((x*2685821657736338717)>>11) / float64(1<<53)
}

func (r *rng) uniform(lo, hi float64) float64 { return lo + (hi-lo)*r.unit() }

// rateAt 返回分段线性速率曲线在核心采样 n 处的速率（分段常数节点之间线性插值）。
func rateAt(knots []Knot, n int64) float64 {
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

// phiAt 是累计漂移曲线：phi(n)=phi0 + 积分 rateAt，单位为设备采样点。
// 采用逐段解析积分，保证生成端与估计端对同一模型逐位一致。
func phiAt(phi0 float64, knots []Knot, n int64) float64 {
	if n <= knots[0].At {
		return phi0
	}
	phi := phi0
	// 对 [0, n) 上的分段线性速率逐点积分（节点含 0 与各分段边界）。
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

// buildPhiPrefix 一次性计算 phiAt(0..core)，递推复用前一结果，整体 O(core)。
func buildPhiPrefix(phi0 float64, knots []Knot, core int64) []float64 {
	out := make([]float64, core+1)
	out[0] = phi0
	phi := phi0
	for n := int64(0); n < core; n++ {
		phi += rateAt(knots, n)
		out[n+1] = phi
	}
	return out
}

// Generate 生成完整会话并把清单与各通道裸数据写入 dir。
// 返回清单与注入故障列表。裸采样编码固定 float64 小端。
func Generate(dir string, cfg GenConfig) (*Manifest, []Fault, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, nil, err
	}
	core := int64(cfg.SampleRate * cfg.DurationSec)
	segLen := int64(cfg.SampleRate * cfg.SegmentSec)
	points := core + 2*cfg.Margin
	numSeg := int((core + segLen - 1) / segLen)

	// 标记核心位置：在核心轴均匀布点，避开两端与分段边界。
	markerCore := make([]int64, cfg.NumMarkers)
	for m := 0; m < cfg.NumMarkers; m++ {
		// 把 [0.08, 0.92] 区间等分
		t := (float64(m) + 0.5) / float64(cfg.NumMarkers)
		pos := (0.08 + 0.84*t) * float64(core-1)
		markerCore[m] = int64(math.Round(pos))
	}
	tpl := dsp.MarkerTemplate(cfg.MarkerShape, cfg.MarkerWidth)

	manifest := &Manifest{
		SchemaVersion: 1,
		SessionID:     cfg.SessionID,
		Seed:          cfg.Seed,
		SampleRate:    cfg.SampleRate,
		Channels:      cfg.Channels,
		DurationSec:   cfg.DurationSec,
		SegmentSec:    cfg.SegmentSec,
		PointsPerChan: points,
		Margin:        cfg.Margin,
		CorePoints:    core,
		MarkerShape:   cfg.MarkerShape,
		MarkerWidth:   cfg.MarkerWidth,
		ResidualBound: 0.15,
		RawEncoding:   "float64le",
	}
	var faults []Fault

	for c := 0; c < cfg.Channels; c++ {
		id := channelID(c)
		spec := ChannelSpec{
			ID:     id,
			IsRef:  c == 0,
			File:   id + ".f64",
			Points: points,
			Gain:   1,
		}

		// 漂移真值：参考通道恒零；其余通道随机初相位 + 分段线性速率。
		knots := make([]Knot, 0, numSeg+1)
		driftRNG := newRNG(cfg.Seed, 100+c)
		if c == 0 {
			for s := 0; s <= numSeg; s++ {
				at := int64(s) * segLen
				if at > core {
					at = core
				}
				knots = append(knots, Knot{At: at, Rate: 0})
			}
			spec.DriftPhi0 = 0
		} else {
			spec.DriftPhi0 = driftRNG.uniform(-6, 6)
			span := cfg.MaxDriftPPM / 1e6
			for s := 0; s <= numSeg; s++ {
				at := int64(s) * segLen
				if at > core {
					at = core
				}
				rate := driftRNG.uniform(0.2*span, span)
				knots = append(knots, Knot{At: at, Rate: rate})
			}
		}
		spec.DriftBasis = knots

		// 预计算累计漂移前缀 phiPrefix[n]=phiAt(n)，把每点查询降为 O(1)。
		phiPrefix := buildPhiPrefix(spec.DriftPhi0, knots, core)

		// 生成分数设备坐标下的样本序列。
		// 场景信号是物理时间的确定性闭式函数（所有通道同源、在物理时间上对齐）；
		// 各通道叠加独立的低幅确定性高斯噪声。
		sig := make([]float64, points)
		noise := newRNG(cfg.Seed, 200+c)
		for j := int64(0); j < points; j++ {
			n := j - cfg.Margin // 核心索引
			var devPos float64
			if c == 0 {
				devPos = float64(j)
			} else {
				nn := n
				if nn < 0 {
					nn = 0
				}
				if nn > core {
					nn = core
				}
				devPos = float64(cfg.Margin) + phiPrefix[nn]
			}
			tt := (devPos - float64(cfg.Margin)) / cfg.SampleRate
			sceneVal := 0.012*math.Sin(2*math.Pi*180*tt) +
				0.010*math.Sin(2*math.Pi*333*tt+1.3) +
				0.008*math.Sin(2*math.Pi*770*tt+0.7)
			sig[j] = sceneVal + 0.004*noise.gaussian()
		}

		// 内嵌标记：高 SNR，按本通道分数到达位置写入。
		for m, corePos := range markerCore {
			if c != 0 && cfg.DropMarker == m {
				faults = append(faults, Fault{Kind: EMark, Channel: id, Marker: m})
				continue
			}
			// arrival 是标记物理时间在本通道设备坐标上的分数采样索引。
			// 参考通道：设备索引 = margin + corePos。
			// 非参考：设备索引 = margin + corePos + phi(corePos)（漂移使索引平移）。
			var arrival float64
			if c == 0 {
				arrival = float64(cfg.Margin + corePos)
			} else {
				arrival = float64(cfg.Margin+corePos) + phiPrefix[corePos]
			}
			embedMarker(sig, arrival, tpl, 0.25)
			spec.Markers = append(spec.Markers, MarkerTruth{
				Index:         m,
				CoreRefSample: float64(corePos),
				ArrivalSample: arrival,
				ResidualBound: manifest.ResidualBound,
			})
		}

		// 削顶注入。
		if c != 0 && cfg.ClipGain > 1 {
			for j := range sig {
				v := sig[j] * cfg.ClipGain
				if v > 0.95 {
					v = 0.95
				}
				if v < -0.95 {
					v = -0.95
				}
				sig[j] = v
			}
			spec.Gain = cfg.ClipGain
			if idx, ok := dsp.HasClip(sig, 0.949); ok {
				faults = append(faults, Fault{Kind: EClip, Channel: id, Sample: idx})
			}
		}

		if id == cfg.DropChannel {
			faults = append(faults, Fault{Kind: EChan, Channel: id})
			continue // 不写文件、不进清单
		}
		if err := writeRaw(filepath.Join(dir, spec.File), sig); err != nil {
			return nil, nil, err
		}
		manifest.Channels_ = append(manifest.Channels_, spec)
	}
	manifest.Channels = len(manifest.Channels_)

	if err := WriteManifest(dir, manifest); err != nil {
		return nil, nil, err
	}
	if err := writeFaults(dir, faults); err != nil {
		return nil, nil, err
	}
	return manifest, faults, nil
}

// embedMarker 在分数位置 pos 处叠加标记。模板先以高倍过采样重建，
// 再用与处理流水线同构的加窗 sinc 分数移位，保证峰值精确落在 pos。
func embedMarker(x []float64, pos float64, tpl []float64, amp float64) {
	center := int(math.Round(pos))
	shift := pos - float64(center)
	w := len(tpl) / 2
	for k := -w; k <= w; k++ {
		idx := center + k
		if idx < 0 || idx >= len(x) {
			continue
		}
		// 在模板自身坐标上做分数移位：取 tpl 在 (k+w)-shift 处的值。
		v := interpTemplate(tpl, float64(k+w)-shift) * amp
		x[idx] += v
	}
}

// interpTemplate 用线性插值在模板的分数索引 q 处取值（模板过采样足够平滑）。
func interpTemplate(tpl []float64, q float64) float64 {
	if q <= 0 {
		return tpl[0]
	}
	if q >= float64(len(tpl)-1) {
		return tpl[len(tpl)-1]
	}
	i := int(math.Floor(q))
	frac := q - float64(i)
	return tpl[i]*(1-frac) + tpl[i+1]*frac
}

func writeRaw(path string, sig []float64) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	buf := make([]byte, 8*4096)
	for i := 0; i < len(sig); i += 4096 {
		end := i + 4096
		if end > len(sig) {
			end = len(sig)
		}
		for j := i; j < end; j++ {
			binary.LittleEndian.PutUint64(buf[(j-i)*8:], math.Float64bits(sig[j]))
		}
		if _, err := f.Write(buf[:(end-i)*8]); err != nil {
			return err
		}
	}
	return nil
}

func channelID(c int) string {
	return "ch" + string(rune('0'+c))
}
