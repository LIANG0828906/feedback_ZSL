package diagnostic

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
)

func TestDiagnosticHTTPWorkbench(t *testing.T) {
	server, err := NewServer(filepath.Join(t.TempDir(), "http.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	ts := httptest.NewServer(server.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/state")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var state StateResponse
	if err := json.NewDecoder(resp.Body).Decode(&state); err != nil {
		t.Fatal(err)
	}
	if len(state.Tiles) != 135 || len(state.Snapshots) != 135 {
		t.Fatalf("tiles=%d snapshots=%d", len(state.Tiles), len(state.Snapshots))
	}

	editResp, err := http.Post(ts.URL+"/api/edit?kind=endpoint", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer editResp.Body.Close()
	var edited StateResponse
	if err := json.NewDecoder(editResp.Body).Decode(&edited); err != nil {
		t.Fatal(err)
	}
	if edited.Comparison == nil || !edited.Comparison.PixelEqual || !edited.Comparison.HashEqual {
		t.Fatalf("bad comparison: %+v", edited.Comparison)
	}

	injectResp, err := http.Post(ts.URL+"/api/inject?case=para", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer injectResp.Body.Close()
	var injected InjectionResult
	if err := json.NewDecoder(injectResp.Body).Decode(&injected); err != nil {
		t.Fatal(err)
	}
	if !injected.OK || injected.Issue == nil || injected.Issue.ID != "wall_right" {
		t.Fatalf("injection=%+v", injected)
	}

	frameResp, err := http.Get(ts.URL + "/api/frame.png")
	if err != nil {
		t.Fatal(err)
	}
	defer frameResp.Body.Close()
	if frameResp.Header.Get("Content-Type") != "image/png" {
		t.Fatalf("content type=%q", frameResp.Header.Get("Content-Type"))
	}
}
