package dsp

import (
	"testing"

	"audiobench/internal/model"
)

func TestValidateParamsErrorCodes(t *testing.T) {
	base := model.DefaultParams()
	cases := []struct {
		name   string
		mutate func(*model.Params)
		field  string
	}{
		{"帧长过短", func(p *model.Params) { p.FrameLength = 16 }, "frame_length"},
		{"帧长非2的幂", func(p *model.Params) { p.FrameLength = 1000 }, "frame_length"},
		{"帧移为零", func(p *model.Params) { p.FrameShift = 0 }, "frame_shift"},
		{"帧移大于帧长", func(p *model.Params) { p.FrameShift = p.FrameLength + 1 }, "frame_shift"},
		{"频带越界", func(p *model.Params) { p.Bands = 9999 }, "bands"},
		{"间隙容忍越界", func(p *model.Params) { p.GapToleranceFrames = -1 }, "gap_tolerance_frames"},
		{"阈值越界", func(p *model.Params) { p.MatchThreshold = 2 }, "match_threshold"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := base
			tc.mutate(&p)
			err := ValidateParams(p)
			if err == nil {
				t.Fatalf("期望 E_PARA，实际通过")
			}
			appErr, ok := err.(*model.AppError)
			if !ok || appErr.Code != model.EPara {
				t.Fatalf("期望 E_PARA，实际 %v", err)
			}
			if appErr.Details["field"] != tc.field {
				t.Fatalf("期望字段 %s，实际 %v", tc.field, appErr.Details["field"])
			}
		})
	}
}

func TestFingerprintDeterministicAndOrderIndependent(t *testing.T) {
	samples := make([]float64, 8192)
	for i := range samples {
		samples[i] = 0.5 * hannish(i, 4096)
	}
	p := model.DefaultParams()
	seq := ExtractAll(samples, p)
	// 乱序逐帧提取必须与顺序提取逐位相等。
	for _, idx := range []int{len(seq) - 1, 0, 3, 7, 1, 5} {
		got := ExtractFrame(samples, idx, p.FrameLength, p.FrameShift, p.Bands)
		want := seq[idx]
		if got.OffsetSample != want.OffsetSample || got.TotalEnergy != want.TotalEnergy {
			t.Fatalf("帧 %d 提取结果随调用顺序变化", idx)
		}
		for b := range got.Quantized {
			if got.Quantized[b] != want.Quantized[b] {
				t.Fatalf("帧 %d 量化码本不一致", idx)
			}
		}
	}
	// 相同输入重复提取必须稳定。
	again := ExtractAll(samples, p)
	for i := range seq {
		if seq[i].TotalEnergy != again[i].TotalEnergy {
			t.Fatalf("帧 %d 重复提取不稳定", i)
		}
	}
}

func TestFrameCount(t *testing.T) {
	if got := FrameCount(1000, 1024, 512); got != 0 {
		t.Fatalf("短于一帧应得 0，实际 %d", got)
	}
	// 1024 + 512*2 = 2048 → 3 帧。
	if got := FrameCount(2048, 1024, 512); got != 3 {
		t.Fatalf("期望 3 帧，实际 %d", got)
	}
}

func hannish(n, length int) float64 {
	return HannWindow(n, length)
}
