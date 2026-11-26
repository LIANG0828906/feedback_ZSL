package sample_test

import (
	"bytes"
	"image/png"
	"os"
	"path/filepath"
	"testing"

	"maskqc/model"
	"maskqc/sample"
	"maskqc/topology"
)

func TestDeterministicMultiResolutionGeneration(t *testing.T) {
	first, err := sample.Generate(5150, []int{96, 128})
	if err != nil {
		t.Fatal(err)
	}
	second, err := sample.Generate(5150, []int{96, 128})
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Projects) != 2 || len(second.Projects) != 2 {
		t.Fatal("multiple resolutions were not generated")
	}
	for index := range first.Projects {
		left, right := first.Projects[index], second.Projects[index]
		if left.ID != right.ID || left.Canonical.Width != right.Canonical.Width {
			t.Fatal("same seed produced different project order or dimensions")
		}
		if !bytes.Equal(left.ImagePNG, right.ImagePNG) {
			t.Fatalf("%s image is not deterministic", left.ID)
		}
		for _, version := range []model.SchemaVersion{model.SchemaV1, model.SchemaV2, model.SchemaV3} {
			if !bytes.Equal(left.Artifacts[version], right.Artifacts[version]) {
				t.Fatalf("%s %s artifact is not deterministic", left.ID, version)
			}
		}
	}
	different, err := sample.Generate(5151, []int{96, 128})
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(first.Projects[0].ImagePNG, different.Projects[0].ImagePNG) ||
		bytes.Equal(first.Projects[0].Artifacts[model.SchemaV3], different.Projects[0].Artifacts[model.SchemaV3]) {
		t.Fatal("different seed did not change generated data")
	}
}

func TestCandidatePerturbationsAndIconContent(t *testing.T) {
	bundle, err := sample.Generate(6100, []int{96})
	if err != nil {
		t.Fatal(err)
	}
	generated := bundle.Projects[0]
	project := generated.Canonical
	reference := make([]byte, project.Width*project.Height)
	for _, truth := range project.Truth {
		for index, value := range truth.ReferenceMask {
			if value >= model.ForegroundThreshold {
				reference[index] = 255
			}
		}
	}
	eroded, spilled := 0, 0
	for index := range reference {
		ref := reference[index] >= model.ForegroundThreshold
		candidate := project.CandidateMask[index] >= model.ForegroundThreshold
		if ref && !candidate {
			eroded++
		}
		if !ref && candidate {
			spilled++
		}
	}
	if eroded == 0 || spilled == 0 {
		t.Fatalf("candidate lacks erosion or spill: eroded=%d spilled=%d", eroded, spilled)
	}
	candidateAnalysis, err := topology.Analyze(project.CandidateMask, project.Width, project.Height, project.Strokes)
	if err != nil {
		t.Fatal(err)
	}
	holes := 0
	for _, domain := range candidateAnalysis.Domains {
		holes += domain.Holes
	}
	if holes == 0 {
		t.Fatal("candidate has no injected small holes")
	}
	img, err := png.Decode(bytes.NewReader(generated.ImagePNG))
	if err != nil {
		t.Fatal(err)
	}
	if img.Bounds().Dx() != project.Width || img.Bounds().Dy() != project.Height {
		t.Fatal("icon dimensions do not match project")
	}
	colors := map[uint32]bool{}
	translucent := 0
	for y := 0; y < project.Height; y++ {
		for x := 0; x < project.Width; x++ {
			r, g, b, a := img.At(x, y).RGBA()
			colors[r>>8<<16|g>>8<<8|b>>8] = true
			if a>>8 < 255 {
				translucent++
			}
		}
	}
	if len(colors) < 20 {
		t.Fatal("icon lacks gradient, foreground, or noise variation")
	}
	if translucent == 0 {
		t.Fatal("icon lacks semi-transparent outline pixels")
	}
}

func TestWriteSamplesAndManifestReload(t *testing.T) {
	bundle, err := sample.Generate(7100, []int{96})
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	manifest, err := sample.Write(root, bundle)
	if err != nil {
		t.Fatal(err)
	}
	reloaded, err := sample.LoadManifest(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest.Projects) != 1 || len(reloaded.Projects) != 1 {
		t.Fatal("manifest project count changed")
	}
	for _, relative := range reloaded.Projects[0].Files {
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(relative))); err != nil {
			t.Fatalf("artifact missing: %s", relative)
		}
	}
	if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(reloaded.Projects[0].Image))); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "manifest.json")); err != nil {
		t.Fatal(err)
	}
}
