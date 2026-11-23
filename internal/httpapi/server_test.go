package httpapi_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"occlbench/internal/culling"
	"occlbench/internal/generator"
	"occlbench/internal/httpapi"
	"occlbench/internal/persist"
	"occlbench/internal/streaming"
	"occlbench/internal/workbench"
)

func newTestServer(t *testing.T) (*httptest.Server, func()) {
	t.Helper()
	w := generator.Generate(generator.DefaultConfig())
	cfg := streaming.Config{
		WindowSize: 3,
		Budget:     streaming.Budget{Slots: 2, Memory: 50},
		CullParams: culling.DefaultPreciseParams(),
	}
	store, err := persist.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	app, err := workbench.NewApp(w, cfg, store, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(httpapi.New(app).Handler())
	return srv, func() { srv.Close(); store.Close() }
}

func TestIndexAndSnapshot(t *testing.T) {
	srv, cleanup := newTestServer(t)
	defer cleanup()

	resp, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("index status %d", resp.StatusCode)
	}

	resp, err = http.Get(srv.URL + "/api/snapshot")
	if err != nil {
		t.Fatal(err)
	}
	var snap map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&snap); err != nil {
		t.Fatal(err)
	}
	if snap["chunkCount"].(float64) != 16 {
		t.Fatalf("unexpected chunk count: %v", snap["chunkCount"])
	}
}

func TestBudgetValidationEndpoint(t *testing.T) {
	srv, cleanup := newTestServer(t)
	defer cleanup()
	body, _ := json.Marshal(map[string]any{"slots": 0, "memory": 10})
	resp, err := http.Post(srv.URL+"/api/budget", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	if out["error"] != "E_BUDGET" {
		t.Fatalf("want E_BUDGET got %v", out)
	}
}

func TestVerifyEndpoint(t *testing.T) {
	srv, cleanup := newTestServer(t)
	defer cleanup()
	resp, err := http.Get(srv.URL + "/api/verify")
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	json.NewDecoder(resp.Body).Decode(&out)
	if out["ok"] != true {
		t.Fatalf("three-path verify failed: %v", out)
	}
}
