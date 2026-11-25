package iconmask

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
)

func testProject(t *testing.T) *Project {
	t.Helper()
	project := GenerateProject(42)
	if err := ValidateProject(project); err != nil {
		t.Fatalf("generated project must validate: %v", err)
	}
	return project
}

func TestDeterministicProject(t *testing.T) {
	left := GenerateProject(42)
	right := GenerateProject(42)
	if !reflect.DeepEqual(left, right) {
		t.Fatal("same seed must generate byte-identical project structures")
	}
	other := GenerateProject(43)
	if reflect.DeepEqual(left, other) {
		t.Fatal("different seeds should produce different samples")
	}
	if len(left.Images) != 3 {
		t.Fatalf("expected three resolutions, got %d", len(left.Images))
	}
	for _, image := range left.Images {
		if image.Width != image.Height {
			t.Fatalf("%s: expected square sample", image.ID)
		}
		if image.InitialMask.Width != image.Width || image.InitialMask.Height != image.Height {
			t.Fatalf("%s: initial mask dimensions do not match image", image.ID)
		}
		if !image.InitialMask.Binary() {
			t.Fatalf("%s: initial mask is not binary", image.ID)
		}
		if len(image.Truth.Components) != 3 {
			t.Fatalf("%s: expected three analytic components, got %d", image.ID, len(image.Truth.Components))
		}
		if len(image.ImagePNG) == 0 {
			t.Fatalf("%s: expected deterministic PNG", image.ID)
		}
	}
}

func TestSchemaValidationErrors(t *testing.T) {
	project := testProject(t)
	imageID := project.Images[0].ID

	t.Run("mask width conflict", func(t *testing.T) {
		broken := cloneProject(t, project)
		broken.Images[0].InitialMask.Width++
		err := ValidateProject(broken)
		assertSchemaError(t, err, imageID, "initialMask.width")
	})

	t.Run("non binary mask", func(t *testing.T) {
		broken := cloneProject(t, project)
		broken.Images[0].InitialMask.Data[0] = 7
		err := ValidateProject(broken)
		assertSchemaError(t, err, imageID, "initialMask.data")
	})

	t.Run("missing truth", func(t *testing.T) {
		broken := cloneProject(t, project)
		broken.Images[0].Truth = Truth{}
		err := ValidateProject(broken)
		assertSchemaError(t, err, imageID, "truth.referenceMask.width")
	})

	t.Run("missing image png", func(t *testing.T) {
		broken := cloneProject(t, project)
		broken.Images[0].ImagePNG = nil
		err := ValidateProject(broken)
		assertSchemaError(t, err, imageID, "imagePng")
	})

	t.Run("segment gap", func(t *testing.T) {
		broken := cloneProject(t, project)
		segments := broken.Images[0].Truth.Segments
		segments[1].Start += 0.01
		err := ValidateProject(broken)
		if err == nil || err.Code != CodeSchema {
			t.Fatalf("expected E_SCHEMA, got %v", err)
		}
		if err.ImageID != imageID || err.Field != "truth.segments.start" {
			t.Fatalf("unexpected error context: %+v", err)
		}
	})

	t.Run("missing segment curvature loop end", func(t *testing.T) {
		broken := cloneProject(t, project)
		segments := broken.Images[0].Truth.Segments
		segments[len(segments)-1].End = 0.99
		err := ValidateProject(broken)
		assertSchemaError(t, err, imageID, "truth.segments.end")
	})
}

func TestProjectJSONRoundTrip(t *testing.T) {
	project := testProject(t)
	path := filepath.Join(t.TempDir(), "project.json")
	if err := SaveProject(path, project); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadProject(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(project, loaded) {
		t.Fatal("project JSON round trip changed the domain data")
	}
}

func assertSchemaError(t *testing.T, err *Error, imageID, field string) {
	t.Helper()
	if err == nil {
		t.Fatal("expected schema error")
	}
	if err.Code != CodeSchema {
		t.Fatalf("expected E_SCHEMA, got %s (%v)", err.Code, err)
	}
	if err.ImageID != imageID {
		t.Fatalf("expected image id %q, got %q", imageID, err.ImageID)
	}
	if err.Field != field {
		t.Fatalf("expected field %q, got %q", field, err.Field)
	}
}

func cloneProject(t *testing.T, project *Project) *Project {
	t.Helper()
	data, err := json.Marshal(project)
	if err != nil {
		t.Fatal(err)
	}
	var cloned Project
	if err := json.Unmarshal(data, &cloned); err != nil {
		t.Fatal(err)
	}
	return &cloned
}

func findImage(t *testing.T, project *Project, id string) ImageSample {
	t.Helper()
	for _, image := range project.Images {
		if image.ID == id {
			return image
		}
	}
	t.Fatalf("image %q not found", id)
	return ImageSample{}
}

func assertCode(t *testing.T, err error, code string) *Error {
	t.Helper()
	var domainErr *Error
	if !errors.As(err, &domainErr) {
		t.Fatalf("expected domain error %s, got %v", code, err)
	}
	if domainErr.Code != code {
		t.Fatalf("expected %s, got %s (%v)", code, domainErr.Code, domainErr)
	}
	return domainErr
}
