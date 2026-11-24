package sample

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"audioeval/internal/model"
)

func testGenerateOptions(dir string) GenerateOptions {
	return GenerateOptions{
		Dir:         dir,
		SampleID:    "unit-sample",
		Seed:        987654321,
		SampleRate:  48000,
		Channels:    2,
		FrameLength: 128,
		FrameHop:    64,
		FrameCount:  4,
		Quality:     model.QualityQ3,
	}
}

func TestGenerateDeterministicAndLoad(t *testing.T) {
	dir1 := t.TempDir()
	dir2 := t.TempDir()
	generated1, err := Generate(testGenerateOptions(dir1))
	if err != nil {
		t.Fatal(err)
	}
	generated2, err := Generate(testGenerateOptions(dir2))
	if err != nil {
		t.Fatal(err)
	}
	if generated1.Manifest.OriginalHash != generated2.Manifest.OriginalHash ||
		generated1.Manifest.ReconstructionHash != generated2.Manifest.ReconstructionHash ||
		generated1.Manifest.TruthFingerprint != generated2.Manifest.TruthFingerprint ||
		generated1.Manifest.InputHash != generated2.Manifest.InputHash {
		t.Fatal("same seed and options must generate identical hashes")
	}
	loaded, err := Load(dir1)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Original) != len(loaded.Reconstruction) {
		t.Fatal("aligned sequences must have equal lengths")
	}
	for frame, truth := range loaded.Truth.Frames {
		if len(truth.PulsePositions) == 0 {
			t.Fatalf("frame %d has no analytic pulse markers", frame)
		}
		if truth.NoiseFloorStdQ16 <= 0 {
			t.Fatalf("frame %d has no analytic noise floor", frame)
		}
	}
}

func TestLoadReturnsParameterErrors(t *testing.T) {
	t.Run("missing manifest field", func(t *testing.T) {
		dir := t.TempDir()
		if _, err := Generate(testGenerateOptions(dir)); err != nil {
			t.Fatal(err)
		}
		manifestPath := filepath.Join(dir, "manifest.json")
		data := readMap(t, manifestPath)
		delete(data, "frame_hop")
		writeMap(t, manifestPath, data)
		_, err := Load(dir)
		assertError(t, err, model.CodePara, "frame_hop")
	})

	t.Run("unsupported quality", func(t *testing.T) {
		dir := t.TempDir()
		if _, err := Generate(testGenerateOptions(dir)); err != nil {
			t.Fatal(err)
		}
		manifestPath := filepath.Join(dir, "manifest.json")
		data := readMap(t, manifestPath)
		data["quality"] = json.RawMessage(`"Q99"`)
		writeMap(t, manifestPath, data)
		_, err := Load(dir)
		assertError(t, err, model.CodePara, "quality")
	})

	t.Run("missing truth frame field", func(t *testing.T) {
		dir := t.TempDir()
		if _, err := Generate(testGenerateOptions(dir)); err != nil {
			t.Fatal(err)
		}
		truthPath := filepath.Join(dir, "truth.json")
		truthMap := readMap(t, truthPath)
		var frames []map[string]json.RawMessage
		if err := json.Unmarshal(truthMap["frames"], &frames); err != nil {
			t.Fatal(err)
		}
		delete(frames[0], "noise_floor_std_q16")
		encoded, _ := json.Marshal(frames)
		truthMap["frames"] = encoded
		writeMap(t, truthPath, truthMap)
		_, err := Load(dir)
		assertError(t, err, model.CodePara, "frames[0].noise_floor_std_q16")
	})

	t.Run("damaged sequence", func(t *testing.T) {
		dir := t.TempDir()
		if _, err := Generate(testGenerateOptions(dir)); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, "reconstruction.i16")
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		data[0] ^= 0xff
		if err := os.WriteFile(path, data, 0o644); err != nil {
			t.Fatal(err)
		}
		_, err = Load(dir)
		assertError(t, err, model.CodeDecode, "reconstruction_frame_hash")
	})
}

func readMap(t *testing.T, path string) map[string]json.RawMessage {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var value map[string]json.RawMessage
	if err := json.Unmarshal(data, &value); err != nil {
		t.Fatal(err)
	}
	return value
}

func writeMap(t *testing.T, path string, value map[string]json.RawMessage) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func assertError(t *testing.T, err error, code, field string) {
	t.Helper()
	if err == nil {
		t.Fatal("expected an error")
	}
	var evalErr *model.EvalError
	if !errors.As(err, &evalErr) {
		t.Fatalf("expected EvalError, got %T: %v", err, err)
	}
	if evalErr.Code != code {
		t.Fatalf("expected code %s, got %s (%v)", code, evalErr.Code, err)
	}
	if field != "" && evalErr.Field != field {
		t.Fatalf("expected field %s, got %s (%v)", field, evalErr.Field, err)
	}
}
