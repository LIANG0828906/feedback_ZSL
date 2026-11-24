package project_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"mediaforge/internal/errs"
	"mediaforge/internal/project"
	"mediaforge/internal/samplegen"
)

func sampleProject(t *testing.T) (string, string) {
	t.Helper()
	root := t.TempDir()
	if _, err := samplegen.Generate(samplegen.Options{OutDir: root, Seed: 7, Profile: "small"}); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "arcade-hud", "project.json")
	return path, root
}

func TestValidationErrorCodes(t *testing.T) {
	path, _ := sampleProject(t)
	original, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name   string
		mutate func(map[string]any)
		code   errs.Code
		field  string
	}{
		{
			name: "parameter range",
			mutate: func(p map[string]any) {
				p["baseRecipe"].(map[string]any)["imageBitDepth"] = 7
			},
			code:  errs.EPara,
			field: "baseRecipe.imageBitDepth",
		},
		{
			name: "source hash",
			mutate: func(p map[string]any) {
				p["sources"].([]any)[0].(map[string]any)["sha256"] = "0000000000000000000000000000000000000000000000000000000000000000"
			},
			code:  errs.EDecode,
			field: "sources[0].sha256",
		},
		{
			name: "operation target",
			mutate: func(p map[string]any) {
				p["operations"].([]any)[0].(map[string]any)["variant"] = "missing"
			},
			code:  errs.EDecode,
			field: "operations[0].variant",
		},
		{
			name: "operation timestamp",
			mutate: func(p map[string]any) {
				ops := p["operations"].([]any)
				ops[1].(map[string]any)["time"] = ops[0].(map[string]any)["time"]
			},
			code:  errs.EDecode,
			field: "operations[1].time",
		},
		{
			name: "operation parameter",
			mutate: func(p map[string]any) {
				ops := p["operations"].([]any)
				ops[3].(map[string]any)["value"] = 1
			},
			code:  errs.EPara,
			field: "operations[3].value.targetWidth",
		},
		{
			name: "truth deletion",
			mutate: func(p map[string]any) {
				p["truth"].(map[string]any)["variants"] = []any{}
			},
			code:  errs.EDecode,
			field: "truth.variants",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var p map[string]any
			if err := json.Unmarshal(original, &p); err != nil {
				t.Fatal(err)
			}
			tc.mutate(p)
			raw, _ := json.Marshal(p)
			if err := os.WriteFile(path, raw, 0o644); err != nil {
				t.Fatal(err)
			}
			loaded, err := project.Load(path)
			if err == nil {
				_, err = project.Validate(loaded, filepath.Dir(path))
			}
			if errs.CodeOf(err) != tc.code {
				t.Fatalf("code = %s, want %s (%v)", errs.CodeOf(err), tc.code, err)
			}
			if got := errs.As(err).Field; got != tc.field {
				t.Fatalf("field = %q, want %q", got, tc.field)
			}
		})
	}
}
