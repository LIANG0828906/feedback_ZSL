package web_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"integritykernel/internal/web"
)

func TestDiagnosticAPIAndHTML(t *testing.T) {
	server, err := web.NewServer(t.TempDir(), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	httpServer := httptest.NewServer(server.Handler())
	defer httpServer.Close()

	resp, err := http.Get(httpServer.URL + "/api/state")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("state status %d", resp.StatusCode)
	}
	var state map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&state); err != nil {
		t.Fatal(err)
	}
	report, ok := state["report"].(map[string]any)
	if !ok || report["frames"] == nil {
		t.Fatalf("state missing report: %#v", state)
	}

	adjustResp, err := http.Get(httpServer.URL + "/api/adjust?block_size=12&verdict_threshold=0.65")
	if err != nil {
		t.Fatal(err)
	}
	defer adjustResp.Body.Close()
	if adjustResp.StatusCode != http.StatusOK {
		t.Fatalf("adjust status %d", adjustResp.StatusCode)
	}

	htmlResp, err := http.Get(httpServer.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer htmlResp.Body.Close()
	if htmlResp.StatusCode != http.StatusOK {
		t.Fatalf("html status %d", htmlResp.StatusCode)
	}
}
