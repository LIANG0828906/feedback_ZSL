package schema

import (
	"proxydiag/bizerr"
	"proxydiag/domain"
)

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

type v3Retry struct {
	BackoffMS     int64 `json:"backoff_ms"`
	ScalePermille int64 `json:"scale_permille"`
}

type v3Clock struct {
	TickMS int64 `json:"tick_ms"`
}

type v3Document struct {
	Schema    string       `json:"schema"`
	Version   string       `json:"version"`
	Seed      int64        `json:"seed"`
	Clock     v3Clock      `json:"clock"`
	Fixed     v3Fixed      `json:"fixed"`
	Retry     v3Retry      `json:"retry"`
	Listeners []v3Listener `json:"listeners"`
	Upstreams []v3Upstream `json:"upstreams"`
	Requests  []v3Request  `json:"requests"`
}

func loadV3(data []byte) (*domain.Project, error) {
	var raw v3Document
	if err := decodeStrict(data, &raw, SchemaV3); err != nil {
		return nil, err
	}
	if err := required(SchemaV3, "version", raw.Version); err != nil {
		return nil, err
	}
	if raw.Clock.TickMS <= 0 {
		return nil, bizerr.New(bizerr.TimeoutOrder, "v3 clock.tick_ms must be positive").
			With("schema", SchemaV3)
	}
	if raw.Retry.BackoffMS <= 0 {
		return nil, bizerr.New(bizerr.TimeoutOrder, "v3 retry.backoff_ms must be positive").
			With("schema", SchemaV3)
	}
	p := &domain.Project{
		SourceSchema: SchemaV3,
		Version:      raw.Version,
		TickMS:       raw.Clock.TickMS,
		Fixed: domain.FixedConfig{
			PathWidthBits: raw.Fixed.PathWidthBits, PathFracBits: raw.Fixed.PathFracBits,
			WeightWidthBits: raw.Fixed.WeightWidthBits, WeightFracBits: raw.Fixed.WeightFracBits,
			Tolerance: raw.Fixed.Tolerance,
		},
		RetryTickMS:     raw.Retry.BackoffMS,
		BackoffPermille: raw.Retry.ScalePermille,
	}
	for _, l := range raw.Listeners {
		out := domain.Listener{
			ID: l.ID, Prefix: l.Prefix, TimeoutMS: l.TimeoutMS,
			InflightLimit: l.MaxInflight, Routes: []domain.Route{},
		}
		for _, r := range l.Routes {
			out.Routes = append(out.Routes, domain.Route{
				ID: r.ID, ListenerID: l.ID, Prefix: r.Prefix,
				Rewrite: domain.Rewrite{
					StripPrefix: r.Rewrite.StripPrefix, AddPrefix: r.Rewrite.AddPrefix,
					RemoveHeaders: append([]string(nil), r.Rewrite.RemoveHeaders...),
					SetHeaders:    convertHeaders(r.Rewrite.SetHeaders),
				},
				UpstreamIDs: append([]string(nil), r.Upstreams...),
			})
		}
		p.Listeners = append(p.Listeners, out)
	}
	for _, u := range raw.Upstreams {
		if u.WeightPerMyriad <= 0 {
			return nil, bizerr.New(bizerr.WeightRange, "v3 weight_per_myriad must be positive").
				With("schema", SchemaV3).With("upstream", u.ID).WithInt64("weight_per_myriad", u.WeightPerMyriad)
		}
		up := domain.Upstream{
			ID: u.ID, Address: u.Address, WeightPPM: u.WeightPerMyriad * 100,
			Healthy: u.Healthy, MaxRetries: u.MaxRetries,
		}
		if u.Probe != nil {
			up.Probe = &domain.HealthProbe{
				Path: u.Probe.Path, IntervalMS: u.Probe.IntervalMS, TimeoutMS: u.Probe.TimeoutMS,
			}
		}
		p.Upstreams = append(p.Upstreams, up)
	}
	for _, r := range raw.Requests {
		p.Requests = append(p.Requests, domain.Request{
			Seq: r.Seq, ArrivalMS: r.ArrivalMS, Method: r.Method, RawPath: r.RawPath,
			Headers:          convertHeaders(r.Headers),
			FailurePlan:      append([]string(nil), r.FailurePlan...),
			AttemptLatencyMS: append([]int64(nil), r.LatencyMS...),
		})
	}
	return p, nil
}
