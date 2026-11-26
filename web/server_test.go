package web_test

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"dialoguefit/web"
)

func TestDashboardAndState(t *testing.T) {
	server := web.New(web.Config{Report: map[string]any{"ok": true}})
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "DialogueFit") {
		t.Fatalf("dashboard code=%d body=%q", rec.Code, rec.Body.String()[:min(200, rec.Body.Len())])
	}

	req = httptest.NewRequest(http.MethodGet, "/api/state", nil)
	rec = httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"report"`) {
		t.Fatalf("state code=%d body=%s", rec.Code, rec.Body.String())
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
