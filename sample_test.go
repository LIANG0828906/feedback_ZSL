package vpcompose_test

import (
	"bytes"
	"testing"

	"vpcompose"
	"vpcompose/sample"
)

func TestGeneratorIsDeterministic(t *testing.T) {
	a, err := sample.Generate(42)
	if err != nil {
		t.Fatal(err)
	}
	b, err := sample.Generate(42)
	if err != nil {
		t.Fatal(err)
	}
	for name, data := range a.Files {
		if !bytes.Equal(data, b.Files[name]) {
			t.Fatalf("%s is not deterministic", name)
		}
	}
	c, err := sample.Generate(43)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(a.Files["project-v3.json"], c.Files["project-v3.json"]) {
		t.Fatal("different seeds produced identical projects")
	}
}

func TestThreeFormatsLoadAndMatchAnalyticTruth(t *testing.T) {
	generated, err := sample.Generate(1234)
	if err != nil {
		t.Fatal(err)
	}
	names := []string{"project-v1.json", "project-v2.json", "project-v3.json"}
	var reference *vpcompose.Project
	for _, name := range names {
		project, err := vpcompose.LoadProject(generated.Files[name])
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if err := vpcompose.VerifyTruth(project); err != nil {
			t.Fatalf("%s truth mismatch: %v", name, err)
		}
		if len(project.Layers) != 4 {
			t.Fatalf("%s loaded %d layers", name, len(project.Layers))
		}
		if got, want := project.LayerOrder(), []string{"plate-backdrop", "talent-subject", "beauty-glow", "grade-look"}; !equalStrings(got, want) {
			t.Fatalf("%s layer order = %v, want %v", name, got, want)
		}
		if project.Width != sample.Width || project.Height != sample.Height || project.Channels != sample.Channels {
			t.Fatalf("%s dimensions = %dx%dx%d", name, project.Width, project.Height, project.Channels)
		}
		expectedEnds := map[string]int64{
			"plate-backdrop": int64(1100) * sample.TickRate / 1000,
			"talent-subject": int64(900) * sample.TickRate / 1000,
			"beauty-glow":    int64(600) * sample.TickRate / 1000,
			"grade-look":     int64(1200) * sample.TickRate / 1000,
		}
		for _, layer := range project.Layers {
			if layer.VisibleUntil != expectedEnds[layer.ID] {
				t.Fatalf("%s %s end = %d, want %d", name, layer.ID, layer.VisibleUntil, expectedEnds[layer.ID])
			}
			if layer.AlphaMode != vpcompose.AlphaPremultiplied {
				t.Fatalf("%s %s internal alpha = %s", name, layer.ID, layer.AlphaMode)
			}
		}
		if project.SourceVersion == vpcompose.Version1 {
			for _, layer := range project.Layers {
				if layer.SourceAlphaMode != vpcompose.AlphaStraight {
					t.Fatalf("v1 %s source alpha = %s", layer.ID, layer.SourceAlphaMode)
				}
			}
		}
		if reference == nil {
			reference = project
		} else {
			for i, layer := range project.Layers {
				ref := reference.Layers[i]
				if layer.ID != ref.ID || layer.Opacity != ref.Opacity ||
					layer.VisibleUntil != ref.VisibleUntil || layer.ContentHash != ref.ContentHash {
					t.Fatalf("%s layer %d differs from reference after normalization", name, i)
				}
			}
		}
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
