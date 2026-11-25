package dependency_test

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"lipsync.local/prototype/pkg/checkpoint"
	"lipsync.local/prototype/pkg/dependency"
	"lipsync.local/prototype/pkg/domain"
	"lipsync.local/prototype/pkg/pipeline"
	"lipsync.local/prototype/pkg/sample"
)

func TestPreciseWeightInvalidationAndPersistentReuse(t *testing.T) {
	project, _, err := sample.Generate(sample.DefaultSeed)
	if err != nil {
		t.Fatal(err)
	}
	oldReport := runPipeline(t, project, "old")
	edited := cloneProject(t, project)
	edited.Dialogues[0].Phonemes[1].WeightPermille += 7
	newReport := runPipeline(t, edited, "new")

	report := dependency.Diff(oldReport.Graph, newReport.Graph)
	expected := packageContaining(oldReport.Artifacts[edited.Dialogues[0].ID].Lip.Packages, edited.Dialogues[0].Phonemes[1].ID)
	if expected == "" {
		t.Fatal("could not find affected package")
	}
	if len(report.InvalidatedPackages) != 1 || report.InvalidatedPackages[0] != expected {
		t.Fatalf("want only %s invalidated, got %v", expected, report.InvalidatedPackages)
	}
	if len(report.InvalidatedDispatchPackages) != 1 || report.InvalidatedDispatchPackages[0] != expected {
		t.Fatalf("downstream dispatch invalidation not precise: %v", report.InvalidatedDispatchPackages)
	}
	total := 0
	for _, art := range oldReport.Artifacts {
		total += len(art.Lip.Packages)
	}
	if len(report.ReusedPackages) != total-1 {
		t.Fatalf("want %d reused packages, got %d", total-1, len(report.ReusedPackages))
	}

	store, err := checkpoint.Open(filepath.Join(t.TempDir(), "cache.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := dependency.PersistPackages(context.Background(), store, oldReport.Graph, project.Recipe); err != nil {
		t.Fatal(err)
	}
	reused, missing, err := dependency.LoadReusablePackages(context.Background(), store, newReport.Graph, report, project.Recipe)
	if err != nil {
		t.Fatal(err)
	}
	if len(missing) != 0 {
		t.Fatalf("persisted packages missing: %v", missing)
	}
	if _, bad := reused[expected]; bad {
		t.Fatalf("invalidated package %s was reused", expected)
	}
	if len(reused) != total-1 {
		t.Fatalf("want %d reused packages from sqlite, got %d", total-1, len(reused))
	}
}

func TestFrameRateInvalidatesAllPackages(t *testing.T) {
	project, _, err := sample.Generate(sample.DefaultSeed)
	if err != nil {
		t.Fatal(err)
	}
	oldReport := runPipeline(t, project, "old")
	edited := cloneProject(t, project)
	edited.Recipe.FrameRate = 60
	newReport := runPipeline(t, edited, "new")
	report := dependency.Diff(oldReport.Graph, newReport.Graph)
	all := make(map[string]struct{})
	for _, art := range newReport.Artifacts {
		for _, pkg := range art.Lip.Packages {
			all[pkg.ID] = struct{}{}
		}
	}
	if len(report.InvalidatedPackages) != len(all) {
		t.Fatalf("frame-rate change should invalidate all %d packages, got %v", len(all), report.InvalidatedPackages)
	}
	for _, id := range report.InvalidatedPackages {
		if _, ok := all[id]; !ok {
			t.Fatalf("unexpected invalidated package %s", id)
		}
	}
}

func TestAffinityEditInvalidatesOnlyBoundaryPackage(t *testing.T) {
	project, _, err := sample.Generate(sample.DefaultSeed)
	if err != nil {
		t.Fatal(err)
	}
	oldReport := runPipeline(t, project, "old")
	edited := cloneProject(t, project)
	var expected string
	var leftPhoneme string
	for _, pkg := range oldReport.Artifacts[edited.Dialogues[0].ID].Lip.Packages {
		if len(pkg.PhonemeIDs) >= 2 {
			expected = pkg.ID
			leftPhoneme = pkg.PhonemeIDs[0]
			break
		}
	}
	if expected == "" {
		t.Fatal("sample lacks a merged package")
	}
	for i := range edited.Dialogues[0].Phonemes {
		if edited.Dialogues[0].Phonemes[i].ID == leftPhoneme {
			if edited.Dialogues[0].Phonemes[i].AffinityToNext == 1000 {
				edited.Dialogues[0].Phonemes[i].AffinityToNext = 950
			} else {
				edited.Dialogues[0].Phonemes[i].AffinityToNext = 1000
			}
			break
		}
	}
	newReport := runPipeline(t, edited, "new")
	report := dependency.Diff(oldReport.Graph, newReport.Graph)
	if len(report.InvalidatedPackages) != 1 || report.InvalidatedPackages[0] != expected {
		t.Fatalf("want only affinity package %s invalidated, got %v", expected, report.InvalidatedPackages)
	}
}

func TestBoundaryInvalidationDoesNotTouchOtherDialogue(t *testing.T) {
	project, _, err := sample.Generate(sample.DefaultSeed)
	if err != nil {
		t.Fatal(err)
	}
	oldReport := runPipeline(t, project, "old")
	edited := cloneProject(t, project)
	edited.Dialogues[2].Phonemes[2].EndSample -= 120
	newReport := runPipeline(t, edited, "new")
	report := dependency.Diff(oldReport.Graph, newReport.Graph)
	expected := packageContaining(oldReport.Artifacts[edited.Dialogues[2].ID].Lip.Packages, edited.Dialogues[2].Phonemes[2].ID)
	if len(report.InvalidatedPackages) != 1 || report.InvalidatedPackages[0] != expected {
		t.Fatalf("want only %s invalidated, got %v", expected, report.InvalidatedPackages)
	}
	for _, pkg := range report.InvalidatedPackages {
		for i, d := range edited.Dialogues {
			if i != 2 && strings.HasPrefix(pkg, d.ID+"/") {
				t.Fatalf("package from another dialogue invalidated: %s", pkg)
			}
		}
	}
}

func runPipeline(t *testing.T, project *domain.Project, name string) *pipeline.Report {
	t.Helper()
	store, err := checkpoint.Open(filepath.Join(t.TempDir(), name+".sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	report, err := pipeline.Run(context.Background(), project, pipeline.Options{Path: pipeline.PathFull, Store: store})
	if err != nil {
		t.Fatal(err)
	}
	return report
}

func cloneProject(t *testing.T, project *domain.Project) *domain.Project {
	t.Helper()
	data, err := json.Marshal(project)
	if err != nil {
		t.Fatal(err)
	}
	var clone domain.Project
	if err := json.Unmarshal(data, &clone); err != nil {
		t.Fatal(err)
	}
	return &clone
}

func packageContaining(packages []domain.LipPackage, phonemeID string) string {
	for _, pkg := range packages {
		for _, id := range pkg.PhonemeIDs {
			if id == phonemeID {
				return pkg.ID
			}
		}
	}
	return ""
}
