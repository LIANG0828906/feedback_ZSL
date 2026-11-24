package session

import (
	"os"
	"path/filepath"
	"testing"
)

func freshGen(t *testing.T, mutate func(*GenConfig)) (*Manifest, string) {
	t.Helper()
	dir := t.TempDir()
	cfg := DefaultGenConfig()
	if mutate != nil {
		mutate(&cfg)
	}
	m, _, err := Generate(dir, cfg)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	return m, dir
}

func TestGenerateAndLoad(t *testing.T) {
	m, dir := freshGen(t, nil)
	sess, err := Load(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(sess.Channels) != m.Channels || sess.RefIndex != 0 {
		t.Fatalf("unexpected channels=%d ref=%d", len(sess.Channels), sess.RefIndex)
	}
	for _, ch := range sess.Channels {
		if int64(len(ch.Data)) != m.PointsPerChan {
			t.Fatalf("channel %s points mismatch", ch.Spec.ID)
		}
		if ch.Hash == "" {
			t.Fatalf("channel %s missing hash", ch.Spec.ID)
		}
	}
}

func TestLoadErrors(t *testing.T) {
	_, dir := freshGen(t, nil)

	// E_DECODE：截断裸数据。
	raw := filepath.Join(dir, "ch1.f64")
	data, _ := os.ReadFile(raw)
	if err := os.WriteFile(raw, data[:len(data)-8], 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dir); CodeOf(err) != EDecode {
		t.Fatalf("want E_DECODE, got %v", err)
	}

	// E_META：清单点数与文件不一致。
	m2, dir2 := freshGen(t, nil)
	m2.PointsPerChan++
	if err := WriteManifest(dir2, m2); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dir2); CodeOf(err) != EMeta {
		t.Fatalf("want E_META, got %v", err)
	}

	// E_META：重复通道标识。
	m3, dir3 := freshGen(t, nil)
	m3.Channels_[1].ID = m3.Channels_[0].ID
	if err := WriteManifest(dir3, m3); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(dir3); CodeOf(err) != EMeta {
		t.Fatalf("want E_META for duplicate id, got %v", err)
	}
}

func TestFaultChannelMissing(t *testing.T) {
	dir := t.TempDir()
	cfg := DefaultGenConfig()
	cfg.DropChannel = "ch1"
	if _, _, err := Generate(dir, cfg); err != nil {
		t.Fatal(err)
	}
	sess, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if sess.Manifest.ChannelByID("ch1") != nil {
		t.Fatal("dropped channel must not appear in manifest")
	}
	if err := VerifyChannelPresent(dir, ChannelSpec{ID: "ch1", File: "ch1.f64", Points: 1}); CodeOf(err) != EChan {
		t.Fatalf("want E_CHAN, got %v", err)
	}
}

func TestPhiAtModel(t *testing.T) {
	knots := []Knot{{At: 0, Rate: 1e-4}, {At: 1000, Rate: 1e-4}}
	got := phiAt(2.0, knots, 500)
	want := 2.0 + 500*1e-4
	if absd(got, want) > 1e-9 {
		t.Fatalf("phiAt=%v want %v", got, want)
	}
}

func absd(a, b float64) float64 {
	if a > b {
		return a - b
	}
	return b - a
}
