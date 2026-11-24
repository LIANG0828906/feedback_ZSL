package diag_test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"

	"levelacoustic/internal/diag"
	"levelacoustic/internal/generator"
	"levelacoustic/internal/model"
	"levelacoustic/internal/solver"
)

func TestDiagnosticPageAndEditAPI(t *testing.T) {
	ctx := context.Background()
	sample, err := generator.Generate(42)
	if err != nil {
		t.Fatal(err)
	}
	engine, err := solver.NewEngine(ctx, sample.Project, solver.Config{Workers: 2, MaxBuffers: 1, BufferSamples: 8, CheckTruth: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := engine.Solve(ctx); err != nil {
		t.Fatal(err)
	}
	server := diag.New(engine, filepath.Join(t.TempDir(), "edited.json"))
	ts := httptest.NewServer(server.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("page status = %d: %s", resp.StatusCode, body)
	}
	html := string(body)
	for _, marker := range []string{"Room State Matrix", "Per-Listener Decay Curves", "Invalidation Propagation", "Worker / Buffer Queue"} {
		if !strings.Contains(html, marker) {
			t.Fatalf("page missing %q", marker)
		}
	}

	form := url.Values{
		"op":            {"set_room_param"},
		"room_id":       {"R6"},
		"rt60_ms":       {"700"},
		"density_milli": {"800"},
		"gain_milli":    {"1100"},
	}
	client := &http.Client{CheckRedirect: func(req *http.Request, via []*http.Request) error {
		return http.ErrUseLastResponse
	}}
	resp, err = client.PostForm(ts.URL+"/api/edit", form)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("edit status = %d", resp.StatusCode)
	}

	resp, err = http.Get(ts.URL + "/api/state")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var state struct {
		Result struct {
			Progress   string   `json:"progress"`
			Recomputed []string `json:"recomputed_rooms"`
			Reused     []string `json:"reused_rooms"`
		} `json:"result"`
		Invalidation *model.InvalidationReport `json:"invalidation"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&state); err != nil {
		t.Fatal(err)
	}
	if state.Result.Progress != "6/6" {
		t.Fatalf("progress = %s", state.Result.Progress)
	}
	if strings.Join(state.Result.Recomputed, ",") != "R5,R6" {
		t.Fatalf("recomputed = %v", state.Result.Recomputed)
	}
	if len(state.Result.Reused) != 4 {
		t.Fatalf("reused = %v", state.Result.Reused)
	}
	if state.Invalidation == nil || len(state.Invalidation.InvalidatedRooms) != 2 {
		t.Fatalf("invalidation = %+v", state.Invalidation)
	}
}
