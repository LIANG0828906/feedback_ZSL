package server_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"qualitybench/internal/dataset"
	"qualitybench/internal/generator"
	"qualitybench/internal/server"
	"qualitybench/internal/store"
)

func TestDashboardStateImageAndControl(t *testing.T) {
	opts := generator.DefaultOptions()
	opts.ImageCount = 1
	ds, err := generator.Generate(opts)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "dashboard.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	svc := server.New(fromGenerator(ds), st, server.Options{Workers: 2, ValidatePaths: false})
	svc.Start()
	ts := httptest.NewServer(svc.Handler())
	defer ts.Close()

	var state stateJSON
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(ts.URL + "/api/state")
		if err != nil {
			t.Fatal(err)
		}
		err = json.NewDecoder(resp.Body).Decode(&state)
		_ = resp.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		if state.Status == "completed" {
			break
		}
		time.Sleep(80 * time.Millisecond)
	}
	if state.Status != "completed" {
		t.Fatalf("dashboard did not complete: %s %s", state.Status, state.Message)
	}
	if len(state.Images) != 1 || len(state.Images[0].Evals) == 0 {
		t.Fatalf("missing heatmap evaluations: %+v", state.Images)
	}
	if len(state.Invalidations) == 0 {
		t.Fatal("expected invalidation demo data")
	}
	imgResp, err := http.Get(ts.URL + "/api/image?image=img-00&step=2")
	if err != nil {
		t.Fatal(err)
	}
	png, _ := io.ReadAll(imgResp.Body)
	_ = imgResp.Body.Close()
	if len(png) < 8 || string(png[:8]) != "\x89PNG\r\n\x1a\n" {
		t.Fatalf("not a PNG: %x", png[:min(8, len(png))])
	}
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/retry?image=img-00&step=2&x=16&y=16", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("retry failed: %s", body)
	}
	page, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	html, _ := io.ReadAll(page.Body)
	_ = page.Body.Close()
	if len(html) < 1000 {
		t.Fatalf("diagnostic page too small: %d", len(html))
	}
}

func TestPauseAndCancel(t *testing.T) {
	opts := generator.DefaultOptions()
	opts.ImageCount = 1
	ds, err := generator.Generate(opts)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(filepath.Join(t.TempDir(), "control.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	svc := server.New(fromGenerator(ds), st, server.Options{Workers: 2, TaskDelay: 3 * time.Millisecond, ValidatePaths: false})
	svc.Start()
	svc.Pause()
	if !svc.State().Paused {
		t.Fatal("pause did not set paused state")
	}
	svc.Resume()
	svc.Cancel()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if svc.State().Status == "canceled" {
			return
		}
		time.Sleep(30 * time.Millisecond)
	}
	t.Fatalf("service did not cancel: %+v", svc.State())
}

type stateJSON struct {
	Status        string `json:"status"`
	Message       string `json:"message"`
	Invalidations []any  `json:"invalidations"`
	Images        []struct {
		ID    string `json:"id"`
		Evals []any  `json:"evaluations"`
	} `json:"images"`
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func fromGenerator(ds *generator.Dataset) *dataset.Dataset {
	return &dataset.Dataset{
		Project: ds.Project, Truth: ds.Truth, Baseline: ds.Baseline,
		ProjectFP: ds.ProjectFP, TruthFP: ds.TruthFP,
	}
}
