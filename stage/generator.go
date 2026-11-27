package stage

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
)

type SchemaTruth struct {
	Version      string `json:"version"`
	File         string `json:"file"`
	SHA256       string `json:"sha256"`
	FixtureCount int    `json:"fixture_count"`
	TraceCount   int    `json:"conversion_trace_count"`
	DefaultCount int    `json:"default_count"`
	Losses       []Loss `json:"losses,omitempty"`
}

type TruthManifest struct {
	Format           string         `json:"format"`
	Seed             int64          `json:"seed"`
	ProjectID        string         `json:"project_id"`
	ParameterVersion string         `json:"parameter_version"`
	AlgorithmVersion string         `json:"algorithm_version"`
	Sampling         SamplingConfig `json:"sampling"`
	Windows          []WindowResult `json:"windows"`
	Schemas          []SchemaTruth  `json:"schemas"`
	PointCount       int            `json:"point_count"`
	UncertainCount   int            `json:"uncertain_count"`
}

type SampleManifest struct {
	Format           string            `json:"format"`
	Seed             int64             `json:"seed"`
	ProjectID        string            `json:"project_id"`
	ParameterVersion string            `json:"parameter_version"`
	AlgorithmVersion string            `json:"algorithm_version"`
	SchemaFiles      map[string]string `json:"schema_files"`
	SHA256           map[string]string `json:"sha256"`
	TruthFile        string            `json:"truth_file"`
	TruthSHA256      string            `json:"truth_sha256"`
	FixtureCount     int               `json:"fixture_count"`
	WindowCount      int               `json:"window_count"`
	SchemaLosses     map[string][]Loss `json:"schema_losses,omitempty"`
	Notes            []string          `json:"notes"`
}

type SampleBundle struct {
	Files map[string][]byte
}

// GenerateProject creates the canonical internal project. Random choices are
// drawn only from the supplied seed and are quantized before entering the
// project so every schema can describe the same logical show.
func GenerateProject(seed int64) (*Project, error) {
	rng := rand.New(rand.NewSource(seed))
	panJitter := int64(rng.Intn(5)-2) * 1_000_000
	tiltJitter := int64(rng.Intn(3)) * 1_000_000
	xJitter := int64(rng.Intn(5)-2) * 100 * 1000
	yJitter := int64(rng.Intn(5)-2) * 100 * 1000
	levelA := int64(4000 + rng.Intn(5)*500)
	levelB := int64(5000 + rng.Intn(5)*500)
	cctPermille := int64(200 + rng.Intn(4)*200)
	p := &Project{
		ProjectID: DefaultProjectID, SourceVersion: InternalSchema, Seed: seed,
		ParameterVersion: DefaultParameterVersion, Revision: 1,
	}
	fixtures := []Fixture{
		{
			ID: "F1", Rig: "main", Group: "key",
			PositionUM:  Vec3{-1_000_000 + xJitter, 0, 5_000_000},
			PanMicroDeg: panJitter, TiltMicroDeg: -90_000_000 + tiltJitter,
			InnerHalfMicroDeg: 8_000_000, OuterHalfMicroDeg: 20_000_000,
			CCTKelvin: 0, LevelPermyriad: levelA, RGBPermyriad: Vec3{10_000, 8_000, 6_000},
		},
		{
			ID: "F2", Rig: "main", Group: "key",
			PositionUM:  Vec3{1_000_000 - xJitter, 0, 5_000_000},
			PanMicroDeg: 0, TiltMicroDeg: -90_000_000 + tiltJitter,
			InnerHalfMicroDeg: 10_000_000, OuterHalfMicroDeg: 24_000_000,
			CCTKelvin: 0, LevelPermyriad: levelB, RGBPermyriad: Vec3{2_000, 6_000, 10_000},
		},
		{
			ID: "F3", Rig: "main", Group: "wash",
			PositionUM:  Vec3{0, -1_200_000 + yJitter, 4_500_000},
			PanMicroDeg: 90_000_000, TiltMicroDeg: -90_000_000 + tiltJitter,
			InnerHalfMicroDeg: 6_000_000, OuterHalfMicroDeg: 14_000_000,
			CCTKelvin: 0, LevelPermyriad: 6000, RGBPermyriad: Vec3{8_000, 4_000, 10_000},
		},
		{
			ID: "F4", Rig: "main", Group: "wash",
			PositionUM:  Vec3{0, 1_200_000 - yJitter, 4_500_000},
			PanMicroDeg: 270_000_000, TiltMicroDeg: -90_000_000 + tiltJitter,
			InnerHalfMicroDeg: 12_000_000, OuterHalfMicroDeg: 28_000_000,
			CCTKelvin: 0, LevelPermyriad: 5500, RGBPermyriad: Vec3{10_000, 10_000, 10_000},
		},
	}
	for i := range fixtures {
		kelvin, _, err := CCTPermilleToKelvin(cctPermille)
		if err != nil {
			return nil, err
		}
		if i%2 == 1 {
			kelvin, _, err = CCTPermilleToKelvin(cctPermille + 200)
			if err != nil {
				return nil, err
			}
		}
		fixtures[i].CCTKelvin = kelvin
	}
	p.Fixtures = fixtures
	p.Groups = []FixtureGroup{
		{Rig: "main", Name: "key", Label: "主灯位"},
		{Rig: "main", Name: "wash", Label: "染色灯位"},
	}
	p.Cues = []Cue{
		{
			ID: "cue-fade-f1", FixtureID: "F1", Kind: CueFade, StartMS: 0, EndMS: 400,
			FromLevel: 2000, ToLevel: 4000,
		},
		{
			ID: "cue-sweep-f2", FixtureID: "F2", Kind: CueSweep, StartMS: 0, EndMS: 200,
			FromPan: -10_000_000, ToPan: 10_000_000,
			FromTilt: -90_000_000 + tiltJitter, ToTilt: -90_000_000 + tiltJitter,
		},
		{
			ID: "cue-color-f3", FixtureID: "F3", Kind: CueColor, StartMS: 200, EndMS: 400,
			FromRGB: [3]int64{10_000, 2_000, 2_000}, ToRGB: [3]int64{2_000, 2_000, 10_000},
		},
	}
	p.Timeline = Timeline{
		DurationMS: 400,
		Windows: []SampleWindow{
			{Number: 1, StartMS: 0, EndMS: 75, Samples: []int64{0}},
			{Number: 2, StartMS: 100, EndMS: 175, Samples: []int64{100}},
			{Number: 3, StartMS: 200, EndMS: 275, Samples: []int64{200}},
			{Number: 4, StartMS: 300, EndMS: 375, Samples: []int64{300}},
			{Number: 5, StartMS: 400, EndMS: 400, Samples: []int64{400}},
		},
	}
	if err := ValidateProject(p); err != nil {
		return nil, err
	}
	return p, nil
}

// GenerateSampleBundle builds the three schema files, truth, and a manifest.
// The output is byte-for-byte deterministic for a given seed.
func GenerateSampleBundle(seed int64) (*SampleBundle, error) {
	p, err := GenerateProject(seed)
	if err != nil {
		return nil, err
	}
	files := make(map[string][]byte)
	lossesBySchema := make(map[string][]Loss)
	var schemas []SchemaTruth
	schemaFiles := map[string]string{SchemaV1: "v1.json", SchemaV2: "v2.json", SchemaV3: "v3.json"}
	for _, version := range SupportedSchemas {
		res, err := ExportProject(p, version)
		if err != nil && !HasCode(err, CodeLossy) {
			return nil, err
		}
		loaded, err := LoadProject(res.Data)
		if err != nil {
			return nil, fmt.Errorf("generated %s does not load: %w", version, err)
		}
		name := schemaFiles[version]
		files[name] = res.Data
		sum := sha256.Sum256(res.Data)
		defaults := 0
		for _, tr := range loaded.Traces {
			if tr.Operation == "apply_default" {
				defaults++
			}
		}
		schemas = append(schemas, SchemaTruth{
			Version: version, File: name, SHA256: hex.EncodeToString(sum[:]),
			FixtureCount: len(loaded.Fixtures), TraceCount: len(loaded.Traces),
			DefaultCount: defaults, Losses: res.Losses,
		})
		if len(res.Losses) > 0 {
			lossesBySchema[version] = res.Losses
		}
	}
	run, err := RunProject(context.Background(), p, RunOptions{Sampling: ExactSampling(), Workers: 1})
	if err != nil {
		return nil, err
	}
	pointCount, uncertain := 0, 0
	for _, w := range run.Windows {
		for _, fixture := range w.Fixtures {
			pointCount += len(fixture.Points)
		}
		uncertain += w.UncertainCount
	}
	truth := TruthManifest{
		Format: "stage-truth.v1", Seed: seed, ProjectID: p.ProjectID,
		ParameterVersion: p.ParameterVersion, AlgorithmVersion: AlgorithmVersion,
		Sampling: ExactSampling(), Windows: run.Windows, Schemas: schemas,
		PointCount: pointCount, UncertainCount: uncertain,
	}
	truthData, err := json.MarshalIndent(truth, "", "  ")
	if err != nil {
		return nil, err
	}
	truthData = append(truthData, '\n')
	files["truth.json"] = truthData
	truthSum := sha256.Sum256(truthData)
	hashes := make(map[string]string, len(files)+1)
	for name, data := range files {
		sum := sha256.Sum256(data)
		hashes[name] = hex.EncodeToString(sum[:])
	}
	manifest := SampleManifest{
		Format: "stage-sample-bundle.v1", Seed: seed, ProjectID: p.ProjectID,
		ParameterVersion: p.ParameterVersion, AlgorithmVersion: AlgorithmVersion,
		SchemaFiles: schemaFiles, SHA256: hashes, TruthFile: "truth.json",
		TruthSHA256: hex.EncodeToString(truthSum[:]), FixtureCount: len(p.Fixtures),
		WindowCount: len(p.Timeline.Windows), SchemaLosses: lossesBySchema,
		Notes: []string{
			"v1 omits the outer cone angle where it equals the declared 2.5x inner default.",
			"v1/v2 flatten v3 rig/group organization; every such downgrade is reported as E_LOSSY.",
			"Truth is generated from the canonical internal project with exact sampling.",
		},
	}
	manifestData, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return nil, err
	}
	manifestData = append(manifestData, '\n')
	files["manifest.json"] = manifestData
	return &SampleBundle{Files: files}, nil
}

func (b *SampleBundle) Write(dir string) error {
	if b == nil {
		return domainError(CodeValue, "nil sample bundle", "samples")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	names := make([]string, 0, len(b.Files))
	for name := range b.Files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if filepath.Base(name) != name {
			return &Error{Code: CodeValue, Message: "invalid sample file name", Field: "samples." + name}
		}
		final := filepath.Join(dir, name)
		tmp, err := os.CreateTemp(dir, "."+name+".tmp-*")
		if err != nil {
			return err
		}
		tmpName := tmp.Name()
		if _, err := tmp.Write(b.Files[name]); err != nil {
			_ = tmp.Close()
			_ = os.Remove(tmpName)
			return err
		}
		if err := tmp.Sync(); err != nil {
			_ = tmp.Close()
			_ = os.Remove(tmpName)
			return err
		}
		if err := tmp.Chmod(0o644); err != nil {
			_ = tmp.Close()
			_ = os.Remove(tmpName)
			return err
		}
		if err := tmp.Close(); err != nil {
			_ = os.Remove(tmpName)
			return err
		}
		if err := os.Rename(tmpName, final); err != nil {
			_ = os.Remove(tmpName)
			return err
		}
	}
	return nil
}

func LoadSampleProject(dir string) (*Project, error) {
	data, err := os.ReadFile(filepath.Join(dir, "v3.json"))
	if err != nil {
		return nil, err
	}
	return LoadProject(data)
}
