package schema_test

import (
	"strings"
	"testing"

	"wardrobekernel/internal/samplegen"
	"wardrobekernel/internal/schema"
	"wardrobekernel/internal/xerr"
)

func TestValidGeneratedProject(t *testing.T) {
	project, err := samplegen.Generate(42)
	if err != nil {
		t.Fatal(err)
	}
	if err := schema.ValidateProject(project); err != nil {
		t.Fatalf("generated project must be valid: %v", err)
	}
}

func TestSchemaViolationsCarryPartAndFieldPath(t *testing.T) {
	project := samplegen.InvalidSchemaProject()
	err := schema.ValidateProject(project)
	coded, ok := xerr.AsError(err)
	if !ok || coded.Code != xerr.ESchema {
		t.Fatalf("expected E_SCHEMA, got %T %v", err, err)
	}
	if len(coded.Violations) < 5 {
		t.Fatalf("expected multiple field violations, got %#v", coded.Violations)
	}

	joined := ""
	hasPartPath := false
	for _, v := range coded.Violations {
		joined += v.FieldPath + "\n"
		if v.PartID == "p_bad_ref" && strings.Contains(v.FieldPath, "parts[0]") {
			hasPartPath = true
		}
	}
	if !hasPartPath {
		t.Fatalf("expected part-scoped field path, got %#v", coded.Violations)
	}
	for _, want := range []string{"attach_points", "vertex_start", "index_start", "mutex_rules", "max_vertex_budget"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("violations did not include %q:\n%s", want, joined)
		}
	}
}
