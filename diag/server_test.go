package diag_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"lipsync/diag"
)

func TestDiagnosticStateAndHTTPRoutes(t *testing.T) {
	options := diag.DefaultBuildOptions()
	options.DBPath = filepath.Join(t.TempDir(), "diag.db")
	state, err := diag.BuildState(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	if !state.TruthMatches || state.Run == nil || !state.Run.Complete {
		t.Fatalf("diagnostic run did not match truth: %+v", state.Run)
	}
	if len(state.Run.Preemptions) == 0 || len(state.Run.Resumes) == 0 || len(state.Run.Abandonments) == 0 {
		t.Fatal("diagnostic sample lacks preemption/recovery/abandonment coverage")
	}
	if state.Faults["decode"].Code != "E_DECODE" ||
		state.Faults["order"].Code != "E_ORDER" ||
		state.Faults["cap"].Code != "E_CAP" {
		t.Fatalf("fault routes are incomplete: %+v", state.Faults)
	}
	handler := diag.NewHandler(state)
	server := httptest.NewServer(handler)
	defer server.Close()

	response, err := http.Get(server.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body := make([]byte, 4096)
	n, _ := response.Body.Read(body)
	if !strings.Contains(string(body[:n]), "逐帧通道仲裁") {
		t.Fatal("diagnostic page did not render frame section")
	}

	response, err = http.Get(server.URL + "/api/state")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var decoded diag.State
	if err := json.NewDecoder(response.Body).Decode(&decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Seed != 42 || !decoded.TruthMatches {
		t.Fatalf("unexpected API state: seed=%d truth=%v", decoded.Seed, decoded.TruthMatches)
	}

	response, err = http.Get(server.URL + "/api/fault?kind=decode")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var fault diag.FaultView
	if err := json.NewDecoder(response.Body).Decode(&fault); err != nil {
		t.Fatal(err)
	}
	if fault.Code != "E_DECODE" {
		t.Fatalf("unexpected fault response: %+v", fault)
	}
}
