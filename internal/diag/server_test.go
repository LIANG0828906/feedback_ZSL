package diag_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"terrain-occlusion/internal/diag"
	"terrain-occlusion/internal/model"
)

func TestDiagnosticServerServesPageAndAPI(t *testing.T) {
	server := diag.NewServer(&model.DiagnosticReport{OK: true})
	req := httptest.NewRequest(http.MethodGet, "/api/health", nil)
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"ok":true`) {
		t.Fatalf("health code=%d body=%s", rec.Code, rec.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/", nil)
	rec = httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "地形高度场") {
		t.Fatalf("page code=%d and expected title", rec.Code)
	}

	req = httptest.NewRequest(http.MethodGet, "/api/diagnostic", nil)
	rec = httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"OK"`) && !strings.Contains(rec.Body.String(), `"ok"`) {
		t.Fatalf("api code=%d body=%s", rec.Code, rec.Body.String()[:min(200, len(rec.Body.String()))])
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
