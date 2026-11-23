package terrainbake

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDiagnosticsWritesAllViews(t *testing.T) {
	p, _ := GenerateProject(71)
	out, res := fullOutputs(t, p, func(o *EngineOptions) { o.MaxWorkers = 2 })
	diff := &DiffReport{
		Equal:   false,
		BlockID: "tl",
		X:       2,
		Y:       3,
		Field:   "normal_slope",
	}
	dir := t.TempDir()
	path, err := WriteDiagnostics(dir, DiagnosticInput{
		Title: "test report", Project: p, Result: res, Outputs: out, Diff: diff,
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{
		"diagnostics.html", "normals-atlas.png", "blocks-colored.png",
		"seam-heatmap.png", "diff-heatmap.png",
	} {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil || info.Size() == 0 {
			t.Fatalf("%s missing or empty: %v", name, err)
		}
	}
	html, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, needle := range []string{"Block shading preview", "Seam angle heatmap", "Difference heatmap", "Invalidation propagation"} {
		if !strings.Contains(string(html), needle) {
			t.Fatalf("report missing %q", needle)
		}
	}
}
