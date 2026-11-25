package terrainpatch_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"terrainpatch/internal/boundary"
	"terrainpatch/internal/engine"
	"terrainpatch/internal/loader"
	"terrainpatch/internal/mesh"
	"terrainpatch/internal/model"
	"terrainpatch/internal/sample"
	"terrainpatch/internal/store"
	"terrainpatch/internal/verifier"
	"terrainpatch/internal/web"
)

func freshProject(t *testing.T) (*model.Project, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "sample")
	_, ae := sample.Generate(dir, 42)
	if ae != nil {
		t.Fatalf("generate: %v", ae)
	}
	loaded, ae := loader.LoadProject(dir)
	if ae != nil {
		t.Fatalf("load: %v", ae)
	}
	return loaded, dir
}

func TestSampleVerificationAcceptance(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "sample")
	if _, ae := sample.Generate(dir, 42); ae != nil {
		t.Fatal(ae)
	}
	db := filepath.Join(t.TempDir(), "verify.db")
	rep, ae := verifier.VerifySample(context.Background(), dir, db)
	if ae != nil {
		t.Fatalf("VerifySample returned %v; report=%+v", ae, rep)
	}
	if !rep.OK {
		for _, c := range rep.Checks {
			if !c.OK {
				t.Errorf("check %s failed: %+v", c.Name, c.Detail)
			}
		}
	}
	if rep.ThreeWay == nil || !rep.ThreeWay.Equal {
		t.Fatalf("three-way report not equal: %+v", rep.ThreeWay)
	}
}

func TestMeshDeterminismAndBudgetCode(t *testing.T) {
	p, _ := freshProject(t)
	tile := *p.Tile("T10")
	var wg sync.WaitGroup
	results := make(chan string, 32)
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			m, err := mesh.Build(tile)
			if err != nil {
				t.Errorf("build: %v", err)
				return
			}
			results <- model.DigestJSON(engine.CanonicalMesh(m))
		}()
	}
	wg.Wait()
	close(results)
	first := ""
	for h := range results {
		if first == "" {
			first = h
		} else if h != first {
			t.Fatal("mesh digest changed across workers")
		}
	}

	over := *p.Tile("T00")
	over.MaxTriangles = 1
	_, err := mesh.Build(over)
	if err == nil || err.Code != model.CodeBudget {
		t.Fatalf("expected E_BUDGET, got %+v", err)
	}
	if err.Actual == nil || err.TileID != "T00" {
		t.Fatalf("E_BUDGET must include tile and actual count: %+v", err)
	}
}

func TestTightBudgetIncreasesErrorAndReducesBoundaryVertices(t *testing.T) {
	p, _ := freshProject(t)
	tightTile := *p.Tile("T00")
	tight, ae := mesh.Build(tightTile)
	if ae != nil {
		t.Fatal(ae)
	}

	looseTile := tightTile
	looseTile.SampleBudget = 300
	looseTile.MaxTriangles = 512
	loose, ae := mesh.Build(looseTile)
	if ae != nil {
		t.Fatal(ae)
	}

	tightEdge := boundary.ExtractSeries(tight, "right")
	looseEdge := boundary.ExtractSeries(loose, "right")
	if tight.Depth >= loose.Depth {
		t.Fatalf("tight depth %d should be lower than loose depth %d", tight.Depth, loose.Depth)
	}
	if tight.MaxError <= loose.MaxError {
		t.Fatalf("tight error %d should exceed loose error %d", tight.MaxError, loose.MaxError)
	}
	if len(tightEdge) >= len(looseEdge) {
		t.Fatalf("tight boundary vertices %d should be fewer than loose %d", len(tightEdge), len(looseEdge))
	}
}

func TestLoaderSchemaAndMetaErrors(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "sample")
	if _, ae := sample.Generate(dir, 42); ae != nil {
		t.Fatal(ae)
	}

	declPath := filepath.Join(dir, "declarations.json")
	var doc map[string]any
	b, _ := os.ReadFile(declPath)
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	decls := doc["declarations"].(map[string]any)
	t00 := decls["T00"].(map[string]any)
	t00["sampleBudget"] = 0
	b, _ = json.MarshalIndent(doc, "", "  ")
	if err := os.WriteFile(declPath, b, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, ae := loader.LoadProject(dir); ae == nil || ae.Code != model.CodeMeta || ae.Field != "sampleBudget" {
		t.Fatalf("expected E_META sampleBudget, got %+v", ae)
	}

	if _, ae := sample.Generate(dir, 42); ae != nil {
		t.Fatal(ae)
	}
	b, _ = os.ReadFile(declPath)
	_ = json.Unmarshal(b, &doc)
	delete(doc["declarations"].(map[string]any)["T00"].(map[string]any), "sampleBudget")
	b, _ = json.MarshalIndent(doc, "", "  ")
	_ = os.WriteFile(declPath, b, 0o644)
	if _, ae := loader.LoadProject(dir); ae == nil || ae.Code != model.CodeSchema {
		t.Fatalf("expected E_SCHEMA missing field, got %+v", ae)
	}

	if _, ae := sample.Generate(dir, 42); ae != nil {
		t.Fatal(ae)
	}
	boundPath := filepath.Join(dir, "boundaries.json")
	var bdoc map[string]any
	b, _ = os.ReadFile(boundPath)
	_ = json.Unmarshal(b, &bdoc)
	first := bdoc["boundaries"].([]any)[0].(map[string]any)
	sideA := first["sideA"].(map[string]any)
	cols := sideA["columns"].([]any)
	sideA["columns"] = cols[:2]
	b, _ = json.MarshalIndent(bdoc, "", "  ")
	_ = os.WriteFile(boundPath, b, 0o644)
	_, ae := loader.LoadProject(dir)
	if ae == nil || ae.Code != model.CodeMeta || !strings.Contains(ae.Field, "columns") {
		t.Fatalf("expected E_META columns length, got %+v", ae)
	}
}

func TestEditParameterAndImpactPropagation(t *testing.T) {
	p, _ := freshProject(t)
	eng := engine.NewEngine(p, engine.Options{Workers: 2})
	if _, ae := eng.RebuildAll(context.Background(), "base"); ae != nil {
		t.Fatal(ae)
	}
	rep, ae := eng.ApplyEdit(context.Background(), engine.EditSpec{TileID: "T00", Field: "sampleBudget", IntValue: 50})
	if ae != nil {
		t.Fatal(ae)
	}
	status := map[string]bool{}
	for _, inv := range rep.Invalidations {
		status[inv.TileID] = inv.Reused
	}
	if status["T00"] || status["T10"] || status["T01"] {
		t.Fatalf("edited tile and boundary neighbors must be invalid: %+v", rep.Invalidations)
	}
	if !status["T11"] {
		t.Fatalf("non-adjacent T11 must reuse cache: %+v", rep.Invalidations)
	}

	_, ae = eng.ApplyEdit(context.Background(), engine.EditSpec{TileID: "T00", Field: "sampleBudget", IntValue: 0})
	if ae == nil || ae.Code != model.CodePara {
		t.Fatalf("expected E_PARA, got %+v", ae)
	}
}

func TestFailureIsolationAndRetryMatchesCleanPath(t *testing.T) {
	p, _ := freshProject(t)
	clean := engine.NewEngine(engine.CloneProject(p), engine.Options{Workers: 2})
	cleanReport, ae := clean.RebuildAll(context.Background(), "clean")
	if ae != nil {
		t.Fatal(ae)
	}

	eng := engine.NewEngine(engine.CloneProject(p), engine.Options{Workers: 2})
	if _, ae := eng.RebuildAll(context.Background(), "base"); ae != nil {
		t.Fatal(ae)
	}
	eng.InjectFailure("T00", model.CodeInternal)
	failed, ae := eng.RecomputeOnly(context.Background(), "T00", "injected")
	if ae != nil {
		t.Fatalf("tile failure should be captured in report, not escape: %v", ae)
	}
	var failedInfo *engine.TileInfo
	for i := range failed.Tiles {
		if failed.Tiles[i].TileID == "T00" {
			failedInfo = &failed.Tiles[i]
		}
	}
	if failedInfo == nil || failedInfo.Status != "failed" || failedInfo.Error == nil {
		t.Fatalf("T00 should be failed: %+v", failed.Tiles)
	}
	if failed.Meshes["T11"] == nil {
		t.Fatal("other tile cache must remain available")
	}

	retry, ae := eng.RetryTile(context.Background(), "T00")
	if ae != nil {
		t.Fatal(ae)
	}
	if d := engine.CompareCanonicalMesh("T00", cleanReport.Meshes["T00"], retry.Meshes["T00"]); d != nil {
		t.Fatalf("retry differs from clean path: %+v", d)
	}
}

func TestMemoryBudgetBlocksAndRecordsQueue(t *testing.T) {
	p, _ := freshProject(t)
	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	eng := engine.NewEngine(p, engine.Options{
		Workers: 2, VertexBufferCap: 4096, MemoryBudgetBytes: 80_000,
		ComputeHook: func(id string) {
			if id == "T00" {
				select {
				case entered <- struct{}{}:
				default:
				}
				<-release
			}
		},
	})
	done := make(chan *engine.Report, 1)
	errCh := make(chan *model.AppError, 1)
	go func() {
		r, e := eng.RebuildAll(context.Background(), "queue-test")
		if e != nil {
			errCh <- e
			return
		}
		done <- r
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("T00 did not enter compute hook")
	}
	time.Sleep(120 * time.Millisecond)
	close(release)
	var rep *engine.Report
	select {
	case rep = <-done:
	case e := <-errCh:
		t.Fatal(e)
	case <-time.After(5 * time.Second):
		t.Fatal("queued tiles did not finish")
	}
	found := false
	for _, q := range rep.Queue {
		for _, reason := range q.Reasons {
			if reason == "memory-budget" {
				found = true
			}
		}
	}
	if !found {
		t.Fatalf("expected memory-budget queue record: %+v", rep.Queue)
	}
}

func TestSQLiteVersionMismatchResetsWholeSnapshot(t *testing.T) {
	p, _ := freshProject(t)
	db := filepath.Join(t.TempDir(), "version.db")
	st, err := store.NewSQLite(db)
	if err != nil {
		t.Fatal(err)
	}
	eng := engine.NewEngine(p, engine.Options{Store: st})
	if _, ae := eng.RebuildAll(context.Background(), "base"); ae != nil {
		t.Fatal(ae)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	p2 := engine.CloneProject(p)
	p2.Tile("T00").ParamVersion++
	st2, err := store.NewSQLite(db)
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()
	eng2 := engine.NewEngine(p2, engine.Options{Store: st2})
	rec, ae := eng2.Recover(context.Background())
	if ae != nil {
		t.Fatal(ae)
	}
	if !rec.Reset {
		t.Fatalf("expected whole snapshot reset on parameter version mismatch, got %+v", rec)
	}
}

func TestHTTPDiagnosticAPI(t *testing.T) {
	p, _ := freshProject(t)
	eng := engine.NewEngine(p, engine.Options{Workers: 2})
	if _, ae := eng.RebuildAll(context.Background(), "base"); ae != nil {
		t.Fatal(ae)
	}
	srv := (&web.Server{Engine: eng, BaseProject: p, VerifyDBPath: filepath.Join(t.TempDir(), "web-verify.db")}).Handler()
	ts := httptest.NewServer(srv)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("health status %d", resp.StatusCode)
	}
	resp, err = http.Get(ts.URL + "/api/state")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("state status %d", resp.StatusCode)
	}

	body := strings.NewReader(`{"tileId":"T11","field":"sampleBudget","value":200}`)
	resp, err = http.Post(ts.URL+"/api/edit", "application/json", body)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("edit status %d", resp.StatusCode)
	}
}
