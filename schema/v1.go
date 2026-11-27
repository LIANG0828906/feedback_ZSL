package schema

import (
	"proxydiag/bizerr"
	"proxydiag/domain"
)

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

type v1Document struct {
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

func loadV1(data []byte) (*domain.Project, error) {
	var raw v1Document
	if err := decodeStrict(data, &raw, SchemaV1); err != nil {
		return nil, err
	}
	if err := required(SchemaV1, "version", raw.Version); err != nil {
		return nil, err
	}
	if raw.TickMS <= 0 {
		return nil, bizerr.New(bizerr.TimeoutOrder, "v1 tick_ms must be positive").
			With("schema", SchemaV1).WithInt64("tick_ms", raw.TickMS)
	}
	if len(raw.Routes) == 0 {
		return nil, bizerr.New(bizerr.SchemaRequired, "v1 requires the flat routes table").
			With("schema", SchemaV1)
	}
	p := &domain.Project{
		SourceSchema: SchemaV1,
		Version:      raw.Version,
		TickMS:       raw.TickMS,
		Fixed: domain.FixedConfig{
			PathWidthBits: raw.Fixed.PathWidthBits, PathFracBits: raw.Fixed.PathFracBits,
			WeightWidthBits: raw.Fixed.WeightWidthBits, WeightFracBits: raw.Fixed.WeightFracBits,
			Tolerance: raw.Fixed.Tolerance,
		},
		RetryTickMS:     raw.RetryTickMS,
		BackoffPermille: raw.BackoffPermille,
	}
	for _, l := range raw.Listeners {
		p.Listeners = append(p.Listeners, domain.Listener{
			ID: l.ID, Prefix: l.Prefix, TimeoutMS: l.TimeoutMS,
			InflightLimit: l.InflightLimit, Routes: []domain.Route{},
		})
	}
	listenerIndex := map[string]int{}
	for i := range p.Listeners {
		listenerIndex[p.Listeners[i].ID] = i
	}
	for _, r := range raw.Routes {
		idx, ok := listenerIndex[r.Listener]
		if !ok {
			return nil, bizerr.New(bizerr.ListenerMissing, "v1 route references an unknown listener").
				With("schema", SchemaV1).With("route", r.ID).With("listener", r.Listener)
		}
		p.Listeners[idx].Routes = append(p.Listeners[idx].Routes, domain.Route{
			ID: r.ID, ListenerID: r.Listener, Prefix: r.Prefix,
			Rewrite: domain.Rewrite{
				StripPrefix: r.StripPrefix, AddPrefix: r.AddPrefix,
				RemoveHeaders: append([]string(nil), r.RemoveHeaders...),
				SetHeaders:    convertHeaders(r.SetHeaders),
			},
			UpstreamIDs: append([]string(nil), r.Upstreams...),
		})
	}
	for _, u := range raw.Upstreams {
		if u.WeightPermille <= 0 {
			return nil, bizerr.New(bizerr.WeightRange, "v1 weight_permille must be positive").
				With("schema", SchemaV1).With("upstream", u.ID).WithInt64("weight_permille", u.WeightPermille)
		}
		p.Upstreams = append(p.Upstreams, domain.Upstream{
			ID: u.ID, Address: u.Address,
			WeightPPM: u.WeightPermille * 1000,
			Healthy:   u.Healthy, MaxRetries: u.MaxRetries,
		})
	}
	for _, r := range raw.Requests {
		p.Requests = append(p.Requests, domain.Request{
			Seq: r.Seq, ArrivalMS: r.ArrivalMS, Method: r.Method, RawPath: r.Path,
			Headers:          convertHeaders(r.Headers),
			FailurePlan:      append([]string(nil), r.Failures...),
			AttemptLatencyMS: append([]int64(nil), r.LatenciesMS...),
		})
	}
	return p, nil
}
