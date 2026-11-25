package vpcompose_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"vpcompose"
)

func TestThreePathsPixelAndCommitAgreement(t *testing.T) {
	original := loadProjectForTest(t, 40)
	dir := t.TempDir()

	realtime, err := vpcompose.NewEngine(original, mustStore(t, filepath.Join(dir, "realtime.db")), vpcompose.EngineOptions{Workers: 3})
	if err != nil {
		t.Fatal(err)
	}
	applyCommonEdit(t, realtime)
	if _, err := realtime.Run(context.Background(), vpcompose.ModeRealtime); err != nil {
		t.Fatal(err)
	}

	recoverStore := mustStore(t, filepath.Join(dir, "recover.db"))
	recoverEngine, err := vpcompose.NewEngine(original, recoverStore, vpcompose.EngineOptions{Workers: 2})
	if err != nil {
		t.Fatal(err)
	}
	applyCommonEdit(t, recoverEngine)
	editedProject := recoverEngine.Project()
	if _, err := recoverEngine.ComposeWindow(context.Background(), 0); err != nil {
		t.Fatal(err)
	}
	if err := recoverStore.Close(); err != nil {
		t.Fatal(err)
	}
	recoverStore, err = vpcompose.OpenCheckpoint(filepath.Join(dir, "recover.db"))
	if err != nil {
		t.Fatal(err)
	}
	recovered, err := vpcompose.NewEngine(editedProject, recoverStore, vpcompose.EngineOptions{Workers: 2})
	if err != nil {
		t.Fatal(err)
	}
	recoverReport, err := recovered.Run(context.Background(), vpcompose.ModeRecover)
	if err != nil {
		t.Fatal(err)
	}
	if !equalInts(recoverReport.Recovery.Committed, []int{0}) {
		t.Fatalf("pre-run recovery = %v", recoverReport.Recovery.Committed)
	}

	fullStore := mustStore(t, filepath.Join(dir, "full.db"))
	full, err := vpcompose.NewEngine(editedProject, fullStore, vpcompose.EngineOptions{Workers: 1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := full.Run(context.Background(), vpcompose.ModeFull); err != nil {
		t.Fatal(err)
	}

	runs := []*vpcompose.ComparableRun{}
	for name, engine := range map[string]*vpcompose.Engine{
		"realtime": realtime, "recover": recovered, "full": full,
	} {
		comparable, err := engine.Comparable(name)
		if err != nil {
			t.Fatal(err)
		}
		runs = append(runs, comparable)
	}
	if err := vpcompose.CompareRuns(runs...); err != nil {
		t.Fatalf("three paths differ: %v", err)
	}

	corrupted, err := recovered.Comparable("corrupted")
	if err != nil {
		t.Fatal(err)
	}
	corrupted.Results[1].Frames[0].Pixels[0]++
	err = vpcompose.CompareRuns(runs[0], corrupted)
	if !vpcompose.IsCode(err, vpcompose.CodeDiff) {
		t.Fatalf("expected E_DIFF, got %v", err)
	}
	ee := err.(*vpcompose.EngineError)
	if ee.Window != 1 || !strings.Contains(ee.Field, "frames[0].pixels[0]") {
		t.Fatalf("diff location = window %d field %s", ee.Window, ee.Field)
	}
}

func applyCommonEdit(t *testing.T, engine *vpcompose.Engine) {
	t.Helper()
	blend := vpcompose.BlendAdd
	if _, err := engine.EditLayer(vpcompose.LayerEdit{LayerID: "talent-subject", BlendMode: &blend}); err != nil {
		t.Fatal(err)
	}
}

func mustStore(t *testing.T, path string) *vpcompose.CheckpointStore {
	t.Helper()
	store, err := vpcompose.OpenCheckpoint(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}
