package schema

import (
	"proxydiag/bizerr"
	"proxydiag/domain"
)

type v2Fixed struct {
	PathWidthBits   int   `json:"pathWidthBits"`
	PathFracBits    uint  `json:"pathFractionBits"`
	WeightWidthBits int   `json:"weightWidthBits"`
	WeightFracBits  uint  `json:"weightFractionBits"`
	Tolerance       int64 `json:"tolerance"`
}

type v2Clock struct {
	TickMS int64 `json:"tickMs"`
}

type v2Timeout struct {
	Ticks int64 `json:"ticks"`
}

type v2Budget struct {
	RequestsPerSecond int64 `json:"requestsPerSecond"`
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
	ID      string    `json:"id"`
	Prefix  string    `json:"prefix"`
	Timeout v2Timeout `json:"timeout"`
	Budget  v2Budget  `json:"budget"`
	Routes  []v2Route `json:"routes"`
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

type v2Retry struct {
	BackoffTicks     int64 `json:"backoffTicks"`
	ScaleBasisPoints int64 `json:"scaleBasisPoints"`
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

type v2Document struct {
	Schema     string       `json:"schema"`
	Version    string       `json:"version"`
	Seed       int64        `json:"seed"`
	Clock      v2Clock      `json:"clock"`
	FixedPoint v2Fixed      `json:"fixedPoint"`
	Retry      v2Retry      `json:"retry"`
	Listeners  []v2Listener `json:"listeners"`
	Upstreams  []v2Upstream `json:"upstreams"`
	Requests   []v2Request  `json:"requests"`
}

func loadV2(data []byte) (*domain.Project, error) {
	var raw v2Document
	if err := decodeStrict(data, &raw, SchemaV2); err != nil {
		return nil, err
	}
	if err := required(SchemaV2, "version", raw.Version); err != nil {
		return nil, err
	}
	tick := raw.Clock.TickMS
	if tick <= 0 {
		return nil, bizerr.New(bizerr.TimeoutOrder, "v2 clock.tickMs must be positive").
			With("schema", SchemaV2).WithInt64("tickMs", tick)
	}
	if raw.Retry.BackoffTicks <= 0 {
		return nil, bizerr.New(bizerr.TimeoutOrder, "v2 retry.backoffTicks must be positive").
			With("schema", SchemaV2)
	}
	if raw.Retry.ScaleBasisPoints%10 != 0 {
		return nil, bizerr.New(bizerr.SchemaFieldCombo, "v2 retry scale cannot be represented in per-mille").
			With("schema", SchemaV2).WithInt64("scaleBasisPoints", raw.Retry.ScaleBasisPoints)
	}
	retryTickMS, err := scaleCeil(raw.Retry.BackoffTicks, tick, 1, "retry_tick")
	if err != nil {
		return nil, err
	}
	p := &domain.Project{
		SourceSchema: SchemaV2,
		Version:      raw.Version,
		TickMS:       tick,
		Fixed: domain.FixedConfig{
			PathWidthBits: raw.FixedPoint.PathWidthBits, PathFracBits: raw.FixedPoint.PathFracBits,
			WeightWidthBits: raw.FixedPoint.WeightWidthBits, WeightFracBits: raw.FixedPoint.WeightFracBits,
			Tolerance: raw.FixedPoint.Tolerance,
		},
		RetryTickMS:     retryTickMS,
		BackoffPermille: raw.Retry.ScaleBasisPoints / 10,
	}
	for _, l := range raw.Listeners {
		timeoutMS, err := scaleCeil(l.Timeout.Ticks, tick, 1, "listener_timeout")
		if err != nil {
			return nil, err
		}
		inflight, err := scaleCeil(l.Budget.RequestsPerSecond, timeoutMS, 1000, "listener_inflight")
		if err != nil {
			return nil, err
		}
		out := domain.Listener{
			ID: l.ID, Prefix: l.Prefix, TimeoutMS: timeoutMS,
			InflightLimit: int(inflight), Routes: []domain.Route{},
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
		if u.WeightBasisPoints <= 0 {
			return nil, bizerr.New(bizerr.WeightRange, "v2 weightBasisPoints must be positive").
				With("schema", SchemaV2).With("upstream", u.ID).WithInt64("weightBasisPoints", u.WeightBasisPoints)
		}
		up := domain.Upstream{
			ID: u.ID, Address: u.URL, WeightPPM: u.WeightBasisPoints * 100,
			Healthy: u.Healthy, MaxRetries: u.MaxRetries,
		}
		if u.HealthProbe != nil {
			interval, err := scaleCeil(u.HealthProbe.IntervalTicks, tick, 1, "probe_interval")
			if err != nil {
				return nil, err
			}
			timeout, err := scaleCeil(u.HealthProbe.TimeoutTicks, tick, 1, "probe_timeout")
			if err != nil {
				return nil, err
			}
			up.Probe = &domain.HealthProbe{
				Path: u.HealthProbe.Path, IntervalMS: interval, TimeoutMS: timeout,
			}
		}
		p.Upstreams = append(p.Upstreams, up)
	}
	for _, r := range raw.Requests {
		arrival, err := scaleCeil(r.ArrivalTicks, tick, 1, "arrival")
		if err != nil {
			return nil, err
		}
		p.Requests = append(p.Requests, domain.Request{
			Seq: r.Sequence, ArrivalMS: arrival, Method: r.Method, RawPath: r.RawPath,
			Headers:          convertHeaders(r.Headers),
			FailurePlan:      append([]string(nil), r.FailurePlan...),
			AttemptLatencyMS: append([]int64(nil), r.LatenciesMs...),
		})
	}
	return p, nil
}
