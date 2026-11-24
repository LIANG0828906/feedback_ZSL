package fsm_test

import (
	"testing"

	"ocrworkbench/internal/core"
	"ocrworkbench/internal/fsm"
)

func TestIllegalTransitionKeepsOriginalState(t *testing.T) {
	tile := fsm.NewTile(3)
	if err := tile.Transition(fsm.TileCommitted); err == nil {
		t.Fatal("pending -> committed must be illegal")
	}
	if tile.PhaseValue() != fsm.TilePending {
		t.Fatalf("state changed to %s", tile.PhaseValue())
	}
	if err := tile.Transition(fsm.TileDetecting); err != nil {
		t.Fatal(err)
	}
	if err := tile.Transition(fsm.TileFailed); err != nil {
		t.Fatal(err)
	}
	if err := tile.Transition(fsm.TilePending); err != nil {
		t.Fatal(err)
	}
	if err := tile.Transition(fsm.TileDetecting); err != nil {
		t.Fatal(err)
	}
	if err := tile.Transition(fsm.TileCommitted); err != nil {
		t.Fatal(err)
	}
	if err := tile.Transition(fsm.TilePending); !core.IsCode(err, core.CodeState) {
		t.Fatalf("committed tile must be frozen, got %v", err)
	}
}

func TestLineCommitFreezesParamsAndResult(t *testing.T) {
	params := core.DefaultSegmentParams()
	box := core.AxisBox(core.Rect{X: 1, Y: 2, Width: 30, Height: 12})
	line := fsm.NewLineRuntime(0, params, box)
	if err := line.Transition(fsm.LineRecognizing); err != nil {
		t.Fatal(err)
	}
	result := core.LineResult{Text: "ABC", Confidence: 0.8, Characters: []core.CharResult{{Text: "A", Confidence: 0.8}}}
	if err := line.Commit(result); err != nil {
		t.Fatal(err)
	}
	changed := params
	changed.MaxCharWidth = 128
	applied, err := line.ApplyParams(changed)
	if err != nil {
		t.Fatal(err)
	}
	if applied {
		t.Fatal("committed line accepted a parameter change")
	}
	view := line.View()
	if view.Result == nil || view.Result.Text != "ABC" || view.Params.MaxCharWidth != params.MaxCharWidth || view.Box != box {
		t.Fatalf("committed line changed: %+v", view)
	}
	view.Result.Characters[0].Text = "X"
	if line.View().Result.Characters[0].Text != "A" {
		t.Fatal("LineView leaked mutable character storage")
	}
}

func TestProgressCountsCommittedUnits(t *testing.T) {
	progress := fsm.NewProgress()
	tile := progress.AddTile(0)
	progress.AddTile(1)
	line := progress.AddLine(0)
	_ = tile.Transition(fsm.TileDetecting)
	_ = tile.Transition(fsm.TileCommitted)
	_ = line.Transition(fsm.LineRecognizing)
	_ = line.Transition(fsm.LineCommitted)
	tiles, totalTiles, lines, totalLines := progress.Counts()
	if tiles != 1 || totalTiles != 2 || lines != 1 || totalLines != 1 {
		t.Fatalf("unexpected counts %d/%d %d/%d", tiles, totalTiles, lines, totalLines)
	}
}

func TestLineFailureTransitionIsReversible(t *testing.T) {
	line := fsm.NewLine(0)
	_ = line.Transition(fsm.LineRecognizing)
	_ = line.Transition(fsm.LineFailed)
	if line.PhaseValue() != fsm.LineFailed {
		t.Fatal("line should be failed")
	}
	if err := line.Transition(fsm.LineCommitted); !core.IsCode(err, core.CodeState) {
		t.Fatalf("unexpected error type: %v", err)
	}
	if line.PhaseValue() != fsm.LineFailed {
		t.Fatal("failed -> committed must not be legal")
	}
	if err := line.Transition(fsm.LinePending); err != nil {
		t.Fatal(err)
	}
}
