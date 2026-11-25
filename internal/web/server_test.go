package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func TestDiagnosticHandlerServesPageAndJSON(t *testing.T) {
	dash, err := BuildDashboard(context.Background(), filepath.Join(t.TempDir(), "web"))
	if err != nil {
		t.Fatal(err)
	}
	if dash.LiveMismatch != nil || dash.ResumeMismatch != nil || !dash.IncrementalMatch {
		t.Fatalf("dashboard comparisons not clean: live=%+v resume=%+v inc=%v", dash.LiveMismatch, dash.ResumeMismatch, dash.IncrementalMatch)
	}
	srv := httptest.NewServer(NewHandler(dash))
	defer srv.Close()

	resp, err := http.Get(srv.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	buf := make([]byte, 4096)
	n, _ := resp.Body.Read(buf)
	if !strings.Contains(string(buf[:n]), "战斗判定") {
		t.Fatal("diagnostic HTML did not render expected title")
	}

	api, err := http.Get(srv.URL + "/api/dashboard")
	if err != nil {
		t.Fatal(err)
	}
	defer api.Body.Close()
	if api.StatusCode != http.StatusOK || !strings.HasPrefix(api.Header.Get("Content-Type"), "application/json") {
		t.Fatalf("api response status=%d type=%s", api.StatusCode, api.Header.Get("Content-Type"))
	}
}
