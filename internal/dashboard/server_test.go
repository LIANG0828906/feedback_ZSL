package dashboard_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"semanticshading/internal/dashboard"
	"semanticshading/internal/diagnostics"
)

func TestDashboardRoutesAndInjectionAPI(t *testing.T) {
	server := dashboard.NewServer(&dashboard.State{Title: "test"}, filepath.Join(t.TempDir(), "diagnostics"))
	ts := httptest.NewServer(server.Handler())
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/api/health")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("health status = %d", resp.StatusCode)
	}

	resp, err = http.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	buf := make([]byte, 4096)
	n, _ := resp.Body.Read(buf)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(buf[:n]), "语义着色") {
		t.Fatalf("index did not render expected title, status=%d", resp.StatusCode)
	}

	resp, err = http.Get(ts.URL + "/api/inject?kind=state")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var injection diagnostics.Report
	if err := json.NewDecoder(resp.Body).Decode(&injection); err != nil {
		t.Fatal(err)
	}
	if !injection.OK || injection.ObservedCode != "E_STATE" {
		t.Fatalf("injection = %#v", injection)
	}
}
