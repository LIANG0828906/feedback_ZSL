package replaybench_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"testing"

	"replaybench/internal/diagnostics"
	"replaybench/internal/engine"
	"replaybench/internal/model"
)

func TestDiagnosticsEndpoints(t *testing.T) {
	svc, err := diagnostics.NewService(filepath.Join(t.TempDir(), "diag.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()
	server := httptest.NewServer(svc.Handler())
	defer server.Close()

	for _, path := range []string{"/healthz", "/api/overview", "/api/full?format=3", "/api/incremental", "/api/checkpoint", "/api/scheduler", "/"} {
		resp, err := http.Get(server.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s status=%d", path, resp.StatusCode)
		}
		resp.Body.Close()
	}

	resp, err := http.Get(server.URL + "/api/incremental")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var inc diagnostics.IncrementalReport
	if err := json.NewDecoder(resp.Body).Decode(&inc); err != nil {
		t.Fatal(err)
	}
	if !inc.FullDiffMatched {
		t.Fatalf("incremental diagnostic mismatch: %s", inc.DiffError)
	}

	resp, err = http.Get(server.URL + "/api/full?format=99")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnprocessableEntity {
		t.Fatalf("unknown format status=%d", resp.StatusCode)
	}
	var apiErr struct {
		Code  string `json:"Code"`
		Field string `json:"Field"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&apiErr); err != nil {
		t.Fatal(err)
	}
	if apiErr.Code != "E_SCHEMA" || apiErr.Field != "$.query.format" {
		t.Fatalf("api error = %+v", apiErr)
	}
}

func TestDeterministicSnapshotAcrossRepeatedRuns(t *testing.T) {
	loaded := loadSample(t, model.FormatCooldownQueue)
	first, err := engine.Run(context.Background(), loaded.Replay)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 20; i++ {
		next, err := engine.Run(context.Background(), loaded.Replay)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(first.Snapshots, next.Snapshots) {
			t.Fatalf("run %d produced different snapshots", i)
		}
	}
}
