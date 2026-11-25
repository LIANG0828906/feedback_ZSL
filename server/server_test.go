package server_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"audiofp"
	"audiofp/samplegen"
	httpserver "audiofp/server"
)

func TestHTTPWorkflowAndCompare(t *testing.T) {
	material := samplegen.GenerateDefault()
	srv := httpserver.New(httpserver.Config{
		Material: material, Params: audiofp.DefaultParams(),
		Limits: audiofp.ResourceLimits{MaxWorkers: 2, PerWorkerBufferSamples: 32768, MaxQueuedJobs: 8},
		DBPath: filepath.Join(t.TempDir(), "http-checkpoint.db"),
	})
	if err := srv.InitialAnnotation(context.Background()); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	state := getJSON(t, ts.URL+"/api/state")
	if int(state["committed_windows"].(float64)) != int(state["total_windows"].(float64)) {
		t.Fatalf("initial annotation should commit every window")
	}

	compare := postJSON(t, ts.URL+"/api/compare", map[string]any{})
	if compare["compare_error"] != nil {
		t.Fatalf("compare error: %#v", compare["compare_error"])
	}
	if compare["compare"].(map[string]any)["consistent"] != true {
		t.Fatalf("compare should be consistent: %#v", compare["compare"])
	}

	postJSON(t, ts.URL+"/api/reset", map[string]any{"committed_prefix": 20})
	postJSON(t, ts.URL+"/api/windows/21/simulate-failure", map[string]any{})
	retry := postJSON(t, ts.URL+"/api/windows/21/retry", map[string]any{})
	if retry["windows"].([]any)[21].(map[string]any)["state"] != "committed" {
		t.Fatalf("retry should commit selected window")
	}

	inc := postJSON(t, ts.URL+"/api/annotate", map[string]any{"mode": "incremental"})
	if int(inc["committed_windows"].(float64)) != int(inc["total_windows"].(float64)) {
		t.Fatalf("incremental should finish all windows")
	}
	result := inc["result"].(map[string]any)
	quality := result["quality"].(map[string]any)
	if quality["passed"] != true {
		t.Fatalf("quality should pass after incremental annotation")
	}

	bad, err := http.Post(ts.URL+"/api/params", "application/json", bytes.NewReader([]byte(`{"frame_length":16}`)))
	if err != nil {
		t.Fatal(err)
	}
	defer bad.Body.Close()
	if bad.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 for E_PARA, got %d", bad.StatusCode)
	}
}

func getJSON(t *testing.T, url string) map[string]any {
	t.Helper()
	res, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("GET %s status %d", url, res.StatusCode)
	}
	var out map[string]any
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	return out
}

func postJSON(t *testing.T, url string, body any) map[string]any {
	t.Helper()
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	res, err := http.Post(url, "application/json", bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(res.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusOK {
		t.Fatalf("POST %s status %d body=%#v", url, res.StatusCode, out)
	}
	return out
}
