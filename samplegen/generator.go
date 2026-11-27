package samplegen

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"proxydiag/domain"
	"proxydiag/schema"
)

const (
	DefaultSeed = int64(424242)
	ProjectVer  = "edge-lab-2026@1"
)

type Manifest struct {
	Seed     int64             `json:"seed"`
	Version  string            `json:"version"`
	Files    map[string]string `json:"files_sha256"`
	Requests int               `json:"requests"`
}

type rng struct {
	state uint64
}

func newRNG(seed int64) *rng {
	return &rng{state: uint64(seed) + 0x9e3779b97f4a7c15}
}

func (r *rng) next() uint64 {
	r.state += 0x9e3779b97f4a7c15
	z := r.state
	z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9
	z = (z ^ (z >> 27)) * 0x94d049bb133111eb
	return z ^ (z >> 31)
}

func BuildCanonical(seed int64) *domain.Project {
	r := newRNG(seed)
	_ = r
	probe := func(path string) *domain.HealthProbe {
		return &domain.HealthProbe{Path: path, IntervalMS: 30, TimeoutMS: 10}
	}
	proj := &domain.Project{
		SourceSchema: "canonical",
		Version:      ProjectVer,
		TickMS:       10,
		Fixed: domain.FixedConfig{
			PathWidthBits: 32, PathFracBits: 16,
			WeightWidthBits: 48, WeightFracBits: 16,
			Tolerance: 1,
		},
		RetryTickMS:     20,
		BackoffPermille: 1500,
		Listeners: []domain.Listener{
			{
				ID: "edge-api", Prefix: "/api/", TimeoutMS: 200, InflightLimit: 3,
				Routes: []domain.Route{
					{
						ID: "R-api-v1-admin", ListenerID: "edge-api", Prefix: "/v1/admin/",
						Rewrite: domain.Rewrite{
							StripPrefix: "/api/v1/admin/", AddPrefix: "/backend/admin/",
							RemoveHeaders: []string{"X-Internal-Debug"},
							SetHeaders: []domain.Header{
								{Name: "X-Forwarded-Prefix", Value: "/api/v1/admin"},
								{Name: "X-Admin-Route", Value: "true"},
							},
						},
						UpstreamIDs: []string{"u-blue", "u-gold"},
					},
					{
						ID: "R-api-v1", ListenerID: "edge-api", Prefix: "/v1/",
						Rewrite: domain.Rewrite{
							StripPrefix: "/api/v1/", AddPrefix: "/backend/v1/",
							RemoveHeaders: []string{"X-Internal-Debug"},
							SetHeaders: []domain.Header{
								{Name: "X-Forwarded-Prefix", Value: "/api/v1"},
							},
						},
						UpstreamIDs: []string{"u-blue", "u-green", "u-gold", "u-standby"},
					},
					{
						ID: "R-api-root", ListenerID: "edge-api", Prefix: "/",
						Rewrite: domain.Rewrite{
							StripPrefix: "/api/", AddPrefix: "/backend/",
							SetHeaders: []domain.Header{
								{Name: "X-Forwarded-Prefix", Value: "/api"},
							},
						},
						UpstreamIDs: []string{"u-green", "u-gold"},
					},
				},
			},
			{
				ID: "edge-ops", Prefix: "/ops/", TimeoutMS: 250, InflightLimit: 4,
				Routes: []domain.Route{
					{
						ID: "R-ops-metrics", ListenerID: "edge-ops", Prefix: "/metrics/",
						Rewrite: domain.Rewrite{
							StripPrefix: "/ops/metrics/", AddPrefix: "/internal/metrics/",
							RemoveHeaders: []string{"X-Debug"},
							SetHeaders: []domain.Header{
								{Name: "X-Forwarded-Prefix", Value: "/ops/metrics"},
							},
						},
						UpstreamIDs: []string{"u-ops", "u-blue"},
					},
					{
						ID: "R-ops-root", ListenerID: "edge-ops", Prefix: "/",
						Rewrite: domain.Rewrite{
							StripPrefix: "/ops/", AddPrefix: "/ops-internal/",
						},
						UpstreamIDs: []string{"u-ops", "u-green"},
					},
				},
			},
			{
				ID: "edge-static", Prefix: "/static/", TimeoutMS: 300, InflightLimit: 2,
				Routes: []domain.Route{
					{
						ID: "R-static-assets", ListenerID: "edge-static", Prefix: "/assets/",
						Rewrite: domain.Rewrite{
							StripPrefix: "/static/assets/", AddPrefix: "/cdn/",
							SetHeaders: []domain.Header{
								{Name: "X-CDN-Tier", Value: "edge"},
							},
						},
						UpstreamIDs: []string{"u-cdn", "u-archive"},
					},
					{
						ID: "R-static-root", ListenerID: "edge-static", Prefix: "/",
						Rewrite: domain.Rewrite{
							StripPrefix: "/static/", AddPrefix: "/assets/",
						},
						UpstreamIDs: []string{"u-cdn"},
					},
				},
			},
		},
		Upstreams: []domain.Upstream{
			{ID: "u-blue", Address: "http://blue.local:8080", WeightPPM: 140000, Healthy: true, MaxRetries: 2, Probe: probe("/health")},
			{ID: "u-green", Address: "http://green.local:8080", WeightPPM: 110000, Healthy: true, MaxRetries: 1, Probe: probe("/healthz")},
			{ID: "u-gold", Address: "http://gold.local:8080", WeightPPM: 60000, Healthy: true, MaxRetries: 2},
			{ID: "u-ops", Address: "http://ops.local:9090", WeightPPM: 90000, Healthy: true, MaxRetries: 0, Probe: probe("/ready")},
			{ID: "u-cdn", Address: "http://cdn.local:8081", WeightPPM: 70000, Healthy: true, MaxRetries: 1, Probe: probe("/ping")},
			{ID: "u-archive", Address: "http://archive.local:8081", WeightPPM: 50000, Healthy: false, MaxRetries: 3},
			{ID: "u-standby", Address: "http://standby.local:8080", WeightPPM: 30000, Healthy: true, MaxRetries: 0},
		},
	}
	proj.Requests = buildRequests(seed, proj)
	return proj
}

func buildRequests(seed int64, p *domain.Project) []domain.Request {
	r := newRNG(seed ^ 0x51ed270b)
	methods := []string{"GET", "POST", "PUT", "DELETE"}
	templates := []string{
		"/api/v1/users/%d?expand=roles",
		"/api/v1/admin/audit/%d",
		"/api/health/%d",
		"/ops/metrics/q/%d?window=5m",
		"/static/assets/app-%d.js",
		"/ops/healthz",
		"/static/favicon-%d.ico",
		"/api/v1/items/%d",
	}
	out := make([]domain.Request, 0, 40)
	for i := 0; i < 40; i++ {
		seq := i + 1
		tmpl := templates[(i*7+int(r.next()%3))%len(templates)]
		raw := fmt.Sprintf(tmpl, 100+i)
		if tmpl == "/ops/healthz" {
			raw = tmpl
		}
		headers := []domain.Header{
			{Name: "X-Request-ID", Value: fmt.Sprintf("req-%03d", seq)},
			{Name: "Accept", Value: "application/json"},
			{Name: "User-Agent", Value: "proxydiag-gen/1"},
		}
		if i%2 == 0 {
			headers = append(headers, domain.Header{Name: "X-Internal-Debug", Value: "drop-me"})
		}
		if i%3 == 0 {
			headers = append(headers, domain.Header{Name: "X-Debug", Value: "verbose"})
		}
		failures, latencies := requestPlan(seq)
		out = append(out, domain.Request{
			Seq: seq, ArrivalMS: int64(i/2) * 10,
			Method: methods[i%len(methods)], RawPath: raw, Headers: headers,
			FailurePlan: failures, AttemptLatencyMS: latencies,
		})
	}
	return out
}

func requestPlan(seq int) ([]string, []int64) {
	if seq%13 == 0 {
		return nil, []int64{999, 999, 999, 999}
	}
	latencies := []int64{int64(5 + seq%4), int64(4 + seq%3), int64(6 + seq%2), 7}
	switch seq % 8 {
	case 0, 6:
		return nil, latencies
	case 1:
		return []string{"connect"}, latencies
	case 2:
		return []string{"http_503", "connect"}, latencies
	case 3:
		return []string{"timeout"}, latencies
	case 4:
		return []string{"connect", "http_503", "connect"}, latencies
	case 5:
		return []string{"http_503"}, latencies
	case 7:
		return []string{"timeout", "connect"}, latencies
	default:
		return nil, latencies
	}
}

type rendered struct {
	V1 []byte
	V2 []byte
	V3 []byte
}

type v1Fixed struct {
	PathWidthBits   int   `json:"path_width_bits"`
	PathFracBits    uint  `json:"path_fraction_bits"`
	WeightWidthBits int   `json:"weight_width_bits"`
	WeightFracBits  uint  `json:"weight_fraction_bits"`
	Tolerance       int64 `json:"tolerance"`
}

type v1Listener struct {
	ID            string `json:"id"`
	Prefix        string `json:"prefix"`
	TimeoutMS     int64  `json:"timeout_ms"`
	InflightLimit int    `json:"inflight_limit"`
}

type v1Route struct {
	ID            string          `json:"id"`
	Listener      string          `json:"listener"`
	Prefix        string          `json:"prefix"`
	StripPrefix   string          `json:"strip_prefix"`
	AddPrefix     string          `json:"add_prefix"`
	RemoveHeaders []string        `json:"remove_headers,omitempty"`
	SetHeaders    []domain.Header `json:"set_headers,omitempty"`
	Upstreams     []string        `json:"upstreams"`
}

type v1Upstream struct {
	ID             string `json:"id"`
	Address        string `json:"address"`
	WeightPermille int64  `json:"weight_permille"`
	Healthy        bool   `json:"healthy"`
	MaxRetries     int    `json:"max_retries"`
}

type v1Request struct {
	Seq         int             `json:"seq"`
	ArrivalMS   int64           `json:"arrival_ms"`
	Method      string          `json:"method"`
	Path        string          `json:"path"`
	Headers     []domain.Header `json:"headers"`
	Failures    []string        `json:"failures,omitempty"`
	LatenciesMS []int64         `json:"latencies_ms,omitempty"`
}

type v1Doc struct {
	Schema          string       `json:"schema"`
	Version         string       `json:"version"`
	Seed            int64        `json:"seed"`
	TickMS          int64        `json:"tick_ms"`
	Fixed           v1Fixed      `json:"fixed"`
	RetryTickMS     int64        `json:"retry_tick_ms"`
	BackoffPermille int64        `json:"backoff_permille"`
	Listeners       []v1Listener `json:"listeners"`
	Routes          []v1Route    `json:"routes"`
	Upstreams       []v1Upstream `json:"upstreams"`
	Requests        []v1Request  `json:"requests"`
}

type v2Fixed struct {
	PathWidthBits   int   `json:"pathWidthBits"`
	PathFracBits    uint  `json:"pathFractionBits"`
	WeightWidthBits int   `json:"weightWidthBits"`
	WeightFracBits  uint  `json:"weightFractionBits"`
	Tolerance       int64 `json:"tolerance"`
}

type v2Rewrite struct {
	StripPrefix   string          `json:"stripPrefix"`
	AddPrefix     string          `json:"addPrefix"`
	RemoveHeaders []string        `json:"removeHeaders,omitempty"`
	SetHeaders    []domain.Header `json:"setHeaders,omitempty"`
}

type v2Route struct {
	ID        string    `json:"id"`
	Prefix    string    `json:"prefix"`
	Rewrite   v2Rewrite `json:"rewrite"`
	Upstreams []string  `json:"upstreams"`
}

type v2Listener struct {
	ID      string `json:"id"`
	Prefix  string `json:"prefix"`
	Timeout struct {
		Ticks int64 `json:"ticks"`
	} `json:"timeout"`
	Budget struct {
		RequestsPerSecond int64 `json:"requestsPerSecond"`
	} `json:"budget"`
	Routes []v2Route `json:"routes"`
}

type v2Probe struct {
	Path          string `json:"path"`
	IntervalTicks int64  `json:"intervalTicks"`
	TimeoutTicks  int64  `json:"timeoutTicks"`
}

type v2Upstream struct {
	ID                string   `json:"id"`
	URL               string   `json:"url"`
	WeightBasisPoints int64    `json:"weightBasisPoints"`
	Healthy           bool     `json:"healthy"`
	MaxRetries        int      `json:"maxRetries"`
	HealthProbe       *v2Probe `json:"healthProbe,omitempty"`
}

type v2Request struct {
	Sequence     int             `json:"sequence"`
	ArrivalTicks int64           `json:"arrivalTicks"`
	Method       string          `json:"method"`
	RawPath      string          `json:"rawPath"`
	Headers      []domain.Header `json:"headers"`
	FailurePlan  []string        `json:"failurePlan,omitempty"`
	LatenciesMs  []int64         `json:"latenciesMs,omitempty"`
}

type v2Doc struct {
	Schema  string `json:"schema"`
	Version string `json:"version"`
	Seed    int64  `json:"seed"`
	Clock   struct {
		TickMS int64 `json:"tickMs"`
	} `json:"clock"`
	FixedPoint v2Fixed `json:"fixedPoint"`
	Retry      struct {
		BackoffTicks     int64 `json:"backoffTicks"`
		ScaleBasisPoints int64 `json:"scaleBasisPoints"`
	} `json:"retry"`
	Listeners []v2Listener `json:"listeners"`
	Upstreams []v2Upstream `json:"upstreams"`
	Requests  []v2Request  `json:"requests"`
}

type v3Fixed struct {
	PathWidthBits   int   `json:"path_width_bits"`
	PathFracBits    uint  `json:"path_fraction_bits"`
	WeightWidthBits int   `json:"weight_width_bits"`
	WeightFracBits  uint  `json:"weight_fraction_bits"`
	Tolerance       int64 `json:"tolerance"`
}

type v3Rewrite struct {
	StripPrefix   string          `json:"strip_prefix"`
	AddPrefix     string          `json:"add_prefix"`
	RemoveHeaders []string        `json:"remove_headers,omitempty"`
	SetHeaders    []domain.Header `json:"set_headers,omitempty"`
}

type v3Route struct {
	ID        string    `json:"id"`
	Prefix    string    `json:"prefix"`
	Rewrite   v3Rewrite `json:"rewrite"`
	Upstreams []string  `json:"upstreams"`
}

type v3Listener struct {
	ID          string    `json:"id"`
	Prefix      string    `json:"prefix"`
	TimeoutMS   int64     `json:"timeout_ms"`
	MaxInflight int       `json:"max_inflight"`
	Routes      []v3Route `json:"routes"`
}

type v3Probe struct {
	Path       string `json:"path"`
	IntervalMS int64  `json:"interval_ms"`
	TimeoutMS  int64  `json:"timeout_ms"`
}

type v3Upstream struct {
	ID              string   `json:"id"`
	Address         string   `json:"address"`
	WeightPerMyriad int64    `json:"weight_per_myriad"`
	Healthy         bool     `json:"healthy"`
	MaxRetries      int      `json:"max_retries"`
	Probe           *v3Probe `json:"probe,omitempty"`
}

type v3Request struct {
	Seq         int             `json:"seq"`
	ArrivalMS   int64           `json:"arrival_ms"`
	Method      string          `json:"method"`
	RawPath     string          `json:"raw_path"`
	Headers     []domain.Header `json:"headers"`
	FailurePlan []string        `json:"failure_plan,omitempty"`
	LatencyMS   []int64         `json:"latency_ms,omitempty"`
}

type v3Doc struct {
	Schema  string `json:"schema"`
	Version string `json:"version"`
	Seed    int64  `json:"seed"`
	Clock   struct {
		TickMS int64 `json:"tick_ms"`
	} `json:"clock"`
	Fixed v3Fixed `json:"fixed"`
	Retry struct {
		BackoffMS     int64 `json:"backoff_ms"`
		ScalePermille int64 `json:"scale_permille"`
	} `json:"retry"`
	Listeners []v3Listener `json:"listeners"`
	Upstreams []v3Upstream `json:"upstreams"`
	Requests  []v3Request  `json:"requests"`
}

func renderV1(p *domain.Project, seed int64) ([]byte, error) {
	d := v1Doc{
		Schema: schema.SchemaV1, Version: p.Version, Seed: seed, TickMS: p.TickMS,
		Fixed: v1Fixed{
			PathWidthBits: p.Fixed.PathWidthBits, PathFracBits: p.Fixed.PathFracBits,
			WeightWidthBits: p.Fixed.WeightWidthBits, WeightFracBits: p.Fixed.WeightFracBits,
			Tolerance: p.Fixed.Tolerance,
		},
		RetryTickMS: p.RetryTickMS, BackoffPermille: p.BackoffPermille,
	}
	for _, l := range p.Listeners {
		d.Listeners = append(d.Listeners, v1Listener{
			ID: l.ID, Prefix: l.Prefix, TimeoutMS: l.TimeoutMS, InflightLimit: l.InflightLimit,
		})
		for _, r := range l.Routes {
			d.Routes = append(d.Routes, v1Route{
				ID: r.ID, Listener: l.ID, Prefix: r.Prefix,
				StripPrefix: r.Rewrite.StripPrefix, AddPrefix: r.Rewrite.AddPrefix,
				RemoveHeaders: append([]string(nil), r.Rewrite.RemoveHeaders...),
				SetHeaders:    append([]domain.Header(nil), r.Rewrite.SetHeaders...),
				Upstreams:     append([]string(nil), r.UpstreamIDs...),
			})
		}
	}
	for _, u := range p.Upstreams {
		d.Upstreams = append(d.Upstreams, v1Upstream{
			ID: u.ID, Address: u.Address, WeightPermille: u.WeightPPM / 1000,
			Healthy: u.Healthy, MaxRetries: u.MaxRetries,
		})
	}
	for _, r := range p.Requests {
		d.Requests = append(d.Requests, v1Request{
			Seq: r.Seq, ArrivalMS: r.ArrivalMS, Method: r.Method, Path: r.RawPath,
			Headers:     append([]domain.Header(nil), r.Headers...),
			Failures:    append([]string(nil), r.FailurePlan...),
			LatenciesMS: append([]int64(nil), r.AttemptLatencyMS...),
		})
	}
	return json.MarshalIndent(d, "", "  ")
}

func minimalRPS(inflight, timeoutMS int64) int64 {
	for rps := int64(1); rps < 1_000_000; rps++ {
		if (rps*timeoutMS+999)/1000 == inflight {
			return rps
		}
	}
	return 0
}

func renderV2(p *domain.Project, seed int64) ([]byte, error) {
	d := v2Doc{Schema: schema.SchemaV2, Version: p.Version, Seed: seed}
	d.Clock.TickMS = p.TickMS
	d.FixedPoint = v2Fixed{
		PathWidthBits: p.Fixed.PathWidthBits, PathFracBits: p.Fixed.PathFracBits,
		WeightWidthBits: p.Fixed.WeightWidthBits, WeightFracBits: p.Fixed.WeightFracBits,
		Tolerance: p.Fixed.Tolerance,
	}
	d.Retry.BackoffTicks = p.RetryTickMS / p.TickMS
	d.Retry.ScaleBasisPoints = p.BackoffPermille * 10
	for _, l := range p.Listeners {
		out := v2Listener{ID: l.ID, Prefix: l.Prefix}
		out.Timeout.Ticks = l.TimeoutMS / p.TickMS
		out.Budget.RequestsPerSecond = minimalRPS(int64(l.InflightLimit), l.TimeoutMS)
		for _, r := range l.Routes {
			out.Routes = append(out.Routes, v2Route{
				ID: r.ID, Prefix: r.Prefix,
				Rewrite: v2Rewrite{
					StripPrefix: r.Rewrite.StripPrefix, AddPrefix: r.Rewrite.AddPrefix,
					RemoveHeaders: append([]string(nil), r.Rewrite.RemoveHeaders...),
					SetHeaders:    append([]domain.Header(nil), r.Rewrite.SetHeaders...),
				},
				Upstreams: append([]string(nil), r.UpstreamIDs...),
			})
		}
		d.Listeners = append(d.Listeners, out)
	}
	for _, u := range p.Upstreams {
		out := v2Upstream{
			ID: u.ID, URL: u.Address, WeightBasisPoints: u.WeightPPM / 100,
			Healthy: u.Healthy, MaxRetries: u.MaxRetries,
		}
		if u.Probe != nil {
			out.HealthProbe = &v2Probe{
				Path: u.Probe.Path, IntervalTicks: u.Probe.IntervalMS / p.TickMS,
				TimeoutTicks: u.Probe.TimeoutMS / p.TickMS,
			}
		}
		d.Upstreams = append(d.Upstreams, out)
	}
	for _, r := range p.Requests {
		d.Requests = append(d.Requests, v2Request{
			Sequence: r.Seq, ArrivalTicks: r.ArrivalMS / p.TickMS,
			Method: r.Method, RawPath: r.RawPath,
			Headers:     append([]domain.Header(nil), r.Headers...),
			FailurePlan: append([]string(nil), r.FailurePlan...),
			LatenciesMs: append([]int64(nil), r.AttemptLatencyMS...),
		})
	}
	return json.MarshalIndent(d, "", "  ")
}

func renderV3(p *domain.Project, seed int64) ([]byte, error) {
	d := v3Doc{Schema: schema.SchemaV3, Version: p.Version, Seed: seed}
	d.Clock.TickMS = p.TickMS
	d.Fixed = v3Fixed{
		PathWidthBits: p.Fixed.PathWidthBits, PathFracBits: p.Fixed.PathFracBits,
		WeightWidthBits: p.Fixed.WeightWidthBits, WeightFracBits: p.Fixed.WeightFracBits,
		Tolerance: p.Fixed.Tolerance,
	}
	d.Retry.BackoffMS = p.RetryTickMS
	d.Retry.ScalePermille = p.BackoffPermille
	for _, l := range p.Listeners {
		out := v3Listener{
			ID: l.ID, Prefix: l.Prefix, TimeoutMS: l.TimeoutMS, MaxInflight: l.InflightLimit,
		}
		for _, r := range l.Routes {
			out.Routes = append(out.Routes, v3Route{
				ID: r.ID, Prefix: r.Prefix,
				Rewrite: v3Rewrite{
					StripPrefix: r.Rewrite.StripPrefix, AddPrefix: r.Rewrite.AddPrefix,
					RemoveHeaders: append([]string(nil), r.Rewrite.RemoveHeaders...),
					SetHeaders:    append([]domain.Header(nil), r.Rewrite.SetHeaders...),
				},
				Upstreams: append([]string(nil), r.UpstreamIDs...),
			})
		}
		d.Listeners = append(d.Listeners, out)
	}
	for _, u := range p.Upstreams {
		out := v3Upstream{
			ID: u.ID, Address: u.Address, WeightPerMyriad: u.WeightPPM / 100,
			Healthy: u.Healthy, MaxRetries: u.MaxRetries,
		}
		if u.Probe != nil {
			out.Probe = &v3Probe{
				Path: u.Probe.Path, IntervalMS: u.Probe.IntervalMS, TimeoutMS: u.Probe.TimeoutMS,
			}
		}
		d.Upstreams = append(d.Upstreams, out)
	}
	for _, r := range p.Requests {
		d.Requests = append(d.Requests, v3Request{
			Seq: r.Seq, ArrivalMS: r.ArrivalMS, Method: r.Method, RawPath: r.RawPath,
			Headers:     append([]domain.Header(nil), r.Headers...),
			FailurePlan: append([]string(nil), r.FailurePlan...),
			LatencyMS:   append([]int64(nil), r.AttemptLatencyMS...),
		})
	}
	return json.MarshalIndent(d, "", "  ")
}

func RenderAll(p *domain.Project, seed int64) (rendered, error) {
	v1, err := renderV1(p, seed)
	if err != nil {
		return rendered{}, err
	}
	v2, err := renderV2(p, seed)
	if err != nil {
		return rendered{}, err
	}
	v3, err := renderV3(p, seed)
	if err != nil {
		return rendered{}, err
	}
	return rendered{V1: v1, V2: v2, V3: v3}, nil
}

func Write(outDir string, seed int64) (*Manifest, error) {
	p := BuildCanonical(seed)
	truth, err := BuildTruth(p, seed)
	if err != nil {
		return nil, err
	}
	files, err := RenderAll(p, seed)
	if err != nil {
		return nil, err
	}
	truthJSON, err := json.MarshalIndent(truth, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return nil, err
	}
	contents := map[string][]byte{
		"project_v1.json": files.V1,
		"project_v2.json": files.V2,
		"project_v3.json": files.V3,
		"truth.json":      truthJSON,
	}
	manifest := &Manifest{
		Seed: seed, Version: p.Version, Files: map[string]string{}, Requests: len(p.Requests),
	}
	names := make([]string, 0, len(contents))
	for name := range contents {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		sum := sha256.Sum256(contents[name])
		manifest.Files[name] = hex.EncodeToString(sum[:])
		tmp := filepath.Join(outDir, "."+name+".tmp")
		if err := os.WriteFile(tmp, contents[name], 0o644); err != nil {
			return nil, fmt.Errorf("write %s: %w", name, err)
		}
		if err := os.Rename(tmp, filepath.Join(outDir, name)); err != nil {
			return nil, fmt.Errorf("rename %s: %w", name, err)
		}
	}
	manifestJSON, _ := json.MarshalIndent(manifest, "", "  ")
	if err := os.WriteFile(filepath.Join(outDir, "manifest.json"), manifestJSON, 0o644); err != nil {
		return nil, err
	}
	return manifest, nil
}

func LoadTruth(path string) (*domain.Truth, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var t domain.Truth
	if err := json.Unmarshal(b, &t); err != nil {
		return nil, err
	}
	return &t, nil
}
