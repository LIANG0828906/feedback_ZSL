package verify

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"

	"mirrorcast/internal/generator"
	"mirrorcast/internal/projectio"
	"mirrorcast/internal/session"
)

type ManifestProject struct {
	Name      string `json:"name"`
	ProjectID string `json:"project_id"`
	V1        string `json:"v1_file"`
	V2        string `json:"v2_file"`
	V3        string `json:"v3_file"`
	Truth     string `json:"truth_file"`
}

type Manifest struct {
	SchemaVersion string            `json:"schema_version"`
	Seed          int64             `json:"seed"`
	FixedSeedHex  string            `json:"fixed_seed_hex"`
	Projects      []ManifestProject `json:"projects"`
	CriticalFile  string            `json:"critical_file"`
	PrecisionFile string            `json:"precision_report"`
	VerifyReport  string            `json:"verification_report"`
}

func WriteSuiteFiles(suite *generator.Suite, dir string) (*Manifest, *Report, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, nil, err
	}
	report, err := VerifySuite(suite)
	if err != nil {
		return nil, nil, err
	}
	manifest := &Manifest{
		SchemaVersion: "mirrorcast.manifest/v1",
		Seed:          suite.Seed,
		FixedSeedHex:  fmt.Sprintf("0x%X", suite.Seed),
		CriticalFile:  "critical-cases.json",
		PrecisionFile: "precision-report.json",
		VerifyReport:  "verification-report.json",
	}
	for _, generated := range suite.Projects {
		base := generated.Name
		files := map[string]string{
			session.SchemaV1: base + ".v1.json",
			session.SchemaV2: base + ".v2.json",
			session.SchemaV3: base + ".v3.json",
		}
		for _, version := range []string{session.SchemaV1, session.SchemaV2, session.SchemaV3} {
			data, err := projectio.Write(generated.Project, version)
			if err != nil {
				return nil, nil, err
			}
			if err := os.WriteFile(filepath.Join(dir, files[version]), data, 0o644); err != nil {
				return nil, nil, err
			}
		}
		truthBytes, err := projectio.WriteTruth(generated.Truth)
		if err != nil {
			return nil, nil, err
		}
		truthName := base + ".truth.json"
		if err := os.WriteFile(filepath.Join(dir, truthName), truthBytes, 0o644); err != nil {
			return nil, nil, err
		}
		manifest.Projects = append(manifest.Projects, ManifestProject{
			Name: generated.Name, ProjectID: generated.Project.ProjectID,
			V1: files[session.SchemaV1], V2: files[session.SchemaV2], V3: files[session.SchemaV3], Truth: truthName,
		})
	}
	criticalBytes, err := json.MarshalIndent(suite.Critical, "", "  ")
	if err != nil {
		return nil, nil, err
	}
	if err := os.WriteFile(filepath.Join(dir, manifest.CriticalFile), criticalBytes, 0o644); err != nil {
		return nil, nil, err
	}
	precisionItems := []PrecisionItem{}
	for _, projectReport := range report.Projects {
		precisionItems = append(precisionItems, projectReport.Precision...)
	}
	precisionBytes, err := json.MarshalIndent(struct {
		DeclaredArithmeticBits int                `json:"declared_arithmetic_bits"`
		Tolerances             session.Tolerances `json:"tolerances"`
		Items                  []PrecisionItem    `json:"items"`
	}{DeclaredArithmeticBits: 64, Tolerances: session.DeclaredTolerances(), Items: precisionItems}, "", "  ")
	if err != nil {
		return nil, nil, err
	}
	if err := os.WriteFile(filepath.Join(dir, manifest.PrecisionFile), precisionBytes, 0o644); err != nil {
		return nil, nil, err
	}
	reportBytes, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return nil, nil, err
	}
	if err := os.WriteFile(filepath.Join(dir, manifest.VerifyReport), reportBytes, 0o644); err != nil {
		return nil, nil, err
	}
	manifestBytes, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return nil, nil, err
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), manifestBytes, 0o644); err != nil {
		return nil, nil, err
	}
	return manifest, report, nil
}

func VerifyDirectory(dir string) (*Report, error) {
	manifestBytes, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return nil, err
	}
	var manifest Manifest
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		return nil, err
	}
	report := &Report{OK: true, Seed: manifest.Seed}
	for _, mp := range manifest.Projects {
		pr := ProjectReport{Name: mp.Name, ProjectID: mp.ProjectID, VersionsEquivalent: true, TruthMatches: true}
		loaded := map[string]*session.Project{}
		paths := map[string]string{session.SchemaV1: mp.V1, session.SchemaV2: mp.V2, session.SchemaV3: mp.V3}
		for _, version := range []string{session.SchemaV1, session.SchemaV2, session.SchemaV3} {
			data, err := os.ReadFile(filepath.Join(dir, paths[version]))
			if err != nil {
				return nil, err
			}
			project, err := projectio.LoadVersion(data, version)
			if err != nil {
				return nil, fmt.Errorf("%s/%s: %w", mp.Name, version, err)
			}
			loaded[version] = project
		}
		for _, version := range []string{session.SchemaV2, session.SchemaV3} {
			if mismatch := projectMismatch(loaded[session.SchemaV1], loaded[version]); mismatch != "" {
				pr.VersionsEquivalent = false
				pr.Mismatches = append(pr.Mismatches, version+": "+mismatch)
			}
		}
		truthBytes, err := os.ReadFile(filepath.Join(dir, mp.Truth))
		if err != nil {
			return nil, err
		}
		var truth session.TruthFile
		if err := json.Unmarshal(truthBytes, &truth); err != nil {
			return nil, err
		}
		calculatedTruth, err := generator.BuildTruth(*loaded[session.SchemaV1])
		if err != nil {
			return nil, err
		}
		if !reflect.DeepEqual(calculatedTruth.Ticks, truth.Ticks) {
			pr.TruthMatches = false
			pr.Mismatches = append(pr.Mismatches, "on-disk truth does not match replay")
		}
		pr.ScreenCount = len(loaded[session.SchemaV1].Screens)
		pr.TickCount = len(truth.Ticks)
		pr.Precision = BuildPrecision(*loaded[session.SchemaV1], truth)
		for _, item := range pr.Precision {
			if !item.WithinTolerance {
				pr.TruthMatches = false
				pr.Mismatches = append(pr.Mismatches, fmt.Sprintf("precision mismatch %s@%d", item.ScreenID, item.Tick))
			}
		}
		if !pr.VersionsEquivalent || !pr.TruthMatches {
			report.OK = false
		}
		report.Projects = append(report.Projects, pr)
	}
	criticalBytes, err := os.ReadFile(filepath.Join(dir, manifest.CriticalFile))
	if err != nil {
		return nil, err
	}
	var criticalFile generator.CriticalFile
	if err := json.Unmarshal(criticalBytes, &criticalFile); err != nil {
		return nil, err
	}
	for _, test := range criticalFile.Cases {
		cr := RunCritical(test)
		if !cr.OK {
			report.OK = false
		}
		report.Critical = append(report.Critical, cr)
	}
	return report, nil
}
