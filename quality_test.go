package maskqc

import (
	"bytes"
	"encoding/json"
	"math"
	"net/http/httptest"
	"path/filepath"
	"testing"
)

func TestParameterAndOverflowStructuredErrors(t *testing.T) {
	badHardness := Stroke{ID: "x", Seq: 1, WeightBP: 10000, HardnessPermille: 0, Radius: 2}
	err := ValidateStroke(badHardness)
	qerr, ok := err.(*QCError)
	if !ok || qerr.Code != CodeParameter {
		t.Fatalf("expected E_PARA hardness, got %#v", err)
	}
	if len(qerr.LegalRange) != 2 || qerr.LegalRange[0] != 1 || qerr.LegalRange[1] != 1000 {
		t.Fatalf("missing legal range: %+v", qerr)
	}

	badWeight := badHardness
	badWeight.HardnessPermille = 1000
	badWeight.WeightBP = 123
	err = ValidateStroke(badWeight)
	qerr, ok = err.(*QCError)
	if !ok || qerr.Code != CodeParameter || qerr.Details["allowed"] == nil {
		t.Fatalf("expected E_PARA allowed set, got %#v", err)
	}

	_, _, err = ConvertScale(math.MaxInt64, 1, 10000, RoundFloor, "coverage", "$.coverage", nil)
	qerr, ok = err.(*QCError)
	if !ok || qerr.Code != CodeOverflow || len(qerr.Operands) < 2 {
		t.Fatalf("expected E_OVF with operands, got %#v", err)
	}

	_, err = AbsoluteFromOffset(Point{}, math.MaxInt64, "strokes.big.start", nil)
	qerr, ok = err.(*QCError)
	if !ok || qerr.Code != CodeOverflow || len(qerr.Operands) < 2 {
		t.Fatalf("expected coordinate E_OVF, got %#v", err)
	}
}

func TestTopologyErrorListsMeasuredDomain(t *testing.T) {
	set, _ := GenerateSampleSet(FixedSeed)
	p := set.Projects[0]
	report := VerifyTopology(p, p.Candidate)
	if report.OK {
		t.Fatal("defective candidate should fail topology tolerance")
	}
	if report.Error == nil || report.Error.Code != CodeTopology {
		t.Fatalf("expected E_TOP, got %+v", report.Error)
	}
	domains, ok := report.Error.Details["domains"].([]map[string]any)
	if !ok || len(domains) == 0 {
		t.Fatalf("E_TOP details must list domains: %+v", report.Error.Details)
	}
}

func TestPreciseRecomputePreservesUnaffectedTiles(t *testing.T) {
	set, _ := GenerateSampleSet(FixedSeed)
	p := set.Projects[0]
	engine := NewEngine(p)
	baseline, err := engine.RunFullRecompute()
	if err != nil {
		t.Fatal(err)
	}

	plan, err := PlanEdit(p, EditRequest{
		Type: "update", StrokeID: "ring-top",
		Start: &Point{X: 10, Y: 9},
	})
	if err != nil {
		t.Fatal(err)
	}
	affected := map[string]bool{}
	for _, id := range plan.TileIDs {
		affected[id] = true
	}
	var unaffected Tile
	found := false
	for _, tile := range engine.tiles {
		if !affected[tile.ID] {
			unaffected = tile
			found = true
			break
		}
	}
	if !found {
		t.Fatal("expected at least one unaffected tile")
	}
	before := HashMaskRegion(baseline.Mask, unaffected.Rect)

	editedEngine := NewEngine(p)
	result, err := editedEngine.RecomputeAffected(baseline.Mask, plan.TileIDs)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.RecomputedTiles) != len(plan.TileIDs) {
		t.Fatalf("recomputed=%v plan=%v", result.RecomputedTiles, plan.TileIDs)
	}
	after := HashMaskRegion(result.Mask, unaffected.Rect)
	if before != after {
		t.Fatalf("unaffected tile %s changed: %s -> %s", unaffected.ID, before, after)
	}
}

func TestOrderIndependentOfDeclarationOrder(t *testing.T) {
	set, _ := GenerateSampleSet(FixedSeed)
	strokes := append([]Stroke{}, set.Projects[0].Strokes...)
	reversed := make([]Stroke, len(strokes))
	for i := range strokes {
		reversed[i] = strokes[len(strokes)-1-i]
	}
	a := OrderedStrokes(strokes)
	b := OrderedStrokes(reversed)
	for i := range a {
		if a[i].ID != b[i].ID {
			t.Fatalf("order differs after map/slice shuffle at %d: %s vs %s", i, a[i].ID, b[i].ID)
		}
	}
}

func TestHTTPWorkbenchEndpoints(t *testing.T) {
	set, err := GenerateSampleSet(FixedSeed)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if err := set.WriteTo(root); err != nil {
		t.Fatal(err)
	}
	server, err := NewServer(root, filepath.Join(t.TempDir(), "http.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	handler := server.Handler()

	req := httptest.NewRequest("GET", "/api/state", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != 200 || !bytes.Contains(rec.Body.Bytes(), []byte("ring-32")) {
		t.Fatalf("state code=%d body=%s", rec.Code, rec.Body.String())
	}

	body := bytes.NewBufferString(`{"projectId":"ring-32","mode":"incremental"}`)
	req = httptest.NewRequest("POST", "/api/verify", body)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	var verifyResp struct {
		OK     bool      `json:"ok"`
		Result RunResult `json:"result"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &verifyResp); err != nil {
		t.Fatal(err)
	}
	if !verifyResp.OK || !verifyResp.Result.AlphaExact {
		t.Fatalf("http verify failed: %s", rec.Body.String())
	}

	body = bytes.NewBufferString(`{"projectId":"ring-32","mode":"scheduled","panicTileId":"r00c00"}`)
	req = httptest.NewRequest("POST", "/api/verify", body)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	var panicResp struct {
		OK     bool      `json:"ok"`
		Result RunResult `json:"result"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &panicResp); err != nil {
		t.Fatal(err)
	}
	if panicResp.OK {
		t.Fatalf("injected panic should make response ok=false: %s", rec.Body.String())
	}
	foundPanic := false
	for _, job := range panicResp.Result.SchedulerResults {
		if job.TileID == "r00c00" && job.Panic {
			foundPanic = true
		}
	}
	if !foundPanic {
		t.Fatalf("missing isolated r0c0 panic: %+v", panicResp.Result.SchedulerResults)
	}

	req = httptest.NewRequest("GET", "/api/png?project=ring-32&kind=reference", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != 200 || rec.Header().Get("Content-Type") != "image/png" || rec.Body.Len() == 0 {
		t.Fatalf("png endpoint failed code=%d type=%s len=%d", rec.Code, rec.Header().Get("Content-Type"), rec.Body.Len())
	}
}
