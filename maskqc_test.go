package maskqc

import (
	"bytes"
	"math"
	"testing"
)

func TestSampleGenerationDeterministicAndSchemaRoundTrip(t *testing.T) {
	a, err := GenerateSamples(DefaultSeed)
	if err != nil {
		t.Fatal(err)
	}
	b, err := GenerateSamples(DefaultSeed)
	if err != nil {
		t.Fatal(err)
	}
	if len(a.Samples) != 3 || len(b.Samples) != 3 {
		t.Fatalf("expected three samples, got %d/%d", len(a.Samples), len(b.Samples))
	}
	for i := range a.Samples {
		pa, pb := a.Samples[i].Project, b.Samples[i].Project
		if pa.Candidate.Hash() != pb.Candidate.Hash() || pa.Truth.Mask.Hash() != pb.Truth.Mask.Hash() {
			t.Fatalf("sample %s is not deterministic", pa.ID)
		}
		for _, version := range []string{SchemaV1, SchemaV2, SchemaV3} {
			la := a.Samples[i].Loaded[version]
			lb := b.Samples[i].Loaded[version]
			if la == nil || lb == nil {
				t.Fatalf("version %s was not loaded", version)
			}
			if len(la.Strokes) != len(pa.Strokes) {
				t.Fatalf("%s stroke count mismatch", version)
			}
			for j := range pa.Strokes {
				want := pa.Strokes[j]
				got := la.Strokes[j]
				if want.ID != got.ID || want.Seq != got.Seq || want.Start != got.Start || want.End != got.End || want.WeightMyriad != got.WeightMyriad {
					t.Fatalf("%s core roundtrip mismatch: want %+v got %+v", version, want, got)
				}
			}
		}
		v1 := a.Samples[i].Loaded[SchemaV1]
		if len(v1.Groups) != len(pa.Groups) {
			t.Fatalf("v1 sidecar did not restore groups: %d vs %d", len(v1.Groups), len(pa.Groups))
		}
	}
}

func TestSchemaErrors(t *testing.T) {
	if _, err := LoadProject([]byte(`{"schema":"nope","id":"x"}`)); !IsCode(err, CodeSchema) {
		t.Fatalf("expected E_SCHEMA unknown version, got %v", err)
	}
	_, err := LoadProject([]byte(`{"schema":"` + SchemaV3 + `"}`))
	if !IsCode(err, CodeSchema) {
		t.Fatalf("expected missing id E_SCHEMA, got %v", err)
	}
	if be, _ := AsBizError(err); be.Path != "$.id" {
		t.Fatalf("expected $.id path, got %q", be.Path)
	}
}

func TestLossyExportKeepsExactCoreWithAuditSidecar(t *testing.T) {
	p := &Project{
		ID: "lossy", Width: 16, Height: 16, TileWidth: 16, TileHeight: 16,
		Strokes: []Stroke{{
			ID: "s", Seq: 1, Priority: 1, Start: Point{X: 1, Y: 1}, End: Point{X: 12, Y: 12},
			Width: 3, HardnessPct: 40, WeightMyriad: 1234, DirectionClass: DirMixed,
		}},
		Groups: []StrokeGroup{{ID: "g", Kind: "special", StrokeIDs: []string{"s"}}},
	}
	for _, version := range []string{SchemaV1, SchemaV2} {
		res, err := ExportProject(p, version)
		if err != nil {
			t.Fatal(err)
		}
		loaded, err := LoadProject(res.Data)
		if err != nil {
			t.Fatalf("%s load: %v", version, err)
		}
		if loaded.Strokes[0].WeightMyriad != 1234 {
			t.Fatalf("%s weight not restored: %d", version, loaded.Strokes[0].WeightMyriad)
		}
		if loaded.Strokes[0].Start != p.Strokes[0].Start || loaded.Strokes[0].End != p.Strokes[0].End {
			t.Fatalf("%s coordinates changed", version)
		}
		if loaded.Strokes[0].DirectionClass != DirMixed || len(loaded.Groups) != 1 {
			t.Fatalf("%s lossy sidecar not restored: dir=%s groups=%d", version, loaded.Strokes[0].DirectionClass, len(loaded.Groups))
		}
		if version == SchemaV1 && len(res.Lossy) < 2 {
			t.Fatalf("expected direction and group E_LOSSY items, got %+v", res.Lossy)
		}
	}
}

func TestIntegerConversions(t *testing.T) {
	q, rem, tr := MyriadToPermille(1234)
	if q != 123 || rem != 4 || tr.Remainder != 4 {
		t.Fatalf("unexpected weight conversion q=%d rem=%d trace=%+v", q, rem, tr)
	}
	restored, err := RestoreMyriad(q, rem)
	if err != nil || restored != 1234 {
		t.Fatalf("restore failed: %d %v", restored, err)
	}
	start, end, _, err := AbsoluteToOffsets(Point{X: 40, Y: 50}, Point{X: 70, Y: 80}, 3, "s3", DefaultCoordinateRule)
	if err != nil {
		t.Fatal(err)
	}
	backStart, backEnd, _, err := OffsetsToAbsolute(start, end, 3, "s3", DefaultCoordinateRule)
	if err != nil || backStart != (Point{X: 40, Y: 50}) || backEnd != (Point{X: 70, Y: 80}) {
		t.Fatalf("coordinate roundtrip failed: %+v %+v %v", backStart, backEnd, err)
	}
	if _, err := CheckedMul64(math.MaxInt64, 2, "overflow", "obj"); !IsCode(err, CodeOVF) {
		t.Fatalf("expected E_OVF, got %v", err)
	}
}

func TestRasterDeterminismAndParameters(t *testing.T) {
	stroke := Stroke{ID: "s", Seq: 1, Start: Point{X: 2, Y: 2}, End: Point{X: 12, Y: 9}, Width: 5, HardnessPct: 70, WeightMyriad: 7500}
	chunk := Chunk{ID: 0, X: 0, Y: 0, Width: 16, Height: 16}
	a, err := RasterStrokeChunk(stroke, chunk)
	if err != nil {
		t.Fatal(err)
	}
	b, err := RasterStrokeChunk(stroke, chunk)
	if err != nil || !bytes.Equal(a, b) {
		t.Fatal("same stroke parameters must produce identical coverage")
	}
	bad := stroke
	bad.HardnessPct = 101
	if _, err := RasterStrokeChunk(bad, chunk); !IsCode(err, CodePara) {
		t.Fatalf("expected E_PARA hardness, got %v", err)
	}
	bad = stroke
	bad.WeightMyriad = 10001
	if _, err := RasterStrokeChunk(bad, chunk); !IsCode(err, CodePara) {
		t.Fatalf("expected E_PARA weight, got %v", err)
	}
}

func TestAnalyzeKnownSquare(t *testing.T) {
	m := NewGrayMask(16, 16)
	for y := 3; y <= 9; y++ {
		for x := 4; x <= 10; x++ {
			m.Set(x, y, 255)
		}
	}
	report := AnalyzeMask(m, AnalyzeOptions{})
	if len(report.Domains) != 1 {
		t.Fatalf("expected one domain, got %d", len(report.Domains))
	}
	d := report.Domains[0]
	if d.Area != 7*7 || d.Holes != 0 || d.BBox != (BBox{MinX: 4, MinY: 3, MaxX: 10, MaxY: 9}) {
		t.Fatalf("unexpected square stats: %+v", d)
	}
}

func TestOrderIsIndependentOfInputSliceOrder(t *testing.T) {
	set, err := GenerateSamples(DefaultSeed)
	if err != nil {
		t.Fatal(err)
	}
	p := set.Samples[0].Project
	g := NewChunkGrid(p.Width, p.Height, p.TileWidth, p.TileHeight)
	a := PlanOrders(p, g)
	reversed := CloneProject(p)
	for i, j := 0, len(reversed.Strokes)-1; i < j; i, j = i+1, j-1 {
		reversed.Strokes[i], reversed.Strokes[j] = reversed.Strokes[j], reversed.Strokes[i]
	}
	b := PlanOrders(reversed, g)
	for cid := range a {
		if !stringsEqual(a[cid].StrokeIDs, b[cid].StrokeIDs) {
			t.Fatalf("chunk %d order changed with input order", cid)
		}
	}
}

func TestPreciseInvalidationScope(t *testing.T) {
	p := &Project{
		ID: "tiny", Width: 32, Height: 32, TileWidth: 16, TileHeight: 16,
		Strokes: []Stroke{
			{ID: "s1", Seq: 1, Priority: 1, Start: Point{X: 2, Y: 2}, End: Point{X: 5, Y: 5}, Width: 3, HardnessPct: 80, WeightMyriad: 8000},
			{ID: "s2", Seq: 2, Priority: 2, Start: Point{X: 22, Y: 22}, End: Point{X: 28, Y: 28}, Width: 3, HardnessPct: 80, WeightMyriad: 8000},
		},
	}
	g := NewChunkGrid(p.Width, p.Height, p.TileWidth, p.TileHeight)
	edited := CloneProject(p)
	edited.Strokes[0].End.X = 7
	plan := PlanEditInvalidation(p, edited, g, []Edit{{Type: "update_end", StrokeID: "s1"}})
	if !sameIntSet(plan.InvalidChunks, []int{0}) || !sameIntSet(plan.ReusableChunks, []int{1, 2, 3}) {
		t.Fatalf("unexpected coordinate invalidation: invalid=%v reusable=%v", plan.InvalidChunks, plan.ReusableChunks)
	}

	inserted := CloneProject(p)
	inserted.Strokes = append(inserted.Strokes, Stroke{ID: "s4", Seq: 4, Priority: 4, Start: Point{X: 20, Y: 20}, End: Point{X: 24, Y: 24}, Width: 3, HardnessPct: 80, WeightMyriad: 8000})
	plan = PlanEditInvalidation(p, inserted, g, []Edit{{Type: "insert", StrokeID: "s4"}})
	if !sameIntSet(plan.InvalidChunks, []int{3}) {
		t.Fatalf("insert should invalidate only chunk 3, got %v", plan.InvalidChunks)
	}
	if len(plan.Items[0].PropagationPath) < 3 {
		t.Fatalf("missing propagation path: %+v", plan.Items[0])
	}
}

func TestCheckpointStateMachine(t *testing.T) {
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.TransitionStage(1, PhaseCoverage, StageCommitted, "jump"); !IsCode(err, CodeState) {
		t.Fatalf("expected E_STATE, got %v", err)
	}
	if got := store.StageState(1, PhaseCoverage); got != StagePending {
		t.Fatalf("failed transition must preserve pending, got %s", got)
	}
	if _, err := store.TransitionStage(1, PhaseCoverage, StageProcessing, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := store.TransitionStage(1, PhaseCoverage, StageCommitted, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := store.TransitionStage(1, PhaseCoverage, StageProcessing, "new invalidation generation"); err != nil {
		t.Fatalf("committed->processing should be allowed for recompute: %v", err)
	}
}

func TestSchedulerBlocksOnBudgetAndRecordsWait(t *testing.T) {
	events := QueueBudgetBlockingDemo()
	found := false
	for _, ev := range events {
		if ev.Reason == "queue-budget-exhausted" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected queue budget exhaustion event: %+v", events)
	}
}

func TestThreePathPixelAndSequenceConsistency(t *testing.T) {
	set, err := GenerateSamples(DefaultSeed)
	if err != nil {
		t.Fatal(err)
	}
	p := set.Samples[0].Project
	opts := SchedulerOptions{Parallelism: 2, PixelBufferCap: 4096, QueueBudget: 1}
	paths, err := RunThreePath(p, t.TempDir(), opts, 2)
	if err != nil {
		t.Fatal(err)
	}
	if !paths.OK {
		t.Fatalf("three paths differ: rec=%+v full=%+v", paths.LiveVsRec, paths.LiveVsFull)
	}
	if len(paths.Live.Sequence) == 0 || !stringsEqual(paths.Live.Sequence, paths.Full.Sequence) {
		t.Fatalf("commit sequences differ or empty")
	}
}

func TestWorkbenchAcceptance(t *testing.T) {
	report, err := BuildWorkbenchReport(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if !report.AcceptanceOK {
		for _, c := range report.Checks {
			if !c.OK {
				t.Errorf("failed check: %+v", c)
			}
		}
		t.Fatal("workbench acceptance is not OK")
	}
}
