package demo

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	acoustic "levelacoustic"
	"levelacoustic/internal/samplegen"
)

func TestRunSuiteThreePathsAndServer(t *testing.T) {
	project, err := samplegen.Sample(samplegen.DefaultSeed)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	report, err := RunSuite(ctx, project, samplegen.DefaultSeed, t.TempDir(), acoustic.WorkerOptions{MaxWorkers: 3, PerWorkerBuffer: 2})
	if err != nil {
		t.Fatal(err)
	}
	if !report.AllPathsEqual {
		t.Fatalf("three paths differ: %+v", report.Comparisons)
	}
	if !report.Truth.Passed {
		t.Fatalf("truth mismatch: %+v", report.Truth.Mismatches)
	}
	if len(report.Comparisons) != 3 {
		t.Fatalf("expected three pairwise comparisons, got %d", len(report.Comparisons))
	}
	if report.Resume.Restore == nil || len(report.Resume.Restore.LoadedRooms) != 1 {
		t.Fatalf("resume did not load the first checkpoint: %+v", report.Resume.Restore)
	}
	if report.Invalidation == nil {
		t.Fatalf("invalidation report missing: %s", report.InvalidationError)
	}
	if len(report.Invalidation.InvalidatedRooms) != 0 || len(report.Invalidation.ReusedRooms) != 3 {
		t.Fatalf("unused door should reuse all rooms: %+v", report.Invalidation)
	}
	for _, path := range []*PathOutput{report.Preview, report.Resume, report.Full} {
		if path.Result == nil || len(path.Result.CommitSequence) != 3 {
			t.Fatalf("path %s has incomplete commits: %+v", path.Name, path)
		}
	}

	server := NewServer(report)
	handler := server.Handler()
	index := httptest.NewRecorder()
	handler.ServeHTTP(index, httptest.NewRequest(http.MethodGet, "/", nil))
	if index.Code != http.StatusOK {
		t.Fatalf("index status=%d", index.Code)
	}
	if !strings.Contains(index.Body.String(), "三路径比对") || !strings.Contains(index.Body.String(), "canvas") {
		t.Fatal("index page does not contain scene and comparison UI")
	}
	api := httptest.NewRecorder()
	handler.ServeHTTP(api, httptest.NewRequest(http.MethodGet, "/api/report", nil))
	if api.Code != http.StatusOK {
		t.Fatalf("api status=%d", api.Code)
	}
	var decoded SuiteReport
	if err := json.Unmarshal(api.Body.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	if !decoded.AllPathsEqual || decoded.Truth.ActualPaths != 8 {
		t.Fatalf("unexpected API report: equal=%v paths=%d", decoded.AllPathsEqual, decoded.Truth.ActualPaths)
	}
	health := httptest.NewRecorder()
	handler.ServeHTTP(health, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if health.Code != http.StatusOK || health.Body.String() != "ok\n" {
		t.Fatalf("unexpected health response: %d %q", health.Code, health.Body.String())
	}
}
