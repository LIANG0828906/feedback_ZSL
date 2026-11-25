package diagnostics

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestBuildReportPassesAllChecks(t *testing.T) {
	report, err := BuildReport(42)
	if err != nil {
		t.Fatal(err)
	}
	if !report.Pass() {
		t.Fatalf("failed checks: %#v", report.FailedChecks())
	}
	if len(report.Versions) != 3 || len(report.RoundTrips) != 2 || len(report.Paths) != 3 {
		t.Fatalf("incomplete report: %#v", report)
	}
}

func TestHTTPHandlerServesHTMLAndJSON(t *testing.T) {
	report, err := BuildReport(7)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(NewHandler(report))
	defer server.Close()
	response, err := http.Get(server.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status=%d", response.StatusCode)
	}
	var page strings.Builder
	buffer := make([]byte, 4096)
	for {
		n, err := response.Body.Read(buffer)
		page.Write(buffer[:n])
		if err != nil {
			break
		}
	}
	if !strings.Contains(page.String(), "技能冷却与资源预算编排内核") {
		t.Fatal("diagnostics page content missing")
	}
	apiResponse, err := http.Get(server.URL + "/api/report")
	if err != nil {
		t.Fatal(err)
	}
	defer apiResponse.Body.Close()
	var decoded Report
	if err := json.NewDecoder(apiResponse.Body).Decode(&decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.RuleHash != report.RuleHash {
		t.Fatal("JSON report mismatch")
	}
}

func TestListenLoopbackRejectsWildcard(t *testing.T) {
	if _, err := ListenLoopback("0.0.0.0:0"); err == nil {
		t.Fatal("wildcard bind was accepted")
	}
	listener, err := ListenLoopback("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	ip := listener.Addr().(*net.TCPAddr).IP
	if !ip.IsLoopback() {
		t.Fatalf("listener bound to non-loopback address %s", ip)
	}
}
