package server_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"ms-workbench/internal/engine"
	"ms-workbench/internal/server"
)

func TestHTTPApplicationLifecycle(t *testing.T) {
	cfg := engine.DefaultConfig()
	cfg.ChunkDelayMS = 2
	app, err := server.New(server.Options{DBDir: t.TempDir(), Seed: 42, Config: cfg})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(app.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	index, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !bytes.Contains(index, []byte("SPECTRA")) {
		t.Fatalf("index status=%d", resp.StatusCode)
	}

	body := []byte(`{"scene":"neon-foundry","schemaVersion":3,"mode":"full"}`)
	resp, err = http.Post(ts.URL+"/api/session", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create session status=%d", resp.StatusCode)
	}

	var state map[string]any
	deadline := time.Now().Add(10 * time.Second)
	for {
		resp, err = http.Get(ts.URL + "/api/session")
		if err != nil {
			t.Fatal(err)
		}
		if err := json.NewDecoder(resp.Body).Decode(&state); err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		view := state["view"].(map[string]any)
		if view["committedSegments"].(float64) == view["totalSegments"].(float64) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("session did not complete: %+v", view)
		}
		time.Sleep(30 * time.Millisecond)
	}
	if len(state["patches"].([]any)) != 7 {
		t.Fatalf("patch measurements missing")
	}
	if len(state["dependencies"].([]any)) == 0 {
		t.Fatal("dependency ledger missing")
	}

	for _, path := range []string{
		"/api/image?kind=composite&overlay=1",
		"/api/image?kind=band&band=B450",
		"/api/image?kind=heatmap",
		"/api/histogram",
		"/api/export",
	} {
		resp, err = http.Get(ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("%s status=%d", path, resp.StatusCode)
		}
	}

	resp, err = http.Post(ts.URL+"/api/verify", "application/json", strings.NewReader(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	var verification map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&verification); err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 || verification["equal"] != true {
		t.Fatalf("verify status=%d report=%+v", resp.StatusCode, verification)
	}
}
