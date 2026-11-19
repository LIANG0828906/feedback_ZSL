package fixture

import (
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"feedback_zsl/internal/tlscert"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
)

type Config struct {
	H1Addr   string
	H2Addr   string
	H2CAddr  string
	CertFile string
	KeyFile  string
}

type Service struct {
	protocol string
	state    *state
	handler  http.Handler
}

type state struct {
	protocol string
	healthy  atomic.Bool
	active   atomic.Int64
	failures *failTracker

	mu        sync.Mutex
	requests  uint64
	methods   map[string]uint64
	paths     map[string]uint64
	recent    []Received
	lastError string
}

type failTracker struct {
	mu   sync.Mutex
	seen map[string]bool
}

func newFailTracker() *failTracker {
	return &failTracker{seen: make(map[string]bool)}
}

type Received struct {
	Time           time.Time         `json:"time"`
	Method         string            `json:"method"`
	Path           string            `json:"path"`
	Host           string            `json:"host"`
	RequestID      string            `json:"request_id"`
	ClientProtocol string            `json:"client_protocol"`
	Headers        map[string]string `json:"headers"`
}

type Stats struct {
	Protocol  string            `json:"protocol"`
	Healthy   bool              `json:"healthy"`
	Requests  uint64            `json:"requests"`
	Active    int64             `json:"active"`
	Methods   map[string]uint64 `json:"methods"`
	Paths     map[string]uint64 `json:"paths"`
	Recent    []Received        `json:"recent"`
	LastError string            `json:"last_error,omitempty"`
}

func New(protocol string) *Service {
	s := &state{
		protocol: protocol,
		methods:  make(map[string]uint64),
		paths:    make(map[string]uint64),
		failures: newFailTracker(),
	}
	s.healthy.Store(true)
	return &Service{protocol: protocol, state: s, handler: http.HandlerFunc(s.serve)}
}

func (s *Service) SetHealthy(ok bool) { s.state.healthy.Store(ok) }

func (s *Service) Stats() Stats {
	return s.state.stats()
}

func (s *state) stats() Stats {
	s.mu.Lock()
	defer s.mu.Unlock()
	methods := make(map[string]uint64, len(s.methods))
	for k, v := range s.methods {
		methods[k] = v
	}
	paths := make(map[string]uint64, len(s.paths))
	for k, v := range s.paths {
		paths[k] = v
	}
	recent := append([]Received(nil), s.recent...)
	return Stats{
		Protocol:  s.protocol,
		Healthy:   s.healthy.Load(),
		Requests:  s.requests,
		Active:    s.active.Load(),
		Methods:   methods,
		Paths:     paths,
		Recent:    recent,
		LastError: s.lastError,
	}
}

func (s *state) serve(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/__health":
		if s.healthy.Load() {
			w.WriteHeader(http.StatusNoContent)
		} else {
			http.Error(w, "unhealthy", http.StatusServiceUnavailable)
		}
		return
	case "/__stats":
		writeJSON(w, http.StatusOK, s.stats())
		return
	case "/__control/health":
		s.healthy.Store(strings.EqualFold(r.URL.Query().Get("value"), "up") || r.URL.Query().Get("value") == "1")
		writeJSON(w, http.StatusOK, map[string]any{"protocol": s.protocol, "healthy": s.healthy.Load()})
		return
	case "/__control/reset":
		s.mu.Lock()
		s.requests = 0
		s.methods = make(map[string]uint64)
		s.paths = make(map[string]uint64)
		s.recent = nil
		s.failures.mu.Lock()
		s.failures.seen = make(map[string]bool)
		s.failures.mu.Unlock()
		s.lastError = ""
		s.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]string{"status": "reset"})
		return
	}
	if !s.protocolMatches(r) {
		http.Error(w, `{"code":"UPSTREAM_PROTOCOL_MISMATCH","message":"this listener only accepts `+s.protocol+`"}`, http.StatusHTTPVersionNotSupported)
		return
	}

	s.active.Add(1)
	defer s.active.Add(-1)
	id := r.Header.Get("X-Request-ID")
	if id == "" {
		id = fmt.Sprintf("%s-%d", s.protocol, time.Now().UnixNano())
	}
	s.record(r, id)

	if !s.healthy.Load() {
		http.Error(w, `{"code":"UPSTREAM_DRAINED"}`, http.StatusServiceUnavailable)
		return
	}
	if slow := parseSlow(r); slow > 0 {
		select {
		case <-time.After(slow):
		case <-r.Context().Done():
			return
		}
	}
	closeMode := strings.ToLower(strings.TrimSpace(r.Header.Get("X-Fault-Close")))
	if closeMode != "" && s.shouldFail(id, closeMode) {
		switch closeMode {
		case "after_headers":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			select {
			case <-time.After(5 * time.Millisecond):
			}
			panic(http.ErrAbortHandler)
		default:
			panic(http.ErrAbortHandler)
		}
	}
	s.respond(w, r, id)
}

func (s *state) protocolMatches(r *http.Request) bool {
	switch s.protocol {
	case "http1":
		return r.ProtoMajor == 1
	case "http2":
		return r.ProtoMajor == 2
	case "h2c":
		return r.ProtoMajor == 2
	default:
		return false
	}
}

func (s *state) shouldFail(id, mode string) bool {
	if mode == "always" {
		return true
	}
	if mode != "once" {
		return true
	}
	s.failures.mu.Lock()
	defer s.failures.mu.Unlock()
	if s.failures.seen[id] {
		return false
	}
	s.failures.seen[id] = true
	return true
}

func (s *state) record(r *http.Request, id string) {
	headers := map[string]string{}
	for _, k := range []string{
		"Content-Type", "User-Agent", "Accept", "Priority", "X-Test-Key",
		"X-Forwarded-For", "X-Forwarded-Proto", "X-Gateway-Entry-Protocol", "X-Gateway-Priority",
	} {
		if v := r.Header.Get(k); v != "" {
			headers[k] = v
		}
	}
	entry := Received{
		Time:           time.Now().UTC(),
		Method:         r.Method,
		Path:           r.URL.RequestURI(),
		Host:           r.Host,
		RequestID:      id,
		ClientProtocol: r.Header.Get("X-Gateway-Entry-Protocol"),
		Headers:        headers,
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests++
	s.methods[r.Method]++
	s.paths[r.URL.Path]++
	s.recent = append(s.recent, entry)
	if len(s.recent) > 32 {
		s.recent = append([]Received(nil), s.recent[len(s.recent)-32:]...)
	}
}

func (s *state) respond(w http.ResponseWriter, r *http.Request, id string) {
	body, _ := io.ReadAll(io.LimitReader(r.Body, 2<<20))
	sum := sha256.New()
	_, _ = fmt.Fprintf(sum, "%s\n%s\n%s\n", r.Method, r.URL.RequestURI(), id)
	for _, k := range []string{"Content-Type", "User-Agent", "Accept", "Priority", "X-Test-Key"} {
		_, _ = fmt.Fprintf(sum, "%s=%s\n", strings.ToLower(k), r.Header.Get(k))
	}
	_, _ = sum.Write(body)
	out := map[string]any{
		"ok":           true,
		"upstream":     s.protocol,
		"method":       r.Method,
		"path":         r.URL.RequestURI(),
		"request_id":   id,
		"echo_sha256":  hex.EncodeToString(sum.Sum(nil)),
		"request_body": len(body),
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Upstream-Protocol", s.protocol)
	writeJSON(w, http.StatusOK, out)
}

func parseSlow(r *http.Request) time.Duration {
	raw := r.URL.Query().Get("slow_ms")
	if raw == "" {
		raw = r.Header.Get("X-Fault-Slow-Ms")
	}
	if raw == "" {
		return 0
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return 0
	}
	if n > 5000 {
		n = 5000
	}
	return time.Duration(n) * time.Millisecond
}

type Set struct {
	H1    *Service
	H2    *Service
	H2C   *Service
	Addrs map[string]string
	stop  []func()
}

func Start(cfg Config) (*Set, error) {
	cert, err := tlscert.LoadOrGenerate(cfg.CertFile, cfg.KeyFile)
	if err != nil {
		return nil, err
	}
	if cfg.H1Addr == "" {
		cfg.H1Addr = "127.0.0.1:18081"
	}
	if cfg.H2Addr == "" {
		cfg.H2Addr = "127.0.0.1:18082"
	}
	if cfg.H2CAddr == "" {
		cfg.H2CAddr = "127.0.0.1:18083"
	}
	set := &Set{H1: New("http1"), H2: New("http2"), H2C: New("h2c"), Addrs: make(map[string]string)}
	sharedFailures := newFailTracker()
	set.H1.state.failures = sharedFailures
	set.H2.state.failures = sharedFailures
	set.H2C.state.failures = sharedFailures

	h1ln, err := net.Listen("tcp", cfg.H1Addr)
	if err != nil {
		return nil, err
	}
	h2ln, err := net.Listen("tcp", cfg.H2Addr)
	if err != nil {
		_ = h1ln.Close()
		return nil, err
	}
	h2cln, err := net.Listen("tcp", cfg.H2CAddr)
	if err != nil {
		_ = h1ln.Close()
		_ = h2ln.Close()
		return nil, err
	}
	set.Addrs["http1"] = h1ln.Addr().String()
	set.Addrs["http2"] = h2ln.Addr().String()
	set.Addrs["h2c"] = h2cln.Addr().String()

	h1srv := &http.Server{Handler: set.H1.handler, ReadHeaderTimeout: 3 * time.Second}
	h2srv := &http.Server{
		Handler:           set.H2.handler,
		TLSConfig:         &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12, NextProtos: []string{"h2"}},
		ReadHeaderTimeout: 3 * time.Second,
	}
	if err := http2.ConfigureServer(h2srv, &http2.Server{MaxConcurrentStreams: 256}); err != nil {
		_ = h1ln.Close()
		_ = h2ln.Close()
		_ = h2cln.Close()
		return nil, err
	}
	h2cHandler := h2c.NewHandler(set.H2C.handler, &http2.Server{MaxConcurrentStreams: 256})
	h2csrv := &http.Server{Handler: h2cHandler, ReadHeaderTimeout: 3 * time.Second}

	set.stop = append(set.stop, func() { _ = h1srv.Close() }, func() { _ = h2srv.Close() }, func() { _ = h2csrv.Close() })
	go func() { _ = h1srv.Serve(h1ln) }()
	go func() { _ = h2srv.ServeTLS(h2ln, "", "") }()
	go func() { _ = h2csrv.Serve(h2cln) }()
	return set, nil
}

func (s *Set) Close() {
	for _, stop := range s.stop {
		stop()
	}
}

func (s *Set) All() []*Service {
	return []*Service{s.H1, s.H2, s.H2C}
}

func (s *Set) Summary() map[string]Stats {
	out := make(map[string]Stats, 3)
	for _, svc := range s.All() {
		out[svc.state.protocol] = svc.Stats()
	}
	return out
}

func (s *Set) LinkHeaders() []string {
	var out []string
	for _, p := range []string{"http1", "http2", "h2c"} {
		out = append(out, fmt.Sprintf("<%s>; rel=\"%s\"", s.Addrs[p], p))
	}
	sort.Strings(out)
	return out
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
