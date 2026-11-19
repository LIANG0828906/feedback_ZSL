package loadgen

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"feedback_zsl/internal/config"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"
	"golang.org/x/net/http2"
)

type Scenario struct {
	GatewayH2        string            `json:"gateway_h2"`
	GatewayH3        string            `json:"gateway_h3"`
	AdminURL         string            `json:"admin_url"`
	Duration         config.Duration   `json:"duration"`
	Rate             int               `json:"rate_per_second"`
	H3Ratio          float64           `json:"h3_ratio"`
	MaxConcurrent    int               `json:"max_concurrent"`
	Templates        []Template        `json:"requests"`
	Consistency      []Template        `json:"consistency_requests"`
	UpstreamStats    map[string]string `json:"upstream_stats"`
	Migration        MigrationConfig   `json:"migration"`
	PriorityProbe    PriorityProbe     `json:"priority_probe"`
	FailoverProbe    FailoverProbe     `json:"failover_probe"`
	IdempotencyProbe IdempotencyProbe  `json:"idempotency_probe"`
}

type Template struct {
	Method   string            `json:"method"`
	Path     string            `json:"path"`
	Priority string            `json:"priority"`
	Headers  map[string]string `json:"headers"`
	Body     string            `json:"body"`
	Weight   int               `json:"weight"`
}

type MigrationConfig struct {
	Enabled      bool            `json:"enabled"`
	LocalIPs     []string        `json:"local_ips"`
	SlowPath     string          `json:"slow_path"`
	ResumePath   string          `json:"resume_path"`
	SessionID    string          `json:"session_id"`
	RequestDelay config.Duration `json:"request_delay"`
}

type PriorityProbe struct {
	Enabled  bool            `json:"enabled"`
	LowCount int             `json:"low_count"`
	SlowPath string          `json:"slow_path"`
	HighPath string          `json:"high_path"`
	Delay    config.Duration `json:"delay"`
}

type FailoverProbe struct {
	Enabled        bool            `json:"enabled"`
	PrimaryID      string          `json:"primary_id"`
	ControlDownURL string          `json:"control_down_url"`
	ControlUpURL   string          `json:"control_up_url"`
	Path           string          `json:"path"`
	HealthWait     config.Duration `json:"health_wait"`
}

type IdempotencyProbe struct {
	Enabled bool   `json:"enabled"`
	Path    string `json:"path"`
	Key     string `json:"idempotency_key"`
}

type Client struct {
	protocol string
	http     *http.Client
	h3rt     *http3.RoundTripper
	qt       *quic.Transport
	udp      *net.UDPConn
}

func NewH2Client(insecure bool) *Client {
	tr := &http2.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: insecure, NextProtos: []string{"h2"}},
	}
	return &Client{protocol: "h2", http: &http.Client{Transport: tr, Timeout: 15 * time.Second}}
}

func NewH3Client(localIP string, insecure bool) (*Client, error) {
	if localIP == "" {
		localIP = "127.0.0.1"
	}
	udp, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP(localIP), Port: 0})
	if err != nil {
		return nil, err
	}
	qt := &quic.Transport{Conn: udp}
	rt := &http3.RoundTripper{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: insecure, NextProtos: []string{http3.NextProtoH3}},
		QUICConfig:      &quic.Config{MaxIdleTimeout: 20 * time.Second},
		Dial: func(ctx context.Context, addr string, tlsCfg *tls.Config, cfg *quic.Config) (quic.EarlyConnection, error) {
			remote, err := net.ResolveUDPAddr("udp", addr)
			if err != nil {
				return nil, err
			}
			return qt.DialEarly(ctx, remote, tlsCfg, cfg)
		},
	}
	return &Client{protocol: "h3", http: &http.Client{Transport: rt, Timeout: 15 * time.Second}, h3rt: rt, qt: qt, udp: udp}, nil
}

func (c *Client) Close() {
	if c.h3rt != nil {
		_ = c.h3rt.Close()
	}
	if c.qt != nil {
		_ = c.qt.Close()
	}
	if c.udp != nil {
		_ = c.udp.Close()
	}
}

type Result struct {
	Protocol         string
	Priority         string
	Status           int
	Code             string
	Duration         time.Duration
	Wait             time.Duration
	Retries          int
	RetryReason      string
	Upstream         string
	BodySize         int
	BodyHash         string
	Migration        string
	IdempotentReplay bool
	Err              error
	Started          time.Time
}

func (c *Client) Do(ctx context.Context, base string, tpl Template, requestID, session string) Result {
	start := time.Now()
	res := Result{Protocol: c.protocol, Priority: tpl.Priority, Started: start}
	method := tpl.Method
	if method == "" {
		method = http.MethodGet
	}
	body := []byte(tpl.Body)
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(base, "/")+tpl.Path, bytes.NewReader(body))
	if err != nil {
		res.Err = err
		return res
	}
	for k, v := range tpl.Headers {
		req.Header.Set(k, v)
	}
	if req.Header.Get("User-Agent") == "" {
		req.Header.Set("User-Agent", "feedback-zsl-loadgen")
	}
	req.Header.Set("X-Request-ID", requestID)
	if tpl.Priority != "" {
		req.Header.Set("X-Gateway-Priority", tpl.Priority)
		if req.Header.Get("Priority") == "" {
			switch tpl.Priority {
			case "high":
				req.Header.Set("Priority", "u=0")
			case "normal":
				req.Header.Set("Priority", "u=3")
			case "low":
				req.Header.Set("Priority", "u=6")
			}
		}
	}
	if session != "" {
		req.Header.Set("X-Gateway-Session-ID", session)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		res.Err = err
		res.Duration = time.Since(start)
		return res
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	res.Duration = time.Since(start)
	res.Status = resp.StatusCode
	res.BodySize = len(data)
	res.BodyHash = hashBytes(data)
	if err != nil {
		res.Err = err
	}
	res.Wait = durationHeader(resp.Header.Get("X-Gateway-Priority-Wait-Ms"))
	res.Retries = intHeader(resp.Header.Get("X-Gateway-Retries"))
	res.RetryReason = resp.Header.Get("X-Gateway-Retry-Reason")
	res.Upstream = resp.Header.Get("X-Gateway-Upstream")
	res.Migration = resp.Header.Get("X-Gateway-Migration")
	res.IdempotentReplay = resp.Header.Get("X-Gateway-Idempotent-Replay") == "true"
	var payload struct {
		Code string `json:"code"`
	}
	if json.Unmarshal(data, &payload) == nil {
		res.Code = payload.Code
	}
	if res.Code == "" {
		res.Code = resp.Header.Get("X-Gateway-Error-Code")
	}
	return res
}

type Report struct {
	StartedAt        time.Time              `json:"started_at"`
	FinishedAt       time.Time              `json:"finished_at"`
	DurationMS       int64                  `json:"duration_ms"`
	Config           Scenario               `json:"config"`
	Entries          map[string]EntryReport `json:"entries"`
	Consistency      []ConsistencyResult    `json:"consistency"`
	Retries          int                    `json:"retries"`
	RetriedRequests  int                    `json:"retried_requests"`
	RetryReasons     map[string]int         `json:"retry_reasons"`
	Rejections       map[string]int         `json:"rejections"`
	Migrations       MigrationReport        `json:"migration"`
	PriorityProbe    *PriorityProbeReport   `json:"priority_probe,omitempty"`
	Failover         *FailoverReport        `json:"failover,omitempty"`
	Idempotency      *IdempotencyReport     `json:"idempotency,omitempty"`
	Upstreams        map[string]any         `json:"upstream_stats"`
	AdminStatusAfter map[string]any         `json:"admin_status_after,omitempty"`
}

type EntryReport struct {
	Requests    int                    `json:"requests"`
	Errors      int                    `json:"errors"`
	LatencyMS   Percentiles            `json:"latency_ms"`
	WaitMS      map[string]Percentiles `json:"priority_wait_ms"`
	StatusCodes map[string]int         `json:"status_codes"`
	Upstreams   map[string]int         `json:"upstreams"`
}

type Percentiles struct {
	P50 float64 `json:"p50"`
	P95 float64 `json:"p95"`
	P99 float64 `json:"p99"`
}

type ConsistencyResult struct {
	Path       string `json:"path"`
	Method     string `json:"method"`
	Equivalent bool   `json:"equivalent"`
	H2Status   int    `json:"h2_status"`
	H3Status   int    `json:"h3_status"`
	H2Hash     string `json:"h2_body_sha256"`
	H3Hash     string `json:"h3_body_sha256"`
}

type MigrationReport struct {
	Configured           bool   `json:"configured"`
	NativeTransport      bool   `json:"native_transport_migration"`
	ResumeObserved       bool   `json:"resume_observed"`
	EstablishedCompleted bool   `json:"established_completed"`
	ResumedCompleted     bool   `json:"resumed_completed"`
	UnfinishedDropped    int    `json:"unfinished_dropped"`
	Success              int    `json:"success"`
	Note                 string `json:"note"`
}

type PriorityProbeReport struct {
	HighWaitMS           float64     `json:"high_wait_ms"`
	LowWaitMS            Percentiles `json:"low_wait_ms"`
	LowCompleted         int         `json:"low_completed"`
	HighCompleted        bool        `json:"high_completed"`
	NoStarvation         bool        `json:"no_starvation"`
	HighPriorityAdvanced bool        `json:"high_priority_advanced"`
	UpstreamOrder        []string    `json:"upstream_received_order,omitempty"`
}

type FailoverReport struct {
	PrimaryDownDetected bool   `json:"primary_down_detected"`
	FailoverUpstream    string `json:"failover_upstream"`
	RecoveredUpstream   string `json:"recovered_upstream"`
	FailoverSucceeded   bool   `json:"failover_succeeded"`
	RecoverySucceeded   bool   `json:"recovery_succeeded"`
	Success             bool   `json:"success"`
}

type IdempotencyReport struct {
	FirstStatus  int  `json:"first_status"`
	SecondStatus int  `json:"second_status"`
	SameBody     bool `json:"same_body"`
	Replay       bool `json:"replay_observed"`
	Success      bool `json:"success"`
}

type sampler struct {
	mu      sync.Mutex
	byEntry map[string][]Result
}

func Run(ctx context.Context, sc Scenario) (Report, error) {
	sc.applyDefaults()
	if sc.GatewayH2 == "" || sc.GatewayH3 == "" {
		return Report{}, fmt.Errorf("gateway_h2 and gateway_h3 are required")
	}
	h2 := NewH2Client(true)
	defer h2.Close()
	h3, err := NewH3Client("127.0.0.1", true)
	if err != nil {
		return Report{}, err
	}
	defer h3.Close()

	report := Report{
		StartedAt:    time.Now().UTC(),
		Config:       sc,
		Entries:      make(map[string]EntryReport),
		RetryReasons: make(map[string]int),
		Rejections:   make(map[string]int),
		Upstreams:    make(map[string]any),
	}
	consistency := runConsistency(ctx, h2, h3, sc)
	report.Consistency = consistency

	sample := &sampler{byEntry: make(map[string][]Result)}
	sem := make(chan struct{}, sc.MaxConcurrent)
	var wg sync.WaitGroup
	deadline := time.Now().Add(sc.Duration.Duration())
	interval := time.Second / time.Duration(sc.Rate)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	seq := 0
	for {
		select {
		case <-ctx.Done():
			goto done
		case now := <-ticker.C:
			if now.After(deadline) {
				goto done
			}
			tpl := sc.pickTemplate(seq)
			seq++
			client := h2
			if rand.Float64() < sc.H3Ratio {
				client = h3
			}
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				goto done
			}
			wg.Add(1)
			go func(client *Client, tpl Template, id int) {
				defer wg.Done()
				defer func() { <-sem }()
				res := client.Do(ctx, sc.baseFor(client.protocol), tpl, fmt.Sprintf("load-%d", id), "")
				sample.add(res)
			}(client, tpl, seq)
		}
	}
done:
	wg.Wait()
	report.FinishedAt = time.Now().UTC()
	report.DurationMS = report.FinishedAt.Sub(report.StartedAt).Milliseconds()
	report.Entries, report.Retries, report.RetriedRequests, report.RetryReasons, report.Rejections = summarize(sample)

	if sc.PriorityProbe.Enabled {
		probe := runPriorityProbe(ctx, h2, sc, sc.baseFor("h2"))
		report.PriorityProbe = &probe
	}
	if sc.FailoverProbe.Enabled {
		probe := runFailoverProbe(ctx, h2, sc)
		report.Failover = &probe
	}
	if sc.IdempotencyProbe.Enabled {
		probe := runIdempotencyProbe(ctx, h2, sc)
		report.Idempotency = &probe
	}
	if sc.Migration.Enabled {
		report.Migrations = runMigration(ctx, sc)
	} else {
		report.Migrations = MigrationReport{Note: "disabled"}
	}
	report.Upstreams = fetchUpstreamStats(ctx, sc.UpstreamStats)
	report.AdminStatusAfter = fetchJSON(ctx, sc.AdminURL+"/__gateway/status")
	return report, nil
}

func runConsistency(ctx context.Context, h2, h3 *Client, sc Scenario) []ConsistencyResult {
	var out []ConsistencyResult
	for i, tpl := range sc.Consistency {
		id := fmt.Sprintf("consistency-%d", i)
		r2 := h2.Do(ctx, sc.GatewayH2, tpl, id, "")
		r3 := h3.Do(ctx, sc.GatewayH3, tpl, id, "")
		out = append(out, ConsistencyResult{
			Path: tpl.Path, Method: tpl.Method,
			Equivalent: r2.Status == r3.Status && r2.BodyHash == r3.BodyHash && r2.Err == nil && r3.Err == nil,
			H2Status:   r2.Status, H3Status: r3.Status, H2Hash: r2.BodyHash, H3Hash: r3.BodyHash,
		})
	}
	return out
}

func runMigration(ctx context.Context, sc Scenario) MigrationReport {
	mc := sc.Migration
	out := MigrationReport{
		Configured:      true,
		NativeTransport: false,
		Note:            "quic-go v0.45 cannot negotiate active QUIC migration; this verifies session continuity across a changed client address/connection while the established request remains active",
	}
	a, err := NewH3Client(mc.firstIP(), true)
	if err != nil {
		out.Note = err.Error()
		return out
	}
	defer a.Close()
	b, err := NewH3Client(mc.secondIP(), true)
	if err != nil {
		out.Note = err.Error()
		return out
	}
	defer b.Close()
	session := mc.SessionID
	if session == "" {
		session = "migration-session"
	}
	slow := Template{Method: http.MethodGet, Path: mc.SlowPath, Priority: "high"}
	fast := Template{Method: http.MethodGet, Path: mc.ResumePath, Priority: "high"}
	type result struct{ r Result }
	ch1 := make(chan Result, 1)
	ch2 := make(chan Result, 1)
	go func() { ch1 <- a.Do(ctx, sc.GatewayH3, slow, "migration-established", session) }()
	time.Sleep(mc.RequestDelay.Duration())
	go func() { ch2 <- b.Do(ctx, sc.GatewayH3, fast, "migration-resumed", session) }()
	r1 := <-ch1
	r2 := <-ch2
	out.EstablishedCompleted = r1.Err == nil && r1.Status > 0
	out.ResumedCompleted = r2.Err == nil && r2.Status > 0
	out.ResumeObserved = r2.Migration == "resumed"
	if out.EstablishedCompleted && out.ResumedCompleted && out.ResumeObserved {
		out.Success = 1
	}
	if r1.Err != nil || r2.Err != nil {
		out.UnfinishedDropped = 1
	}
	return out
}

func runPriorityProbe(ctx context.Context, client *Client, sc Scenario, base string) PriorityProbeReport {
	p := sc.PriorityProbe
	out := PriorityProbeReport{}
	type result struct {
		res Result
		low bool
	}
	ch := make(chan result, p.LowCount+1)
	var lowWaits []float64
	for i := 0; i < p.LowCount; i++ {
		go func(i int) {
			ch <- result{low: true, res: client.Do(ctx, base, Template{
				Method: http.MethodGet, Path: p.SlowPath, Priority: "low",
			}, fmt.Sprintf("probe-low-%d", i), "")}
		}(i)
	}
	time.Sleep(p.Delay.Duration())
	go func() {
		ch <- result{res: client.Do(ctx, base, Template{
			Method: http.MethodGet, Path: p.HighPath, Priority: "high",
		}, "probe-high", "")}
	}()
	for i := 0; i < p.LowCount+1; i++ {
		r := <-ch
		if r.low {
			if r.res.Err == nil && r.res.Status > 0 {
				out.LowCompleted++
			}
			lowWaits = append(lowWaits, float64(r.res.Wait)/float64(time.Millisecond))
			continue
		}
		out.HighCompleted = r.res.Err == nil && r.res.Status > 0
		out.HighWaitMS = float64(r.res.Wait) / float64(time.Millisecond)
	}
	out.NoStarvation = out.LowCompleted == p.LowCount
	out.LowWaitMS = percentiles(lowWaits)
	out.HighPriorityAdvanced = out.HighCompleted && out.NoStarvation && out.HighWaitMS <= out.LowWaitMS.P50
	return out
}

func runFailoverProbe(ctx context.Context, client *Client, sc Scenario) FailoverReport {
	p := sc.FailoverProbe
	out := FailoverReport{}
	control := &http.Client{Transport: &http.Transport{Proxy: nil}}
	doControl := func(url string) {
		if url == "" {
			return
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return
		}
		resp, err := control.Do(req)
		if err == nil {
			_ = resp.Body.Close()
		}
	}
	doControl(p.ControlDownURL)
	time.Sleep(p.HealthWait.Duration())
	first := client.Do(ctx, sc.GatewayH2, Template{
		Method: http.MethodGet, Path: p.Path, Priority: "high",
	}, "failover-down", "")
	out.FailoverUpstream = first.Upstream
	out.PrimaryDownDetected = first.Err == nil && first.Status == http.StatusOK
	out.FailoverSucceeded = first.Err == nil && first.Status == http.StatusOK &&
		first.Upstream != "" && first.Upstream != p.PrimaryID
	doControl(p.ControlUpURL)
	time.Sleep(p.HealthWait.Duration())
	second := client.Do(ctx, sc.GatewayH2, Template{
		Method: http.MethodGet, Path: p.Path, Priority: "high",
	}, "failover-up", "")
	out.RecoveredUpstream = second.Upstream
	out.RecoverySucceeded = second.Err == nil && second.Status == http.StatusOK &&
		second.Upstream == p.PrimaryID
	out.Success = out.FailoverSucceeded && out.RecoverySucceeded
	return out
}

func runIdempotencyProbe(ctx context.Context, client *Client, sc Scenario) IdempotencyReport {
	p := sc.IdempotencyProbe
	tpl := Template{
		Method: http.MethodPost, Path: p.Path, Priority: "normal",
		Headers: map[string]string{
			"Idempotency-Key": p.Key,
			"Content-Type":    "application/json",
		},
		Body: `{"idempotent":true}`,
	}
	first := client.Do(ctx, sc.GatewayH2, tpl, "idempotency-probe", "")
	second := client.Do(ctx, sc.GatewayH2, tpl, "idempotency-probe", "")
	out := IdempotencyReport{
		FirstStatus: first.Status, SecondStatus: second.Status,
		SameBody: first.BodyHash == second.BodyHash,
		Replay:   second.IdempotentReplay,
	}
	out.Success = first.Err == nil && second.Err == nil &&
		out.FirstStatus == http.StatusOK && out.SecondStatus == http.StatusOK &&
		out.SameBody && out.Replay
	return out
}

func (s *sampler) add(r Result) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.byEntry[r.Protocol] = append(s.byEntry[r.Protocol], r)
}

func summarize(s *sampler) (map[string]EntryReport, int, int, map[string]int, map[string]int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entries := make(map[string]EntryReport)
	retries, retried := 0, 0
	reasons := make(map[string]int)
	rejections := make(map[string]int)
	for protocol, results := range s.byEntry {
		e := EntryReport{
			Requests: len(results), WaitMS: make(map[string]Percentiles),
			StatusCodes: make(map[string]int), Upstreams: make(map[string]int),
		}
		var lat []float64
		waitByPrio := make(map[string][]float64)
		for _, r := range results {
			if r.Err != nil || r.Status >= 500 {
				e.Errors++
			}
			lat = append(lat, float64(r.Duration)/float64(time.Millisecond))
			waitByPrio[r.Priority] = append(waitByPrio[r.Priority], float64(r.Wait)/float64(time.Millisecond))
			e.StatusCodes[strconv.Itoa(r.Status)]++
			if r.Upstream != "" {
				e.Upstreams[r.Upstream]++
			}
			retries += r.Retries
			if r.Retries > 0 {
				retried++
			}
			if r.RetryReason != "" {
				reasons[r.RetryReason]++
			}
			if r.Code != "" && (r.Status >= 400) {
				rejections[r.Code]++
			}
		}
		e.LatencyMS = percentiles(lat)
		for priority, values := range waitByPrio {
			e.WaitMS[priority] = percentiles(values)
		}
		entries[protocol] = e
	}
	return entries, retries, retried, reasons, rejections
}

func percentiles(values []float64) Percentiles {
	if len(values) == 0 {
		return Percentiles{}
	}
	sort.Float64s(values)
	q := func(p float64) float64 {
		if len(values) == 1 {
			return values[0]
		}
		pos := p * float64(len(values)-1)
		lo := int(pos)
		hi := lo + 1
		if hi >= len(values) {
			hi = lo
		}
		frac := pos - float64(lo)
		return values[lo]*(1-frac) + values[hi]*frac
	}
	return Percentiles{P50: q(0.50), P95: q(0.95), P99: q(0.99)}
}

func fetchUpstreamStats(ctx context.Context, urls map[string]string) map[string]any {
	out := make(map[string]any)
	for protocol, url := range urls {
		out[protocol] = fetchJSON(ctx, url)
	}
	return out
}

func fetchJSON(ctx context.Context, url string) map[string]any {
	if url == "" {
		return nil
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return map[string]any{"error": err.Error()}
	}
	client := &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}
	resp, err := client.Do(req)
	if err != nil {
		return map[string]any{"error": err.Error()}
	}
	defer resp.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return map[string]any{"error": err.Error()}
	}
	return out
}

func (s Scenario) baseFor(protocol string) string {
	if protocol == "h3" {
		return s.GatewayH3
	}
	return s.GatewayH2
}

func (s Scenario) pickTemplate(seq int) Template {
	if len(s.Templates) == 0 {
		return Template{Method: http.MethodGet, Path: "/api/shared/echo"}
	}
	total := 0
	for _, t := range s.Templates {
		if t.Weight <= 0 {
			total++
		} else {
			total += t.Weight
		}
	}
	n := seq % total
	for _, t := range s.Templates {
		w := t.Weight
		if w <= 0 {
			w = 1
		}
		if n < w {
			return t
		}
		n -= w
	}
	return s.Templates[len(s.Templates)-1]
}

func (s *Scenario) applyDefaults() {
	if s.Duration.Duration() <= 0 {
		s.Duration = config.Duration(5 * time.Second)
	}
	if s.Rate <= 0 {
		s.Rate = 20
	}
	if s.MaxConcurrent <= 0 {
		s.MaxConcurrent = 32
	}
	if s.H3Ratio < 0 || s.H3Ratio > 1 {
		s.H3Ratio = 0.5
	}
	if len(s.Templates) == 0 {
		s.Templates = []Template{
			{Method: http.MethodGet, Path: "/api/shared/echo?sample=1", Priority: "normal", Weight: 4},
			{Method: http.MethodGet, Path: "/api/h1/echo", Priority: "high", Weight: 1},
			{Method: http.MethodGet, Path: "/api/h2/echo", Priority: "normal", Weight: 1},
			{Method: http.MethodGet, Path: "/api/h2c/echo", Priority: "low", Weight: 1},
			{Method: http.MethodGet, Path: "/fault/slow?slow_ms=60", Priority: "low", Weight: 1},
			{Method: http.MethodGet, Path: "/fault/close", Priority: "high", Headers: map[string]string{"X-Fault-Close": "once"}, Weight: 1},
			{Method: http.MethodPost, Path: "/api/shared/write", Priority: "normal", Headers: map[string]string{"X-Fault-Close": "always"}, Body: `{"write":true}`, Weight: 1},
		}
	}
	if len(s.Consistency) == 0 {
		s.Consistency = []Template{
			{Method: http.MethodGet, Path: "/api/shared/echo?consistency=1", Priority: "normal"},
			{Method: http.MethodPost, Path: "/api/shared/echo", Priority: "high", Body: `{"hello":"world"}`, Headers: map[string]string{"Content-Type": "application/json", "X-Test-Key": "consistency"}},
		}
	}
	if s.Migration.LocalIPs == nil {
		s.Migration.LocalIPs = []string{"127.0.0.1", "127.0.0.1"}
	}
	if s.Migration.SlowPath == "" {
		s.Migration.SlowPath = "/fault/slow?slow_ms=140"
	}
	if s.Migration.ResumePath == "" {
		s.Migration.ResumePath = "/api/shared/migration-resume"
	}
	if s.Migration.RequestDelay.Duration() <= 0 {
		s.Migration.RequestDelay = config.Duration(30 * time.Millisecond)
	}
	if s.PriorityProbe.LowCount <= 0 {
		s.PriorityProbe.LowCount = 8
	}
	if s.PriorityProbe.SlowPath == "" {
		s.PriorityProbe.SlowPath = "/fault/slow?slow_ms=220"
	}
	if s.PriorityProbe.HighPath == "" {
		s.PriorityProbe.HighPath = "/api/shared/probe-high"
	}
	if s.PriorityProbe.Delay.Duration() <= 0 {
		s.PriorityProbe.Delay = config.Duration(25 * time.Millisecond)
	}
	if s.FailoverProbe.Path == "" {
		s.FailoverProbe.Path = "/api/shared/failover"
	}
	if s.FailoverProbe.HealthWait.Duration() <= 0 {
		s.FailoverProbe.HealthWait = config.Duration(1200 * time.Millisecond)
	}
	if s.IdempotencyProbe.Path == "" {
		s.IdempotencyProbe.Path = "/api/shared/idempotency-probe"
	}
	if s.IdempotencyProbe.Key == "" {
		s.IdempotencyProbe.Key = "idempotency-probe-key"
	}
}

func (m MigrationConfig) firstIP() string {
	if len(m.LocalIPs) > 0 {
		return m.LocalIPs[0]
	}
	return "127.0.0.1"
}

func (m MigrationConfig) secondIP() string {
	if len(m.LocalIPs) > 1 {
		return m.LocalIPs[1]
	}
	return m.firstIP()
}

func durationHeader(v string) time.Duration {
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return 0
	}
	return time.Duration(f * float64(time.Millisecond))
}

func intHeader(v string) int {
	n, _ := strconv.Atoi(v)
	return n
}

func hashBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
