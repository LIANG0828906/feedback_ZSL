package synth

// 本模块只使用整数/定点运算，不引入任何浮点运算，保证三路径逐采样点一致。

// AlgoVersion 是量化与编码算法版本，持久化快照与内存算法版本不一致时整体作废。
const AlgoVersion = "haptic-algo-1.0.0"

// 内部定点位宽：24 位累加器，输出 16 位设备包络帧。
const (
	FixedBits   = 24
	FixedOne    = 1 << FixedBits
	OutputBits  = 16
	OutputScale = 1 << (OutputBits - 1) // 输出满幅 32767
	OutputMax   = OutputScale - 1
	OutputMin   = -OutputScale
)

// EventType 触觉事件类型。
type EventType string

const (
	TypeHit    EventType = "hit"
	TypeShoot  EventType = "shoot"
	TypeBlast  EventType = "blast"
	TypeStep   EventType = "step"
	TypeUIFeed EventType = "ui"
)

// EnvelopePoint 是模板折线的一个折点（整型表示，避免浮点）。
// At 表示相对模板起点的偏移（采样点数，与量化单位同单位），Level 为千分比 0..1000。
type EnvelopePoint struct {
	At    int `json:"at"`
	Level int `json:"level"`
}

// Template 是一条包络模板：折线在 Duration 个采样点上声明。
type Template struct {
	Name     string          `json:"name"`
	Duration int             `json:"duration"`
	Points   []EnvelopePoint `json:"points"`
}

// TimingWindow 是设备包允许的时序标记窗口（微秒）。
type TimingWindow struct {
	MinOffsetUS int64 `json:"min_offset_us"`
	MaxOffsetUS int64 `json:"max_offset_us"`
}

// ChannelCap 是单通道能力声明。
type ChannelCap struct {
	Channel      int          `json:"channel"`
	Templates    []Template   `json:"templates"`
	MinEventGap  int          `json:"min_event_interval"`
	TimingWindow TimingWindow `json:"timing_window"`
}

// Manifest 是设备能力清单（持久化/生成器格式）。
type Manifest struct {
	ManifestVersion  string       `json:"manifest_version"`
	ChannelCount     int          `json:"channel_count"`
	SampleRateHz     int          `json:"sample_rate_hz"`
	QuantUnit        int          `json:"quant_unit"`
	SamplesPerPacket int          `json:"samples_per_packet"`
	FrameGrid        int          `json:"frame_grid"` // 每个游戏帧占多少量化网格点
	Channels         []ChannelCap `json:"channels"`
}

// Template 返回指定通道上的模板，不存在返回 nil。
func (m *Manifest) Template(ch int, name string) *Template {
	if ch < 0 || ch >= len(m.Channels) {
		return nil
	}
	for i := range m.Channels[ch].Templates {
		if m.Channels[ch].Templates[i].Name == name {
			return &m.Channels[ch].Templates[i]
		}
	}
	return nil
}

// GridUS 把网格点偏移换算成微秒时序标记。
func (m *Manifest) GridUS(grid int) int64 {
	return int64(grid) * 1_000_000 / int64(m.SampleRateHz)
}

// Event 是单个带类型的触觉事件（内存表示，全部为必填字段）。
type Event struct {
	ID          string    `json:"id"`
	Frame       int       `json:"frame"`
	FrameIndex  int       `json:"frame_index"`
	Type        EventType `json:"type"`
	Channel     int       `json:"channel"`
	Strength    int       `json:"strength"` // 千分比 1..1000
	Priority    int       `json:"priority"` // 数值越大优先级越高
	NoInterrupt bool      `json:"no_interrupt"`
	Template    string    `json:"template"`
}

// StartGrid 返回事件在量化网格上的起始偏移。
func (e *Event) StartGrid(frameGrid int) int {
	return e.Frame*frameGrid + e.FrameIndex
}

// Decision 是一个事件在通道仲裁中的处置结论。
type Decision string

const (
	DecisionAccepted Decision = "accepted"
	DecisionReplaced Decision = "replaced"
	DecisionDropped  Decision = "dropped"
)

// EventDecision 记录逐事件决策（供诊断界面与验证使用）。
type EventDecision struct {
	EventID  string   `json:"event_id"`
	Frame    int      `json:"frame"`
	Channel  int      `json:"channel"`
	Decision Decision `json:"decision"`
	Reason   string   `json:"reason"`
	AtGrid   int      `json:"at_grid"`
}

// Contributor 是单个采样点上参与叠加的事件权重明细。
type Contributor struct {
	EventID  string `json:"event_id"`
	Template string `json:"template"`
	WeightQ  int    `json:"weight_q"` // 千分比权重 0..1000
	LevelQ   int    `json:"level_q"`  // 模板在该点的千分比电平
}

// SampleDecision 是逐采样点量化决策记录。
type SampleDecision struct {
	Channel      int           `json:"channel"`
	GridOffset   int           `json:"grid_offset"`
	Contributors []Contributor `json:"contributors"`
	AccumFixed   int64         `json:"accum_fixed"` // 24 位定点累加值（饱和前）
	Output       int           `json:"output"`      // 16 位量化输出
	Residual     int64         `json:"residual"`    // 量化残差（定点域）
	Saturated    bool          `json:"saturated"`
}

// Frame 是一个量化采样点（定点包络帧）。
type Frame struct {
	Channel    int `json:"channel"`
	GridOffset int `json:"grid_offset"`
	Value      int `json:"value"`
}

// ChannelCurve 是单通道合成后的连续包络曲线。
type ChannelCurve struct {
	Channel   int              `json:"channel"`
	Frames    []Frame          `json:"frames"`
	Decisions []SampleDecision `json:"sample_decisions"`
	Events    []EventDecision  `json:"event_decisions"`
}
