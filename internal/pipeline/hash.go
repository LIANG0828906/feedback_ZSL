package pipeline

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math"

	"spatialir/internal/model"
)

func float64Bits(v float64) uint64 { return math.Float64bits(v) }

// InputHash 覆盖清单元数据（不含故障注入声明）、逐通道采样与干声片段，
// 保证任一声学输入字节变化都会使旧快照整体作废。故障注入属于运行参数，
// 其配置/解除不得改变输入哈希，否则失败帧重试会错误地作废全部已提交帧。
func InputHash(m *model.Manifest, signals, dry [][]float64) (string, error) {
	h := sha256.New()
	meta, err := json.Marshal(struct {
		SchemaVersion string               `json:"schema_version"`
		Seed          int64                `json:"seed"`
		SampleRate    int                  `json:"sample_rate"`
		Layout        model.ChannelLayout  `json:"layout"`
		LengthSamples int                  `json:"length_samples"`
		SignalFiles   []string             `json:"signal_files"`
		DryFiles      []string             `json:"dry_files"`
		Reference     string               `json:"reference_channel"`
		Truth         []model.ChannelTruth `json:"truth"`
		GenVersion    string               `json:"generator_version"`
	}{
		SchemaVersion: m.SchemaVersion, Seed: m.Seed, SampleRate: m.SampleRate,
		Layout: m.Layout, LengthSamples: m.LengthSamples, SignalFiles: m.SignalFiles,
		DryFiles: m.DryFiles, Reference: m.Reference, Truth: m.Truth, GenVersion: m.GenVersion,
	})
	if err != nil {
		return "", err
	}
	h.Write(meta)
	for _, ch := range signals {
		for _, v := range ch {
			var buf [8]byte
			bits := float64Bits(v)
			for i := 0; i < 8; i++ {
				buf[i] = byte(bits >> (8 * i))
			}
			h.Write(buf[:])
		}
	}
	for _, ch := range dry {
		for _, v := range ch {
			var buf [8]byte
			bits := float64Bits(v)
			for i := 0; i < 8; i++ {
				buf[i] = byte(bits >> (8 * i))
			}
			h.Write(buf[:])
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
