package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
)

type Header struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

type FixedConfig struct {
	PathWidthBits   int   `json:"path_width_bits"`
	PathFracBits    uint  `json:"path_fraction_bits"`
	WeightWidthBits int   `json:"weight_width_bits"`
	WeightFracBits  uint  `json:"weight_fraction_bits"`
	Tolerance       int64 `json:"tolerance"`
}

type HealthProbe struct {
	Path       string `json:"path"`
	IntervalMS int64  `json:"interval_ms"`
	TimeoutMS  int64  `json:"timeout_ms"`
}

type Upstream struct {
	ID         string       `json:"id"`
	Address    string       `json:"address"`
	WeightPPM  int64        `json:"weight_ppm"`
	Healthy    bool         `json:"healthy"`
	MaxRetries int          `json:"max_retries"`
	Probe      *HealthProbe `json:"probe,omitempty"`
}

type Rewrite struct {
	StripPrefix   string   `json:"strip_prefix"`
	AddPrefix     string   `json:"add_prefix"`
	RemoveHeaders []string `json:"remove_headers,omitempty"`
	SetHeaders    []Header `json:"set_headers,omitempty"`
}

type Route struct {
	ID          string   `json:"id"`
	ListenerID  string   `json:"listener_id"`
	Prefix      string   `json:"prefix"`
	Rewrite     Rewrite  `json:"rewrite"`
	UpstreamIDs []string `json:"upstream_ids"`
}

type Listener struct {
	ID            string  `json:"id"`
	Prefix        string  `json:"prefix"`
	TimeoutMS     int64   `json:"timeout_ms"`
	InflightLimit int     `json:"inflight_limit"`
	Routes        []Route `json:"routes"`
}

type Request struct {
	Seq              int      `json:"seq"`
	ArrivalMS        int64    `json:"arrival_ms"`
	Method           string   `json:"method"`
	RawPath          string   `json:"raw_path"`
	Headers          []Header `json:"headers"`
	FailurePlan      []string `json:"failure_plan,omitempty"`
	AttemptLatencyMS []int64  `json:"attempt_latency_ms,omitempty"`
}

type Project struct {
	SourceSchema    string      `json:"source_schema"`
	Version         string      `json:"version"`
	TickMS          int64       `json:"tick_ms"`
	Fixed           FixedConfig `json:"fixed"`
	RetryTickMS     int64       `json:"retry_tick_ms"`
	BackoffPermille int64       `json:"backoff_permille"`
	Listeners       []Listener  `json:"listeners"`
	Upstreams       []Upstream  `json:"upstreams"`
	Requests        []Request   `json:"requests"`
}

// TruthHop is emitted by the deterministic sample generator. It is the only
// expected-value source used by the precision verifier.
type TruthHop struct {
	Name  string `json:"name"`
	Value int64  `json:"value"`
}

type TruthAttempt struct {
	Index          int    `json:"index"`
	UpstreamID     string `json:"upstream_id"`
	StartMS        int64  `json:"start_ms"`
	EndMS          int64  `json:"end_ms"`
	Outcome        string `json:"outcome"`
	TriggerReason  string `json:"trigger_reason"`
	RetryScheduled bool   `json:"retry_scheduled"`
}

type TruthRequest struct {
	Seq                  int            `json:"seq"`
	ListenerID           string         `json:"listener_id"`
	RouteID              string         `json:"route_id"`
	DecisionBasis        []string       `json:"decision_basis"`
	RewrittenRequestLine string         `json:"rewritten_request_line"`
	Headers              []Header       `json:"headers"`
	SelectedUpstreamID   string         `json:"selected_upstream_id"`
	Attempts             []TruthAttempt `json:"attempts"`
	Terminal             string         `json:"terminal"`
	TerminalReason       string         `json:"terminal_reason,omitempty"`
	Hops                 []TruthHop     `json:"hops"`
}

type Truth struct {
	Version  string         `json:"version"`
	Seed     int64          `json:"seed"`
	Requests []TruthRequest `json:"requests"`
}

func (p *Project) Clone() *Project {
	if p == nil {
		return nil
	}
	b, _ := json.Marshal(p)
	var out Project
	if err := json.Unmarshal(b, &out); err != nil {
		panic(fmt.Sprintf("domain.Project clone: %v", err))
	}
	return &out
}

func (p *Project) RouteByID(id string) (*Route, *Listener, bool) {
	for li := range p.Listeners {
		for ri := range p.Listeners[li].Routes {
			if p.Listeners[li].Routes[ri].ID == id {
				return &p.Listeners[li].Routes[ri], &p.Listeners[li], true
			}
		}
	}
	return nil, nil, false
}

func (p *Project) UpstreamByID(id string) (*Upstream, bool) {
	for i := range p.Upstreams {
		if p.Upstreams[i].ID == id {
			return &p.Upstreams[i], true
		}
	}
	return nil, false
}

func (p *Project) RequestBySeq(seq int) (*Request, bool) {
	for i := range p.Requests {
		if p.Requests[i].Seq == seq {
			return &p.Requests[i], true
		}
	}
	return nil, false
}

// CoreFingerprint covers semantics that must be identical for the same
// project loaded through every schema. Optional health-probe diagnostics are
// deliberately excluded; their presence is schema-specific by design.
func (p *Project) CoreFingerprint() string {
	type coreUpstream struct {
		ID         string `json:"id"`
		Address    string `json:"address"`
		WeightPPM  int64  `json:"weight_ppm"`
		Healthy    bool   `json:"healthy"`
		MaxRetries int    `json:"max_retries"`
	}
	type core struct {
		Version         string         `json:"version"`
		TickMS          int64          `json:"tick_ms"`
		Fixed           FixedConfig    `json:"fixed"`
		RetryTickMS     int64          `json:"retry_tick_ms"`
		BackoffPermille int64          `json:"backoff_permille"`
		Listeners       []Listener     `json:"listeners"`
		Upstreams       []coreUpstream `json:"upstreams"`
		Requests        []Request      `json:"requests"`
	}
	ups := make([]coreUpstream, 0, len(p.Upstreams))
	for _, u := range p.Upstreams {
		ups = append(ups, coreUpstream{
			ID: u.ID, Address: u.Address, WeightPPM: u.WeightPPM,
			Healthy: u.Healthy, MaxRetries: u.MaxRetries,
		})
	}
	c := core{
		Version: p.Version, TickMS: p.TickMS, Fixed: p.Fixed,
		RetryTickMS: p.RetryTickMS, BackoffPermille: p.BackoffPermille,
		Listeners: p.Listeners, Upstreams: ups, Requests: p.Requests,
	}
	b, _ := json.Marshal(c)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func HeaderValue(headers []Header, name string) (string, bool) {
	for _, h := range headers {
		if strings.EqualFold(h.Name, name) {
			return h.Value, true
		}
	}
	return "", false
}

func CloneHeaders(in []Header) []Header {
	if in == nil {
		return nil
	}
	out := make([]Header, len(in))
	copy(out, in)
	return out
}
