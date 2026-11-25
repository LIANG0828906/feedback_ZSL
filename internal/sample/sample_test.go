package sample

import (
	"testing"

	"meshdoctor/internal/errs"
	"meshdoctor/internal/mesh"
)

func TestGeneratedSamplesMatchManifest(t *testing.T) {
	samples, err := GenerateAll(DefaultSeed)
	if err != nil {
		t.Fatal(err)
	}
	if len(samples) < 3 {
		t.Fatalf("samples=%d want >=3", len(samples))
	}
	for _, s := range samples {
		if s.Manifest.Truth.Euler != 2 {
			t.Fatalf("%s euler=%d want 2", s.Manifest.ID, s.Manifest.Truth.Euler)
		}
		if len(s.Manifest.HoleRings) == 0 || len(s.Manifest.KnownPoints) == 0 {
			t.Fatalf("%s missing truth rings/points", s.Manifest.ID)
		}
	}
	for _, id := range []string{"cube", "octa"} {
		var found *Sample
		for i := range samples {
			if samples[i].Manifest.ID == id {
				found = &samples[i]
			}
		}
		if pairs := mesh.SelfIntersectingPairs(&found.Mesh); len(pairs) == 0 {
			t.Fatalf("%s does not contain a detectable self-intersecting sliver", id)
		}
	}
}

func TestLoadValidationErrorCodes(t *testing.T) {
	badIndex := &mesh.Mesh{Vertices: make([]mesh.Vec3, 2), Faces: []mesh.Face{{A: 0, B: 1, C: 7}}}
	err := ValidateLoadedMesh(badIndex, &Manifest{InputVertices: 2, InputFaces: 1})
	if errs.Code(err) != errs.EIDX {
		t.Fatalf("code=%q err=%v want E_IDX", errs.Code(err), err)
	}

	badCount := &mesh.Mesh{
		Vertices: []mesh.Vec3{{0, 0, 0}, {1, 0, 0}, {0, 1, 0}},
		Faces:    []mesh.Face{{A: 0, B: 1, C: 2}},
	}
	err = ValidateLoadedMesh(badCount, &Manifest{InputVertices: 3, InputFaces: 9})
	if errs.Code(err) != errs.EMETA {
		t.Fatalf("code=%q err=%v want E_META", errs.Code(err), err)
	}

	degenerate := &mesh.Mesh{
		Vertices: []mesh.Vec3{{0, 0, 0}, {1, 0, 0}, {2, 0, 0}},
		Faces:    []mesh.Face{{A: 0, B: 1, C: 2}},
	}
	err = ValidateLoadedMesh(degenerate, &Manifest{InputVertices: 3, InputFaces: 1})
	if errs.Code(err) != errs.EMETA {
		t.Fatalf("code=%q err=%v want E_META for degenerate", errs.Code(err), err)
	}
}
