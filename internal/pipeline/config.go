package pipeline

import "mcapture/internal/session"

// AlgoConfig 是处理参数。三条路径（实时/恢复/全量）必须使用同一组参数。
type AlgoConfig struct {
	SegmentSamples int64   `json:"segment_samples"`
	SincOrder      float64 `json:"sinc_order"`
	HalfWindow     int     `json:"half_window"`
	Tolerance      float64 `json:"tolerance"`      // 漂移估计核对容差（采样点）
	ResidualBound  float64 `json:"residual_bound"` // 残余偏移声明上界
	MaxDriftRate   float64 `json:"max_drift_rate"` // 允许的最大漂移率绝对值
	MaxWorkers     int     `json:"max_workers"`
	BufferCap      int     `json:"buffer_cap"`      // 每线程采样缓冲上限（点）
	ResampleBudget int     `json:"resample_budget"` // 全局重采样缓存预算（点）
	ParamVersion   string  `json:"param_version"`
	AlgoVersion    string  `json:"algo_version"`
}

// AlgoVersion 当前算法版本；任何确定性输出语义变更都必须升版本。
const AlgoVersion = "algo-1.0.0"

// DefaultConfig 给出与默认会话匹配的参数。
func DefaultConfig(m *session.Manifest) AlgoConfig {
	seg := int64(m.SampleRate * m.SegmentSec)
	return AlgoConfig{
		SegmentSamples: seg,
		SincOrder:      1.0,
		HalfWindow:     16,
		Tolerance:      0.25,
		ResidualBound:  m.ResidualBound,
		MaxDriftRate:   1e-3,
		MaxWorkers:     3,
		BufferCap:      int(seg) * 2,
		ResampleBudget: int(m.CorePoints) * 2,
		ParamVersion:   "params-1.0.0",
		AlgoVersion:    AlgoVersion,
	}
}

// Validate 做参数越界检查，越界返回 E_PARA 并指出字段。
func (c AlgoConfig) Validate() error {
	check := func(bad bool, field, msg string) error {
		if bad {
			return session.NewError(session.EPara, msg).With("", field)
		}
		return nil
	}
	if err := check(c.SegmentSamples <= 0, "segment_samples", "segment length must be positive"); err != nil {
		return err
	}
	if err := check(c.SincOrder < 0.5 || c.SincOrder > 2.0, "sinc_order", "sinc order out of [0.5,2.0]"); err != nil {
		return err
	}
	if err := check(c.HalfWindow < 4 || c.HalfWindow > 64, "half_window", "half window out of [4,64]"); err != nil {
		return err
	}
	if err := check(c.Tolerance <= 0 || c.Tolerance > 2, "tolerance", "tolerance out of (0,2] samples"); err != nil {
		return err
	}
	if err := check(c.ResidualBound <= 0 || c.ResidualBound > 1, "residual_bound", "residual bound out of (0,1]"); err != nil {
		return err
	}
	if err := check(c.MaxDriftRate <= 0 || c.MaxDriftRate > 0.01, "max_drift_rate", "drift rate out of (0,0.01]"); err != nil {
		return err
	}
	if err := check(c.MaxWorkers < 1, "max_workers", "need at least one worker"); err != nil {
		return err
	}
	if err := check(c.BufferCap < int(c.SegmentSamples), "buffer_cap", "per-worker buffer smaller than one segment"); err != nil {
		return err
	}
	if err := check(c.ResampleBudget < int(c.SegmentSamples), "resample_budget", "resample budget smaller than one segment"); err != nil {
		return err
	}
	return nil
}
