package httpserver_test

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"volumelab/pkg/httpserver"
	"volumelab/pkg/service"
	"volumelab/pkg/webui"
)

func TestHTTPWorkflow(t *testing.T) {
	dir := t.TempDir()
	svc := service.New(service.Options{MaxParallelism: 3, MaxBufferPixels: 64})
	handler := httpserver.Handler(svc, webui.FS)
	server := httptest.NewServer(handler)
	defer server.Close()

	generateBody, _ := json.Marshal(map[string]any{
		"action":  "generate",
		"payload": map[string]any{"outputDir": dir, "seed": 42},
	})
	response, err := http.Post(server.URL+"/api/invoke", "application/json", bytes.NewReader(generateBody))
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()

	openBody, _ := json.Marshal(map[string]any{
		"action":  "open",
		"payload": map[string]any{"manifestPath": dir + "/manifest.json", "volumeId": "vol_a"},
	})
	response, err = http.Post(server.URL+"/api/invoke", "application/json", bytes.NewReader(openBody))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var result map[string]any
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	if result["ok"] != true {
		t.Fatalf("open failed: %+v", result)
	}

	index, err := http.Get(server.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer index.Body.Close()
	page, err := io.ReadAll(index.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(page), "VolumeLab") {
		t.Fatal("embedded frontend was not served")
	}
}
