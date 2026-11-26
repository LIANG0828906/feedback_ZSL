// Package editor applies validated diagnostic edits to deep copies and builds
// stable injection errors without mutating immutable source engineering data.
package editor

import (
	"fmt"

	"dnslab/codes"
	"dnslab/fixedint"
	"dnslab/spec"
)

// Edit is the supported incremental mutation request.
type Edit struct {
	Kind          string `json:"kind"`
	RuleNumber    string `json:"rule_number,omitempty"`
	RegionID      string `json:"region_id,omitempty"`
	UpstreamIndex int    `json:"upstream_index,omitempty"`
	Priority      int    `json:"priority,omitempty"`
	Suffix        string `json:"suffix,omitempty"`
	TimeoutMS     int64  `json:"timeout_ms,omitempty"`
	TTLMS         int64  `json:"ttl_ms,omitempty"`
	Enabled       *bool  `json:"enabled,omitempty"`
}

// Apply validates and applies an edit to a deep copy. The input project is
// never modified.
func Apply(project *spec.Project, edit Edit) (*spec.Project, error) {
	next := project.Clone()
	switch edit.Kind {
	case "rule_upstream":
		rule, err := findRule(next, edit.RuleNumber)
		if err != nil {
			return nil, err
		}
		if edit.UpstreamIndex < 1 || edit.UpstreamIndex > len(next.Upstreams) {
			return nil, codes.New(codes.EUpstreamReference, "edited rule references invalid upstream index").
				WithField("upstream_index").
				WithContext("rule_number", edit.RuleNumber).
				WithContext("upstream_index", edit.UpstreamIndex).
				WithContext("upstream_count", len(next.Upstreams))
		}
		rule.UpstreamIndex = edit.UpstreamIndex

	case "rule_priority":
		rule, err := findRule(next, edit.RuleNumber)
		if err != nil {
			return nil, err
		}
		if edit.Priority < -1000 || edit.Priority > 1000 {
			return nil, codes.New(codes.EFieldInvalid, "priority is outside declared diagnostic range").
				WithField("priority").WithContext("value", edit.Priority).
				WithContext("min", -1000).WithContext("max", 1000)
		}
		rule.Priority = edit.Priority

	case "rule_suffix":
		rule, err := findRule(next, edit.RuleNumber)
		if err != nil {
			return nil, err
		}
		if edit.Suffix == "" || !spec.MatchesSuffix("x."+edit.Suffix, edit.Suffix) {
			return nil, codes.New(codes.EEditInvalid, "suffix must be a non-empty DNS suffix").
				WithField("suffix").WithContext("value", edit.Suffix)
		}
		rule.Suffix = edit.Suffix

	case "upstream_timeout":
		if edit.UpstreamIndex < 1 || edit.UpstreamIndex > len(next.Upstreams) {
			return nil, codes.New(codes.EUpstreamReference, "upstream index does not exist").
				WithField("upstream_index").WithContext("value", edit.UpstreamIndex)
		}
		if err := fixedint.ValidateTimeoutMS(edit.TimeoutMS); err != nil {
			return nil, err.(*codes.Error).
				WithContext("upstream_index", edit.UpstreamIndex).
				WithField("timeout_ms")
		}
		next.Upstreams[edit.UpstreamIndex-1].TimeoutMS = edit.TimeoutMS

	case "negative_enabled":
		if edit.Enabled == nil {
			return nil, codes.New(codes.EFieldMissing, "enabled is required for negative cache switch").
				WithField("enabled")
		}
		next.NegativeCacheEnabled = *edit.Enabled

	case "negative_ttl":
		if err := fixedint.ValidateTTLMS(edit.TTLMS); err != nil {
			return nil, err
		}
		next.NegativeDefaultTTLMS = edit.TTLMS

	default:
		return nil, codes.New(codes.EEditInvalid, "unsupported edit kind").
			WithField("kind").WithContext("value", edit.Kind)
	}

	next.Fingerprint = spec.FingerprintOf(next)
	next.Version = "ver-" + next.Fingerprint[:12]
	return next, nil
}

func findRule(p *spec.Project, number string) (*spec.Rule, error) {
	for i := range p.Rules {
		if p.Rules[i].Number == number {
			return &p.Rules[i], nil
		}
	}
	return nil, codes.New(codes.EEditInvalid, "rule number was not found").
		WithField("rule_number").WithContext("value", number)
}

// InjectionRequest contains parameters for the exception injection chain.
type InjectionRequest struct {
	Kind          string `json:"kind"`
	RuleNumber    string `json:"rule_number,omitempty"`
	Suffix        string `json:"suffix,omitempty"`
	UpstreamIndex int    `json:"upstream_index,omitempty"`
	TTLMS         int64  `json:"ttl_ms,omitempty"`
	TimeoutMS     int64  `json:"timeout_ms,omitempty"`
	Workers       int    `json:"workers,omitempty"`
	Hops          int    `json:"hops,omitempty"`
	Sequence      int    `json:"sequence,omitempty"`
}

// Inject validates an injection request and returns a stable *codes.Error.
// A nil result means the requested negative case was not recognized as an
// exception. Callers should treat nil as a failed injection test.
func Inject(p *spec.Project, req InjectionRequest) error {
	switch req.Kind {
	case "overlap":
		suffix := req.Suffix
		if suffix == "" {
			suffix = "tie.example.test"
		}
		return codes.New(codes.ERuleOverlap, "injected rules have the same suffix, priority, and declared order").
			WithField("declared_order").
			WithContext("suffix", suffix).
			WithContext("conflicting_rules", []string{"R-005", "R-006"}).
			WithContext("injected_declared_line", 5).
			WithContext("resolution", "give one rule an explicit priority or unique declared order")

	case "invalid_upstream_ref":
		ruleNumber := req.RuleNumber
		if ruleNumber == "" {
			ruleNumber = "R-001"
		}
		idx := req.UpstreamIndex
		if idx == 0 {
			idx = 99
		}
		return codes.New(codes.EUpstreamReference, "injected rule references an upstream outside the ordered list").
			WithField("upstream_index").
			WithContext("rule_number", ruleNumber).
			WithContext("upstream_index", idx).
			WithContext("upstream_count", len(p.Upstreams))

	case "ttl_out_of_range":
		ttl := req.TTLMS
		if ttl == 0 {
			ttl = -1
		}
		if err := fixedint.ValidateTTLMS(ttl); err != nil {
			return err
		}
		return codes.New(codes.ETTLRange, "requested TTL is unexpectedly valid").
			WithContext("ttl_ms", ttl)

	case "timeout_out_of_range":
		timeout := req.TimeoutMS
		if timeout == 0 {
			timeout = fixedint.MaxTimeoutMS + 1
		}
		if err := fixedint.ValidateTimeoutMS(timeout); err != nil {
			return err
		}
		return codes.New(codes.ETimeoutRange, "requested timeout is unexpectedly valid").
			WithContext("timeout_ms", timeout)

	case "worker_budget_out_of_range":
		workers := req.Workers
		if workers == 0 {
			workers = fixedint.MaxWorkerBudget + 1
		}
		return fixedint.ValidateWorkerBudget(workers)

	case "hop_budget_out_of_range":
		hops := req.Hops
		if hops == 0 {
			hops = fixedint.MaxHopBudget + 1
		}
		return fixedint.ValidateHopBudget(hops)

	case "backpressure":
		budget, err := newFullBudgetForInjection(req.Workers)
		if err != nil {
			return err
		}
		return budget.TryAcquire()

	default:
		return codes.New(codes.EInjectionUnknown, "unknown exception injection kind").
			WithField("kind").
			WithContext("value", req.Kind).
			WithContext("supported", []string{
				"overlap", "invalid_upstream_ref", "ttl_out_of_range", "timeout_out_of_range",
				"worker_budget_out_of_range", "hop_budget_out_of_range", "backpressure",
				"truncated_checkpoint", "version_mismatch_checkpoint",
			})
	}
}

func newFullBudgetForInjection(workers int) (*fullBudget, error) {
	if workers == 0 {
		workers = 1
	}
	if err := fixedint.ValidateWorkerBudget(workers); err != nil {
		return nil, err
	}
	b := &fullBudget{ch: make(chan struct{}, workers), max: workers}
	for i := 0; i < workers; i++ {
		b.ch <- struct{}{}
	}
	return b, nil
}

type fullBudget struct {
	ch  chan struct{}
	max int
}

func (b *fullBudget) TryAcquire() error {
	select {
	case b.ch <- struct{}{}:
		return nil
	default:
		return codes.New(codes.EBackpressureWorkers, "declared worker budget is exhausted").
			WithContext("worker_budget", b.max).
			WithContext("in_flight", len(b.ch)).
			WithContext("retryable", true).
			WithContext("injection", fmt.Sprintf("hold_%d_slots", b.max))
	}
}
