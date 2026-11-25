package verify_test

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"spectralforge/internal/domain"
	"spectralforge/internal/render"
	"spectralforge/internal/sample"
	"spectralforge/internal/store"
	"spectralforge/internal/verify"
)

func setup(t *testing.T) (*domain.SampleBundle, string, string) {
	t.Helper()
	sampleDir := t.TempDir()
	b, err := sample.Generate(sampleDir, 55)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	cp := filepath.Join(root, "cp")
	out := filepath.Join(root, "out")
	report, err := render.Render(context.Background(), b, render.Options{
		Config:        *b.Config,
		CheckpointDir: cp,
		OutputDir:     out,
		Mode:          "exact",
	})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if !report.Completed {
		t.Fatal("render incomplete")
	}
	return b, cp, out
}

func TestIndependentVerifierAcceptsConsistentRun(t *testing.T) {
	b, cp, out := setup(t)
	report, err := verify.Run(b, cp, out)
	if err != nil {
		t.Fatal(err)
	}
	if !report.OK || report.FirstMismatch != nil {
		t.Fatalf("verifier rejected consistent run: %+v", report.FirstMismatch)
	}
	if report.TilesChecked != 40 || report.PixelsChecked != 40960 {
		t.Fatalf("unexpected checked counts: %d/%d", report.TilesChecked, report.PixelsChecked)
	}
	if !report.CommitOrderOK || len(report.CommitSequence) != 40 {
		t.Fatalf("commit order not verified: %+v", report.CommitSequence)
	}
}

func TestVerifierReportsFirstPixelMismatchAndField(t *testing.T) {
	b, cp, out := setup(t)
	manifest, err := store.ReadManifest(cp)
	if err != nil {
		t.Fatal(err)
	}
	tiles, err := store.ReadTiles(cp, manifest)
	if err != nil {
		t.Fatal(err)
	}
	tile := tiles[0]
	tile.LinearRGB[0] += 0.25
	tile.Digest = store.TileDigest(tile.TileW, tile.TileH, tile.LinearRGB)
	if err := store.WriteJSONAtomic(filepath.Join(cp, "tiles", "000000.json"), tile); err != nil {
		t.Fatal(err)
	}
	report, err := verify.Run(b, cp, out)
	if err != nil {
		t.Fatal(err)
	}
	if report.OK || report.FirstMismatch == nil {
		t.Fatal("expected a mismatch")
	}
	m := report.FirstMismatch
	if m.Tile != 0 || m.X != 0 || m.Y != 0 || m.Field != "linear_rgb.r" {
		t.Fatalf("unexpected first mismatch: %+v", m)
	}
}

func TestVerifierReportsFrameDisplayMismatch(t *testing.T) {
	b, cp, out := setup(t)
	path := filepath.Join(out, "frame.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var frame struct {
		Width           int       `json:"width"`
		Height          int       `json:"height"`
		ContentHash     string    `json:"content_hash"`
		SamplingVersion string    `json:"sampling_version"`
		ParamVersion    string    `json:"param_version"`
		LinearRGB       []float64 `json:"linear_rgb"`
		DisplayRGB      []byte    `json:"display_rgb"`
		LinearDigest    string    `json:"linear_digest"`
		DisplayDigest   string    `json:"display_digest"`
		Blocks          []any     `json:"blocks"`
	}
	if err := json.Unmarshal(data, &frame); err != nil {
		t.Fatal(err)
	}
	if len(frame.DisplayRGB) == 0 {
		t.Fatal("frame display buffer missing")
	}
	frame.DisplayRGB[0] = byte((int(frame.DisplayRGB[0]) + 127) % 256)
	if err := store.WriteJSONAtomic(path, frame); err != nil {
		t.Fatal(err)
	}
	report, err := verify.Run(b, cp, out)
	if err != nil {
		t.Fatal(err)
	}
	if report.OK || report.FirstMismatch == nil || report.FirstMismatch.Field != "display_rgb.r" {
		t.Fatalf("expected display mismatch, got %+v", report.FirstMismatch)
	}
}
