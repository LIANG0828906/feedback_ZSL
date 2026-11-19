package proxy

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"feedback_zsl/internal/config"
)

func writeConfig(t *testing.T, path, upstreamURL string) {
	t.Helper()
	cfg := testConfig(upstreamURL, upstreamURL)
	cfg.Upstreams = cfg.Upstreams[:1]
	cfg.Routes = []config.RouteConfig{{
		ID: "shared", PathPrefix: "/test", Upstreams: []string{"bad"},
	}}
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestReloadSwitchesWholeRuntimeWithoutMixedRoute(t *testing.T) {
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	oldUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/__health" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		started <- struct{}{}
		<-release
		_, _ = w.Write([]byte("old"))
	}))
	defer oldUpstream.Close()
	newUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/__health" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		_, _ = w.Write([]byte("new"))
	}))
	defer newUpstream.Close()

	dir := t.TempDir()
	path := filepath.Join(dir, "gateway.json")
	writeConfig(t, path, oldUpstream.URL)
	p, err := New(path, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	defer p.Shutdown(context.Background())
	p.Start()

	oldDone := make(chan string, 1)
	go func() {
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://gateway/test/old", nil))
		oldDone <- rec.Body.String()
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("old request did not start")
	}

	writeConfig(t, path, newUpstream.URL)
	if err := p.Reload(); err != nil {
		t.Fatal(err)
	}
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://gateway/test/new", nil))
	if rec.Body.String() != "new" {
		t.Fatalf("new request body = %q", rec.Body.String())
	}
	close(release)
	if body := <-oldDone; body != "old" {
		t.Fatalf("in-flight request switched runtime: %q", body)
	}
}

func TestInvalidReloadKeepsPreviousRuntime(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/__health" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		_, _ = w.Write([]byte("stable"))
	}))
	defer upstream.Close()
	dir := t.TempDir()
	path := filepath.Join(dir, "gateway.json")
	writeConfig(t, path, upstream.URL)
	p, err := New(path, log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	defer p.Shutdown(context.Background())

	cfg := testConfig(upstream.URL, upstream.URL)
	cfg.Timeouts.Dial = config.Duration(-time.Second)
	data, _ := json.Marshal(cfg)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := p.Reload(); err == nil {
		t.Fatal("invalid reload unexpectedly succeeded")
	}
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://gateway/test/still", nil))
	if rec.Body.String() != "stable" {
		t.Fatalf("previous runtime was lost: %q", rec.Body.String())
	}
}
