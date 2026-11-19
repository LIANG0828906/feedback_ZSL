package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"driftconverge/internal/engine"
	"driftconverge/internal/metrics"
	"driftconverge/internal/model"
)

func TestStatusEndpointReturnsStableJSON(t *testing.T) {
	status := model.ClusterStatus{
		RoundID:     "round-1",
		Cluster:     "cluster-prod",
		Branch:      "cluster-prod",
		GitCommit:   "abc",
		CommittedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		Observable:  true,
		DiffCount:   1,
		Convergence: model.ConclusionConverged,
		Differences: []model.Difference{{
			ResourceKey: "cluster-prod/cluster-prod/app",
			Field:       "image",
			Component:   "app",
			Expected:    "app:2",
			Actual:      "app:1",
			Resolution:  model.ResolutionConverged,
		}},
		// PolicyBlocks and ApplyResults intentionally nil to verify [] is emitted.
	}
	registry := metrics.New([]string{"cluster-dev", "cluster-prod"})
	registry.Update(status)
	server := New(
		&fakeRunner{},
		&fakeStatusStore{status: status},
		registry,
		[]string{"cluster-dev", "cluster-prod"},
	)
	request := httptest.NewRequest(http.MethodGet, "/api/v1/clusters/cluster-prod/status", nil)
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status code %d: %s", recorder.Code, recorder.Body.String())
	}
	var decoded map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("decode status: %v", err)
	}
	if decoded["round_id"] != "round-1" || decoded["convergence"] != "converged" {
		t.Fatalf("unexpected status JSON: %s", recorder.Body.String())
	}
	if _, ok := decoded["policy_blocks"].([]any); !ok {
		t.Fatalf("policy_blocks must be an array: %s", recorder.Body.String())
	}
	if _, ok := decoded["apply_results"].([]any); !ok {
		t.Fatalf("apply_results must be an array: %s", recorder.Body.String())
	}
}

func TestStatusEndpointRejectsUnknownCluster(t *testing.T) {
	server := New(
		&fakeRunner{},
		&fakeStatusStore{},
		metrics.New([]string{"cluster-dev", "cluster-prod"}),
		[]string{"cluster-dev", "cluster-prod"},
	)
	request := httptest.NewRequest(http.MethodGet, "/api/v1/status?cluster=other", nil)
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("expected 404, got %d", recorder.Code)
	}
	if !strings.Contains(recorder.Body.String(), "E_UNKNOWN_CLUSTER") {
		t.Fatalf("unexpected error body: %s", recorder.Body.String())
	}
}

func TestMetricsEndpoint(t *testing.T) {
	registry := metrics.New([]string{"cluster-dev", "cluster-prod"})
	registry.Update(model.ClusterStatus{
		Cluster:          "cluster-prod",
		Observable:       false,
		ExpectedCount:    2,
		DiffCount:        1,
		PolicyBlockCount: 1,
		BlockedCount:     1,
	})
	server := New(&fakeRunner{}, &fakeStatusStore{}, registry, []string{"cluster-dev", "cluster-prod"})
	request := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status code %d", recorder.Code)
	}
	body := recorder.Body.String()
	for _, expected := range []string{
		`drift_unobservable{cluster="cluster-prod"} 1`,
		`drift_expected_resources{cluster="cluster-dev"} 0`,
		`drift_policy_blocks{cluster="cluster-prod"} 1`,
	} {
		if !strings.Contains(body, expected) {
			t.Fatalf("missing %q in:\n%s", expected, body)
		}
	}
	if contentType := recorder.Header().Get("Content-Type"); !strings.Contains(contentType, "text/plain") {
		t.Fatalf("unexpected content type %q", contentType)
	}
}

func TestReconcileBusy(t *testing.T) {
	server := New(
		&fakeRunner{err: engine.ErrBusy},
		&fakeStatusStore{},
		metrics.New([]string{"cluster-dev", "cluster-prod"}),
		[]string{"cluster-dev", "cluster-prod"},
	)
	request := httptest.NewRequest(http.MethodPost, "/api/v1/reconcile", nil)
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusConflict {
		t.Fatalf("expected 409, got %d: %s", recorder.Code, recorder.Body.String())
	}
}

type fakeRunner struct {
	report model.RoundReport
	err    error
}

func (f *fakeRunner) RunRound(context.Context) (model.RoundReport, error) {
	return f.report, f.err
}

type fakeStatusStore struct {
	status model.ClusterStatus
	err    error
}

func (f *fakeStatusStore) LatestClusterStatus(context.Context, string) (model.ClusterStatus, error) {
	if f.err != nil {
		return model.ClusterStatus{}, f.err
	}
	return f.status, nil
}

func (f *fakeStatusStore) Health(context.Context) error {
	return nil
}
