// Package ingest 负责素材描述文件、PCM 与事件清单的加载及结构校验。
package ingest

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"

	"audiobench/internal/model"
	"audiobench/internal/sample"
)

const minSampleRate, maxSampleRate = 8000, 48000

// LoadMaterial 读取描述 JSON 并执行完整结构校验，返回带绝对 PCM 路径的素材。
func LoadMaterial(descPath string) (*model.Material, error) {
	raw, err := os.ReadFile(descPath)
	if err != nil {
		return nil, model.NewErr(model.EIO, "读取素材描述失败: %v", err)
	}
	var mat model.Material
	if err := json.Unmarshal(raw, &mat); err != nil {
		return nil, model.NewErr(model.ESchema, "描述文件不是合法 JSON: %v", err)
	}
	if err := validateStructure(&mat); err != nil {
		return nil, err
	}
	// PCM 路径相对描述文件解析，并核对实际字节数。
	if !filepath.IsAbs(mat.PCMFile) {
		mat.PCMFile = filepath.Join(filepath.Dir(descPath), mat.PCMFile)
	}
	pcmBytes, err := os.ReadFile(mat.PCMFile)
	if err != nil {
		return nil, model.NewErr(model.EIO, "读取 PCM 失败: %v", err)
	}
	if len(pcmBytes) != mat.TotalSamples*4 {
		return nil, model.NewErr(model.ERange,
			"PCM 字节数与总采样点数不符").
			With("expected_bytes", mat.TotalSamples*4).
			With("actual_bytes", len(pcmBytes))
	}
	return &mat, nil
}

// ReadPCM 读取单声道 float32 小端 PCM 为 float64 采样。
func ReadPCM(path string) ([]float64, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, model.NewErr(model.EIO, "读取 PCM 失败: %v", err)
	}
	if len(data)%4 != 0 {
		return nil, model.NewErr(model.ESchema, "PCM 长度不是 4 字节整数倍")
	}
	out := make([]float64, len(data)/4)
	for i := range out {
		bits := binary.LittleEndian.Uint32(data[i*4:])
		out[i] = float64(math.Float32frombits(bits))
	}
	return out, nil
}

// ContentHash 计算 PCM 原始字节的 SHA-256，用于检查点快照身份校验。
func ContentHash(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", model.NewErr(model.EIO, "计算素材哈希失败: %v", err)
	}
	return fmt.Sprintf("%x", sha256.Sum256(data)), nil
}

func validateStructure(mat *model.Material) error {
	// 字段缺失/非法 → E_SCHEMA。
	if mat.ID == "" {
		return model.NewErr(model.ESchema, "字段缺失: id")
	}
	if mat.PCMFile == "" {
		return model.NewErr(model.ESchema, "字段缺失: pcm_file")
	}
	if mat.Channels != 1 {
		return model.NewErr(model.ESchema, "仅支持单声道，实测 channels=%d", mat.Channels).
			With("field", "channels").With("actual", mat.Channels)
	}
	if mat.SampleRate < minSampleRate || mat.SampleRate > maxSampleRate ||
		!sample.SupportedSampleRates[mat.SampleRate] {
		return model.NewErr(model.ESchema, "采样率 %d 不在支持枚举 %v 内", mat.SampleRate,
			[]int{8000, 16000, 22050, 44100, 48000}).
			With("field", "sample_rate").With("actual", mat.SampleRate)
	}
	if mat.TotalSamples <= 0 {
		return model.NewErr(model.ESchema, "total_samples 必须为正").
			With("field", "total_samples").With("actual", mat.TotalSamples)
	}
	if len(mat.Categories) == 0 {
		return model.NewErr(model.ESchema, "字段缺失: categories")
	}
	labelSet := map[string]bool{}
	for _, c := range mat.Categories {
		if c.Label == "" || c.BaseFreqHz <= 0 || c.Partials <= 0 {
			return model.NewErr(model.ESchema, "类别枚举存在空标签或非法音调参数: %+v", c)
		}
		labelSet[c.Label] = true
	}
	if mat.SampleTolerance < 0 {
		return model.NewErr(model.ESchema, "sample_tolerance 不能为负")
	}

	// 逐事件校验：越界/重叠 → E_RANGE，类别非法 → E_SCHEMA。
	var prevEnd int
	for idx, ev := range mat.Events {
		if ev.Label == "" {
			return model.NewErr(model.ESchema, "事件 %d 缺少类别标签", ev.Index).
				With("event_index", idx)
		}
		if !labelSet[ev.Label] {
			return model.NewErr(model.ESchema, "事件 %d 类别 %q 不在声明枚举内", idx, ev.Label).
				With("event_index", idx).With("label", ev.Label)
		}
		if ev.StartSample < 0 || ev.EndSample > mat.TotalSamples || ev.StartSample >= ev.EndSample {
			return model.NewErr(model.ERange,
				"事件 %d 区间越界或非法: [%d, %d)，素材边界 [0, %d)",
				idx, ev.StartSample, ev.EndSample, mat.TotalSamples).
				With("event_index", idx).
				With("actual_start", ev.StartSample).
				With("actual_end", ev.EndSample).
				With("material_bound", mat.TotalSamples)
		}
		if idx > 0 && ev.StartSample < prevEnd {
			return model.NewErr(model.ERange,
				"事件 %d 与前一事件重叠: 起点 %d 早于前一事件终点 %d",
				idx, ev.StartSample, prevEnd).
				With("event_index", idx).
				With("actual_start", ev.StartSample).
				With("prev_end", prevEnd)
		}
		prevEnd = ev.EndSample
	}
	return nil
}
