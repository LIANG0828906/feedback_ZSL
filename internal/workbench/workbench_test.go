package workbench

import (
	"path/filepath"
	"testing"

	"volumediag/internal/engine"
	"volumediag/internal/model"
)

func TestWorkbenchDiagnosticFlow(t *testing.T) {
	wb := New()
	sceneDir := filepath.Join("..", "..", "artifacts", "sample")
	dbPath := filepath.Join(t.TempDir(), "gui-checkpoint.db")
	boot := wb.Bootstrap(sceneDir, dbPath, 20261124)
	if !boot.OK || boot.View == nil {
		t.Fatalf("bootstrap failed: %+v", boot)
	}
	if !boot.View.State.Complete || boot.View.State.CacheCount != boot.View.State.TotalBlocks {
		t.Fatalf("bootstrap did not resume/full render: %+v", boot.View.State)
	}
	if len(boot.View.Image.Volume) != boot.View.State.Viewport.Width*boot.View.State.Viewport.Height*3 {
		t.Fatalf("bad image buffer length %d", len(boot.View.Image.Volume))
	}
	truth := wb.VerifyTruth()
	if !truth.OK || truth.View.LastTruth == nil || !truth.View.LastTruth.Equal {
		t.Fatalf("truth verification failed: %+v", truth)
	}
	cmp := wb.Compare()
	if !cmp.OK || cmp.View.LastCompare == nil || !cmp.View.LastCompare.Equal {
		t.Fatalf("compare failed: %+v", cmp)
	}
	value := model.C3(5.9, 4.1, 2.7)
	edit := wb.ApplyEdit(engine.EditRequest{LightID: "point_key", Intensity: &value})
	if !edit.OK || edit.View.LastEdit == nil {
		t.Fatalf("edit failed: %+v", edit)
	}
	if edit.View.LastEdit.AffectedBlocks == 0 || edit.View.LastEdit.AffectedBlocks >= 48 {
		t.Fatalf("unexpected invalidation range: %+v", edit.View.LastEdit)
	}
	if edit.View.LastReport == nil || !edit.View.LastReport.Complete {
		t.Fatalf("incremental render did not complete: %+v", edit.View.LastReport)
	}
	detail := wb.Block(edit.View.LastEdit.Invalidated[0].BlockX, edit.View.LastEdit.Invalidated[0].BlockY)
	if !detail.Found || len(detail.Pixels) == 0 || len(detail.Composite) == 0 {
		t.Fatalf("block detail unavailable: %+v", detail)
	}
	if got := wb.LightCoverage("point_key"); len(got) == 0 {
		t.Fatal("light influence overlay is empty")
	}
}
