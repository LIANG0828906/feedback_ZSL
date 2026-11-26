package workbench

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHTTPRoutesSmoke(t *testing.T) {
	svc := newTestService(t)
	handler := NewHandler(svc)

	get := func(path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec
	}

	health := get("/api/health")
	if health.Code != http.StatusOK || !strings.Contains(health.Body.String(), "enginelab-workbench") {
		t.Fatalf("health code=%d body=%s", health.Code, health.Body.String())
	}
	index := get("/")
	if index.Code != http.StatusOK || !strings.Contains(index.Body.String(), "日志与崩溃恢复") {
		t.Fatalf("index code=%d", index.Code)
	}
	versions := get("/api/versions")
	if versions.Code != http.StatusOK || !strings.Contains(versions.Body.String(), `"equal":true`) {
		t.Fatalf("versions code=%d body=%s", versions.Code, versions.Body.String())
	}
	moment := get("/api/timeline/2")
	if moment.Code != http.StatusOK || !strings.Contains(moment.Body.String(), "m2-overwrite") {
		t.Fatalf("moment code=%d body=%s", moment.Code, moment.Body.String()[:min(200, moment.Body.Len())])
	}
	resume := get("/api/checkpoint/resume")
	if resume.Code != http.StatusOK || !strings.Contains(resume.Body.String(), "committed_checkpoint") {
		t.Fatalf("resume code=%d body=%s", resume.Code, resume.Body.String())
	}
}
