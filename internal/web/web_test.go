package web_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"audioforge/internal/app"
	"audioforge/internal/web"
)

func TestDashboardAndReportAPI(t *testing.T) {
	report := &app.Report{Seed: 7, SelectedBranch: "mobile-ms"}
	server := web.New(func() *app.Report { return report })
	baseURL, err := server.Start(0)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	if !strings.HasPrefix(baseURL, "http://127.0.0.1:") {
		t.Fatalf("UI bound to non-loopback address: %s", baseURL)
	}
	response, err := http.Get(baseURL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("dashboard status %d", response.StatusCode)
	}
	responseAPI, err := http.Get(baseURL + "/api/report")
	if err != nil {
		t.Fatal(err)
	}
	defer responseAPI.Body.Close()
	var decoded app.Report
	if err := json.NewDecoder(responseAPI.Body).Decode(&decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Seed != 7 || decoded.SelectedBranch != "mobile-ms" {
		t.Fatalf("unexpected report: %+v", decoded)
	}
}
