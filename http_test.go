package syncgame

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func TestHTTPDemoEndpoints(t *testing.T) {
	sample := testFixture(t)
	store, err := OpenStore(filepath.Join(t.TempDir(), "http.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	demo, err := NewDemoServer(sample, store)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(demo.Handler())
	defer server.Close()

	response, err := http.Get(server.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("index status = %d", response.StatusCode)
	}
	body, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "SyncForge") {
		t.Fatal("index page is missing demo UI")
	}

	response, err = http.Get(server.URL + "/api/demo?tier=low")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("demo status = %d", response.StatusCode)
	}
	var run RunResult
	if err := json.NewDecoder(response.Body).Decode(&run); err != nil {
		t.Fatal(err)
	}
	if len(run.Authority.States) == 0 || len(run.Clients) != len(sample.Clients) || len(run.Windows) == 0 {
		t.Fatalf("incomplete demo response: %+v", run)
	}

	response, err = http.Post(server.URL+"/api/verify", "application/json", strings.NewReader(`{"tier":"low"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("verify status = %d", response.StatusCode)
	}
	var verification map[string]any
	if err := json.NewDecoder(response.Body).Decode(&verification); err != nil {
		t.Fatal(err)
	}
	if passed, _ := verification["passed"].(bool); !passed {
		t.Fatalf("HTTP verification failed: %+v", verification)
	}
}

func TestLoopbackServerRejectsNonLoopbackBind(t *testing.T) {
	_, _, err := NewLoopbackServer(http.NewServeMux(), "0.0.0.0:0")
	if err == nil {
		t.Fatal("expected non-loopback bind to be rejected")
	}
	server, listener, err := NewLoopbackServer(http.NewServeMux(), "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	if server == nil || listener == nil {
		t.Fatal("loopback server was not created")
	}
	listener.Close()
}
