package inject_test

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"quotaflow/internal/domain"
	"quotaflow/internal/generator"
	"quotaflow/internal/inject"
	"quotaflow/internal/schema"
)

func TestProjectInjectionsReturnStableCodesAndDoNotMutateSource(t *testing.T) {
	dir := t.TempDir()
	if _, _, err := generator.Generate(42, dir); err != nil {
		t.Fatalf("generate: %v", err)
	}
	tests := []struct {
		kind string
		file string
		code string
	}{
		{inject.NegativeQuota, "project-v2.json", domain.ENegativeQuota},
		{inject.ReserveOverQuota, "project-v3.json", domain.EReserveExceedsQuota},
		{inject.FaultDomainCycle, "project-v2.json", domain.EFaultDomainCycle},
		{inject.SnapshotVersion, "project-v1.json", domain.ESchemaVersion},
		{inject.UnknownUnitField, "project-v1.json", domain.ESchemaMixedUnits},
	}
	for _, test := range tests {
		t.Run(test.kind, func(t *testing.T) {
			path := filepath.Join(dir, test.file)
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			mutated, err := inject.MutateProject(test.kind, before)
			if err != nil {
				t.Fatalf("mutate: %v", err)
			}
			if _, err := schema.LoadBytes(mutated, "injected"); domain.CodeOf(err) != test.code {
				t.Fatalf("got %v (%s), want %s", err, domain.CodeOf(err), test.code)
			}
			after, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read after: %v", err)
			}
			if !bytes.Equal(before, after) {
				t.Fatal("source project changed during injection")
			}
		})
	}
}
