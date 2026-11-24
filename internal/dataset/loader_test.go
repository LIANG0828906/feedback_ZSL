package dataset_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"screenverify/internal/dataset"
	"screenverify/internal/model"
	"screenverify/internal/rverr"
	"screenverify/internal/sample"
)

func TestLoaderValidSample(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "noise")
	if _, err := sample.GenerateClip(dir, "noise", "noise_tamper", 42, 16); err != nil {
		t.Fatal(err)
	}
	set, err := dataset.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(set.Frames) != 16 || set.Truth.Width != 96 {
		t.Fatalf("unexpected load: frames=%d width=%d", len(set.Frames), set.Truth.Width)
	}
}

func TestLoaderErrorCodesAndFields(t *testing.T) {
	cases := []struct {
		name   string
		code   rverr.Code
		field  string
		frame  int
		mutate func(*testing.T, string, *model.Truth)
	}{
		{
			name: "width meta", code: rverr.EMeta, field: "width",
			mutate: func(t *testing.T, dir string, tr *model.Truth) { tr.Width = 128 },
		},
		{
			name: "frame count meta", code: rverr.EMeta, field: "frame_count",
			mutate: func(t *testing.T, dir string, tr *model.Truth) { tr.FrameCount = 99 },
		},
		{
			name: "timestamp order", code: rverr.EOrder, field: "timestamp_ns", frame: 2,
			mutate: func(t *testing.T, dir string, tr *model.Truth) {
				tr.Frames[1].TimestampNS = tr.Frames[0].TimestampNS - 1
			},
		},
		{
			name: "watermark range", code: rverr.ERange, field: "watermark_region", frame: 1,
			mutate: func(t *testing.T, dir string, tr *model.Truth) {
				tr.Frames[0].WatermarkRegion.X = 1000
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := filepath.Join(t.TempDir(), "clip")
			truth, err := sample.GenerateClip(dir, "clip", "clean", 7, 16)
			if err != nil {
				t.Fatal(err)
			}
			tc.mutate(t, dir, truth)
			writeTruth(t, dir, truth)
			_, err = dataset.Load(dir)
			assertRV(t, err, tc.code, tc.field, tc.frame)
		})
	}

	t.Run("missing manifest decode", func(t *testing.T) {
		_, err := dataset.Load(t.TempDir())
		assertRV(t, err, rverr.EDecode, "", 0)
	})

	t.Run("truncated frame decode", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "clip")
		truth, err := sample.GenerateClip(dir, "clip", "clean", 8, 16)
		if err != nil {
			t.Fatal(err)
		}
		framePath := filepath.Join(dir, truth.Frames[0].File)
		info, err := os.Stat(framePath)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Truncate(framePath, info.Size()-10); err != nil {
			t.Fatal(err)
		}
		_, err = dataset.Load(dir)
		assertRV(t, err, rverr.EDecode, "frame_data", 1)
	})
}

func writeTruth(t *testing.T, dir string, truth *model.Truth) {
	t.Helper()
	b, err := json.MarshalIndent(truth, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "truth.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func assertRV(t *testing.T, err error, code rverr.Code, field string, frame int) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected %s error", code)
	}
	e := rverr.As(err)
	if e == nil {
		t.Fatalf("expected rverr.Error, got %T %v", err, err)
	}
	if e.Code != code {
		t.Fatalf("code=%s want %s (field=%s frame=%d msg=%s)", e.Code, code, e.Field, e.Frame, e.Message)
	}
	if field != "" && e.Field != field {
		t.Fatalf("field=%q want %q", e.Field, field)
	}
	if frame != 0 && e.Frame != frame {
		t.Fatalf("frame=%d want %d", e.Frame, frame)
	}
}
