package web_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"mocapworkbench/internal/loader"
	"mocapworkbench/internal/model"
	"mocapworkbench/internal/pipeline"
	"mocapworkbench/internal/retarget"
	"mocapworkbench/internal/sample"
	"mocapworkbench/internal/verify"
	"mocapworkbench/internal/web"
)

func TestDashboardAPI(t *testing.T) {
	dir := t.TempDir()
	if _, err := sample.Generate(dir, 4208691); err != nil {
		t.Fatal(err)
	}
	clip, err := loader.LoadClip(filepath.Join(dir, "clips", "clip_short.json"))
	if err != nil {
		t.Fatal(err)
	}
	target, err := loader.LoadTarget(filepath.Join(dir, "targets", "target_tall_29.json"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := model.DefaultConfig()
	db := filepath.Join(t.TempDir(), "dashboard.db")
	animation, err := pipeline.Run(clip, target, cfg, pipeline.Options{StorePath: db})
	if err != nil {
		t.Fatal(err)
	}
	exactCfg := cfg
	exactCfg.Mode = "exact"
	exact, err := retarget.Run(clip, target, exactCfg)
	if err != nil {
		t.Fatal(err)
	}
	report, err := verify.Run(clip, target, cfg, filepath.Join(t.TempDir(), "verify.db"))
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(web.New(clip, target, animation, exact, report, db, cfg).Handler())
	defer server.Close()

	response, err := http.Get(server.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(response.Body)
	response.Body.Close()
	if response.StatusCode != http.StatusOK || !strings.Contains(string(body), "CONSTRAINT WORKBENCH") {
		t.Fatalf("dashboard page failed: %d", response.StatusCode)
	}

	response, err = http.Get(server.URL + "/api/state")
	if err != nil {
		t.Fatal(err)
	}
	var dashboard web.Dashboard
	if err := json.NewDecoder(response.Body).Decode(&dashboard); err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if dashboard.ClipID != clip.ID || len(dashboard.Frames) != len(clip.Frames) || len(dashboard.Skeleton) != len(target.Joints) {
		t.Fatalf("unexpected dashboard payload: %#v", dashboard)
	}
	if dashboard.Mode != animation.Plan.Mode || dashboard.Strategy == "" {
		t.Fatalf("adaptive mode not surfaced: %#v", dashboard.Plan)
	}

	response, err = http.Post(server.URL+"/api/retry", "application/json", strings.NewReader(`{"frame":9}`))
	if err != nil {
		t.Fatal(err)
	}
	var retry map[string]any
	if err := json.NewDecoder(response.Body).Decode(&retry); err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if retry["ok"] != true || retry["equal"] != true {
		t.Fatalf("retry verification failed: %#v", retry)
	}

	response, err = http.Get(server.URL + "/api/export")
	if err != nil {
		t.Fatal(err)
	}
	var exported model.Animation
	if err := json.NewDecoder(response.Body).Decode(&exported); err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if len(exported.Frames) != len(animation.Frames) {
		t.Fatalf("export truncated animation")
	}
}
