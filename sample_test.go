package audiocal

import (
	"path/filepath"
	"testing"
)

func TestDeterministicSamples(t *testing.T) {
	a := GenerateSamples(20261124, 3)
	b := GenerateSamples(20261124, 3)
	if len(a) != 3 || len(b) != 3 {
		t.Fatalf("样本数量错误: %d vs %d", len(a), len(b))
	}
	for i := range a {
		if a[i].ID != b[i].ID || a[i].Seed != b[i].Seed {
			t.Fatalf("样本 %d 元数据不确定", i)
		}
		for ci := range a[i].Channels {
			ca, cb := a[i].Channels[ci], b[i].Channels[ci]
			if len(ca.IR) != len(cb.IR) {
				t.Fatalf("IR 长度不确定")
			}
			for k := range ca.IR {
				if ca.IR[k] != cb.IR[k] {
					t.Fatalf("样本 %d 通道 %d 抽头 %d 不确定", i, ci, k)
				}
			}
			if ca.Truth.DirectDelaySec != cb.Truth.DirectDelaySec {
				t.Fatalf("直达延迟解析真值不确定")
			}
		}
	}
}

func TestValidateSampleErrors(t *testing.T) {
	s := GenerateSamples(1, 1)[0]

	missingRate := cloneSample(s)
	missingRate.SampleRateHz = 0
	assertCode(t, ValidateSample(missingRate), CodeMeta, "sample_rate_hz")

	missingLayout := cloneSample(s)
	missingLayout.ChannelLayout = ""
	assertCode(t, ValidateSample(missingLayout), CodeMeta, "channel_layout")

	badCurve := cloneSample(s)
	badCurve.Target[2].FrequencyHz = badCurve.Target[1].FrequencyHz
	assertCode(t, ValidateSample(badCurve), CodeCurve, "target[2]")

	nanCurve := cloneSample(s)
	nanCurve.Target[3].GainDB = nan()
	assertCode(t, ValidateSample(nanCurve), CodeCurve, "target[3]")
}

func TestManifestRoundTrip(t *testing.T) {
	dir := t.TempDir()
	samples := GenerateSamples(7, 2)
	path, err := SaveManifest(dir, "s.json", samples)
	if err != nil {
		t.Fatal(err)
	}
	got, err := LoadManifest(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Channels[0].IR[10] != samples[0].Channels[0].IR[10] {
		t.Fatal("清单往返内容不一致")
	}
	if filepath.Base(path) != "s.json" {
		t.Fatal("清单路径错误")
	}
}

func TestConfigParaValidation(t *testing.T) {
	cfg := DefaultFitConfig()
	cfg.Regularization = -1
	assertCode(t, cfg.Validate(), CodePara, "regularization")

	cfg = DefaultFitConfig()
	cfg.BasisCount = 1
	assertCode(t, cfg.Validate(), CodePara, "basis_count")

	cfg = DefaultFitConfig()
	cfg.WindowUS = 0
	assertCode(t, cfg.Validate(), CodeWin, "window_us")

	cfg = DefaultFitConfig()
	cfg.MaxSegments = 0
	assertCode(t, cfg.Validate(), CodeWin, "max_segments")

	cfg = DefaultFitConfig()
	cfg.GridFrequencies = []float64{100, 50}
	assertCode(t, cfg.Validate(), CodePara, "grid_frequencies")
}

func cloneSample(s *Sample) *Sample {
	out := *s
	out.Channels = append([]ChannelIR(nil), s.Channels...)
	out.Target = append([]TargetPoint(nil), s.Target...)
	return &out
}

func assertCode(t *testing.T, err error, want ErrorCode, fieldSub string) {
	t.Helper()
	be, ok := AsBizError(err)
	if !ok {
		t.Fatalf("期望业务错误 %s，得到 %v", want, err)
	}
	if be.Code != want {
		t.Fatalf("期望码 %s，得到 %s", want, be.Code)
	}
	if fieldSub != "" && !contains(be.Field, fieldSub) {
		t.Fatalf("期望字段包含 %s，得到 %s", fieldSub, be.Field)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func nan() float64 {
	var zero float64
	return zero / zero
}
