package session

// Manifest 是确定性样本生成器产出的会话清单（session.json）。
// 真值字段仅用于离线验收与漂移/残差核对，处理流水线严禁从真值反推采样。
type Manifest struct {
	SchemaVersion int           `json:"schema_version"`
	SessionID     string        `json:"session_id"`
	Seed          int64         `json:"seed"`
	SampleRate    float64       `json:"sample_rate"` // 标称采样率，所有通道必须相同
	Channels      int           `json:"channels"`
	DurationSec   float64       `json:"duration_sec"`
	SegmentSec    float64       `json:"segment_sec"`     // 默认分段长度
	PointsPerChan int64         `json:"points_per_chan"` // 含边界裕量的总采样点数
	Margin        int64         `json:"margin"`          // 前后边界裕量（采样点）
	CorePoints    int64         `json:"core_points"`     // 对齐输出使用的核心点数
	MarkerShape   string        `json:"marker_shape"`    // pulse | chirp
	MarkerWidth   int           `json:"marker_width"`    // 标记半宽（采样点）
	ResidualBound float64       `json:"residual_bound"`  // 目标残余偏移上界（采样点）
	RawEncoding   string        `json:"raw_encoding"`    // 固定为 float64le
	Channels_     []ChannelSpec `json:"channel_specs"`
}

// ChannelSpec 描述单个设备通道及其裸数据文件与解析真值。
type ChannelSpec struct {
	ID         string        `json:"id"`
	IsRef      bool          `json:"is_ref"`
	File       string        `json:"file"`
	Points     int64         `json:"points"`
	Gain       float64       `json:"gain"`
	DriftPhi0  float64       `json:"drift_phi0"`  // 初相位（采样点）
	DriftBasis []Knot        `json:"drift_basis"` // 分段线性速率节点（单位：每核心采样点）
	Markers    []MarkerTruth `json:"markers"`
}

// Knot 是分段线性速率曲线的一个节点：在核心采样索引 At 处斜率为 Rate。
type Knot struct {
	At   int64   `json:"at"`
	Rate float64 `json:"rate"`
}

// MarkerTruth 是单个对齐标记的解析真值。
type MarkerTruth struct {
	Index         int     `json:"index"`
	CoreRefSample float64 `json:"core_ref_sample"` // 参考时间轴上的核心采样位置
	ArrivalSample float64 `json:"arrival_sample"`  // 本通道含裕量坐标下的分数到达位置
	ResidualBound float64 `json:"residual_bound"`
}

// Fingerprint 汇总会话指纹，用于检查点版本匹配校验。
func (m *Manifest) Fingerprint() string {
	return m.SessionID
}

// RefChannel 返回参考通道规格。
func (m *Manifest) RefChannel() *ChannelSpec {
	for i := range m.Channels_ {
		if m.Channels_[i].IsRef {
			return &m.Channels_[i]
		}
	}
	return nil
}

// ChannelByID 按标识查找通道。
func (m *Manifest) ChannelByID(id string) *ChannelSpec {
	for i := range m.Channels_ {
		if m.Channels_[i].ID == id {
			return &m.Channels_[i]
		}
	}
	return nil
}
