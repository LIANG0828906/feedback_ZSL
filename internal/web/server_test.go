package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"meshdoctor/internal/app"
	"meshdoctor/internal/pipeline"
)

func TestDiagnosticPageAndState(t *testing.T) {
	a, err := app.New(t.TempDir(), pipeline.DefaultParams())
	if err != nil {
		t.Fatal(err)
	}
	s := New(a)

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("page status=%d", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "MeshDoctor") {
		t.Fatal("diagnostic page missing title")
	}

	if _, err := a.Verify(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	req = httptest.NewRequest(http.MethodGet, "/api/state", nil)
	rec = httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("state status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"all_identical": true`) {
		t.Fatalf("state does not report identical paths: %.500s", rec.Body.String())
	}
}
