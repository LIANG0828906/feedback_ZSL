package generator

import (
	"testing"

	"performanceexpressions/workspace/domain"
	"performanceexpressions/workspace/engine"
)

func TestGenerateProjectsDeterministicAndTruthAccurate(t *testing.T) {
	first := GenerateProjects(DefaultSeed)
	second := GenerateProjects(DefaultSeed)
	if len(first) != 3 {
		t.Fatalf("expected 3 projects, got %d", len(first))
	}
	for i := range first {
		h1, err := domain.HashJSON(first[i])
		if err != nil {
			t.Fatal(err)
		}
		h2, err := domain.HashJSON(second[i])
		if err != nil {
			t.Fatal(err)
		}
		if h1 != h2 {
			t.Fatalf("project %d is not deterministic: %s != %s", i, h1, h2)
		}
		if err := engine.ValidateProject(first[i], engine.ValidateOptions{RequireTruth: true}); err != nil {
			t.Fatalf("generated project is invalid: %v", err)
		}
		result, err := engine.EvaluateProject(first[i], domain.DefaultParameters())
		if err != nil {
			t.Fatal(err)
		}
		report, err := engine.ValidatePrecision(first[i], result, 1e-9)
		if err != nil {
			t.Fatalf("precision failed: %v", err)
		}
		if !report.OK || report.FailedPoints != 0 || report.SourceMismatches != 0 {
			t.Fatalf("precision report not ok: %+v", report)
		}
		if report.TotalPoints != 192 {
			t.Fatalf("expected 192 truth points, got %d", report.TotalPoints)
		}
	}
}

func TestWriteSamplesStable(t *testing.T) {
	dir := t.TempDir()
	index, err := WriteSamples(dir, DefaultSeed)
	if err != nil {
		t.Fatal(err)
	}
	if len(index.Projects) != 3 {
		t.Fatalf("expected 3 index entries, got %d", len(index.Projects))
	}
	project, err := LoadProject(dir + "/" + index.Projects[0].File)
	if err != nil {
		t.Fatal(err)
	}
	hash, err := domain.HashJSON(project)
	if err != nil {
		t.Fatal(err)
	}
	if hash != index.Projects[0].Hash {
		t.Fatalf("index hash mismatch: %s != %s", hash, index.Projects[0].Hash)
	}
}
