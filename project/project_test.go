package project

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"testing"

	"loudnessworkbench/apperr"
	"loudnessworkbench/audio"
	"loudnessworkbench/dsp"
)

func fixture(t *testing.T) (*Program, *Manifest, string, map[string]string) {
	t.Helper()
	root := t.TempDir()
	audioDir := filepath.Join(root, "audio")
	signals := []audio.Signal{
		fixtureSignal(48000, 1, 28800, 1000, 0.08),
		fixtureSignal(48000, 2, 24000, 440, 0.06),
	}
	segments := make([]Segment, len(signals))
	manifestSegments := make([]ManifestSegment, len(signals))
	for i, signal := range signals {
		id := []string{"mono", "stereo"}[i]
		path := filepath.Join(audioDir, id+".wav")
		if err := audio.WriteWAV(path, signal); err != nil {
			t.Fatal(err)
		}
		hash := audio.ContentHash(signal)
		segments[i] = Segment{
			Index: i, ID: id, Channels: signal.Channels, SourcePath: path,
			ContentSHA256: hash, Source: signal,
		}
		manifestSegments[i] = ManifestSegment{
			Index: i, ID: id, Channels: signal.Channels, SampleRateHz: 48000,
			DurationSeconds: float64(signal.Frames()) / 48000, FrameCount: signal.Frames(),
			Audio: "audio/" + id + ".wav", ContentSHA256: hash,
			Truth: SegmentTruth{
				IntegratedLUFS: -24, TruePeakDBTP: -6, TargetGainDB: 1,
				SegmentIdealGainDB: 1, PostCompressionIntegratedLUFS: -24,
				Gate: GateTruth{
					FrameCount: 5, AboveAbsolute: 5, AboveBothGates: 5,
					AbsoluteThresholdLUFS: -70, RelativeThresholdLUFS: -80,
				},
			},
		}
	}
	program := &Program{
		ProjectID: "fixture", SourceVersion: SourceV1, SampleRateHz: 48000,
		TargetLUFS: -23, FrameMs: 400, HopMs: 100,
		Gate:       dsp.GateConfig{AbsoluteLUFS: -70, RelativeLU: 10},
		Compressor: dsp.CompressorConfig{ThresholdDBFS: -20, Ratio: 3, AttackMs: 20, ReleaseMs: 200},
		Limiter:    dsp.LimiterConfig{CeilingDBTP: -1, LookaheadMs: 5},
		StageOrder: append([]string(nil), CanonicalStageOrder...),
		Segments:   segments,
	}
	manifest := &Manifest{
		Schema: "lw-manifest/1", ProgramID: program.ProjectID, Seed: 7, SampleRateHz: 48000,
		TargetLUFS: -23, FrameMs: 400, HopMs: 100, Gate: program.Gate,
		Compressor: program.Compressor, Limiter: program.Limiter,
		Tolerance:    Tolerance{LoudnessLU: 0.1, TruePeakDB: 0.1, GainDB: 0.1},
		Segments:     manifestSegments,
		ProgramTruth: ProgramTruth{IntegratedLUFS: -24, PostCompressionIntegratedLUFS: -24, TruePeakDBTP: -6, TargetGainDB: 1},
	}
	return program, manifest, root, nil
}

func fixtureSignal(rate, channels, frames int, frequency, amplitude float64) audio.Signal {
	signal := audio.New(rate, channels, frames)
	for frame := 0; frame < frames; frame++ {
		value := amplitude * math.Sin(2*math.Pi*frequency*float64(frame)/float64(rate))
		for channel := 0; channel < channels; channel++ {
			signal.Set(frame, channel, value)
		}
	}
	return audio.QuantizeF32(signal)
}

func TestThreeVersionsRoundTripToEquivalentModel(t *testing.T) {
	program, _, root, _ := fixture(t)
	paths, err := WriteVersions(filepath.Join(root, "projects"), program)
	if err != nil {
		t.Fatal(err)
	}
	loaded := map[string]*Program{}
	for _, version := range []string{SourceV1, SourceV2, SourceV3} {
		model, err := Load(paths[version])
		if err != nil {
			t.Fatalf("load %s: %v", version, err)
		}
		if model.SourceVersion != version {
			t.Fatalf("source version = %s, want %s", model.SourceVersion, version)
		}
		loaded[version] = model
	}
	for _, pair := range [][2]string{{SourceV1, SourceV2}, {SourceV1, SourceV3}, {SourceV2, SourceV3}} {
		report, err := ComparePrograms(loaded[pair[0]], loaded[pair[1]])
		if err != nil {
			t.Fatalf("%s/%s not equivalent: %v", pair[0], pair[1], err)
		}
		if !report.Equivalent || len(report.Trace) == 0 {
			t.Fatal("missing equivalence trace")
		}
		if loaded[pair[0]].DecisionParamsHash() != loaded[pair[1]].DecisionParamsHash() {
			t.Fatal("equivalent versions have different decision parameter hashes")
		}
	}
}

func TestLoaderErrorsAndFirstManifestConflict(t *testing.T) {
	program, manifest, root, _ := fixture(t)
	projectDir := filepath.Join(root, "projects")
	paths, err := WriteVersions(projectDir, program)
	if err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(root, "manifest.json")
	writeTestJSON(t, manifestPath, manifest)
	if _, err := LoadWithManifest(paths[SourceV1], manifestPath); err != nil {
		t.Fatal(err)
	}

	unknownPath := filepath.Join(projectDir, "unknown.json")
	if err := os.WriteFile(unknownPath, []byte(`{"schemaVersion":"99.0"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(unknownPath); !apperr.IsCode(err, apperr.CodeVersion) {
		t.Fatalf("unknown version: got %v, want E_VER", err)
	}

	data, err := os.ReadFile(paths[SourceV2])
	if err != nil {
		t.Fatal(err)
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(data, &object); err != nil {
		t.Fatal(err)
	}
	delete(object, "targetLufs")
	data, _ = json.Marshal(object)
	missingPath := filepath.Join(projectDir, "missing.json")
	if err := os.WriteFile(missingPath, data, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = Load(missingPath)
	if !apperr.IsCode(err, apperr.CodeVersion) {
		t.Fatalf("missing field: got %v, want E_VER", err)
	}

	badManifest := *manifest
	badManifest.SampleRateHz = 44100
	for i := range badManifest.Segments {
		badManifest.Segments[i].SampleRateHz = 44100
		badManifest.Segments[i].DurationSeconds = float64(badManifest.Segments[i].FrameCount) / 44100
	}
	badManifestPath := filepath.Join(root, "bad-manifest.json")
	writeTestJSON(t, badManifestPath, badManifest)
	_, err = LoadWithManifest(paths[SourceV1], badManifestPath)
	if !apperr.IsCode(err, apperr.CodeSchema) {
		t.Fatalf("manifest conflict: got %v, want E_SCHEMA", err)
	}
	if e, _ := apperr.As(err); e.Path != "sample_rate_hz" {
		t.Fatalf("first conflict path = %q (%v)", e.Path, err)
	}
}

func TestCompareReportsFirstLossyField(t *testing.T) {
	program, _, root, _ := fixture(t)
	paths, err := WriteVersions(filepath.Join(root, "projects"), program)
	if err != nil {
		t.Fatal(err)
	}
	left, err := Load(paths[SourceV1])
	if err != nil {
		t.Fatal(err)
	}
	right, err := Load(paths[SourceV3])
	if err != nil {
		t.Fatal(err)
	}
	right.Gate.AbsoluteLUFS = -69
	report, err := ComparePrograms(left, right)
	if !apperr.IsCode(err, apperr.CodeLossy) {
		t.Fatalf("got %v, want E_LOSSY", err)
	}
	if report.FirstMismatch.Path != "gate.absolute_lufs" {
		t.Fatalf("mismatch path = %s", report.FirstMismatch.Path)
	}
	if len(report.Trace) == 0 {
		t.Fatal("missing degradation trace")
	}
}

func writeTestJSON(t *testing.T, path string, value any) {
	t.Helper()
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}
