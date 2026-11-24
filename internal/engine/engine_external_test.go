package engine_test

import (
	"path/filepath"
	"testing"

	"terrainnormalverify/internal/checkpoint"
	"terrainnormalverify/internal/compare"
	"terrainnormalverify/internal/engine"
	"terrainnormalverify/internal/model"
	"terrainnormalverify/internal/sample"
)

func TestSnapshotDiffReportsFirstPixel(t *testing.T) {
	project := sample.Generate(sample.DefaultSeed)
	store, err := checkpoint.Open(filepath.Join(t.TempDir(), "diff.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	runner, err := engine.New(project, store, engine.Config{Budget: engine.DefaultBudget()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := runner.Initialize(); err != nil {
		t.Fatal(err)
	}
	left := runner.Snapshot()
	right := runner.Snapshot()
	right.Blocks[1].Normals[0] = model.Vec3{X: 1, Y: 0, Z: 0}
	diff, err := compare.Snapshots(left, right)
	if err != nil {
		t.Fatal(err)
	}
	if diff == nil || diff.BlockID != 1 || diff.Pixel != 0 || diff.Field != "normal" {
		t.Fatalf("unexpected first difference: %+v", diff)
	}
	typed := compare.AsError(diff)
	if typed.Code != model.ErrDiff {
		t.Fatalf("expected E_DIFF, got %s", typed.Code)
	}
}
