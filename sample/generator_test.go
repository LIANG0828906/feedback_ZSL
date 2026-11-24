package sample_test

import (
	"bytes"
	"math"
	"testing"

	"diffrender/render"
	"diffrender/sample"
)

func TestGeneratorIsDeterministic(t *testing.T) {
	a, err := sample.GenerateTask(401)
	if err != nil {
		t.Fatal(err)
	}
	b, err := sample.GenerateTask(401)
	if err != nil {
		t.Fatal(err)
	}
	if a.Name != b.Name || len(a.Targets) != len(b.Targets) {
		t.Fatal("task metadata differs")
	}
	for version, data := range a.VersionFiles {
		if !bytes.Equal(data, b.VersionFiles[version]) {
			t.Fatalf("scene version %s differs", version)
		}
	}
	for version, data := range a.InitialVersions {
		if !bytes.Equal(data, b.InitialVersions[version]) {
			t.Fatalf("initial version %s differs", version)
		}
	}
	for i := range a.Targets {
		for p := range a.Targets[i].Pixels {
			if math.Float64bits(a.Targets[i].Pixels[p]) != math.Float64bits(b.Targets[i].Pixels[p]) {
				t.Fatalf("target %d pixel %d differs", i, p)
			}
		}
	}
	c, err := sample.GenerateTask(402)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(a.VersionFiles["1.0"], c.VersionFiles["1.0"]) {
		t.Fatal("different seeds produced identical scenes")
	}
}

func TestTargetFileRoundTrip(t *testing.T) {
	task, err := sample.GenerateTask(403)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	for i := range task.Targets {
		path := dir + "/" + task.TargetNames[i]
		if err := render.WriteTargetFile(path, task.Targets[i]); err != nil {
			t.Fatal(err)
		}
		got, err := render.ReadTargetFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if got.CameraID != task.Targets[i].CameraID || got.Width != task.Targets[i].Width || got.Height != task.Targets[i].Height {
			t.Fatal("target header differs")
		}
		for p := range got.Pixels {
			if math.Float64bits(got.Pixels[p]) != math.Float64bits(task.Targets[i].Pixels[p]) {
				t.Fatal("target payload differs")
			}
		}
	}
}
