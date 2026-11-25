package lipsync

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestGeneratorIsDeterministic(t *testing.T) {
	first, err := GenerateSampleSet(123, 3)
	if err != nil {
		t.Fatal(err)
	}
	second, err := GenerateSampleSet(123, 3)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatal("fixed seed generator is not deterministic")
	}
	other, err := GenerateSampleSet(124, 3)
	if err != nil {
		t.Fatal(err)
	}
	if reflect.DeepEqual(first, other) {
		t.Fatal("different seeds generated the same sample set")
	}
}

func TestRunDemoProducesVerifiedArtifacts(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "demo")
	report, err := RunDemo(context.Background(), DemoOptions{
		OutDir:        dir,
		Seed:          7,
		DialogueCount: 2,
		WindowSize:    6,
		MaxWorkers:    2,
		FrameBuffer:   2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !report.Summary.AllPathsEqual || !report.Summary.AllTruthEqual {
		t.Fatalf("demo summary failed: %+v", report.Summary)
	}
	if report.Summary.InjectedMatched != 4 || report.Summary.InjectedFailures != 4 {
		t.Fatalf("injection summary = %+v", report.Summary)
	}
	for _, name := range []string{"projects.json", "truths.json", "manifest.json", "lipsync.db", "report.json", "report.md", "index.html"} {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if info.Size() == 0 {
			t.Fatalf("%s is empty", name)
		}
	}
	reportData, err := os.ReadFile(filepath.Join(dir, "report.json"))
	if err != nil {
		t.Fatal(err)
	}
	var decoded DemoReport
	if err := json.Unmarshal(reportData, &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded.Dialogues) != 2 {
		t.Fatalf("report dialogues = %d, want 2", len(decoded.Dialogues))
	}
	html, err := os.ReadFile(filepath.Join(dir, "index.html"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(html), "口型同步校准诊断台") || !strings.Contains(string(html), "report-data") {
		t.Fatal("diagnostic page is missing expected content")
	}
	for _, dialogue := range decoded.Dialogues {
		for _, comparison := range dialogue.Comparisons {
			if !comparison.Consistent {
				t.Fatalf("%s/%s: %v", dialogue.Project.DialogueID, comparison.Name, comparison.Mismatch)
			}
		}
		for _, scenario := range dialogue.InvalidationScenarios {
			if !scenario.Incremental.OutputMatch {
				t.Fatalf("%s/%s incremental output mismatch", dialogue.Project.DialogueID, scenario.Name)
			}
		}
	}
}
