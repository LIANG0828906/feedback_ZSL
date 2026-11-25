package diagnostic_test

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"terrainbake/diagnostic"
)

func TestHTTPDiagnosticWorkbench(t *testing.T) {
	controller, err := diagnostic.New(424242, filepath.Join(t.TempDir(), "diag.sqlite"))
	if err != nil {
		t.Fatalf("controller: %v", err)
	}
	defer controller.Close()
	server := httptest.NewServer(diagnostic.NewHTTPHandler(controller))
	defer server.Close()

	response, err := http.Get(server.URL + "/")
	if err != nil {
		t.Fatalf("index: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("index status=%d", response.StatusCode)
	}
	var page bytes.Buffer
	_, _ = page.ReadFrom(response.Body)
	if !strings.Contains(page.String(), "分块高度场光照烘焙诊断台") {
		t.Fatal("diagnostic page missing title")
	}

	response, err = http.Get(server.URL + "/api/state?time=2")
	if err != nil {
		t.Fatalf("state: %v", err)
	}
	var state diagnostic.State
	if err := json.NewDecoder(response.Body).Decode(&state); err != nil {
		t.Fatalf("decode state: %v", err)
	}
	response.Body.Close()
	if state.Time != 2 || len(state.Chunks) != 9 || !state.Comparison.Equal {
		t.Fatalf("unexpected initial state: time=%d chunks=%d equal=%t", state.Time, len(state.Chunks), state.Comparison.Equal)
	}
	if state.Truth == nil {
		t.Fatal("initial truth report missing")
	}

	body := bytes.NewBufferString(`{"chunk_id":"chunk-00-00","x":1,"z":1,"delta":3.5}`)
	response, err = http.Post(server.URL+"/api/raise", "application/json", body)
	if err != nil {
		t.Fatalf("raise: %v", err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("raise status=%d", response.StatusCode)
	}
	if err := json.NewDecoder(response.Body).Decode(&state); err != nil {
		t.Fatalf("decode raised state: %v", err)
	}
	response.Body.Close()
	if !state.Edited || len(state.Invalidations) == 0 || !state.Comparison.Equal {
		t.Fatalf("unexpected raised state: edited=%t invalid=%d equal=%t", state.Edited, len(state.Invalidations), state.Comparison.Equal)
	}
}
