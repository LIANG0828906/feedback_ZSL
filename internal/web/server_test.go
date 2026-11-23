package web

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"meshforge/internal/generator"
	"meshforge/internal/repair"
)

func TestLoopbackValidation(t *testing.T) {
	for _, addr := range []string{"127.0.0.1:8787", "[::1]:8787", "localhost:8787"} {
		if err := ValidateLoopback(addr); err != nil {
			t.Fatalf("%s should be allowed: %v", addr, err)
		}
	}
	for _, addr := range []string{"0.0.0.0:8787", "192.168.1.2:8787", ":8787"} {
		if err := ValidateLoopback(addr); err == nil {
			t.Fatalf("%s should be rejected", addr)
		}
	}
}

func TestHTTPViewsAndRun(t *testing.T) {
	sample, err := generator.Generate(3, 7, "web")
	if err != nil {
		t.Fatal(err)
	}
	cfg := repair.DefaultConfig()
	cfg.Mode = repair.ModeExact
	srv, err := New(sample, cfg, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if !strings.Contains(string(body), "<canvas") {
		t.Fatal("UI does not contain canvas")
	}

	resp, err = http.Get(ts.URL + "/api/mesh?which=original")
	if err != nil {
		t.Fatal(err)
	}
	var original MeshPayload
	if err := json.NewDecoder(resp.Body).Decode(&original); err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if len(original.Faces) != len(sample.Mesh.Faces) || len(original.Highlights) == 0 {
		t.Fatalf("bad original payload: faces=%d highlights=%d", len(original.Faces), len(original.Highlights))
	}

	resp, err = http.Post(ts.URL+"/api/run", "application/json", nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		buf, _ := io.ReadAll(resp.Body)
		t.Fatalf("run status=%d body=%s", resp.StatusCode, buf)
	}
	_ = resp.Body.Close()

	resp, err = http.Get(ts.URL + "/api/mesh?which=repaired")
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("repaired status=%d", resp.StatusCode)
	}
	var repaired MeshPayload
	if err := json.NewDecoder(resp.Body).Decode(&repaired); err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if len(repaired.Faces) != sample.Truth.Repaired.Faces {
		t.Fatalf("repaired faces=%d want %d", len(repaired.Faces), sample.Truth.Repaired.Faces)
	}
}
