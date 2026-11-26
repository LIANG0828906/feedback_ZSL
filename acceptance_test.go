package maskqa

import (
	"encoding/json"
	"image"
	"image/color"
	"math"
	"path/filepath"
	"reflect"
	"testing"
)

func TestGeneratorDeterministicAndMultiResolution(t *testing.T) {
	first, err := GenerateSample(0)
	if err != nil {
		t.Fatal(err)
	}
	second, err := GenerateSample(0)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first.Strokes, second.Strokes) {
		t.Fatal("strokes are not deterministic")
	}
	if string(first.Candidate.Pix) != string(second.Candidate.Pix) {
		t.Fatal("candidate pixels are not deterministic")
	}
	if string(first.Reference.Pix) != string(second.Reference.Pix) {
		t.Fatal("reference pixels are not deterministic")
	}
	specs := SampleSpecs()
	if len(specs) < 3 {
		t.Fatalf("expected at least 3 samples, got %d", len(specs))
	}
	seen := map[[2]int]bool{}
	for i := range specs {
		p, err := GenerateSample(i)
		if err != nil {
			t.Fatal(err)
		}
		size := [2]int{p.Width, p.Height}
		if seen[size] {
			t.Fatalf("duplicate resolution %v", size)
		}
		seen[size] = true
	}
}

func TestSchemaRoundTripAndDecodeErrors(t *testing.T) {
	p, err := GenerateSample(0)
	if err != nil {
		t.Fatal(err)
	}
	for _, version := range []string{SchemaV1, SchemaV2, SchemaV3} {
		exported, err := ExportProject(p, version)
		if err != nil {
			t.Fatalf("export %s: %v", version, err)
		}
		loaded, err := LoadProject(exported.Data)
		if err != nil {
			t.Fatalf("load %s: %v", version, err)
		}
		if loaded.Project.SourceVersion != version {
			t.Fatalf("source version = %q", loaded.Project.SourceVersion)
		}
		if len(loaded.Project.Strokes) != len(p.Strokes) {
			t.Fatalf("stroke count changed for %s", version)
		}
	}

	v1, err := ExportProject(p, SchemaV1)
	if err != nil {
		t.Fatal(err)
	}
	if len(v1.Lossy) == 0 {
		t.Fatal("expected E_LOSSY entries for v1 downgrade")
	}
	foundGroupLoss := false
	foundDefaultTrace := false
	loadedV1, err := LoadProject(v1.Data)
	if err != nil {
		t.Fatal(err)
	}
	for _, issue := range v1.Lossy {
		if issue.Code != CodeLossy {
			t.Fatalf("unexpected issue code %q", issue.Code)
		}
		if issue.Field == "stroke_records[0].pen_group" {
			foundGroupLoss = true
		}
	}
	for _, trace := range loadedV1.Traces {
		if trace.Path == "stroke_records[0].hardness" && trace.DefaultValue != nil {
			foundDefaultTrace = true
		}
	}
	if !foundGroupLoss || !foundDefaultTrace {
		t.Fatalf("missing group loss=%v or default trace=%v", foundGroupLoss, foundDefaultTrace)
	}
	for i, original := range p.Strokes {
		roundTripped := loadedV1.Project.Strokes[i]
		if original.Start != roundTripped.Start || original.End != roundTripped.End ||
			original.Seq != roundTripped.Seq || original.WeightPermyriad != roundTripped.WeightPermyriad {
			t.Fatalf("v1 integer roundtrip mismatch at stroke %d: %+v vs %+v", i, original, roundTripped)
		}
		if original.Group != roundTripped.Group ||
			original.HardnessPermyriad != roundTripped.HardnessPermyriad ||
			original.Direction != roundTripped.Direction {
			t.Fatalf("v1 lossy-recovery extension did not restore equivalent stroke %d", i)
		}
	}
	for i, original := range p.Truth {
		if original.Direction != loadedV1.Project.Truth[i].Direction {
			t.Fatalf("v1 truth direction %d not restored", i)
		}
	}

	v2, err := ExportProject(p, SchemaV2)
	if err != nil {
		t.Fatal(err)
	}
	loadedV2, err := LoadProject(v2.Data)
	if err != nil {
		t.Fatal(err)
	}
	for i, original := range p.Strokes {
		roundTripped := loadedV2.Project.Strokes[i]
		if original.Start != roundTripped.Start || original.End != roundTripped.End ||
			original.Seq != roundTripped.Seq || original.WeightPermyriad != roundTripped.WeightPermyriad {
			t.Fatalf("v2 offset roundtrip mismatch at stroke %d", i)
		}
	}

	var unknown map[string]any
	if err := json.Unmarshal(v3Bytes(t, p), &unknown); err != nil {
		t.Fatal(err)
	}
	unknown["schema_version"] = "maskqa.project/v9"
	bad, _ := json.Marshal(unknown)
	_, err = LoadProject(bad)
	assertCodeField(t, err, CodeDecode, "schema_version")

	var missingID map[string]any
	_ = json.Unmarshal(v3Bytes(t, p), &missingID)
	delete(missingID, "id")
	bad, _ = json.Marshal(missingID)
	_, err = LoadProject(bad)
	assertCodeField(t, err, CodeDecode, "id")
}

func v3Bytes(t *testing.T, p *Project) []byte {
	t.Helper()
	exported, err := ExportProject(p, SchemaV3)
	if err != nil {
		t.Fatal(err)
	}
	return exported.Data
}

func assertCodeField(t *testing.T, err error, code, field string) {
	t.Helper()
	biz, ok := AsError(err)
	if !ok {
		t.Fatalf("expected *Error code=%s field=%s, got %T %v", code, field, err, err)
	}
	if biz.Code != code {
		t.Fatalf("code = %q, want %q (%+v)", biz.Code, code, biz)
	}
	if biz.Field != field {
		t.Fatalf("field = %q, want %q", biz.Field, field)
	}
}

func TestIntegerOverflowAndParameterErrors(t *testing.T) {
	_, err := AbsoluteFromOffset(math.MaxInt64, 1, "strokes[0].startOffsetX")
	biz, ok := AsError(err)
	if !ok || biz.Code != CodeOvf || biz.OpA != math.MaxInt64 || biz.OpB != 1 {
		t.Fatalf("expected E_OVF with operands, got %+v", err)
	}
	_, err = ScalePermilleToPermyriad(math.MaxInt64, "weight", nil)
	assertCodeField(t, err, CodeOvf, "weight")

	p, err := GenerateSample(0)
	if err != nil {
		t.Fatal(err)
	}
	p.Strokes[0].HardnessPermyriad = 10001
	_, err = ComputeFull(p, DefaultSchedulerOptions(), nil)
	assertCodeField(t, err, CodePara, "strokes[0].hardness_permyriad")

	p, _ = GenerateSample(0)
	p.Strokes[0].WeightPermyriad = -1
	_, err = ComputeFull(p, DefaultSchedulerOptions(), nil)
	assertCodeField(t, err, CodePara, "strokes[0].weight_permyriad")

	remainderProject := mustGenerate(t, 0)
	remainderProject.Strokes[0].WeightPermyriad = 1234
	exported, err := ExportProject(remainderProject, SchemaV1)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadProject(exported.Data)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Project.Strokes[0].WeightPermyriad != 1234 {
		t.Fatalf("non-aligned weight = %d, want 1234", loaded.Project.Strokes[0].WeightPermyriad)
	}
}

func TestRasterDeterministicAndParallelOrderIndependent(t *testing.T) {
	p, err := GenerateSample(1)
	if err != nil {
		t.Fatal(err)
	}
	a, err := RasterizeStroke(p.Strokes[0], p.Width, p.Height)
	if err != nil {
		t.Fatal(err)
	}
	b, err := RasterizeStroke(p.Strokes[0], p.Width, p.Height)
	if err != nil {
		t.Fatal(err)
	}
	if string(a.Pix) != string(b.Pix) {
		t.Fatal("same stroke did not produce identical coverage")
	}
	serial, err := ComputeFull(p, SchedulerOptions{Parallelism: 1, BufferPixelCap: 4096, QueueTileBudget: 1}, nil)
	if err != nil {
		t.Fatal(err)
	}
	parallel, err := ComputeFull(p, SchedulerOptions{Parallelism: 4, BufferPixelCap: 4096, QueueTileBudget: 2}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := CompareResults(serial, parallel); err != nil {
		t.Fatal(err)
	}
}

func TestInvalidationPrecision(t *testing.T) {
	p, err := GenerateSample(0)
	if err != nil {
		t.Fatal(err)
	}
	wb, err := NewWorkbench(p, filepath.Join(t.TempDir(), "checkpoint"), SchedulerOptions{Parallelism: 2, BufferPixelCap: 4096, QueueTileBudget: 2})
	if err != nil {
		t.Fatal(err)
	}
	defer wb.Close()
	first, err := wb.InitializeFull()
	if err != nil {
		t.Fatal(err)
	}
	total := len(first.Tiles)
	result, plan, err := wb.Edit(Edit{StrokeID: "stroke-h", WeightPermyriad: intPtr(7600)})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.InvalidTiles) == 0 || len(plan.InvalidTiles)+len(plan.ReusableTiles) != total {
		t.Fatalf("bad partition invalid=%d reusable=%d total=%d", len(plan.InvalidTiles), len(plan.InvalidTiles), total)
	}
	if len(result.RecomputedTiles) != len(plan.InvalidTiles) {
		t.Fatalf("recomputed=%d invalid=%d", len(result.RecomputedTiles), len(plan.InvalidTiles))
	}
	if len(result.ReusedTiles) != len(plan.ReusableTiles) {
		t.Fatalf("reused=%d want=%d", len(result.ReusedTiles), len(plan.ReusableTiles))
	}

	_, priorityPlan, err := wb.Edit(Edit{StrokeID: "stroke-ring-7", Priority: intPtr(3)})
	if err != nil {
		t.Fatal(err)
	}
	if len(priorityPlan.InvalidTiles) == 0 {
		t.Fatal("priority edit did not invalidate ring tiles")
	}
	foundTopology := false
	for _, node := range priorityPlan.InvalidNodes {
		if node == "verify:topology" || node == "verify:direction" {
			foundTopology = true
		}
	}
	if !foundTopology {
		t.Fatal("downstream verification was not invalidated")
	}
}

func TestThreePathsCheckpointRecoveryAndDiff(t *testing.T) {
	options := SchedulerOptions{Parallelism: 2, BufferPixelCap: 4096, QueueTileBudget: 2}
	edits := []Edit{
		{StrokeID: "stroke-h", WeightPermyriad: intPtr(7800)},
		{StrokeID: "stroke-ring-7", Priority: intPtr(4)},
	}

	incProject, err := GenerateSample(0)
	if err != nil {
		t.Fatal(err)
	}
	incWB, err := NewWorkbench(incProject, filepath.Join(t.TempDir(), "inc"), options)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := incWB.InitializeFull(); err != nil {
		t.Fatal(err)
	}
	var incResult *ComputeResult
	for _, edit := range edits {
		incResult, _, err = incWB.Edit(edit)
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := incWB.Close(); err != nil {
		t.Fatal(err)
	}

	recProject, err := GenerateSample(0)
	if err != nil {
		t.Fatal(err)
	}
	recDir := filepath.Join(t.TempDir(), "recover")
	recWB, err := NewWorkbench(recProject, recDir, options)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := recWB.InitializeFull(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := recWB.Edit(edits[0]); err != nil {
		t.Fatal(err)
	}
	if err := recWB.Store.SimulateTornWriteForTest([]byte(`{"partial":`)); err != nil {
		t.Fatal(err)
	}
	if err := recWB.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := NewWorkbench(recWB.Project, recDir, options)
	if err != nil {
		t.Fatal(err)
	}
	if reopened.RecoveryReport.Action != RecoveryRecovered || reopened.RecoveryReport.IgnoredTornTemp != 1 {
		t.Fatalf("unexpected recovery: %+v", reopened.RecoveryReport)
	}
	recoveredResult, recoveryReport, err := reopened.Recover()
	if err != nil {
		t.Fatal(err)
	}
	if recoveryReport.LastSequence <= 0 || len(recoveredResult.Tiles) == 0 {
		t.Fatal("recovery did not load committed tiles")
	}
	recoveredResult, _, err = reopened.Edit(edits[1])
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()

	fullProject := mustGenerate(t, 0)
	for _, edit := range edits {
		if _, err := ApplyEdit(fullProject, edit); err != nil {
			t.Fatal(err)
		}
	}
	fullResult, err := ComputeFull(fullProject, options, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := CompareResults(fullResult, incResult); err != nil {
		t.Fatalf("incremental mismatch: %v", err)
	}
	if err := CompareResults(fullResult, recoveredResult); err != nil {
		t.Fatalf("recovery mismatch: %v", err)
	}

	diffProject := mustGenerate(t, 0)
	for _, edit := range edits {
		if _, err := ApplyEdit(diffProject, edit); err != nil {
			t.Fatal(err)
		}
	}
	diffResult, err := ComputeFull(diffProject, options, nil)
	if err != nil {
		t.Fatal(err)
	}
	diffResult.Tiles[0].Pixels[0] ^= 0xff
	_, tile := findTileResult(diffResult, TileCoord{TX: 0, TY: 0})
	_ = tile
	err = CompareResults(fullResult, diffResult)
	biz, ok := AsError(err)
	if !ok || biz.Code != CodeDiff || biz.Location == nil || biz.Location.Field != "accumulated_alpha" {
		t.Fatalf("expected E_DIFF pixel location, got %+v", err)
	}
	if biz.Location.TileX != 0 || biz.Location.TileY != 0 {
		t.Fatalf("unexpected tile location: %+v", biz.Location)
	}
}

func findTileResult(result *ComputeResult, coord TileCoord) (int, TileResult) {
	for i, tile := range result.Tiles {
		if tile.Coord == coord {
			return i, tile
		}
	}
	return -1, TileResult{}
}

func mustGenerate(t *testing.T, idx int) *Project {
	t.Helper()
	p, err := GenerateSample(idx)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func intPtr(v int) *int { return &v }

func TestTopologyInjectionReturnsETop(t *testing.T) {
	p := mustGenerate(t, 0)
	result, err := ComputeFull(p, DefaultSchedulerOptions(), nil)
	if err != nil {
		t.Fatal(err)
	}
	mutated := cloneGray(result.Accumulated)
	for y := 18; y <= 22; y++ {
		for x := 28; x <= 32; x++ {
			mutated.SetGray(x, y, color.Gray{Y: 0})
		}
	}
	_, err = VerifyMask(p, mutated)
	biz, ok := AsError(err)
	if !ok || biz.Code != CodeTop || biz.DomainNo == 0 {
		t.Fatalf("expected E_TOP with domain number, got %+v", err)
	}
	if biz.Field == "" || biz.Actual == nil {
		t.Fatalf("E_TOP missing field/measured value: %+v", biz)
	}
}

func cloneGray(src *image.Gray) *image.Gray {
	dst := image.NewGray(src.Bounds())
	copy(dst.Pix, src.Pix)
	return dst
}
