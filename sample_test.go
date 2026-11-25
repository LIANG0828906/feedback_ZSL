package macrokit

import (
	"path/filepath"
	"testing"
)

func TestThreeFormatsMatchGeneratedTruth(t *testing.T) {
	set := GenerateSamples(FixedSampleSeed)
	root := t.TempDir()
	written, err := WriteSampleSet(root, set)
	if err != nil {
		t.Fatalf("WriteSampleSet: %v", err)
	}

	for _, ws := range written {
		findSample(t, set, ws.Slug)
		truth, err := LoadTruthFile(filepath.Join(ws.Dir, "truth.json"))
		if err != nil {
			t.Fatal(err)
		}
		events, err := LoadEventStreamFile(filepath.Join(ws.Dir, "events.json"))
		if err != nil {
			t.Fatal(err)
		}

		var reference *RunResult
		for _, formatNo := range []int{FormatFlatFrameV1, FormatNestedMSV2, FormatTreeFrameV3} {
			project, err := LoadProjectFile(ws.Files[formatNo])
			if err != nil {
				t.Fatalf("%s format %d load: %v", ws.Slug, formatNo, err)
			}
			if project.FormatNo != formatNo {
				t.Fatalf("format id = %d, want %d", project.FormatNo, formatNo)
			}
			result, err := RunProject(project, events, ExecutorConfig{FrameCount: truth.FrameCount})
			if err != nil {
				t.Fatalf("%s format %d run: %v", ws.Slug, formatNo, err)
			}
			if err := VerifyTruth(result, truth); err != nil {
				t.Fatalf("%s format %d truth mismatch: %v", ws.Slug, formatNo, err)
			}
			if reference == nil {
				reference = result
			} else if err := CompareResults(reference, result); err != nil {
				t.Fatalf("%s format %d differs from format 1: %v", ws.Slug, formatNo, err)
			}
		}
	}
}

func TestGeneratedExampleDirectoryCanBeVerified(t *testing.T) {
	set := GenerateSamples(FixedSampleSeed)
	root := t.TempDir()
	if _, err := WriteSampleSet(root, set); err != nil {
		t.Fatal(err)
	}

	for _, sample := range set.Projects {
		dir := filepath.Join(root, sample.Slug)
		project, err := LoadProjectFile(filepath.Join(dir, "flat-frame-v1.json"))
		if err != nil {
			t.Fatal(err)
		}
		result, err := RunProject(project, sample.Events, ExecutorConfig{FrameCount: sample.Truth.FrameCount})
		if err != nil {
			t.Fatal(err)
		}
		if err := VerifyTruth(result, sample.Truth); err != nil {
			t.Fatal(err)
		}
	}
}

func findSample(t *testing.T, set *SampleSet, slug string) *SampleProject {
	t.Helper()
	for _, sample := range set.Projects {
		if sample.Slug == slug {
			return sample
		}
	}
	t.Fatalf("sample %q not found", slug)
	return nil
}
