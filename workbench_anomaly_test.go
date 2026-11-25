package chaosdiag_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"chaosdiag/model"
	"chaosdiag/workbench"
)

func TestWorkbenchAnomalyCodesKeepStateAvailable(t *testing.T) {
	svc, err := workbench.NewService(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{
		"cycle_dependency":               model.CodeCycleDependency,
		"missing_service_target":         model.CodeTargetMissingService,
		"negative_delay":                 model.CodeNegativeDelay,
		"zero_timeout_budget":            model.CodeTimeoutBudgetZero,
		"concurrency_limit_out_of_range": model.CodeConcurrencyLimitOutOfRange,
		"truncated_checkpoint":           model.CodeCheckpointCorrupt,
		"version_mismatch_snapshot":      model.CodeCheckpointVersionMismatch,
	}
	for anomalyType, wantCode := range cases {
		resp := svc.Anomaly(workbench.AnomalyRequest{Type: anomalyType})
		if resp.OK || resp.Code != wantCode || resp.Field == "" {
			t.Fatalf("%s response=%+v want code %s with field/context", anomalyType, resp, wantCode)
		}
		if !resp.Available {
			t.Fatalf("%s marked workbench unavailable", anomalyType)
		}
	}

	state, err := svc.State("v3")
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Report.Results) != len(state.Project.Requests) {
		t.Fatal("committed report unavailable after anomalies")
	}

	demo, err := svc.CheckpointDemo("v2")
	if err != nil {
		t.Fatal(err)
	}
	if !demo.FullResultsEqual || demo.Partial.LastTick != 10 {
		t.Fatalf("checkpoint demo equal=%v tick=%d", demo.FullResultsEqual, demo.Partial.LastTick)
	}
}

func TestWorkbenchHTTPRoutes(t *testing.T) {
	server, err := workbench.NewServer(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/state?version=v1", nil)
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "R-01-BASE-AUTH") {
		t.Fatalf("state route code=%d body=%s", rec.Code, rec.Body.String()[:min(200, rec.Body.Len())])
	}

	req = httptest.NewRequest(http.MethodGet, "/", nil)
	rec = httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "ChaosDiag") {
		t.Fatalf("index route code=%d", rec.Code)
	}
}
