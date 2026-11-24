package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"terrainnormalverify/internal/engine"
	"terrainnormalverify/internal/projectio"
	"terrainnormalverify/internal/sample"
)

func TestWailsBindingAPIIncrementalAndExport(t *testing.T) {
	dir := t.TempDir()
	projectPath := filepath.Join(dir, "project.json")
	project := sample.Generate(sample.DefaultSeed)
	if err := projectio.Save(projectPath, project); err != nil {
		t.Fatal(err)
	}
	app := &App{
		projectPath: projectPath,
		dbPath:      filepath.Join(dir, "app.sqlite"),
		exportDir:   filepath.Join(dir, "export"),
		budget:      engine.DefaultBudget(),
	}
	defer app.Close()
	state, err := app.Initialize()
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Blocks) != 6 || len(state.Seams) != 4 {
		t.Fatalf("unexpected UI state: blocks=%d seams=%d", len(state.Blocks), len(state.Seams))
	}
	encoded, err := json.Marshal(state.Blocks[0])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(encoded), `"colors":[`) {
		t.Fatalf("Wails color payload must be a numeric JSON array: %s", encoded)
	}
	state, err = app.EditHeightDelta(5, 5, 4, 0.09)
	if err != nil {
		t.Fatal(err)
	}
	if state.LastReport == nil || state.LastReport.Rebaked != 1 {
		t.Fatalf("height edit should rebake one isolated block: %+v", state.LastReport)
	}
	state, err = app.EditMaterial("grass", -0.2, 0.12, 0, 1.1)
	if err != nil {
		t.Fatal(err)
	}
	if state.LastReport.Rebaked != 5 {
		t.Fatalf("material edit should rebake five blocks, got %d", state.LastReport.Rebaked)
	}
	state, err = app.ExportNormals()
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"normalPng", "slopePng"} {
		if _, err := os.Stat(state.ExportPaths[key]); err != nil {
			t.Fatalf("missing export %s: %v", key, err)
		}
	}
}
