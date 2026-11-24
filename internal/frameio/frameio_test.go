package frameio_test

import (
	"encoding/binary"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"gameintegrity/internal/apperr"
	"gameintegrity/internal/frameio"
	"gameintegrity/internal/model"
	"gameintegrity/internal/sample"
)

func tinySpec() sample.Spec {
	return sample.Spec{
		ID: "tiny", Width: 80, Height: 64, Channels: 3, FrameCount: 16,
		BlockSize: 8, Replacement: model.Interval{Start: 6, End: 8},
		SegmentSeconds: 1, Tolerance: 1,
	}
}

func writeTiny(t *testing.T, spec sample.Spec) string {
	t.Helper()
	root := t.TempDir()
	if err := sample.WriteSample(root, spec, 20261124); err != nil {
		t.Fatal(err)
	}
	return root
}

func mutateManifest(t *testing.T, root string, fn func(map[string]any)) {
	t.Helper()
	path := filepath.Join(root, "tiny", "manifest.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	fn(m)
	data, _ = json.MarshalIndent(m, "", "  ")
	if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestOpenValidSample(t *testing.T) {
	root := writeTiny(t, tinySpec())
	ds, err := frameio.Open(root, "tiny")
	if err != nil {
		t.Fatal(err)
	}
	if ds.Manifest.Width != 80 || ds.Manifest.FrameCount != 16 || ds.ContentHash == "" {
		t.Fatalf("unexpected manifest: %+v", ds.Manifest)
	}
	frame, err := ds.ReadFrame(3)
	if err != nil {
		t.Fatal(err)
	}
	if len(frame.Pixels) != 80*64*3 {
		t.Fatalf("pixel bytes = %d", len(frame.Pixels))
	}
}

func TestMissingManifestAndTruthReturnDecode(t *testing.T) {
	root := t.TempDir()
	_, err := frameio.Open(root, "missing")
	if e := apperr.As(err); e == nil || e.Code != apperr.Decode {
		t.Fatalf("missing manifest error = %#v", e)
	}
	root = writeTiny(t, tinySpec())
	if err := os.Remove(filepath.Join(root, "tiny", "truth.json")); err != nil {
		t.Fatal(err)
	}
	_, err = frameio.Open(root, "tiny")
	if e := apperr.As(err); e == nil || e.Code != apperr.Decode {
		t.Fatalf("missing truth error = %#v", e)
	}
}

func TestTruncatedFrameReturnsDecode(t *testing.T) {
	root := writeTiny(t, tinySpec())
	path := filepath.Join(root, "tiny", "frames", "frame-000003.rgbx")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data[:len(data)-7], 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = frameio.Open(root, "tiny")
	if e := apperr.As(err); e == nil || e.Code != apperr.Decode {
		t.Fatalf("truncated frame error = %#v", e)
	}
}

func TestFrameHeaderMismatchesReturnMetaWithField(t *testing.T) {
	t.Run("width", func(t *testing.T) {
		root := writeTiny(t, tinySpec())
		path := filepath.Join(root, "tiny", "frames", "frame-000002.rgbx")
		data, _ := os.ReadFile(path)
		binary.BigEndian.PutUint16(data[6:8], 99)
		_ = os.WriteFile(path, data, 0o644)
		_, err := frameio.Open(root, "tiny")
		e := apperr.As(err)
		if e == nil || e.Code != apperr.Meta || e.Field != "width" || e.Frame != 2 {
			t.Fatalf("error = %#v", e)
		}
	})
	t.Run("channels", func(t *testing.T) {
		root := writeTiny(t, tinySpec())
		path := filepath.Join(root, "tiny", "frames", "frame-000002.rgbx")
		data, _ := os.ReadFile(path)
		data[10] = 4
		_ = os.WriteFile(path, data, 0o644)
		_, err := frameio.Open(root, "tiny")
		e := apperr.As(err)
		if e == nil || e.Code != apperr.Meta || e.Field != "channels" {
			t.Fatalf("error = %#v", e)
		}
	})
}

func TestTruthFrameCountMismatchReturnsMeta(t *testing.T) {
	root := writeTiny(t, tinySpec())
	path := filepath.Join(root, "tiny", "truth.json")
	data, _ := os.ReadFile(path)
	var truth model.Truth
	_ = json.Unmarshal(data, &truth)
	truth.FrameCount++
	data, _ = json.MarshalIndent(truth, "", "  ")
	_ = os.WriteFile(path, data, 0o644)
	_, err := frameio.Open(root, "tiny")
	e := apperr.As(err)
	if e == nil || e.Code != apperr.Meta || e.Field != "frame_count" {
		t.Fatalf("error = %#v", e)
	}
}

func TestAnomalyInjectionCodes(t *testing.T) {
	tests := []struct {
		injection string
		frame     int
		code      string
		field     string
	}{
		{"order", 8, apperr.Order, "timestamps_us"},
		{"mark", 5, apperr.Mark, ""},
		{"frame", 6, apperr.Frame, "frame_flags"},
		{"size", 0, apperr.Meta, "width"},
	}
	for _, tt := range tests {
		t.Run(tt.injection, func(t *testing.T) {
			spec := tinySpec()
			spec.Injection = tt.injection
			spec.InjectionFrame = tt.frame
			root := writeTiny(t, spec)
			_, err := frameio.Open(root, "tiny")
			e := apperr.As(err)
			if e == nil {
				t.Fatal("expected error, got nil")
			}
			if e.Code != tt.code {
				t.Fatalf("code = %s, want %s (%s)", e.Code, tt.code, e.Error())
			}
			if e.Frame != tt.frame {
				t.Fatalf("frame = %d, want %d", e.Frame, tt.frame)
			}
			if tt.field != "" && e.Field != tt.field {
				t.Fatalf("field = %s, want %s", e.Field, tt.field)
			}
		})
	}
}

func TestBlockSizeValidationProvidesRange(t *testing.T) {
	cfg := model.DefaultConfig(96, 192, 3, 36, 8, 24)
	cfg.BlockSize = 7
	err := cfg.Validate(96, 192, 3, 36)
	e := apperr.As(err)
	if e == nil || e.Code != apperr.Para || e.Field != "block_size" || e.AvailableRange == "" {
		t.Fatalf("error = %#v", e)
	}
	cfg.BlockSize = 0
	err = cfg.Validate(96, 192, 3, 36)
	if e := apperr.As(err); e == nil || e.Code != apperr.Para {
		t.Fatalf("zero block error = %#v", e)
	}
}

func TestThresholdRangeErrorsNameField(t *testing.T) {
	cfg := model.DefaultConfig(96, 192, 3, 36, 8, 24)
	cfg.ResidualThreshold = 1.2
	err := cfg.Validate(96, 192, 3, 36)
	e := apperr.As(err)
	if e == nil || e.Code != apperr.Para || e.Field != "residual_threshold" {
		t.Fatalf("error = %#v", e)
	}
	cfg = model.DefaultConfig(96, 192, 3, 36, 8, 24)
	cfg.NoiseThreshold = -0.1
	err = cfg.Validate(96, 192, 3, 36)
	e = apperr.As(err)
	if e == nil || e.Code != apperr.Para || e.Field != "noise_threshold" {
		t.Fatalf("error = %#v", e)
	}
}
