package upstream

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"feedback_zsl/internal/config"
	"feedback_zsl/internal/metrics"
	"feedback_zsl/internal/priority"
	"feedback_zsl/internal/scheduler"

	"golang.org/x/net/http2"
)

type Upstream struct {
	cfg     config.UpstreamConfig
	url     *url.URL
	client  http.RoundTripper
	limiter *connLimiter
	sched   *scheduler.Scheduler
	metrics *metrics.Registry
	timeout config.TimeoutConfig

	healthy   atomic.Bool
	mu        sync.Mutex
	lastError string
	lastCheck time.Time
	lastOK    time.Time
}

type Status struct {
	ID        string    `json:"id"`
	Protocol  string    `json:"protocol"`
	URL       string    `json:"url"`
	Healthy   bool      `json:"healthy"`
	LastError string    `json:"last_error,omitempty"`
	LastCheck time.Time `json:"last_check,omitempty"`
	LastOK    time.Time `json:"last_ok,omitempty"`
}

func New(cfg config.UpstreamConfig, timeouts config.TimeoutConfig, schedCfg config.SchedulerConfig, reg *metrics.Registry) (*Upstream, error) {
	u, err := url.Parse(cfg.URL)
	if err != nil {
		return nil, err
	}
	up := &Upstream{
		cfg:     cfg,
		url:     u,
		metrics: reg,
		timeout: timeouts,
		sched: scheduler.New(scheduler.Config{
			MaxInflight: cfg.MaxInflight,
			MaxWait:     schedCfg.MaxWait.Duration(),
			AgingPeriod: schedCfg.AgingInterval.Duration(),
		}),
	}
	up.healthy.Store(true)
	baseTLS := &tls.Config{
		InsecureSkipVerify: cfg.TLSInsecure, // Local self-signed fixtures.
		MinVersion:         tls.VersionTLS12,
		NextProtos:         []string{"h2"},
	}
	up.limiter = newConnLimiter(cfg.MaxConnections)
	dialer := &net.Dialer{Timeout: timeouts.Dial.Duration(), KeepAlive: 30 * time.Second}
	switch cfg.Protocol {
	case "http1":
		tr := &http.Transport{
			Proxy:                 nil,
			DialContext:           dialer.DialContext,
			ForceAttemptHTTP2:     false,
			TLSNextProto:          map[string]func(string, *tls.Conn) http.RoundTripper{},
			TLSClientConfig:       baseTLS,
			MaxIdleConns:          cfg.MaxConnections,
			MaxIdleConnsPerHost:   cfg.MaxConnections,
			MaxConnsPerHost:       cfg.MaxConnections,
			IdleConnTimeout:       timeouts.IdleConnection.Duration(),
			ResponseHeaderTimeout: timeouts.UpstreamHeaders.Duration(),
			DisableCompression:    true,
		}
		up.client = tr
	case "http2":
		tr := &http2.Transport{
			TLSClientConfig:            baseTLS,
			StrictMaxConcurrentStreams: true,
			IdleConnTimeout:            timeouts.IdleConnection.Duration(),
			ReadIdleTimeout:            timeouts.IdleConnection.Duration(),
			DisableCompression:         true,
			DialTLSContext: func(ctx context.Context, network, addr string, cfg *tls.Config) (net.Conn, error) {
				if err := up.limiter.acquire(ctx); err != nil {
					return nil, err
				}
				raw, err := dialer.DialContext(ctx, network, addr)
				if err != nil {
					up.limiter.release()
					return nil, err
				}
				tc := tls.Client(raw, baseTLS.Clone())
				if err := tc.HandshakeContext(ctx); err != nil {
					_ = raw.Close()
					up.limiter.release()
					return nil, err
				}
				return &limitedConn{Conn: tc, limiter: up.limiter}, nil
			},
		}
		up.client = tr
	case "h2c":
		tr := &http2.Transport{
			AllowHTTP:                  true,
			StrictMaxConcurrentStreams: true,
			IdleConnTimeout:            timeouts.IdleConnection.Duration(),
			ReadIdleTimeout:            timeouts.IdleConnection.Duration(),
			DisableCompression:         true,
			DialTLSContext: func(ctx context.Context, network, addr string, cfg *tls.Config) (net.Conn, error) {
				if err := up.limiter.acquire(ctx); err != nil {
					return nil, err
				}
				raw, err := dialer.DialContext(ctx, network, addr)
				if err != nil {
					up.limiter.release()
					return nil, err
				}
				return &limitedConn{Conn: raw, limiter: up.limiter}, nil
			},
		}
		up.client = tr
	default:
		return nil, fmt.Errorf("unsupported upstream protocol %q", cfg.Protocol)
	}
	reg.RegisterUpstream(cfg.ID, cfg.Protocol)
	return up, nil
}

func (u *Upstream) ID() string       { return u.cfg.ID }
func (u *Upstream) Protocol() string { return u.cfg.Protocol }
func (u *Upstream) URL() *url.URL    { return u.url }
func (u *Upstream) Config() config.UpstreamConfig {
	return u.cfg
}

func (u *Upstream) Healthy() bool { return u.healthy.Load() }

func (u *Upstream) RoundTrip(ctx context.Context, req *http.Request, class priority.Class) (*http.Response, time.Duration, error) {
	lease, wait, err := u.sched.Acquire(ctx, class)
	if err != nil {
		return nil, wait, err
	}
	release := func() {
		lease.Release()
		u.metrics.UpstreamFinish(u.cfg.ID)
	}
	u.metrics.UpstreamStart(u.cfg.ID)
	resp, err := u.client.RoundTrip(req)
	if err != nil {
		release()
		if ctx.Err() == nil && IsDialFailure(err) {
			u.mark(false, err)
		}
		return nil, wait, err
	}
	u.mark(true, nil)
	resp.Body = &releaseBody{
		ReadCloser: resp.Body,
		release:    release,
	}
	return resp, wait, nil
}

func (u *Upstream) StartHealth(ctx context.Context) {
	go func() {
		interval := u.cfg.HealthInterval.Duration()
		if interval <= 0 {
			interval = time.Second
		}
		t := time.NewTicker(interval)
		defer t.Stop()
		u.probe(ctx)
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				u.probe(ctx)
			}
		}
	}()
}

func (u *Upstream) probe(parent context.Context) {
	timeout := u.cfg.HealthTimeout.Duration()
	if timeout <= 0 {
		timeout = 500 * time.Millisecond
	}
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.url.String()+u.cfg.HealthPath, nil)
	if err != nil {
		u.mark(false, err)
		return
	}
	req.Host = u.url.Host
	resp, err := u.client.RoundTrip(req)
	if err != nil {
		u.mark(false, err)
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	_ = resp.Body.Close()
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		u.mark(true, nil)
		return
	}
	u.mark(false, fmt.Errorf("health status %d", resp.StatusCode))
}

func (u *Upstream) mark(ok bool, err error) {
	u.healthy.Store(ok)
	u.mu.Lock()
	defer u.mu.Unlock()
	u.lastCheck = time.Now().UTC()
	if ok {
		u.lastOK = u.lastCheck
		u.lastError = ""
	} else if err != nil {
		u.lastError = err.Error()
	}
}

func (u *Upstream) MarkFailure(err error) {
	if err != nil {
		u.mark(false, err)
	}
}

func (u *Upstream) Status() Status {
	u.mu.Lock()
	defer u.mu.Unlock()
	return Status{
		ID:        u.cfg.ID,
		Protocol:  u.cfg.Protocol,
		URL:       u.cfg.URL,
		Healthy:   u.healthy.Load(),
		LastError: u.lastError,
		LastCheck: u.lastCheck,
		LastOK:    u.lastOK,
	}
}

func (u *Upstream) Close() {
	if u.sched != nil {
		u.sched.Close()
	}
	switch tr := u.client.(type) {
	case interface{ CloseIdleConnections() }:
		tr.CloseIdleConnections()
	case interface{ Close() error }:
		_ = tr.Close()
	}
}

type releaseBody struct {
	io.ReadCloser
	once    sync.Once
	release func()
}

func (b *releaseBody) Close() error {
	err := b.ReadCloser.Close()
	b.once.Do(b.release)
	return err
}

func (b *releaseBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if err != nil {
		b.once.Do(b.release)
	}
	return n, err
}

type connLimiter struct {
	sem chan struct{}
}

func newConnLimiter(n int) *connLimiter {
	if n < 1 {
		n = 1
	}
	return &connLimiter{sem: make(chan struct{}, n)}
}

func (l *connLimiter) acquire(ctx context.Context) error {
	select {
	case l.sem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (l *connLimiter) release() {
	select {
	case <-l.sem:
	default:
	}
}

type limitedConn struct {
	net.Conn
	limiter *connLimiter
	once    sync.Once
}

func (c *limitedConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(c.limiter.release)
	return err
}

func isTransportFailure(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) {
		return false
	}
	var ue *url.Error
	if errors.As(err, &ue) {
		if ue.Timeout() {
			return true
		}
		return isTransportFailure(ue.Err)
	}
	return true
}

func IsHealthFailure(err error) bool {
	return IsDialFailure(err)
}

func IsDialFailure(err error) bool {
	if err == nil {
		return false
	}
	var oe *net.OpError
	if errors.As(err, &oe) && oe.Op == "dial" {
		return true
	}
	var ue *url.Error
	if errors.As(err, &ue) {
		return IsDialFailure(ue.Err)
	}
	return false
}

// Phase classifies an error by the most useful observable boundary.
func Phase(err error) string {
	if err == nil {
		return "none"
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return "downstream_or_deadline"
	}
	var ne net.Error
	if errors.As(err, &ne) {
		if ne.Timeout() {
			return "upstream_timeout"
		}
	}
	var oe *net.OpError
	if errors.As(err, &oe) && oe.Op == "dial" {
		return "dial"
	}
	var ge *http2.GoAwayError
	if errors.As(err, &ge) {
		return "response_headers"
	}
	s := strings.ToLower(err.Error())
	switch {
	case strings.Contains(s, "connection reset"), strings.Contains(s, "broken pipe"),
		strings.Contains(s, "eof"), strings.Contains(s, "connection refused"):
		return "response_headers"
	case strings.Contains(s, "stream error"), strings.Contains(s, "goaway"):
		return "response_headers"
	default:
		return "request"
	}
}

func IsSafeRead(method string, bodyLen int) bool {
	if bodyLen > 0 {
		return false
	}
	switch strings.ToUpper(method) {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	default:
		return false
	}
}
