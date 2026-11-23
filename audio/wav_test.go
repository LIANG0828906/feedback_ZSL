package audio

import (
	"errors"
	"math"
	"os"
	"path/filepath"
	"testing"

	"loudnessworkbench/apperr"
)

func TestWAVFloat32RoundTripPreservesCanonicalSamples(t *testing.T) {
	signal := New(48000, 2, 32)
	for i := range signal.Data {
		signal.Data[i] = math.Sin(float64(i)*0.17) * 0.7
	}
	signal = QuantizeF32(signal)
	path := filepath.Join(t.TempDir(), "roundtrip.wav")
	if err := WriteWAV(path, signal); err != nil {
		t.Fatal(err)
	}
	decoded, err := ReadWAV(path)
	if err != nil {
		t.Fatal(err)
	}
	if decoded.SampleRateHz != signal.SampleRateHz || decoded.Channels != signal.Channels || decoded.Frames() != signal.Frames() {
		t.Fatalf("shape mismatch: %+v vs %+v", decoded, signal)
	}
	for i := range signal.Data {
		if decoded.Data[i] != signal.Data[i] {
			t.Fatalf("sample %d differs: %v != %v", i, decoded.Data[i], signal.Data[i])
		}
	}
	if ContentHash(decoded) != ContentHash(signal) {
		t.Fatal("content hash changed after WAV round trip")
	}
}

func TestDecodeCorruptWAVReturnsEDecode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "broken.wav")
	if err := os.WriteFile(path, []byte("not a wave"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := ReadWAV(path)
	if !apperr.IsCode(err, apperr.CodeDecode) {
		t.Fatalf("got %v, want E_DECODE", err)
	}
}

func TestConcatUpmixesMonoToStereo(t *testing.T) {
	mono := New(48000, 1, 2)
	mono.Data = []float64{0.1, 0.2}
	stereo := New(48000, 2, 2)
	stereo.Data = []float64{0.3, 0.4, 0.5, 0.6}
	mix, err := Concat([]Signal{mono, stereo})
	if err != nil {
		t.Fatal(err)
	}
	if mix.Channels != 2 || mix.Frames() != 4 {
		t.Fatalf("unexpected mix shape: %d/%d", mix.Channels, mix.Frames())
	}
	expected := []float64{0.1, 0.1, 0.2, 0.2, 0.3, 0.4, 0.5, 0.6}
	for i := range expected {
		if mix.Data[i] != expected[i] {
			t.Fatalf("sample %d = %v, want %v", i, mix.Data[i], expected[i])
		}
	}
}

func TestContentHashDetectsSampleChange(t *testing.T) {
	signal := New(48000, 1, 4)
	first := ContentHash(signal)
	signal.Data[2] = 1e-9
	if ContentHash(signal) == first {
		t.Fatal("content hash did not change")
	}
}

func TestReadMissingAudioIsDecodeError(t *testing.T) {
	_, err := ReadWAV(filepath.Join(t.TempDir(), "missing.wav"))
	if !apperr.IsCode(err, apperr.CodeDecode) {
		t.Fatalf("got %v, want E_DECODE", err)
	}
	var target *apperr.Error
	if !errors.As(err, &target) {
		t.Fatal("error does not expose structured apperr")
	}
}
