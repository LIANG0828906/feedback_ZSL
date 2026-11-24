//go:build wails

package main

import (
	"testing"

	"resource-workbench/internal/workbench"
)

func TestWailsAPIInitializeEditAndCompare(t *testing.T) {
	app := NewApp()
	response := app.Initialize("", "none")
	if !response.OK || response.View == nil || response.Error != nil {
		t.Fatalf("initialize response: %+v", response)
	}
	limit := 0.09
	edited := app.EditZone(workbench.ZoneEditRequest{ZoneID: "zone-1-west", DensityLimit: &limit})
	if !edited.OK || edited.View == nil {
		t.Fatalf("edit response: %+v", edited)
	}
	if len(edited.View.Invalidations) == 0 {
		t.Fatal("missing invalidation trace")
	}
	comparison := app.CompareFull()
	if !comparison.OK || comparison.Comparison == nil || !comparison.Comparison.Equal {
		t.Fatalf("comparison response: %+v", comparison)
	}
	exported := app.ExportReport()
	if !exported.OK || exported.Report == "" {
		t.Fatalf("export response: %+v", exported)
	}
}
