package synth_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"qkernel/internal/errs"
	"qkernel/internal/imagex"
	"qkernel/internal/synth"
)

func TestLoadTruthSchemaAndMetaValidation(t *testing.T) {
	b, err := synth.Generate(synth.Config{Width: 128, Height: 128, Seed: 3, Channels: 3}, 0)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	truthPath := filepath.Join(dir, "truth.json")
	if err := synth.SaveTruth(truthPath, b.Truth); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(truthPath)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	delete(m, "seed")
	bad, _ := json.Marshal(m)
	badPath := filepath.Join(dir, "missing.json")
	if err := os.WriteFile(badPath, bad, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := synth.LoadTruth(badPath); !errs.IsCode(err, errs.ESchema) {
		t.Fatalf("missing seed expected E_SCHEMA, got %v", err)
	}

	wrongMeta := *b.Truth
	wrongMeta.Frame.Width += 16
	if err := synth.ValidateLoadedFrame(b.Frame, &wrongMeta); !errs.IsCode(err, errs.EMeta) {
		t.Fatalf("dimension conflict expected E_META, got %v", err)
	}
	wrongCh := *b.Truth
	wrongCh.Frame.Channels = 4
	if err := synth.ValidateLoadedFrame(b.Frame, &wrongCh); !errs.IsCode(err, errs.EMeta) {
		t.Fatalf("channel conflict expected E_META, got %v", err)
	}
}

func TestLoadTruthRejectsOutOfFrameBBoxAndEmptyWatermark(t *testing.T) {
	b, err := synth.Generate(synth.Config{Width: 128, Height: 128, Seed: 4, Channels: 3}, 0)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()

	bad := *b.Truth
	bad.Objects = append([]synth.ObjectTruth(nil), b.Truth.Objects...)
	bad.Objects[0].BBox = imagex.R(0, 0, 999, 20)
	bad.Seal()
	p := filepath.Join(dir, "bbox.json")
	if err := synth.SaveTruth(p, &bad); err != nil {
		t.Fatal(err)
	}
	if _, err := synth.LoadTruth(p); !errs.IsCode(err, errs.ESchema) {
		t.Fatalf("outside bbox expected E_SCHEMA, got %v", err)
	}

	emptyWM := *b.Truth
	emptyWM.Watermark = imagex.R(10, 10, 10, 30)
	emptyWM.Seal()
	p2 := filepath.Join(dir, "watermark.json")
	if err := synth.SaveTruth(p2, &emptyWM); err != nil {
		t.Fatal(err)
	}
	if _, err := synth.LoadTruth(p2); !errs.IsCode(err, errs.ESchema) {
		t.Fatalf("empty watermark expected E_SCHEMA, got %v", err)
	}
}

func TestSyntheticSamplePixelsMatchFrame(t *testing.T) {
	b, err := synth.Generate(synth.Config{Width: 128, Height: 128, Seed: 5, Channels: 3}, 2)
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range b.Truth.Samples {
		p := b.Frame.Pixel(s.X, s.Y)
		if [3]int{int(p[0]), int(p[1]), int(p[2])} != s.Expected {
			t.Fatalf("sample %s mismatch", s.Label)
		}
	}
	if b.Truth.Frame.ID != synth.MustFrameID(2) {
		t.Fatalf("unexpected frame id %s", b.Truth.Frame.ID)
	}
}
