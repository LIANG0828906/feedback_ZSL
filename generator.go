package loudness

import (
	"encoding/json"
	"fmt"
	"sort"
)

const GeneratorVersion = "deterministic-generator-v1"

// FrameTruth is the independently persisted expected value for one frame.
type FrameTruth struct {
	FrameIndex          int   `json:"frameIndex"`
	StartSample         int64 `json:"startSample"`
	EndSample           int64 `json:"endSample"`
	EnergyQ32           int64 `json:"energyQ32"`
	RmsQ16              int64 `json:"rmsQ16"`
	MeasuredDBQ8        int64 `json:"measuredDbQ8"`
	PeakSample          int64 `json:"peakSample"`
	PeakOffset          int64 `json:"peakOffset"`
	CalibratedEnergyQ32 int64 `json:"calibratedEnergyQ32"`
	CalibratedDBQ8      int64 `json:"calibratedDbQ8"`
}

// ClipTruth groups the expected frame values and gain decision for a clip.
type ClipTruth struct {
	ClipID                  int          `json:"clipId"`
	FrameCount              int          `json:"frameCount"`
	GainAdjustmentQ12       int64        `json:"gainAdjustmentQ12"`
	MeasuredSegmentDBQ8     int64        `json:"measuredSegmentDbQ8"`
	CalibratedSegmentDBQ8   int64        `json:"calibratedSegmentDbQ8"`
	CalibratedSegmentRmsQ16 int64        `json:"calibratedSegmentRmsQ16"`
	EnvelopeDigest          string       `json:"envelopeDigest"`
	Frames                  []FrameTruth `json:"frames"`
}

// TruthDocument is the fixed-seed expected-result list used by tests and the
// demo. Values are integer-only and include the declared tolerance.
type TruthDocument struct {
	GeneratorVersion   string      `json:"generatorVersion"`
	AlgorithmVersion   string      `json:"algorithmVersion"`
	Version            Version     `json:"version"`
	ProjectID          string      `json:"projectId"`
	Seed               int64       `json:"seed"`
	SampleRate         int64       `json:"sampleRate"`
	FrameLength        int64       `json:"frameLength"`
	FrameShift         int64       `json:"frameShift"`
	TargetLoudnessDBQ8 int64       `json:"targetLoudnessDbQ8"`
	ToleranceQ8        int64       `json:"toleranceQ8"`
	Clips              []ClipTruth `json:"clips"`
}

// PCMManifest documents the generated mono PCM without shipping recordings.
type PCMManifest struct {
	ClipID      int    `json:"clipId"`
	SampleCount int64  `json:"sampleCount"`
	SampleRate  int64  `json:"sampleRate"`
	PCMHash     string `json:"pcmHash"`
}

// GeneratedCase contains wire bytes, normalized model, PCM, and truth.
type GeneratedCase struct {
	Seed         int64           `json:"seed"`
	Version      Version         `json:"version"`
	Wire         []byte          `json:"-"`
	Project      *Project        `json:"project"`
	Truth        *TruthDocument  `json:"truth"`
	PCM          map[int][]int16 `json:"-"`
	PCMManifest  []PCMManifest   `json:"pcmManifest"`
	ExportLosses []Loss          `json:"exportLosses,omitempty"`
}

// AlgorithmVersion is included in cache keys and checkpoints.
const AlgorithmVersion = "envelope-core-v1"

type deterministicRNG struct {
	state uint64
}

func newRNG(seed int64) *deterministicRNG {
	return &deterministicRNG{state: uint64(seed) + 0x9E3779B97F4A7C15}
}

func (r *deterministicRNG) next() uint64 {
	r.state += 0x9E3779B97F4A7C15
	z := r.state
	z = (z ^ (z >> 30)) * 0xBF58476D1CE4E5B9
	z = (z ^ (z >> 27)) * 0x94D049BB133111EB
	return z ^ (z >> 31)
}

func (r *deterministicRNG) intn(n int64) int64 {
	if n <= 0 {
		return 0
	}
	return int64(r.next() % uint64(n))
}

// GenerateCase creates one version from a fixed seed. The canonical content is
// built once, exported through the selected wire schema, and then loaded again
// so truth always reflects the concrete schema's scale and time conversion.
func GenerateCase(seed int64, version Version) (*GeneratedCase, error) {
	if !version.Valid() {
		return nil, &DomainError{Code: ESchema, Path: "$.schema", SourceVersion: version, Detail: "unknown schema version"}
	}
	canonical, err := buildCanonicalProject(seed)
	if err != nil {
		return nil, err
	}
	exported, exportErr := Export(canonical, version, RoundHalfUp)
	if exportErr != nil && !HasCode(exportErr, ELossy) {
		return nil, exportErr
	}
	if exported.Data == nil {
		return nil, &DomainError{Code: ESchema, Path: "$", SourceVersion: version, Detail: "generator export produced no data"}
	}
	project, err := Load(exported.Data)
	if err != nil {
		return nil, err
	}
	truth, pcm, manifest, err := BuildTruth(project)
	if err != nil {
		return nil, err
	}
	return &GeneratedCase{
		Seed: seed, Version: version, Wire: exported.Data, Project: project,
		Truth: truth, PCM: pcm, PCMManifest: manifest, ExportLosses: exported.Losses,
	}, nil
}

// GenerateAll creates all three schemas from the same seed.
func GenerateAll(seed int64) ([]*GeneratedCase, error) {
	cases := make([]*GeneratedCase, 0, len(SupportedVersions()))
	for _, version := range SupportedVersions() {
		generated, err := GenerateCase(seed, version)
		if err != nil {
			return nil, err
		}
		cases = append(cases, generated)
	}
	return cases, nil
}

func buildCanonicalProject(seed int64) (*Project, error) {
	rng := newRNG(seed)
	project := &Project{
		FormatVersion: SchemaV3, ID: fmt.Sprintf("sound-project-%d", seed), Seed: seed,
		SampleRate: 8000, FrameLength: 256, FrameShift: 128,
		SessionGainQ12: InternalGainScale, TargetLoudnessDBQ8: -30 * InternalLoudnessScale,
		MaxGainQ12: 2 * InternalGainScale, LoudnessToleranceQ8: 2,
		Quantization: DefaultQuantization(),
		Provenance:   Provenance{OriginalVersion: SchemaV3, CurrentVersion: SchemaV3},
	}
	scenes := []struct {
		name      string
		category  Category
		gainQ12   int64
		clipCount int
	}{
		{"ambient-bed", CategoryAmbient, 4096, 2},
		{"combat", CategorySFX, 5120, 2},
		{"ui", CategoryDialog, 6144, 2},
		{"music-bed", CategoryMusic, 8192, 1},
	}
	clipID := 0
	cursor := int64(0)
	for _, sceneSpec := range scenes {
		project.Scenes = append(project.Scenes, Scene{Name: sceneSpec.name})
		for i := 0; i < sceneSpec.clipCount; i++ {
			duration := int64(1536 + rng.intn(2500))
			duration -= duration % 8
			start := cursor + int64(rng.intn(64))*8
			end := start + duration
			baseF0 := int64(160 + rng.intn(520))
			f0 := make([]int64, 4)
			value := baseF0
			for point := range f0 {
				value += int64(rng.intn(161)) - 80
				if value < 60 {
					value = 60
				}
				f0[point] = value
			}
			weight := int64(192 + rng.intn(65))
			amplitude := int64(8500 + rng.intn(4500))
			gain := sceneSpec.gainQ12
			if i == 1 {
				gain = 4096 + int64(rng.intn(3))*1024
			}
			var marker *string
			if clipID%3 == 0 {
				marker = stringPtr("reviewed")
			} else if clipID%3 == 1 {
				marker = stringPtr("pending")
			}
			clip := Clip{
				ID: clipID, Scene: sceneSpec.name, StartSample: start, EndSample: end,
				Category: sceneSpec.category, F0TrackHz: f0, AmplitudeQ12: amplitude,
				LoudnessWeightQ8: weight, GainQ12: gain, CalibrationMarker: marker,
			}
			project.Clips = append(project.Clips, clip)
			project.Scenes[len(project.Scenes)-1].ClipIDs = append(project.Scenes[len(project.Scenes)-1].ClipIDs, clipID)
			clipID++
			cursor = end + int64(16+rng.intn(32))*8
		}
	}
	if err := validateProjectBasics(project); err != nil {
		return nil, err
	}
	return project, nil
}

// BuildTruth measures and calibrates a normalized project, then emits the
// per-frame expected list and generated PCM manifest.
func BuildTruth(project *Project) (*TruthDocument, map[int][]int16, []PCMManifest, error) {
	measurements := make(map[int]ClipMeasurement, len(project.Clips))
	pcmByClip := make(map[int][]int16, len(project.Clips))
	manifest := make([]PCMManifest, 0, len(project.Clips))
	for _, clip := range project.Clips {
		pcm, err := GeneratePCM(project, clip)
		if err != nil {
			return nil, nil, nil, err
		}
		measurement, err := MeasureClipPCM(project, clip, pcm)
		if err != nil {
			return nil, nil, nil, err
		}
		measurements[clip.ID] = measurement
		pcmByClip[clip.ID] = pcm
		manifest = append(manifest, PCMManifest{
			ClipID: clip.ID, SampleCount: int64(len(pcm)),
			SampleRate: project.SampleRate, PCMHash: measurement.PCMHash,
		})
	}
	report, err := CalibrateProject(project, measurements)
	if err != nil {
		return nil, nil, nil, err
	}
	sort.Slice(manifest, func(i, j int) bool { return manifest[i].ClipID < manifest[j].ClipID })
	truth := &TruthDocument{
		GeneratorVersion: GeneratorVersion, AlgorithmVersion: AlgorithmVersion,
		Version: project.FormatVersion, ProjectID: project.ID, Seed: project.Seed,
		SampleRate: project.SampleRate, FrameLength: project.FrameLength, FrameShift: project.FrameShift,
		TargetLoudnessDBQ8: project.TargetLoudnessDBQ8, ToleranceQ8: project.LoudnessToleranceQ8,
	}
	ids := make([]int, 0, len(report.Clips))
	for id := range report.Clips {
		ids = append(ids, id)
	}
	sort.Ints(ids)
	for _, id := range ids {
		result := report.Clips[id]
		clipTruth := ClipTruth{
			ClipID: id, FrameCount: len(result.Frames),
			GainAdjustmentQ12:       result.GainAdjustmentQ12,
			MeasuredSegmentDBQ8:     result.MeasuredSegmentDBQ8,
			CalibratedSegmentDBQ8:   result.CalibratedSegmentDBQ8,
			CalibratedSegmentRmsQ16: result.CalibratedSegmentRmsQ16,
			EnvelopeDigest:          result.EnvelopeDigest,
		}
		for _, frame := range result.Frames {
			clipTruth.Frames = append(clipTruth.Frames, FrameTruth{
				FrameIndex: frame.FrameIndex, StartSample: frame.StartSample, EndSample: frame.EndSample,
				EnergyQ32: frame.EnergyQ32, RmsQ16: frame.RmsQ16, MeasuredDBQ8: frame.MeasuredDBQ8,
				PeakSample: frame.PeakSample, PeakOffset: frame.PeakOffset,
				CalibratedEnergyQ32: frame.CalibratedEnergyQ32, CalibratedDBQ8: frame.CalibratedDBQ8,
			})
		}
		truth.Clips = append(truth.Clips, clipTruth)
	}
	return truth, pcmByClip, manifest, nil
}

// MarshalTruth returns stable indented JSON for on-disk truth files.
func MarshalTruth(truth *TruthDocument) ([]byte, error) {
	data, err := json.MarshalIndent(truth, "", "  ")
	if err != nil {
		return nil, &DomainError{Code: ESchema, Path: "$", Detail: "truth encode failed", Cause: err}
	}
	return data, nil
}
