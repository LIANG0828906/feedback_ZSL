package loader_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"captionx/internal/errs"
	"captionx/internal/loader"
	"captionx/internal/model"
	"captionx/internal/samplegen"
)

func genProject(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	dirs, err := samplegen.Generate(samplegen.Options{OutDir: dir, Count: 1, Seed: 99})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	return dirs[0]
}

// mutate rewrites manifest.json through f and returns the load error.
func mutate(t *testing.T, dir string, f func(*model.Manifest)) error {
	t.Helper()
	p := filepath.Join(dir, "manifest.json")
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	var m model.Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	f(&m)
	nb, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, nb, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = loader.Load(dir)
	return err
}

func TestLoadValid(t *testing.T) {
	dir := genProject(t)
	p, err := loader.Load(dir)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if p.ContentHash == "" {
		t.Fatal("empty content hash")
	}
	if len(p.CharCounts) != len(p.Manifest.Captions) {
		t.Fatal("char counts not parallel")
	}
}

func TestSchemaErrors(t *testing.T) {
	cases := []struct {
		name  string
		field string
		code  string
		mut   func(*model.Manifest)
	}{
		{"bad segment ref", "segmentId", errs.ESchema, func(m *model.Manifest) {
			m.Captions[0].SegmentID = 9999
		}},
		{"explicit window escapes segment", "explicitWindow", errs.ESchema, func(m *model.Manifest) {
			w := &model.Window{Start: m.Segments[0].Start, End: m.Segments[0].End + 100}
			m.Captions[0].ExplicitWindow = w
		}},
		{"empty text", "text", errs.ESchema, func(m *model.Manifest) {
			m.Captions[0].Text = ""
		}},
		{"weights length", "weights", errs.ESchema, func(m *model.Manifest) {
			m.Reading.Weights[0] = m.Reading.Weights[0][:1]
		}},
		{"missing truth", "truth", errs.ESchema, func(m *model.Manifest) {
			m.Truth.Captions = m.Truth.Captions[1:]
		}},
		{"zero speech rate", "reading.speechRate", errs.EPara, func(m *model.Manifest) {
			m.Reading.SpeechRateNum = 0
		}},
		{"negative weight", "weights", errs.EPara, func(m *model.Manifest) {
			m.Reading.Weights[0][0] = -2
		}},
		{"bad highlight mode", "reading.highlightMode", errs.EPara, func(m *model.Manifest) {
			m.Reading.HighlightMode = "spiral"
		}},
		{"hop exceeds frame", "frame.frameHop", errs.EPara, func(m *model.Manifest) {
			m.Frame.FrameHop = m.Frame.FrameLength + 1
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := genProject(t)
			err := mutate(t, dir, tc.mut)
			if err == nil {
				t.Fatalf("expected error")
			}
			if got := errs.Code(err); got != tc.code {
				t.Fatalf("code = %s, want %s (%v)", got, tc.code, err)
			}
			if e, ok := err.(*errs.Error); ok && e.Field != tc.field {
				t.Fatalf("field = %q, want %q", e.Field, tc.field)
			}
		})
	}
}

func TestSampleRateMismatch(t *testing.T) {
	dir := genProject(t)
	// Corrupt the audio declaration.
	ap := filepath.Join(dir, "audio.json")
	b, _ := os.ReadFile(ap)
	var a samplegen.AudioFile
	if err := json.Unmarshal(b, &a); err != nil {
		t.Fatal(err)
	}
	a.SampleRate += 1
	nb, _ := json.Marshal(a)
	if err := os.WriteFile(ap, nb, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := loader.Load(dir)
	if errs.Code(err) != errs.ESchema {
		t.Fatalf("code = %s, want E_SCHEMA (%v)", errs.Code(err), err)
	}
}
