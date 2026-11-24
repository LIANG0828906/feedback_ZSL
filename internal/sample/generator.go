// Package sample 实现本地确定性游戏语音事件素材生成：固定种子 →
// 单声道 float32 PCM 描述文件 + 事件清单 + 类别特征音调参数，完全离线。
package sample

import (
	"encoding/binary"
	"encoding/json"
	"math"
	"math/rand/v2"
	"os"
	"path/filepath"

	"audiobench/internal/dsp"
	"audiobench/internal/model"
)

// SupportedSampleRates 采样率支持范围/枚举。
var SupportedSampleRates = map[int]bool{8000: true, 16000: true, 22050: true, 44100: true, 48000: true}

// DefaultCategories 固定的六类游戏语音事件及特征音调参数。
func DefaultCategories() []model.Category {
	return []model.Category{
		{Label: "footstep", BaseFreqHz: 220, StepHz: 110, Partials: 2},
		{Label: "gunshot", BaseFreqHz: 440, StepHz: 160, Partials: 3},
		{Label: "explosion", BaseFreqHz: 120, StepHz: 60, Partials: 4},
		{Label: "shout", BaseFreqHz: 660, StepHz: 220, Partials: 3},
		{Label: "reload", BaseFreqHz: 330, StepHz: 90, Partials: 2},
		{Label: "alarm", BaseFreqHz: 880, StepHz: 120, Partials: 4},
	}
}

// Options 控制一组素材的生成。
type Options struct {
	OutDir       string
	Seed         int64
	SampleRate   int
	MaterialSecs int
	EventCount   int
	WindowSize   int
}

// Dataset 生成结果清单。
type Dataset struct {
	Seed       int64            `json:"seed"`
	SampleRate int              `json:"sample_rate"`
	Categories []model.Category `json:"categories"`
	Materials  []string         `json:"materials"`
}

// GenerateAll 生成普通、静音、无命中三类素材与类别参数文件。
func GenerateAll(opt Options) (*Dataset, error) {
	if err := os.MkdirAll(opt.OutDir, 0o755); err != nil {
		return nil, model.NewErr(model.EIO, "创建输出目录失败: %v", err)
	}
	categories := DefaultCategories()
	dataset := &Dataset{Seed: opt.Seed, SampleRate: opt.SampleRate, Categories: categories}

	specs := []struct {
		id    string
		quiet bool
		empty bool
	}{
		{id: "mat_main", quiet: false, empty: false},
		{id: "mat_quiet", quiet: true, empty: false},
		{id: "mat_empty", quiet: false, empty: true},
	}
	for _, spec := range specs {
		mat, err := generateOne(opt, categories, spec.id, spec.quiet, spec.empty)
		if err != nil {
			return nil, err
		}
		dataset.Materials = append(dataset.Materials, filepath.Join(opt.OutDir, mat.ID+".json"))
	}

	catPath := filepath.Join(opt.OutDir, "categories.json")
	if err := writeJSON(catPath, categories); err != nil {
		return nil, err
	}
	if err := writeJSON(filepath.Join(opt.OutDir, "dataset.json"), dataset); err != nil {
		return nil, err
	}
	return dataset, nil
}

func generateOne(opt Options, categories []model.Category, id string, quiet, empty bool) (*model.Material, error) {
	// 每个素材使用独立但确定派生的子流，保证同种子逐字节可复现。
	rng := rand.New(rand.NewPCG(uint64(opt.Seed), uint64(opt.Seed)^hashID(id)))
	total := opt.SampleRate * opt.MaterialSecs
	buf := make([]float64, total)
	events := []model.EventDecl{}

	if !empty {
		gapMin := opt.SampleRate / 4
		eventMin := opt.SampleRate / 4
		eventMax := opt.SampleRate
		cursor := opt.SampleRate / 8
		for i := 0; i < opt.EventCount; i++ {
			dur := eventMin + rng.IntN(eventMax-eventMin)
			if cursor+dur >= total-gapMin {
				break
			}
			cat := categories[rng.IntN(len(categories))]
			if quiet {
				// 静音素材：事件振幅压到本底噪声量级，声能量占比低于阈值。
				renderQuietTone(buf, cursor, cursor+dur, opt.SampleRate, cat)
			} else {
				dsp.RenderTone(buf, cursor, cursor+dur, opt.SampleRate, cat.BaseFreqHz, cat.StepHz, cat.Partials)
			}
			events = append(events, model.EventDecl{
				Index:       i,
				Label:       cat.Label,
				StartSample: cursor,
				EndSample:   cursor + dur,
				BaseFreqHz:  cat.BaseFreqHz,
			})
			cursor += dur + gapMin + rng.IntN(gapMin)
		}
	}

	// 确定性本底噪声。静音素材使用极低振幅以触发 E_QUIET；
	// 空素材保留高于静音门限的噪声，用于验证“无命中→空事件列表而非错误”。
	noiseRng := rand.New(rand.NewPCG(uint64(opt.Seed)^0x9e3779b9, hashID(id)))
	noiseAmp := 0.01
	if empty {
		noiseAmp = 0.05
	}
	for i := range buf {
		buf[i] += (noiseRng.Float64()*2 - 1) * noiseAmp
	}

	pcmPath := filepath.Join(opt.OutDir, id+".pcm")
	if err := writePCM(pcmPath, buf); err != nil {
		return nil, err
	}
	mat := &model.Material{
		ID:              id,
		PCMFile:         filepath.Base(pcmPath),
		SampleRate:      opt.SampleRate,
		Channels:        1,
		TotalSamples:    total,
		SampleTolerance: opt.WindowSize,
		Seed:            opt.Seed,
		Quiet:           quiet,
		Categories:      categories,
		Events:          events,
	}
	if err := writeJSON(filepath.Join(opt.OutDir, id+".json"), mat); err != nil {
		return nil, err
	}
	return mat, nil
}

func renderQuietTone(buf []float64, start, end, sampleRate int, cat model.Category) {
	// 与正常合成同一频率结构，但振幅仅为本底噪声量级（0.008），
	// 使声能量占比稳定落在 E_QUIET 阈值之下。
	length := end - start
	for n := 0; n < length; n++ {
		env := dsp.HannWindow(n, length)
		sample := 0.0
		for p := 0; p < cat.Partials; p++ {
			freq := cat.BaseFreqHz + cat.StepHz*float64(p)
			sample += math.Sin(2*math.Pi*freq*float64(start+n)/float64(sampleRate)) / float64(cat.Partials)
		}
		buf[start+n] += 0.008 * env * sample
	}
}

func hashID(id string) uint64 {
	var h uint64 = 1469598103934665603
	for i := 0; i < len(id); i++ {
		h ^= uint64(id[i])
		h *= 1099511628211
	}
	return h
}

func writePCM(path string, samples []float64) error {
	f, err := os.Create(path)
	if err != nil {
		return model.NewErr(model.EIO, "创建 PCM 文件失败: %v", err)
	}
	defer f.Close()
	var buf [4]byte
	for _, v := range samples {
		if v > 1 {
			v = 1
		} else if v < -1 {
			v = -1
		}
		binary.LittleEndian.PutUint32(buf[:], math.Float32bits(float32(v)))
		if _, err := f.Write(buf[:]); err != nil {
			return model.NewErr(model.EIO, "写 PCM 失败: %v", err)
		}
	}
	return nil
}

func writeJSON(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return model.NewErr(model.EIO, "序列化 JSON 失败: %v", err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return model.NewErr(model.EIO, "写 JSON 失败: %v", err)
	}
	return nil
}
