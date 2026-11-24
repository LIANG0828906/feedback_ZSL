package model

// AlgorithmVersion 指纹/聚合算法版本号，算法语义发生变化时必须升级，
// 持久化检查点会据此整体作废。
const AlgorithmVersion = "fp-v1.0.0"

// Category 声明一个语音事件类别及其特征音调参数（基频与谐波步进）。
type Category struct {
	Label      string  `json:"label"`
	BaseFreqHz float64 `json:"base_freq_hz"`
	StepHz     float64 `json:"step_hz"`
	Partials   int     `json:"partials"`
}

// EventDecl 清单中声明的一个事件片段（EndSample 为开区间）。
type EventDecl struct {
	Index       int     `json:"index"`
	Label       string  `json:"label"`
	StartSample int     `json:"start_sample"`
	EndSample   int     `json:"end_sample"`
	BaseFreqHz  float64 `json:"base_freq_hz"`
}

// Material 单声道 PCM 素材的完整描述。
type Material struct {
	ID              string      `json:"id"`
	PCMFile         string      `json:"pcm_file"`
	SampleRate      int         `json:"sample_rate"`
	Channels        int         `json:"channels"`
	TotalSamples    int         `json:"total_samples"`
	SampleTolerance int         `json:"sample_tolerance"`
	Seed            int64       `json:"seed"`
	Quiet           bool        `json:"quiet,omitempty"`
	Categories      []Category  `json:"categories"`
	Events          []EventDecl `json:"events"`
}

// Params 分帧、指纹量化与事件聚合的全部可调参数。
type Params struct {
	FrameLength        int     `json:"frame_length"`
	FrameShift         int     `json:"frame_shift"`
	Bands              int     `json:"bands"`
	GapToleranceFrames int     `json:"gap_tolerance_frames"`
	MinHitFrames       int     `json:"min_hit_frames"`
	MatchThreshold     float64 `json:"match_threshold"`
	QuietRatio         float64 `json:"quiet_ratio_threshold"`
}

// DefaultParams 返回工作台默认参数。
func DefaultParams() Params {
	return Params{
		FrameLength:        1024,
		FrameShift:         512,
		Bands:              32,
		GapToleranceFrames: 2,
		MinHitFrames:       2,
		MatchThreshold:     0.55,
		QuietRatio:         0.05,
	}
}

// Fingerprint 单帧频谱指纹：相对能量分布 + 量化码本 + 时间偏移。
type Fingerprint struct {
	Frame          int       `json:"frame"`
	OffsetSample   int       `json:"offset_sample"`
	TotalEnergy    float64   `json:"total_energy"`
	RelativeEnergy []float64 `json:"relative_energy"`
	Quantized      []int     `json:"quantized"`
}

// Event 聚合得到的事件区间（帧区间为左闭右开，采样点区间同样左闭右开）。
type Event struct {
	Label          string  `json:"label"`
	StartSample    int     `json:"start_sample"`
	EndSample      int     `json:"end_sample"`
	StartFrame     int     `json:"start_frame"`
	EndFrame       int     `json:"end_frame"`
	HitFrames      int     `json:"hit_frames"`
	MeanSimilarity float64 `json:"mean_similarity"`
	Confidence     float64 `json:"confidence"`
}

// QualityReport 标注结果相对清单真值的质量校验报告。
type QualityReport struct {
	TotalDeclared   int               `json:"total_declared"`
	TotalDetected   int               `json:"total_detected"`
	Matched         int               `json:"matched"`
	LabelCorrect    int               `json:"label_correct"`
	LabelAccuracy   float64           `json:"label_accuracy"`
	MeanStartBias   float64           `json:"mean_start_bias"`
	MeanEndBias     float64           `json:"mean_end_bias"`
	MaxAbsStartBias int               `json:"max_abs_start_bias"`
	MaxAbsEndBias   int               `json:"max_abs_end_bias"`
	Tolerance       int               `json:"tolerance"`
	ConfidenceMean  float64           `json:"confidence_mean"`
	ConfidenceMin   float64           `json:"confidence_min"`
	ConfidenceMax   float64           `json:"confidence_max"`
	ConfidenceHist  map[string]int    `json:"confidence_hist"`
	PairDetails     []QualityPair     `json:"pair_details"`
	UnmatchedDecl   []int             `json:"unmatched_declared"`
	UnmatchedDetect []int             `json:"unmatched_detected"`
	Mismatches      []QualityMismatch `json:"mismatches"`
}

type QualityPair struct {
	DeclIndex    int     `json:"decl_index"`
	DeclLabel    string  `json:"decl_label"`
	DetectLabel  string  `json:"detect_label"`
	StartBias    int     `json:"start_bias"`
	EndBias      int     `json:"end_bias"`
	Confidence   float64 `json:"confidence"`
	LabelCorrect bool    `json:"label_correct"`
	WithinTol    bool    `json:"within_tolerance"`
}

type QualityMismatch struct {
	DeclIndex   int    `json:"decl_index"`
	DeclLabel   string `json:"decl_label"`
	DetectLabel string `json:"detect_label"`
	Reason      string `json:"reason"`
	StartBias   int    `json:"start_bias,omitempty"`
	EndBias     int    `json:"end_bias,omitempty"`
	Tolerance   int    `json:"tolerance,omitempty"`
}
