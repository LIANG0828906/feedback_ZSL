package audiocal

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
)

// SaveManifest 把样本清单原子写入 JSON 文件（先写临时文件再 rename）。
func SaveManifest(dir, name string, samples []*Sample) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", newErrf(CodeStore, "dir", "创建样本目录失败: %v", err)
	}
	path := filepath.Join(dir, name)
	tmp := path + ".tmp"
	data, err := json.MarshalIndent(samples, "", "  ")
	if err != nil {
		return "", newErrf(CodeStore, "manifest", "序列化样本失败: %v", err)
	}
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return "", newErrf(CodeStore, "manifest", "写入样本失败: %v", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return "", newErrf(CodeStore, "manifest", "原子替换样本失败: %v", err)
	}
	return path, nil
}

// LoadManifest 读取并逐条做结构校验。
func LoadManifest(path string) ([]*Sample, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, newErrf(CodeStore, "manifest", "读取样本清单失败: %v", err)
	}
	var samples []*Sample
	if err := json.Unmarshal(raw, &samples); err != nil {
		return nil, newErrf(CodeStore, "manifest", "样本清单 JSON 非法: %v", err)
	}
	for i, s := range samples {
		if err := ValidateSample(s); err != nil {
			if be, ok := AsBizError(err); ok {
				be.Field = "sample[" + itoa(i) + "]." + be.Field
			}
			return nil, err
		}
	}
	return samples, nil
}

// ValidateSample 实现业务要求 1 的结构校验。
func ValidateSample(s *Sample) error {
	if s == nil {
		return newErr(CodeMeta, "测量记录为空", "sample")
	}
	if s.SampleRateHz <= 0 {
		return newErrf(CodeMeta, "sample_rate_hz", "采样率元数据缺失或非法: %d", s.SampleRateHz)
	}
	if s.ChannelLayout == "" {
		return newErr(CodeMeta, "声道布局元数据缺失", "channel_layout")
	}
	if s.IRLength <= 0 || len(s.Channels) == 0 {
		return newErr(CodeMeta, "脉冲响应或声道为空", "ir_length")
	}
	for ci, ch := range s.Channels {
		if len(ch.IR) == 0 {
			return newErrf(CodeMeta, "channels["+itoa(ci)+"].ir", "通道脉冲响应为空")
		}
		if len(ch.IR) != s.IRLength {
			return newErrf(CodeMeta, "channels["+itoa(ci)+"].ir",
				"脉冲响应长度 %d 与声明长度 %d 不一致", len(ch.IR), s.IRLength)
		}
	}
	if err := validateTarget(s.Target); err != nil {
		return err
	}
	return nil
}

func validateTarget(t []TargetPoint) error {
	if len(t) < 2 {
		return newErr(CodeCurve, "目标曲线至少需要两个频点", "target")
	}
	prev := 0.0
	for i, p := range t {
		if i > 0 && !(p.FrequencyHz > prev) {
			return newErrf(CodeCurve, "target["+itoa(i)+"].frequency_hz",
				"频点非严格单调: %v 未大于前一频点 %v", p.FrequencyHz, prev)
		}
		if p.FrequencyHz <= 0 {
			return newErrf(CodeCurve, "target["+itoa(i)+"].frequency_hz",
				"频点必须为正: %v", p.FrequencyHz)
		}
		if math.IsNaN(p.GainDB) || math.IsInf(p.GainDB, 0) {
			return newErrf(CodeCurve, "target["+itoa(i)+"].gain_db",
				"增益非有限值: %v", p.GainDB)
		}
		prev = p.FrequencyHz
	}
	return nil
}

// FitConfig 是拟合与导出的可配置参数。
type FitConfig struct {
	GridFrequencies []float64 // 可配置频点网格；nil 时取目标曲线频点
	Regularization  float64   // 岭回归权重 λ（>=0），越大正则越紧
	BasisCount      int       // 对数频三角基函数数量，决定滤波器“阶数”
	FIRTaps         int       // 合成 FIR 的抽头数
	MaxSegments     int       // 固件最大分段数
	MaxCoeffsPerSeg int       // 每段最大滤波器系数数
	WindowUS        int64     // 固件事件时序窗口宽度（微秒）
	ParamVersion    int       // 参数版本号
}

// DefaultFitConfig 返回确定性默认配置。
func DefaultFitConfig() FitConfig {
	return FitConfig{
		Regularization:  1e-4,
		BasisCount:      6,
		FIRTaps:         64,
		MaxSegments:     8,
		MaxCoeffsPerSeg: 64,
		WindowUS:        50000,
		ParamVersion:    1,
	}
}

// Validate 做参数越界检查，违规返回 E_PARA 并指出字段名。
func (c FitConfig) Validate() error {
	checks := []struct {
		name string
		bad  bool
		code ErrorCode
	}{
		{"regularization", c.Regularization < 0 || math.IsNaN(c.Regularization) || math.IsInf(c.Regularization, 0), CodePara},
		{"basis_count", c.BasisCount < 2 || c.BasisCount > 64, CodePara},
		{"fir_taps", c.FIRTaps < 8 || c.FIRTaps > 4096, CodePara},
		{"max_segments", c.MaxSegments < 1 || c.MaxSegments > 256, CodeWin},
		{"max_coeffs_per_segment", c.MaxCoeffsPerSeg < 1 || c.MaxCoeffsPerSeg > 4096, CodePara},
		{"window_us", c.WindowUS <= 0, CodeWin},
		{"param_version", c.ParamVersion < 1, CodePara},
	}
	for _, ck := range checks {
		if ck.bad {
			return newErrf(ck.code, ck.name, "参数越界: %s", ck.name)
		}
	}
	for i, f := range c.GridFrequencies {
		if f <= 0 || math.IsNaN(f) || math.IsInf(f, 0) {
			return newErrf(CodePara, "grid_frequencies["+itoa(i)+"]", "网格频点非法: %v", f)
		}
		if i > 0 && f <= c.GridFrequencies[i-1] {
			return newErr(CodePara, "网格频点必须严格单调", "grid_frequencies")
		}
	}
	return nil
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var b [24]byte
	pos := len(b)
	for i > 0 {
		pos--
		b[pos] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		pos--
		b[pos] = '-'
	}
	return string(b[pos:])
}
