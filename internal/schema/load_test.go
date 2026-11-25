package schema_test

import (
	"path/filepath"
	"testing"

	"multiplexgw/internal/model"
	"multiplexgw/internal/sample"
	"multiplexgw/internal/schema"
)

func TestThreeSchemaVersionsCanonicalizeEqually(t *testing.T) {
	dir := t.TempDir()
	bundle, err := sample.Build(sample.DefaultSeed)
	if err != nil {
		t.Fatal(err)
	}
	if err := bundle.Write(dir); err != nil {
		t.Fatal(err)
	}
	names := []string{"project-v1.json", "project-v2.json", "project-v3.json"}
	var projects []*model.Project
	for _, name := range names {
		p, err := schema.LoadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("load %s: %v", name, err)
		}
		projects = append(projects, p)
	}
	for i := 1; i < len(projects); i++ {
		if diffs := schema.CompareCanonical(projects[0], projects[i]); len(diffs) > 0 {
			t.Fatalf("v%d differs from v1: %v", i+1, diffs)
		}
	}
}
