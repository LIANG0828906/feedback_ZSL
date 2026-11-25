package schema_test

import (
	"errors"
	"testing"

	"wardrobe/generator"
	"wardrobe/schema"
)

func TestGeneratedBundlePassesSchema(t *testing.T) {
	bundle := generator.Generate(generator.DefaultSeed)
	if err := schema.Validate(&bundle); err != nil {
		t.Fatal(err)
	}
}

func TestMissingAndDuplicateFieldsReportSchemaPathAndObject(t *testing.T) {
	t.Run("missing mount id", func(t *testing.T) {
		bundle := generator.Generate(generator.DefaultSeed)
		bundle.Projects[0].Mounts[0].ID = ""
		err := schema.Validate(&bundle)
		assertSchemaIssue(t, err, "$.projects[0].mounts[0].id")
	})
	t.Run("duplicate part id", func(t *testing.T) {
		bundle := generator.Generate(generator.DefaultSeed)
		bundle.Projects[0].Parts[1].ID = bundle.Projects[0].Parts[0].ID
		err := schema.Validate(&bundle)
		assertSchemaIssue(t, err, "$.projects[0].parts[1].id")
	})
	t.Run("missing exclusion reference", func(t *testing.T) {
		bundle := generator.Generate(generator.DefaultSeed)
		bundle.Projects[0].ExclusionRules[0].PartB = "part-does-not-exist"
		err := schema.Validate(&bundle)
		assertSchemaIssue(t, err, "$.projects[0].exclusionRules[0].partB")
	})
}

func TestSubmeshRangesMustFitAndNotOverlap(t *testing.T) {
	t.Run("overflow", func(t *testing.T) {
		bundle := generator.Generate(generator.DefaultSeed)
		mesh := &bundle.Projects[0].Parts[0].Mesh
		mesh.Submeshes[0].VertexCount = mesh.VertexCount + 1
		err := schema.Validate(&bundle)
		assertSchemaIssue(t, err, "$.projects[0].parts[0].mesh.submeshes[0].vertexCount")
	})
	t.Run("vertex overlap", func(t *testing.T) {
		bundle := generator.Generate(generator.DefaultSeed)
		mesh := &bundle.Projects[0].Parts[0].Mesh
		mesh.Submeshes[1].VertexStart = mesh.Submeshes[0].VertexStart + 1
		err := schema.Validate(&bundle)
		assertSchemaIssue(t, err, "$.projects[0].parts[0].mesh.submeshes")
	})
	t.Run("index overlap", func(t *testing.T) {
		bundle := generator.Generate(generator.DefaultSeed)
		mesh := &bundle.Projects[0].Parts[0].Mesh
		mesh.Submeshes[1].IndexStart = mesh.Submeshes[0].IndexStart + 1
		err := schema.Validate(&bundle)
		assertSchemaIssue(t, err, "$.projects[0].parts[0].mesh.submeshes")
	})
}

func TestDecodeRejectsUnknownAndMissingTopLevelFields(t *testing.T) {
	if _, err := schema.Decode([]byte(`{}`)); err == nil {
		t.Fatal("expected E_SCHEMA for missing fields")
	} else {
		assertSchemaIssue(t, err, "$.schemaVersion")
	}
	if _, err := schema.Decode([]byte(`{"schemaVersion":"wardrobe.schema/v1","unknown":true}`)); err == nil {
		t.Fatal("expected E_SCHEMA for unknown field")
	}
}

func assertSchemaIssue(t *testing.T, err error, wantedPath string) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected schema error containing path %s", wantedPath)
	}
	var validationError *schema.ValidationError
	if !errors.As(err, &validationError) {
		t.Fatalf("expected ValidationError, got %T: %v", err, err)
	}
	for _, issue := range validationError.Issues {
		if issue.Code != schema.CodeSchema {
			t.Fatalf("issue code=%s", issue.Code)
		}
		if issue.ObjectID == "" {
			t.Fatal("issue has empty object id")
		}
		if issue.Path == wantedPath {
			return
		}
	}
	t.Fatalf("no issue at %s: %v", wantedPath, validationError.Issues)
}
