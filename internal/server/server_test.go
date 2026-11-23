package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"textureworkbench/internal/sample"
)

func newTestServer(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	dataDir := t.TempDir()
	if _, err := sample.Generate(dataDir, 5150); err != nil {
		t.Fatalf("generate samples: %v", err)
	}
	workbench, err := New(dataDir, filepath.Join(dataDir, "checkpoints"))
	if err != nil {
		t.Fatalf("new workbench: %v", err)
	}
	server := httptest.NewServer(workbench.Handler())
	t.Cleanup(server.Close)
	return server, dataDir
}

func requestJSON(t *testing.T, client *http.Client, method, url string, body any) (int, map[string]any) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		reader = bytes.NewReader(data)
	}
	request, err := http.NewRequest(method, url, reader)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var decoded map[string]any
	if err := json.NewDecoder(response.Body).Decode(&decoded); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return response.StatusCode, decoded
}

func TestHTTPWorkbenchFlow(t *testing.T) {
	server, _ := newTestServer(t)
	client := server.Client()

	status, boot := requestJSON(t, client, "GET", server.URL+"/api/bootstrap", nil)
	if status != http.StatusOK {
		t.Fatalf("bootstrap status=%d body=%v", status, boot)
	}
	if boot["algorithm"] != "quant-v1" {
		t.Fatalf("unexpected algorithm: %v", boot["algorithm"])
	}

	bad := map[string]any{
		"image_id": "rgb8",
		"descriptor": map[string]any{
			"source_layout":    "rgb",
			"target_layout":    "rgba",
			"target_bit_depth": 12,
			"permutation":      []int{0, 1, 2},
			"fill":             "repeat_alpha",
		},
		"config": map[string]any{"session_key": "bad-depth"},
	}
	status, body := requestJSON(t, client, "POST", server.URL+"/api/sessions", bad)
	if status != http.StatusBadRequest || body["code"] != "E_PARA" || body["field"] != "target_bit_depth" {
		t.Fatalf("bad descriptor response status=%d body=%v", status, body)
	}

	create := map[string]any{
		"image_id": "rgb8",
		"descriptor": map[string]any{
			"id":               "http-rgb8-bgra8",
			"source_image":     "rgb8",
			"source_layout":    "rgb",
			"target_layout":    "bgra",
			"target_bit_depth": 8,
			"permutation":      []int{2, 1, 0},
			"fill":             "zero",
		},
		"config": map[string]any{
			"block_width":              32,
			"block_height":             32,
			"max_workers":              4,
			"buffer_pixels_per_worker": 1024,
			"queue_depth":              2,
			"parameter_version":        "http-v1",
			"session_key":              "http-rgb8",
		},
	}
	status, body = requestJSON(t, client, "POST", server.URL+"/api/sessions", create)
	if status != http.StatusCreated {
		t.Fatalf("create status=%d body=%v", status, body)
	}

	status, inverse := requestJSON(t, client, "GET", server.URL+"/api/sessions/http-rgb8/mapping/inverse", nil)
	if status != http.StatusOK || inverse["reversible"] != true {
		t.Fatalf("inverse status=%d body=%v", status, inverse)
	}

	status, _ = requestJSON(t, client, "POST", server.URL+"/api/sessions/http-rgb8/run", map[string]any{"max_blocks": 0})
	if status != http.StatusAccepted {
		t.Fatalf("run status=%d", status)
	}
	deadline := time.Now().Add(3 * time.Second)
	complete := false
	for time.Now().Before(deadline) {
		status, state := requestJSON(t, client, "GET", server.URL+"/api/sessions/http-rgb8", nil)
		if status != http.StatusOK {
			t.Fatalf("state status=%d", status)
		}
		session := state["session"].(map[string]any)
		if session["complete"] == true {
			complete = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !complete {
		t.Fatalf("session did not complete")
	}

	status, comparison := requestJSON(t, client, "POST", server.URL+"/api/sessions/http-rgb8/compare", map[string]any{})
	if status != http.StatusOK {
		t.Fatalf("compare status=%d body=%v", status, comparison)
	}
	comp := comparison["comparison"].(map[string]any)
	if comp["match"] != true {
		t.Fatalf("comparison mismatch: %v", comp)
	}

	response, err := client.Get(server.URL + "/api/sessions/http-rgb8/export?format=png")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	data, _ := io.ReadAll(response.Body)
	if response.StatusCode != http.StatusOK || !bytes.HasPrefix(data, []byte{0x89, 'P', 'N', 'G'}) {
		t.Fatalf("export status=%d bytes=%d", response.StatusCode, len(data))
	}

	lossy := map[string]any{
		"image_id": "rgba8",
		"descriptor": map[string]any{
			"id":               "http-rgba8-rgb8",
			"source_image":     "rgba8",
			"source_layout":    "rgba",
			"target_layout":    "rgb",
			"target_bit_depth": 8,
			"permutation":      []int{0, 1, 2, 3},
			"fill":             "zero",
		},
		"config": map[string]any{"session_key": "http-rgba8"},
	}
	status, body = requestJSON(t, client, "POST", server.URL+"/api/sessions", lossy)
	if status != http.StatusCreated {
		t.Fatalf("lossy create status=%d body=%v", status, body)
	}
	status, body = requestJSON(t, client, "GET", server.URL+"/api/sessions/http-rgba8/mapping/inverse", nil)
	if status != http.StatusUnprocessableEntity || body["code"] != "E_LOSS" {
		t.Fatalf("lossy inverse status=%d body=%v", status, body)
	}
}

func TestEmbeddedWorkbenchPage(t *testing.T) {
	server, _ := newTestServer(t)
	response, err := server.Client().Get(server.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	if response.StatusCode != http.StatusOK || !bytes.Contains(body, []byte("TEXEL FORGE")) {
		t.Fatalf("embedded UI missing")
	}
}
