package diag_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	ballisticgrid "ballisticgrid"
	"ballisticgrid/internal/diag"
)

func TestDiagnosticDashboardAndInjectionAPI(t *testing.T) {
	level := ballisticgrid.Generate(ballisticgrid.DefaultSeed)
	cam := ballisticgrid.DefaultCamera()
	scene, err := ballisticgrid.ProjectScene(level, cam)
	if err != nil {
		t.Fatal(err)
	}
	h := (&diag.Dashboard{Level: level, Camera: cam, Scene: scene}).Handler()

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "分块覆盖") {
		t.Fatalf("index status=%d body=%s", rec.Code, rec.Body.String()[:min(200, rec.Body.Len())])
	}

	req = httptest.NewRequest(http.MethodGet, "/api/inject?kind=block", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "E_BLOCK") {
		t.Fatalf("block api status=%d body=%s", rec.Code, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/diagram.svg", nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Header().Get("Content-Type"), "svg") {
		t.Fatalf("svg status=%d type=%s", rec.Code, rec.Header().Get("Content-Type"))
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
