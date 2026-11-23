package web

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"roomreverb/internal/checkpoint"
	"roomreverb/internal/engine"
	"roomreverb/internal/generator"
	"roomreverb/internal/solver"
)

func TestWorkbenchHTTPAPI(t *testing.T) {
	b, err := generator.Generate(42)
	if err != nil {
		t.Fatal(err)
	}
	store, err := checkpoint.Open(filepath.Join(t.TempDir(), "web.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cfg := engine.Config{
		Solvers:  solver.Config{MaxParallel: 3, PathBufferLimit: 64, GraphCacheBudget: 2},
		UseCache: true,
	}
	srv, err := New(b.Level, b.Truth, store, Options{DBPath: "web.sqlite", Config: cfg})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != 200 || !bytes.Contains(body, []byte("Room Reverb Workbench")) {
		t.Fatalf("index failed: %d", resp.StatusCode)
	}
	resp, err = http.Get(ts.URL + "/api/state")
	if err != nil {
		t.Fatal(err)
	}
	var state map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&state)
	_ = resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("state status %d", resp.StatusCode)
	}
	if state["level"] == nil || state["result"] == nil {
		t.Fatalf("state missing fields: %v", state)
	}

	reqBody := []byte(`{"room_id":"r1","t60_ms":640,"early_density":0.7,"reverb_gain":1.05}`)
	resp, err = http.Post(ts.URL+"/api/params", "application/json", bytes.NewReader(reqBody))
	if err != nil {
		t.Fatal(err)
	}
	var editResp map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&editResp)
	_ = resp.Body.Close()
	if resp.StatusCode != 200 || editResp["ok"] != true {
		t.Fatalf("parameter edit failed: status=%d body=%v", resp.StatusCode, editResp)
	}
	change := editResp["change"].(map[string]any)
	if change["invalidations"] == nil {
		t.Fatal("edit did not return invalidation list")
	}

	resp, err = http.Get(ts.URL + "/api/export")
	if err != nil {
		t.Fatal(err)
	}
	var export map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&export)
	_ = resp.Body.Close()
	if resp.StatusCode != 200 || export["current_level"] == nil || export["result"] == nil {
		t.Fatalf("export failed: %d", resp.StatusCode)
	}

	resp, err = http.Post(ts.URL+"/api/compare", "application/json", bytes.NewReader([]byte(`{}`)))
	if err != nil {
		t.Fatal(err)
	}
	var cmp map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&cmp)
	_ = resp.Body.Close()
	if resp.StatusCode != 200 || cmp["ok"] != true {
		t.Fatalf("compare failed: status=%d body=%v", resp.StatusCode, cmp)
	}
}

func TestWorkbenchRejectsInvalidParameterWithField(t *testing.T) {
	b, _ := generator.Generate(42)
	store, _ := checkpoint.Open(filepath.Join(t.TempDir(), "web2.sqlite"))
	defer store.Close()
	srv, err := New(b.Level, b.Truth, store, Options{Config: engine.Config{
		Solvers: solver.Config{MaxParallel: 2, PathBufferLimit: 64, GraphCacheBudget: 1},
	}})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()
	body := []byte(`{"room_id":"r2","t60_ms":1,"early_density":0.5,"reverb_gain":1}`)
	resp, err := http.Post(ts.URL+"/api/params", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("want 422 got %d", resp.StatusCode)
	}
	e := out["error"].(map[string]any)
	if e["code"] != "E_PARA" || e["field"] != "t60_ms" || e["room"] != "r2" {
		t.Fatalf("bad error payload: %v", e)
	}
}
