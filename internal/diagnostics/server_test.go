package diagnostics

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"iconseg/internal/checkpoint"
	"iconseg/internal/core"
	"iconseg/internal/sample"
)

func TestLoopbackOnly(t *testing.T) {
	if err := ValidateLoopbackAddress("127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
	if err := ValidateLoopbackAddress("localhost:0"); err != nil {
		t.Fatal(err)
	}
	if err := ValidateLoopbackAddress("0.0.0.0:8090"); err == nil {
		t.Fatal("non-loopback address was accepted")
	}
}

func TestPreviewExactAndMaskEndpoints(t *testing.T) {
	icons := sample.Generate(20261125)
	store, err := checkpoint.Open(filepath.Join(t.TempDir(), "diag.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	server, err := New(store, icons, core.DefaultParams(), icons[0].Manifest.Seed)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(server.Handler())
	defer ts.Close()

	var preview map[string]any
	getJSON(t, ts.URL+"/api/preview?icon=0&flood=140&merge_cost=64", &preview)
	if preview["downsample"] == nil || preview["within_latency"] != true {
		t.Fatalf("unexpected preview response: %+v", preview)
	}
	var exact map[string]any
	getJSON(t, ts.URL+"/api/exact?icon=0&flood=140&merge_cost=64", &exact)
	if exact["snapshot_status"] != "complete" {
		t.Fatalf("exact snapshot status=%v", exact["snapshot_status"])
	}
	consistency, ok := exact["consistency"].(map[string]any)
	if !ok || consistency["consistent"] != true {
		t.Fatalf("consistency=%+v", exact["consistency"])
	}
	for _, path := range []string{"/mask.png?kind=preview&icon=0", "/mask.png?kind=exact&icon=0",
		"/mask.png?kind=truth&icon=0", "/icon.png?icon=0"} {
		response, err := http.Get(ts.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		if response.StatusCode != http.StatusOK {
			t.Fatalf("%s status=%d", path, response.StatusCode)
		}
		response.Body.Close()
	}
}

func getJSON(t *testing.T, url string, target any) {
	t.Helper()
	response, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("%s status=%d", url, response.StatusCode)
	}
	if err := json.NewDecoder(response.Body).Decode(target); err != nil {
		t.Fatal(err)
	}
}
