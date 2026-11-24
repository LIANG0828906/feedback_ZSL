package terrain

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestDiagnosticHTTPAndEdit(t *testing.T) {
	project := GenerateSample(42)
	svc, err := NewService(project, ServiceOptions{DBPath: t.TempDir() + "/http.db"})
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()
	if _, err := svc.RebuildAll(context.Background()); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(NewHandler(svc))
	defer server.Close()

	resp, err := http.Get(server.URL + "/api/state")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("state status %d", resp.StatusCode)
	}
	var state ServiceState
	if err := json.NewDecoder(resp.Body).Decode(&state); err != nil {
		t.Fatal(err)
	}
	if len(state.Cache) != len(project.Blocks) || len(state.Reports) != len(project.SharedBoundaries) {
		t.Fatalf("state cache=%d reports=%d", len(state.Cache), len(state.Reports))
	}

	editResp, err := http.Post(server.URL+"/api/blocks/0/edit", "application/json", strings.NewReader(`{"sample_budget":32}`))
	if err != nil {
		t.Fatal(err)
	}
	defer editResp.Body.Close()
	var editPayload map[string]any
	if err := json.NewDecoder(editResp.Body).Decode(&editPayload); err != nil {
		t.Fatal(err)
	}
	if editPayload["ok"] != true {
		t.Fatalf("edit failed: %v", editPayload["run_error"])
	}
	if svc.proj.Blocks[0].SampleBudget != 32 {
		t.Fatalf("budget not edited: %d", svc.proj.Blocks[0].SampleBudget)
	}

	home, err := http.Get(server.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer home.Body.Close()
	buf := make([]byte, 4096)
	n, _ := home.Body.Read(buf)
	if !strings.Contains(string(buf[:n]), "Block Minimal") {
		t.Fatal("dashboard HTML not served")
	}
}
