package proxy

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"feedback_zsl/internal/config"
)

func testConfig(badURL, goodURL string) *config.Config {
	c := &config.Config{
		ListenH2:    "127.0.0.1:18444",
		ListenH3:    "127.0.0.1:18444",
		AdminListen: "127.0.0.1:19091",
		Upstreams: []config.UpstreamConfig{
			{ID: "bad", URL: badURL, Protocol: "http1", MaxConnections: 1, MaxInflight: 1},
			{ID: "good", URL: goodURL, Protocol: "http1", MaxConnections: 1, MaxInflight: 1},
		},
		Routes: []config.RouteConfig{
			{ID: "r", PathPrefix: "/test", Upstreams: []string{"bad", "good"}},
		},
		Priorities: []config.PriorityConfig{
			{Name: "high", Rank: 0, H2WeightMin: 192, H2WeightMax: 256, H3Urgency: []int{0, 1}, HeaderValues: []string{"high"}},
			{Name: "normal", Rank: 10, H2WeightMin: 64, H2WeightMax: 191, H3Urgency: []int{2, 3, 4}, HeaderValues: []string{"normal"}},
			{Name: "low", Rank: 20, H2WeightMin: 1, H2WeightMax: 63, H3Urgency: []int{5, 6, 7}, HeaderValues: []string{"low"}},
		},
		DefaultPrio: "normal",
		Scheduler: config.SchedulerConfig{
			MaxInflight:   2,
			MaxWait:       config.Duration(100 * time.Millisecond),
			AgingInterval: config.Duration(20 * time.Millisecond),
		},
		Timeouts: config.TimeoutConfig{
			Dial:            config.Duration(time.Second),
			UpstreamHeaders: config.Duration(time.Second),
			Request:         config.Duration(2 * time.Second),
			HealthInterval:  config.Duration(time.Second),
			HealthTimeout:   config.Duration(100 * time.Millisecond),
			Shutdown:        config.Duration(time.Second),
			IdleConnection:  config.Duration(time.Second),
		},
		Limits: config.LimitConfig{MaxHeaderBytes: 16 << 10, MaxRequestBytes: 1 << 20},
		Retry:  config.RetryConfig{Max: 1, Backoff: config.Duration(time.Millisecond)},
	}
	c.ApplyDefaults()
	return c
}

func TestSafeReadRetriesAfterDisconnect(t *testing.T) {
	var badHits, goodHits atomic.Int64
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		badHits.Add(1)
		hj, ok := w.(http.Hijacker)
		if !ok {
			t.Fatal("test server does not support hijacking")
		}
		conn, _, err := hj.Hijack()
		if err != nil {
			t.Fatal(err)
		}
		_ = conn.Close()
	}))
	defer bad.Close()
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		goodHits.Add(1)
		_, _ = w.Write([]byte("ok"))
	}))
	defer good.Close()

	p, err := NewWithConfig(testConfig(bad.URL, good.URL), log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	defer p.Shutdown(context.Background())

	req := httptest.NewRequest(http.MethodGet, "http://gateway/test/read", nil)
	req.Header.Set("X-Gateway-Priority", "high")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || rec.Body.String() != "ok" {
		t.Fatalf("response = %d %q", rec.Code, rec.Body.String())
	}
	if badHits.Load() != 1 || goodHits.Load() != 1 {
		t.Fatalf("hits bad=%d good=%d", badHits.Load(), goodHits.Load())
	}
	if got := rec.Header().Get("X-Gateway-Retries"); got != "1" {
		t.Fatalf("retry header = %q", got)
	}
}

func TestWriteRequestIsNotRetried(t *testing.T) {
	var badHits, goodHits atomic.Int64
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		badHits.Add(1)
		hj := w.(http.Hijacker)
		conn, _, _ := hj.Hijack()
		_ = conn.Close()
	}))
	defer bad.Close()
	good := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		goodHits.Add(1)
		w.WriteHeader(http.StatusCreated)
	}))
	defer good.Close()

	p, err := NewWithConfig(testConfig(bad.URL, good.URL), log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	defer p.Shutdown(context.Background())

	req := httptest.NewRequest(http.MethodPost, "http://gateway/test/write", strings.NewReader(`{"x":1}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want 502", rec.Code)
	}
	var body errorBody
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Code != CodeUpstreamWriteAmbiguous {
		t.Fatalf("code = %s", body.Code)
	}
	if body.Phase == "" {
		t.Fatal("disconnect phase was not reported")
	}
	if badHits.Load() != 1 || goodHits.Load() != 0 {
		t.Fatalf("unsafe request was retried: bad=%d good=%d", badHits.Load(), goodHits.Load())
	}
}

func TestCanceledRequestIsNotForwarded(t *testing.T) {
	var hits atomic.Int64
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer up.Close()
	p, err := NewWithConfig(testConfig(up.URL, up.URL), log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	defer p.Shutdown(context.Background())

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequest(http.MethodGet, "http://gateway/test/canceled", nil).WithContext(ctx)
	rec := httptest.NewRecorder()
	p.ServeHTTP(rec, req)
	if hits.Load() != 0 {
		t.Fatalf("canceled downstream request reached upstream %d times", hits.Load())
	}
}

func TestIdempotencyKeyReplaysWithoutSecondUpstreamCall(t *testing.T) {
	var hits atomic.Int64
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"created":true}`))
	}))
	defer up.Close()
	p, err := NewWithConfig(testConfig(up.URL, up.URL), log.New(io.Discard, "", 0))
	if err != nil {
		t.Fatal(err)
	}
	defer p.Shutdown(context.Background())

	for i := 0; i < 2; i++ {
		req := httptest.NewRequest(http.MethodPost, "http://gateway/test/write", strings.NewReader(`{"id":1}`))
		req.Header.Set("Idempotency-Key", "create-1")
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, req)
		if rec.Code != http.StatusCreated || rec.Body.String() != `{"created":true}` {
			t.Fatalf("attempt %d response = %d %q", i, rec.Code, rec.Body.String())
		}
		if i == 1 && rec.Header().Get("X-Gateway-Idempotent-Replay") != "true" {
			t.Fatal("second response was not marked as an idempotent replay")
		}
	}
	if hits.Load() != 1 {
		t.Fatalf("upstream received %d requests, want 1", hits.Load())
	}
}
