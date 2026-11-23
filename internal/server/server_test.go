package server

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"stitchworkbench/internal/samplegen"
	"stitchworkbench/internal/session"
)

func TestAPIAndStatic(t *testing.T) {
	data := t.TempDir()
	work := t.TempDir()
	if err := samplegen.Generate(samplegen.Config{OutDir: data, Seed: 42, Sets: 1}); err != nil {
		t.Fatal(err)
	}
	svc, err := session.NewService(data, work)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(New(svc))
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 || !bytes.Contains(b, []byte("Stitch Workbench")) {
		t.Fatalf("index status=%d body=%q", resp.StatusCode, string(b[:min(80, len(b))]))
	}
	resp, err = http.Get(ts.URL + "/api/sets")
	if err != nil {
		t.Fatal(err)
	}
	var sets struct {
		Sets []session.SetInfo `json:"sets"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&sets)
	resp.Body.Close()
	if len(sets.Sets) != 1 || sets.Sets[0].ID != "harbor-06" {
		t.Fatalf("sets=%+v", sets)
	}
	body, _ := json.Marshal(map[string]any{"feature_budget": 9000})
	resp, err = http.Post(ts.URL+"/api/sets/harbor-06/params", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	var snap struct {
		Version int64 `json:"version"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&snap)
	resp.Body.Close()
	if snap.Version != 2 {
		t.Fatalf("version=%d", snap.Version)
	}
	resp, err = http.Post(ts.URL+"/api/sets/harbor-06/preview", "application/json", bytes.NewReader([]byte(`{"version":1}`)))
	if err != nil {
		t.Fatal(err)
	}
	var envelope map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&envelope)
	resp.Body.Close()
	if resp.StatusCode != 422 || envelope["code"] != "E_VER" {
		t.Fatalf("stale preview status=%d envelope=%v", resp.StatusCode, envelope)
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
