package dashboard

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"audiocheck/internal/model"
)

func TestDashboardServesReport(t *testing.T) {
	app := &App{Report: &model.Report{
		Frames: []model.FrameReport{{Frame: 0, SSIMFixed: model.MetricScale}},
		Blocks: []model.BlockResult{{Block: model.BlockCoord{Index: 0}}},
	}}
	req := httptest.NewRequest(http.MethodGet, "/api/report", nil)
	rec := httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "ssim_fixed") {
		t.Fatalf("report api code=%d body=%s", rec.Code, rec.Body.String()[:min(200, len(rec.Body.String()))])
	}

	req = httptest.NewRequest(http.MethodGet, "/", nil)
	rec = httptest.NewRecorder()
	app.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "块状态矩阵") {
		t.Fatalf("dashboard html code=%d", rec.Code)
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
