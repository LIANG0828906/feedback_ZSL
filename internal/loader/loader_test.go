package loader_test

import (
	"bytes"
	"testing"

	"layerforge/internal/apperr"
	"layerforge/internal/build"
	"layerforge/internal/generator"
	"layerforge/internal/loader"
	"layerforge/internal/model"
)

func TestThreeSchemasLoadToSameSemanticProject(t *testing.T) {
	fixture, err := generator.Generate(42)
	if err != nil {
		t.Fatal(err)
	}
	wantHash := model.SemanticHash(fixture.Project)
	for name, manifest := range fixture.Versions {
		manifest := manifest
		t.Run(name, func(t *testing.T) {
			result, err := loader.Load(manifest)
			if err != nil {
				t.Fatalf("load: %v", err)
			}
			if got := model.SemanticHash(result.Project); got != wantHash {
				t.Fatalf("semantic hash = %s, want %s", got, wantHash)
			}
			derivation, err := build.Derive(result.Graph)
			if err != nil {
				t.Fatal(err)
			}
			if err := build.CompareTruth(derivation, fixture.Derivation); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestUnknownFieldReturnsLocatedBusinessCode(t *testing.T) {
	fixture, err := generator.Generate(42)
	if err != nil {
		t.Fatal(err)
	}
	manifest := bytes.Replace(fixture.Versions["project.v1.json"],
		[]byte(`"schema": "layerforge/v1",`),
		[]byte(`"schema": "layerforge/v1", "surprise": true,`), 1)
	_, err = loader.Load(manifest)
	if !apperr.IsCode(err, apperr.SchemaField) {
		t.Fatalf("code = %s, error = %v", apperr.CodeOf(err), err)
	}
	var business *apperr.Error
	if !asError(err, &business) || business.Path != "$" {
		t.Fatalf("missing path context: %#v", business)
	}
}

func asError(err error, target **apperr.Error) bool {
	business, ok := err.(*apperr.Error)
	if ok {
		*target = business
	}
	return ok
}
