package validate

import (
	"os"
	"path/filepath"
	"testing"

	"haptickernel/internal/sample"
	"haptickernel/internal/synth"
)

func TestLoadOKAndErrorCodes(t *testing.T) {
	dir := t.TempDir()
	mp, ep, err := sample.Generate(dir)
	if err != nil {
		t.Fatal(err)
	}
	m, err := LoadManifest(mp)
	if err != nil {
		t.Fatalf("manifest 加载失败: %v", err)
	}
	events, err := LoadEvents(ep, m)
	if err != nil {
		t.Fatalf("events 加载失败: %v", err)
	}
	if len(events) == 0 {
		t.Fatal("事件为空")
	}

	cases := []struct {
		name     string
		mutate   func(raw []byte) []byte
		wantCode string
	}{
		{
			name: "缺失字段取零值报错",
			mutate: func(raw []byte) []byte {
				return []byte(`[{"id":"x","frame":0,"frame_index":0,"type":"hit","channel":0,"strength":100,"priority":1,"no_interrupt":false}]`)
			},
			wantCode: EPara,
		},
		{
			name: "通道越界 E_CHAN",
			mutate: func(raw []byte) []byte {
				return []byte(`[{"id":"x","frame":0,"frame_index":0,"type":"hit","channel":99,"strength":100,"priority":1,"no_interrupt":false,"template":"pulse"}]`)
			},
			wantCode: EChan,
		},
		{
			name: "模板缺失 E_TPL",
			mutate: func(raw []byte) []byte {
				return []byte(`[{"id":"x","frame":0,"frame_index":0,"type":"hit","channel":0,"strength":100,"priority":1,"no_interrupt":false,"template":"nope"}]`)
			},
			wantCode: ETpl,
		},
		{
			name: "帧序非单调 E_ORDER",
			mutate: func(raw []byte) []byte {
				return []byte(`[
				{"id":"a","frame":2,"frame_index":0,"type":"hit","channel":0,"strength":100,"priority":1,"no_interrupt":false,"template":"pulse"},
				{"id":"b","frame":1,"frame_index":0,"type":"hit","channel":1,"strength":100,"priority":1,"no_interrupt":false,"template":"pulse"}]`)
			},
			wantCode: EOrder,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "events.json")
			if err := os.WriteFile(p, tc.mutate(nil), 0o644); err != nil {
				t.Fatal(err)
			}
			_, err := LoadEvents(p, m)
			biz, ok := err.(*BizError)
			if !ok {
				t.Fatalf("期望 BizError, 实际 %T: %v", err, err)
			}
			if biz.Code != tc.wantCode {
				t.Fatalf("期望 %s, 实际 %s (%v)", tc.wantCode, biz.Code, biz)
			}
			if biz.Field == "" {
				t.Fatalf("错误缺少字段名定位: %v", biz)
			}
		})
	}
}

func TestManifestQuantUnitValidation(t *testing.T) {
	dir := t.TempDir()
	mp, _, err := sample.Generate(dir)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(mp)
	// 令量化单位不整除模板时长。
	bad := append([]byte(nil), raw...)
	bad = []byte(stringReplaceAll(string(bad), `"quant_unit": 2`, `"quant_unit": 7`))
	p := filepath.Join(t.TempDir(), "manifest.json")
	if err := os.WriteFile(p, bad, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = LoadManifest(p)
	biz, ok := err.(*BizError)
	if !ok || biz.Code != EPara {
		t.Fatalf("期望 E_PARA, 实际 %v", err)
	}

	// samples_per_packet 非法。
	bad2 := []byte(stringReplaceAll(string(raw), `"samples_per_packet": 16`, `"samples_per_packet": 0`))
	p2 := filepath.Join(t.TempDir(), "manifest2.json")
	_ = os.WriteFile(p2, bad2, 0o644)
	_, err = LoadManifest(p2)
	biz, ok = err.(*BizError)
	if !ok || biz.Code != EPara {
		t.Fatalf("期望 E_PARA, 实际 %v", err)
	}
}

func stringReplaceAll(s, old, new string) string {
	out := ""
	for {
		i := indexOf(s, old)
		if i < 0 {
			return out + s
		}
		out += s[:i] + new
		s = s[i+len(old):]
	}
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

var _ = synth.TypeHit
