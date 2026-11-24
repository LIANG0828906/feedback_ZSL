package model

// ChannelLayout 描述样本的声道布局。Names 与 Signals/Truth 的通道顺序一一对应。
type ChannelLayout struct {
	ID       string   `json:"id"`
	Names    []string `json:"names"`
	Channels int      `json:"channels"`
}

// ReflectionTruth 是单条早期反射的解析真值，真值仅由生成器构造产生。
type ReflectionTruth struct {
	ArrivalSample int     `json:"arrival_sample"`
	Amplitude     float64 `json:"amplitude"`
	Energy        float64 `json:"energy"`
}

// TailTruth 是指数衰减尾音参数：x[n] = amplitude * exp(-rate*(n-onset))。
type TailTruth struct {
	OnsetSample int     `json:"onset_sample"`
	Amplitude   float64 `json:"amplitude"`
	Rate        float64 `json:"rate"`
}

// ChannelTruth 是逐通道解析真值。
type ChannelTruth struct {
	Channel      string            `json:"channel"`
	DirectSample int               `json:"direct_sample"`
	DirectAmp    float64           `json:"direct_amplitude"`
	Reflections  []ReflectionTruth `json:"reflections"`
	Tail         TailTruth         `json:"tail"`
}

// Manifest 是确定性样本的清单与解析真值。
type Manifest struct {
	SchemaVersion string         `json:"schema_version"`
	Seed          int64          `json:"seed"`
	SampleRate    int            `json:"sample_rate"`
	Layout        ChannelLayout  `json:"layout"`
	LengthSamples int            `json:"length_samples"`
	SignalFiles   []string       `json:"signal_files"`
	DryFiles      []string       `json:"dry_files"`
	Reference     string         `json:"reference_channel"`
	Truth         []ChannelTruth `json:"truth"`
	Faults        FaultSpec      `json:"faults"`
	GenVersion    string         `json:"generator_version"`
}

// FaultSpec 描述通过输入/参数注入的异常，零值表示不注入。
// 所有异常均由清单或运行参数驱动，禁止通过修改源码制造异常。
type FaultSpec struct {
	// MissingChannel 非空时，该声道的信号文件不产出（通道缺失，E_CHAN）。
	MissingChannel string `json:"missing_channel,omitempty"`
	// OrderFrame/OrderChannel 指定在某帧某声道把反射到达点倒置（E_ORDER）。
	OrderFrame   int    `json:"order_frame,omitempty"`
	OrderChannel string `json:"order_channel,omitempty"`
	// ClipFrame/ClipChannel 指定在某帧某声道施加能量增益溢出（E_CLIP）。
	ClipFrame   int    `json:"clip_frame,omitempty"`
	ClipChannel string `json:"clip_channel,omitempty"`
	// ClipGain 为溢出增益倍数，需使叠加结果超过 +-1。
	ClipGain float64 `json:"clip_gain,omitempty"`
	// BadFrames 为坏帧序号集合（E_FRAME）。
	BadFrames []int `json:"bad_frames,omitempty"`
}

// Event 是分析阶段检出的离散声学事件（直达或反射）。
type Event struct {
	Sample    int     `json:"sample"`
	Amplitude float64 `json:"amplitude"`
	Energy    float64 `json:"energy"`
	IsDirect  bool    `json:"is_direct"`
}

// FrameParams 是可配置的分帧与渲染参数。
type FrameParams struct {
	FrameLength int `json:"frame_length"` // 帧长（样本）
	FrameHop    int `json:"frame_hop"`    // 帧移（样本）
	WindowLen   int `json:"window_len"`   // 估计窗长（样本）
	ConvWindow  int `json:"conv_window"`  // 卷积窗长（样本）
	RenderBlock int `json:"render_block"` // 渲染块大小（样本）
	// InterpOrder 为渲染插值阶数：0=最近邻，1=线性，3=三次。
	InterpOrder int `json:"interp_order"`
	// BoundScale 为误差上界收紧系数（诊断用，1 为不收紧），可触发 E_BOUND。
	BoundScale float64 `json:"bound_scale"`
	// MaxWorkers 为线程池最大并行度。
	MaxWorkers int `json:"max_workers"`
	// BufferCap 为每线程采样缓冲上限（样本数），耗尽则阻塞排队。
	BufferCap int `json:"buffer_cap"`
}

// ChannelEstimate 是单帧单声道的估计结果。
type ChannelEstimate struct {
	Channel        string  `json:"channel"`
	DirectSample   int     `json:"direct_sample"`
	DirectEnergy   float64 `json:"direct_energy"`
	EarlyEnergy    float64 `json:"early_energy"`
	ReverbEnergy   float64 `json:"reverb_energy"`
	DryReverbRatio float64 `json:"dry_reverb_ratio"`
	Events         []Event `json:"events"`
	TailOnset      int     `json:"tail_onset"`
	TailAmp        float64 `json:"tail_amp"`
	TailRate       float64 `json:"tail_rate"`
}

// ErrorMetric 是逐帧四项误差与对应上界。
type ErrorMetric struct {
	PeakQuantization float64 `json:"peak_quantization"`
	EnergyTruncation float64 `json:"energy_truncation"`
	ITDEstimation    float64 `json:"itd_estimation"`
	RenderInterp     float64 `json:"render_interp"`
}

// ErrorBound 是四项误差的逐帧上界与随帧序累积上界，
// 仅由帧长/帧移/窗长/插值阶数/采样率量化步长结构推导。
type ErrorBound struct {
	PerFrame   ErrorMetric `json:"per_frame"`
	Cumulative ErrorMetric `json:"cumulative"`
}

// FrameEnergy 是逐帧重建能量统计。
type FrameEnergy struct {
	OriginalEnergy float64   `json:"original_energy"`
	RenderEnergy   float64   `json:"render_energy"`
	PerChannel     []float64 `json:"per_channel_original"`
	PerChannelRend []float64 `json:"per_channel_render"`
}

// FrameStatus 是帧状态机的四态。
type FrameStatus string

const (
	StatusPending   FrameStatus = "pending"
	StatusEstimated FrameStatus = "estimated"
	StatusCommitted FrameStatus = "committed"
	StatusFailed    FrameStatus = "failed"
)

// FrameSnapshot 是一帧的完整持久化/传输单元。
type FrameSnapshot struct {
	Index      int                `json:"index"`
	Status     FrameStatus        `json:"status"`
	Start      int                `json:"start"`
	End        int                `json:"end"`
	Estimates  []ChannelEstimate  `json:"estimates"`
	ITD        map[string]float64 `json:"itd"` // 相对参考声道，单位样本
	Measured   ErrorMetric        `json:"measured"`
	Bound      ErrorBound         `json:"bound"`
	Energy     FrameEnergy        `json:"energy"`
	Rendered   [][]float64        `json:"rendered"` // 仅该帧拥有区间
	FailCode   ErrorCode          `json:"fail_code,omitempty"`
	FailReason string             `json:"fail_reason,omitempty"`
}

// RunReport 是一次完整流水线的输出汇总。
type RunReport struct {
	Mode         string          `json:"mode"` // realtime | resume | replay
	Frames       int             `json:"frames"`
	Committed    int             `json:"committed"`
	FrameReports []FrameSnapshot `json:"frame_reports"`
	Rendered     [][]float64     `json:"rendered"`
	Original     [][]float64     `json:"original"`
	Layout       ChannelLayout   `json:"layout"`
	SampleRate   int             `json:"sample_rate"`
	CommitSeq    []int           `json:"commit_seq"`
}

// Divergence 定位三条执行路径的首个不一致点。
type Divergence struct {
	Frame       int    `json:"frame"`
	Channel     string `json:"channel"`
	SampleIndex int    `json:"sample_index"`
	Field       string `json:"field"`
	ValueA      string `json:"value_a"`
	ValueB      string `json:"value_b"`
}
