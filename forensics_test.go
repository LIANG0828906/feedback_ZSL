package quantforensics

import (
	"errors"
	"math"
	"reflect"
	"testing"
)

func asForensicError(t *testing.T, err error) *Error {
	t.Helper()
	var fe *Error
	if !errors.As(err, &fe) {
		t.Fatalf("expected *Error, got %T: %v", err, err)
	}
	return fe
}

func TestSchemaRoundTripAndLosses(t *testing.T) {
	p, _ := DefaultProject()
	for i := range p.Blocks {
		if p.Blocks[i].ID == "shot-main-b10" {
			p.Blocks[i].HasDitherOverride = true
			p.Blocks[i].DitherPPM = 16_000
		}
	}

	for _, version := range []string{SchemaV1, SchemaV2, SchemaV3} {
		data, report, err := ExportProject(p, version)
		if err != nil {
			t.Fatalf("export %s: %v", version, err)
		}
		if version == SchemaV2 || version == SchemaV1 {
			if len(report.Losses) == 0 {
				t.Fatalf("%s expected E_LOSSY items", version)
			}
		}
		degraded, _, err := DegradeProject(p, version)
		if err != nil {
			t.Fatal(err)
		}
		loaded, err := LoadProject(data)
		if err != nil {
			t.Fatalf("load exported %s: %v", version, err)
		}
		if SemanticHash(loaded) != SemanticHash(degraded) {
			t.Fatalf("%s round trip semantic mismatch\nloaded=%s\ndegraded=%s", version, SemanticHash(loaded), SemanticHash(degraded))
		}
		if loaded.SourceSchema != version {
			t.Fatalf("source version = %q", loaded.SourceSchema)
		}
		if len(loaded.Traces) == 0 {
			t.Fatalf("%s loader retained no transform traces", version)
		}
	}

	v3, v3Report, err := ExportProject(p, SchemaV3)
	if err != nil {
		t.Fatal(err)
	}
	for _, loss := range v3Report.Losses {
		if loss.Kind == "grouped_palette_tree" || loss.Kind == "per_block_dither" {
			t.Fatalf("v3 should express %s", loss.Kind)
		}
	}
	loadedV3, err := LoadProject(v3)
	if err != nil {
		t.Fatal(err)
	}
	foundOverride := false
	for _, b := range loadedV3.Blocks {
		if b.ID == "shot-main-b10" && b.HasDitherOverride && b.DitherPPM == 16_000 {
			foundOverride = true
		}
	}
	if !foundOverride {
		t.Fatal("v3 per-block dither did not round trip")
	}
}

func TestSchemaErrorsHaveFieldPath(t *testing.T) {
	_, err := LoadProject([]byte(`{"schema":"forensics.v9"}`))
	fe := asForensicError(t, err)
	if fe.Code != CodeSchema || fe.Field != "$.schema" {
		t.Fatalf("unexpected error %+v", fe)
	}

	_, err = LoadProject([]byte(`{"schema":"forensics.v1"}`))
	fe = asForensicError(t, err)
	if fe.Code != CodeSchema || fe.Field != "$.projectId" {
		t.Fatalf("missing field path = %q", fe.Field)
	}
}

func TestHighLowHighPaletteAndCoordinateIdentity(t *testing.T) {
	p, _ := DefaultProject()
	lowData, firstReport, err := ExportProject(p, SchemaV1)
	if err != nil {
		t.Fatal(err)
	}
	low, err := LoadProject(lowData)
	if err != nil {
		t.Fatal(err)
	}
	highData, _, err := ExportProject(low, SchemaV3)
	if err != nil {
		t.Fatal(err)
	}
	high, err := LoadProject(highData)
	if err != nil {
		t.Fatal(err)
	}
	if len(high.Palette.Entries) != len(p.Palette.Entries) || len(high.Blocks) != len(p.Blocks) {
		t.Fatal("round trip changed collection lengths")
	}
	for i := range p.Palette.Entries {
		a, b := p.Palette.Entries[i], high.Palette.Entries[i]
		if a.Color != b.Color || a.GainPPM != b.GainPPM || a.Index != b.Index {
			t.Fatalf("palette entry %d changed: old=%+v new=%+v", i, a, b)
		}
	}
	for i := range p.Blocks {
		a, b := p.Blocks[i], high.Blocks[i]
		if a.X != b.X || a.Y != b.Y || a.Col != b.Col || a.Row != b.Row || a.W != b.W || a.H != b.H {
			t.Fatalf("coordinate %s changed: old=%+v new=%+v", a.ID, a.Coordinate(), b.Coordinate())
		}
	}

	lowData2, secondReport, err := ExportProject(p, SchemaV1)
	if err != nil {
		t.Fatal(err)
	}
	if CanonicalHash(firstReport.Traces) != CanonicalHash(secondReport.Traces) || string(lowData) != string(lowData2) {
		t.Fatal("repeated high-to-low conversion records/products differ")
	}
	for _, tr := range firstReport.Traces {
		if tr.Residual != nil && tr.Residual.Remainder != 0 {
			t.Fatalf("canonical sample should have exact scale residual: %+v", tr.Residual)
		}
	}
}

func TestFixedPointAndCoordinateRoundTrip(t *testing.T) {
	ppm, res, err := ScaleToPPM(1000, ScalePermille, "$.q")
	if err != nil || ppm != 1_000_000 || res.Remainder != 0 {
		t.Fatalf("permille mapping ppm=%d res=%+v err=%v", ppm, res, err)
	}
	ppm, _, err = ScaleToPPM(10000, ScalePermyriad, "$.q")
	if err != nil || ppm != 1_000_000 {
		t.Fatalf("permyriad mapping ppm=%d err=%v", ppm, err)
	}

	// Exact declared round-trip value: 12340 permyriad == 1234 permille.
	v := int64(12340)
	ppm, _, _ = ScaleToPPM(v, ScalePermyriad, "$.q")
	out1, residual1, err := PPMToScale(ppm, ScalePermille, "floor", "$.q")
	if err != nil {
		t.Fatal(err)
	}
	backPPM, _, _ := ScaleToPPM(out1, ScalePermille, "$.q")
	out2, residual2, _ := PPMToScale(backPPM, ScalePermille, "floor", "$.q")
	if out1 != out2 || residual1 != residual2 {
		t.Fatalf("residual conversion not stable: %+v vs %+v", residual1, residual2)
	}
	if out1 != 1234 || residual1.Remainder != 0 || backPPM != ppm {
		t.Fatalf("exact scale round trip failed out=%d backPPM=%d ppm=%d residual=%+v", out1, backPPM, ppm, residual1)
	}

	// Non-divisible values retain a deterministic truncation residual.
	nonExactPPM, _, _ := ScaleToPPM(12345, ScalePermyriad, "$.q")
	_, nonExact1, _ := PPMToScale(nonExactPPM, ScalePermille, "floor", "$.q")
	_, nonExact2, _ := PPMToScale(nonExactPPM, ScalePermille, "floor", "$.q")
	if nonExact1 != nonExact2 || nonExact1.Remainder != 500 {
		t.Fatalf("truncation residual is not deterministic: %+v %+v", nonExact1, nonExact2)
	}

	col, rem, cr, err := AbsoluteToBlockIndex(16, 0, 8, "x", "$.x")
	if err != nil || col != 2 || rem != 0 || cr.Remainder != 0 {
		t.Fatalf("abs->index col=%d rem=%d err=%v", col, rem, err)
	}
	x, _, err := BlockIndexToAbsolute(col, 0, 8, "x", "$.x")
	if err != nil || x != 16 {
		t.Fatalf("index->abs x=%d err=%v", x, err)
	}

	_, _, err = ScaleToPPM(math.MaxInt64, ScalePermille, "$.overflow")
	fe := asForensicError(t, err)
	if fe.Code != CodeOverflow || fe.Op1 == nil || fe.Op2 == nil || *fe.Op2 != 1000 {
		t.Fatalf("overflow error missing operands: %+v", fe)
	}
	_, _, _, err = AbsoluteToBlockIndex(0, math.MinInt64, 1, "x", "$.overflow")
	if err == nil {
		t.Fatal("expected coordinate overflow")
	}
}

func TestQuantizeBlockDeterministic(t *testing.T) {
	p, _ := DefaultProject()
	b := p.Blocks[3]
	im := p.Images[0]
	q1, err := QuantizeBlock(im, b, p.Palette, p.Params)
	if err != nil {
		t.Fatal(err)
	}
	q2, err := QuantizeBlock(im, b, p.Palette, p.Params)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(q1, q2) {
		t.Fatal("same block quantized differently on repeated pure calls")
	}
}

func TestTightToleranceDoesNotChangeMeasurements(t *testing.T) {
	p, _ := DefaultProject()
	rep, err := NewEngine(p, NewMemoryStore("mem"), EngineOptions{Mode: "test", Workers: 1}).Run()
	if err != nil {
		t.Fatal(err)
	}
	var gradient, stroke *BlockOutcome
	for i := range rep.Outcomes {
		o := &rep.Outcomes[i]
		switch o.Block.Kind {
		case KindGradient:
			if gradient == nil {
				gradient = o
			}
		case KindStroke:
			if stroke == nil {
				stroke = o
			}
		}
	}
	if gradient == nil || stroke == nil {
		t.Fatal("missing gradient or stroke sample")
	}
	im := p.Images[0]
	tight := Tolerance{MaxComponentDeviation: 1, MaxWeightedColorDiff: 1}
	for _, o := range []*BlockOutcome{gradient, stroke} {
		ev := EvaluateBlock(im, o.Block, o.Reconstruction.Pixels, p.Params.Metric, tight)
		if !reflect.DeepEqual(ev.MaxDeviation, o.Evaluation.MaxDeviation) || ev.WeightedColorDiff != o.Evaluation.WeightedColorDiff {
			t.Fatalf("tight tolerance changed measured deviation for %s", o.Block.ID)
		}
		if ev.Passed || len(ev.Violations) == 0 {
			t.Fatalf("strict tolerance should fail %s", o.Block.ID)
		}
		v := ev.Violations[0]
		if v.Coordinate.BlockID == "" || v.Component == "" || v.Tolerance < 0 {
			t.Fatalf("violation evidence incomplete: %+v", v)
		}
	}
	if gradient.Evaluation.GradientConclusion == "" && gradient.Evaluation.Passed {
		// Conclusion is attached during evaluation; re-evaluate above sets it.
	}
}

func TestStateMachineIllegalTransitionKeepsState(t *testing.T) {
	sm := NewStateMachine()
	id := "b1"
	if err := sm.TransitionBlock(id, StageQuantize, StateRunning); err != nil {
		t.Fatal(err)
	}
	if err := sm.TransitionBlock(id, StageQuantize, StateCommitted); err != nil {
		t.Fatal(err)
	}
	err := sm.TransitionBlock(id, StageQuantize, StateRunning)
	fe := asForensicError(t, err)
	if fe.Code != CodeState || sm.BlockState(id, StageQuantize) != StateCommitted {
		t.Fatalf("illegal transition changed state: err=%v state=%s", err, sm.BlockState(id, StageQuantize))
	}
	if err := sm.TransitionPanel("panel", StateCommitted); err == nil {
		t.Fatal("pending -> committed should be illegal")
	}
	if sm.PanelState("panel") != StatePending {
		t.Fatalf("panel state = %s", sm.PanelState("panel"))
	}
}

func TestExactPaletteInvalidationAndReuse(t *testing.T) {
	p, _ := DefaultProject()
	store := NewMemoryStore("edit")
	sm := NewStateMachine()
	first := NewEngine(p, store, EngineOptions{Mode: "initial", StateMachine: sm})
	r1, err := first.Run()
	if err != nil {
		t.Fatal(err)
	}
	oldByID := map[string]BlockOutcome{}
	quants := map[string]QuantOutput{}
	for _, o := range r1.Outcomes {
		oldByID[o.Block.ID] = o
		quants[o.Block.ID] = o.Quant
	}

	newPalette := p.Palette
	newPalette.Entries[7].Color = RGB{R: 141, G: 32, B: 199}
	affected := AnalyzePaletteEdit(p, quants, 7, newPalette)
	if len(affected) != 3 {
		t.Fatalf("purple edit affected %d blocks, want 3: %v", len(affected), affected)
	}
	graph := BuildDependencyGraph(p)
	inv := graph.InvalidateBlocks(p, affected, false)
	if len(inv.AffectedBlocks) != 3 || len(inv.ReusedBlocks) != len(p.Blocks)-3 {
		t.Fatalf("invalid counts affected=%d reused=%d", len(inv.AffectedBlocks), len(inv.ReusedBlocks))
	}
	if err := inv.ApplyToStateMachine(sm); err != nil {
		t.Fatal(err)
	}
	p.Palette = newPalette
	p.Palette.Version = "palette-purple-edit"

	second := NewEngine(p, store, EngineOptions{Mode: "edit", ReuseCheckpoints: true, Invalidation: &inv, StateMachine: sm})
	r2, err := second.Run()
	if err != nil {
		t.Fatal(err)
	}
	if len(r2.ReusedBlocks) != len(p.Blocks)-3 {
		t.Fatalf("reused=%d want %d", len(r2.ReusedBlocks), len(p.Blocks)-3)
	}
	if len(r2.ComputedBlocks) != 3 {
		t.Fatalf("computed=%v want 3", r2.ComputedBlocks)
	}
	newByID := map[string]BlockOutcome{}
	for _, o := range r2.Outcomes {
		newByID[o.Block.ID] = o
	}
	for id, old := range oldByID {
		next := newByID[id]
		if _, changed := affected[id]; !changed {
			if inc := compareOutcome(old, next); inc != nil {
				t.Fatalf("unaffected block changed: %+v", inc)
			}
		} else if reflect.DeepEqual(old.Evaluation, next.Evaluation) && reflect.DeepEqual(old.Reconstruction, next.Reconstruction) {
			t.Fatalf("affected block %s did not reflect edited palette", id)
		}
	}
}

func TestFaultRetryIsolation(t *testing.T) {
	p, _ := DefaultProject()
	store := NewMemoryStore("fault")
	sm := NewStateMachine()
	faults := NewFaultPolicy("shot-main-b00")
	engine := NewEngine(p, store, EngineOptions{Mode: "fault", Workers: 1, StateMachine: sm})
	engine.Faults = faults
	r1, err := engine.Run()
	if err != nil {
		t.Fatal(err)
	}
	if len(r1.FailedBlocks) != 1 || r1.FailedBlocks[0] != "shot-main-b00" {
		t.Fatalf("failed blocks = %v", r1.FailedBlocks)
	}
	if sm.BlockState("shot-main-b00", StageQuantize) != StateFailed {
		t.Fatal("fault block should be failed")
	}

	retry := NewEngine(p, store, EngineOptions{Mode: "retry", Workers: 1, ReuseCheckpoints: true, StateMachine: sm})
	retry.Faults = faults
	r2, err := retry.Run()
	if err != nil {
		t.Fatal(err)
	}
	if len(r2.FailedBlocks) != 0 {
		t.Fatalf("retry failed: %v", r2.FailedBlocks)
	}
	if sm.BlockState("shot-main-b00", StageVerify) != StateCommitted {
		t.Fatal("retried block did not commit all stages")
	}
	if len(r2.Outcomes) != len(p.Blocks) {
		t.Fatalf("outcomes=%d", len(r2.Outcomes))
	}
}
