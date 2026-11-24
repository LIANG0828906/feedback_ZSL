package formats_test

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"strata/internal/formats"
	"strata/internal/generator"
	"strata/internal/model"
	"strata/internal/pipeline"
)

func TestGeneratedVersionsLoadAndExposeMapping(t *testing.T) {
	dir := t.TempDir()
	manifest, err := generator.Generate(195, dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest.Files) != 3 {
		t.Fatalf("expected 3 generated versions, got %d", len(manifest.Files))
	}
	for _, file := range manifest.Files {
		geometry, err := formats.LoadFile(filepath.Join(dir, file.File), file.VersionID)
		if err != nil {
			t.Fatalf("%s: %v", file.VersionID, err)
		}
		if len(geometry.Points) != 1837 {
			t.Fatalf("%s: points=%d", file.VersionID, len(geometry.Points))
		}
		if geometry.Meta.VersionID != file.VersionID {
			t.Fatalf("source version identity lost")
		}
		if len(geometry.Meta.FieldOrder) == 0 || len(geometry.Meta.Fields) != len(geometry.Meta.FieldOrder) {
			t.Fatalf("%s: field mapping is not queryable", file.VersionID)
		}
		if geometry.Meta.Fields[0].Path == "" {
			t.Fatalf("%s: field path missing", file.VersionID)
		}
	}
}

func TestVersionErrorsCarryPathAndOffset(t *testing.T) {
	dir := t.TempDir()
	missingY := `ply
format ascii 1.0
element vertex 1
property float x
property float z
end_header
0 0
`
	path := filepath.Join(dir, "missing-y.ply")
	if err := os.WriteFile(path, []byte(missingY), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := formats.LoadFile(path, model.VersionPLYASCII)
	var appErr *model.AppError
	if !errors.As(err, &appErr) || appErr.Code != model.EVER {
		t.Fatalf("expected E_VER, got %v", err)
	}
	if appErr.Path != "vertex.y" || appErr.Offset <= 0 {
		t.Fatalf("expected vertex.y offset, got path=%q offset=%d", appErr.Path, appErr.Offset)
	}

	badBinary := `ply
format binary_little_endian 1.0
element vertex 1
property float x
property float y
property float z
end_header
12345678`
	path = filepath.Join(dir, "short-binary.ply")
	if err := os.WriteFile(path, []byte(badBinary), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = formats.LoadFile(path, model.VersionPLYBinary)
	if !errors.As(err, &appErr) || appErr.Code != model.EVER || appErr.Path != "vertex.record" {
		t.Fatalf("expected E_VER vertex.record, got %v", err)
	}
}

func TestUnknownVersionAndDecodeErrors(t *testing.T) {
	_, err := formats.Version("pcbin/v99")
	var appErr *model.AppError
	if !errors.As(err, &appErr) || appErr.Code != model.EVER {
		t.Fatalf("expected E_VER, got %v", err)
	}

	dir := t.TempDir()
	if _, err := generator.Generate(195, dir); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "scene-compact-v1.pcbin"))
	if err != nil {
		t.Fatal(err)
	}
	headerLen := int(data[6]) | int(data[7])<<8
	data[8+headerLen+3] ^= 0x7f
	path := filepath.Join(dir, "corrupt.pcbin")
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = formats.LoadFile(path, model.VersionPCBin)
	if !errors.As(err, &appErr) || appErr.Code != model.EDECODE {
		t.Fatalf("expected E_DECODE, got %v", err)
	}
}

func TestExportEveryVersionRoundTripsWithLossyTrace(t *testing.T) {
	dir := t.TempDir()
	if _, err := generator.Generate(195, dir); err != nil {
		t.Fatal(err)
	}
	source, err := formats.LoadFile(filepath.Join(dir, "scene-ascii-v1.ply"), model.VersionPLYASCII)
	if err != nil {
		t.Fatal(err)
	}
	built, err := pipeline.NewBuilder().Build(source, pipeline.DefaultConfig(), "exact", 1)
	if err != nil {
		t.Fatal(err)
	}
	for _, version := range formats.Versions() {
		ext := ".ply"
		if version.ID == model.VersionPCBin {
			ext = ".pcbin"
		}
		cloudPath := filepath.Join(dir, "export-cloud-"+version.Short+ext)
		report, err := formats.WriteFile(cloudPath, version.ID, source, false)
		if err != nil {
			t.Fatalf("%s cloud export: %v", version.ID, err)
		}
		if report.ReadbackPoints != len(source.Points) {
			t.Fatalf("%s cloud readback=%d", version.ID, report.ReadbackPoints)
		}
		if version.ID != model.VersionPLYASCII && len(report.Warnings) == 0 {
			t.Fatalf("%s must emit E_LOSSY warnings", version.ID)
		}
		meshPath := filepath.Join(dir, "export-mesh-"+version.Short+ext)
		meshSource := &model.Geometry{Points: make([]model.Point, len(built.Mesh.Vertices)), Faces: make([]model.Face, len(built.Mesh.Faces))}
		for i, v := range built.Mesh.Vertices {
			meshSource.Points[i] = model.Point{
				X: v.Position.X, Y: v.Position.Y, Z: v.Position.Z,
				NX: v.Normal.X, NY: v.Normal.Y, NZ: v.Normal.Z,
				R: v.Color[0], G: v.Color[1], B: v.Color[2], Intensity: v.Intensity,
			}
		}
		for i, f := range built.Mesh.Faces {
			meshSource.Faces[i] = model.Face{A: f.A, B: f.B, C: f.C}
		}
		report, err = formats.WriteFile(meshPath, version.ID, meshSource, true)
		if err != nil {
			t.Fatalf("%s mesh export: %v", version.ID, err)
		}
		if report.ReadbackFaces != 3456 || report.ReadbackPoints != 2028 {
			t.Fatalf("%s mesh readback points=%d faces=%d", version.ID, report.ReadbackPoints, report.ReadbackFaces)
		}
	}
}
