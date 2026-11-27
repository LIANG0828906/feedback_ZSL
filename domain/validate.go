package domain

import (
	"fmt"
	"sort"
	"strings"
	"unicode"

	"proxydiag/bizerr"
	"proxydiag/fixed"
)

var allowedFailures = map[string]bool{
	"timeout":  true,
	"connect":  true,
	"http_503": true,
}

// Validate enforces the canonical domain contract shared by all schemas.
func (p *Project) Validate() error {
	if p == nil {
		return bizerr.New(bizerr.SchemaRequired, "project is nil")
	}
	if strings.TrimSpace(p.Version) == "" {
		return bizerr.New(bizerr.SchemaRequired, "project version is required")
	}
	if p.TickMS <= 0 {
		return bizerr.New(bizerr.TimeoutOrder, "clock tick must be positive").
			WithInt64("tick_ms", p.TickMS)
	}
	if p.RetryTickMS <= 0 {
		return bizerr.New(bizerr.TimeoutOrder, "retry tick must be positive").
			WithInt64("retry_tick_ms", p.RetryTickMS)
	}
	if p.BackoffPermille < 1000 || p.BackoffPermille > 10000 {
		return bizerr.New(bizerr.WeightRange, "backoff scale outside accepted per-mille range").
			WithInt64("backoff_permille", p.BackoffPermille)
	}
	if err := validateFixed(p.Fixed); err != nil {
		return err
	}
	if len(p.Listeners) == 0 {
		return bizerr.New(bizerr.SchemaRequired, "at least one listener is required")
	}
	if len(p.Upstreams) == 0 {
		return bizerr.New(bizerr.SchemaRequired, "at least one upstream is required")
	}

	upstreamIDs := map[string]bool{}
	for i, u := range p.Upstreams {
		if strings.TrimSpace(u.ID) == "" {
			return bizerr.New(bizerr.SchemaRequired, "upstream id is required").WithInt("index", i)
		}
		if upstreamIDs[u.ID] {
			return bizerr.New(bizerr.SchemaDuplicateID, "duplicate upstream id").With("upstream", u.ID)
		}
		upstreamIDs[u.ID] = true
		if strings.TrimSpace(u.Address) == "" {
			return bizerr.New(bizerr.SchemaRequired, "upstream address is required").With("upstream", u.ID)
		}
		if u.WeightPPM <= 0 || u.WeightPPM > 1_000_000 {
			return bizerr.New(bizerr.WeightRange, "upstream weight must be in 1..1000000 ppm").
				With("upstream", u.ID).WithInt64("weight_ppm", u.WeightPPM)
		}
		if fixed.MaxForWidth(p.Fixed.WeightWidthBits) < u.WeightPPM {
			return bizerr.New(bizerr.WeightRange, "upstream weight exceeds declared fixed width").
				With("upstream", u.ID).WithInt64("weight_ppm", u.WeightPPM)
		}
		if u.MaxRetries < 0 || u.MaxRetries > 10 {
			return bizerr.New(bizerr.InvalidRequest, "upstream retry limit outside 0..10").
				With("upstream", u.ID).WithInt("max_retries", u.MaxRetries)
		}
		if u.Probe != nil {
			if !strings.HasPrefix(u.Probe.Path, "/") {
				return bizerr.New(bizerr.SchemaRequired, "health probe path must start with /").
					With("upstream", u.ID)
			}
			if u.Probe.IntervalMS <= 0 || u.Probe.TimeoutMS <= 0 {
				return bizerr.New(bizerr.TimeoutOrder, "health probe intervals must be positive").
					With("upstream", u.ID)
			}
			if u.Probe.TimeoutMS > u.Probe.IntervalMS {
				return bizerr.New(bizerr.TimeoutOrder, "health probe timeout exceeds interval").
					With("upstream", u.ID)
			}
		}
	}

	listenerIDs := map[string]bool{}
	for i, l := range p.Listeners {
		if strings.TrimSpace(l.ID) == "" {
			return bizerr.New(bizerr.SchemaRequired, "listener id is required").WithInt("index", i)
		}
		if listenerIDs[l.ID] {
			return bizerr.New(bizerr.SchemaDuplicateID, "duplicate listener id").With("listener", l.ID)
		}
		listenerIDs[l.ID] = true
		if !strings.HasPrefix(l.Prefix, "/") {
			return bizerr.New(bizerr.SchemaRequired, "listener prefix must start with /").
				With("listener", l.ID).With("prefix", l.Prefix)
		}
		if l.TimeoutMS <= 0 {
			return bizerr.New(bizerr.TimeoutOrder, "listener timeout must be positive").
				With("listener", l.ID).WithInt64("timeout_ms", l.TimeoutMS)
		}
		if p.RetryTickMS >= l.TimeoutMS {
			return bizerr.New(bizerr.TimeoutOrder, "retry tick must be smaller than attempt timeout").
				With("listener", l.ID).WithInt64("retry_tick_ms", p.RetryTickMS).
				WithInt64("timeout_ms", l.TimeoutMS)
		}
		if l.InflightLimit <= 0 || l.InflightLimit > 4096 {
			return bizerr.New(bizerr.InflightRange, "listener in-flight budget outside 1..4096").
				With("listener", l.ID).WithInt("inflight_limit", l.InflightLimit)
		}
		if len(l.Routes) == 0 {
			return bizerr.New(bizerr.SchemaRequired, "listener must declare at least one route").
				With("listener", l.ID)
		}
		routeIDs := map[string]bool{}
		for ri, r := range l.Routes {
			if r.ID == "" {
				return bizerr.New(bizerr.SchemaRequired, "route id is required").
					With("listener", l.ID).WithInt("route_index", ri)
			}
			if routeIDs[r.ID] {
				return bizerr.New(bizerr.SchemaDuplicateID, "duplicate route id").With("route", r.ID)
			}
			routeIDs[r.ID] = true
			if r.ListenerID != "" && r.ListenerID != l.ID {
				return bizerr.New(bizerr.ListenerMissing, "route listener reference does not match grouping").
					With("route", r.ID).With("listener", l.ID).With("route_listener", r.ListenerID)
			}
			if !strings.HasPrefix(r.Prefix, "/") {
				return bizerr.New(bizerr.SchemaRequired, "route prefix must start with /").
					With("route", r.ID).With("prefix", r.Prefix)
			}
			if len(r.UpstreamIDs) == 0 {
				return bizerr.New(bizerr.UpstreamMissing, "route must reference at least one upstream").
					With("route", r.ID)
			}
			seen := map[string]bool{}
			for _, uid := range r.UpstreamIDs {
				if !upstreamIDs[uid] {
					return bizerr.New(bizerr.UpstreamMissing, "route references an unknown upstream").
						With("route", r.ID).With("upstream", uid)
				}
				if seen[uid] {
					return bizerr.New(bizerr.SchemaDuplicateID, "duplicate upstream reference in route").
						With("route", r.ID).With("upstream", uid)
				}
				seen[uid] = true
			}
			if r.Rewrite.StripPrefix != "" && !strings.HasPrefix(r.Rewrite.StripPrefix, "/") {
				return bizerr.New(bizerr.RewriteStrip, "rewrite strip prefix must start with /").
					With("route", r.ID)
			}
			if r.Rewrite.AddPrefix != "" && !strings.HasPrefix(r.Rewrite.AddPrefix, "/") {
				return bizerr.New(bizerr.RewriteEmpty, "rewrite add prefix must start with /").
					With("route", r.ID)
			}
			for _, h := range r.Rewrite.SetHeaders {
				if !validToken(h.Name) || strings.ContainsAny(h.Value, "\r\n") {
					return bizerr.New(bizerr.InvalidRequest, "invalid rewrite header").
						With("route", r.ID).With("header", h.Name)
				}
			}
			for _, name := range r.Rewrite.RemoveHeaders {
				if !validToken(name) {
					return bizerr.New(bizerr.InvalidRequest, "invalid removed header name").
						With("route", r.ID).With("header", name)
				}
			}
		}
	}

	// Overlapping listener prefixes are an intentionally rejected injection.
	for i := 0; i < len(p.Listeners); i++ {
		for j := i + 1; j < len(p.Listeners); j++ {
			a, b := p.Listeners[i], p.Listeners[j]
			if strings.HasPrefix(a.Prefix, b.Prefix) || strings.HasPrefix(b.Prefix, a.Prefix) {
				return bizerr.New(bizerr.ListenerOverlap, "overlapping listener prefixes are ambiguous").
					With("listener_a", a.ID).With("listener_b", b.ID).
					With("prefix_a", a.Prefix).With("prefix_b", b.Prefix)
			}
		}
	}

	seqs := make([]int, 0, len(p.Requests))
	seenSeq := map[int]bool{}
	var lastArrival int64
	for i, r := range p.Requests {
		if r.Seq <= 0 {
			return bizerr.New(bizerr.InvalidRequest, "request sequence must be positive").
				WithInt("index", i)
		}
		if seenSeq[r.Seq] {
			return bizerr.New(bizerr.SchemaDuplicateID, "duplicate request sequence").WithInt("seq", r.Seq)
		}
		seenSeq[r.Seq] = true
		seqs = append(seqs, r.Seq)
		if r.ArrivalMS < 0 || r.ArrivalMS < lastArrival {
			return bizerr.New(bizerr.TimeoutOrder, "request arrival time is not monotonic").
				WithInt("seq", r.Seq).WithInt64("arrival_ms", r.ArrivalMS)
		}
		lastArrival = r.ArrivalMS
		if !validToken(r.Method) {
			return bizerr.New(bizerr.InvalidRequest, "invalid request method").
				WithInt("seq", r.Seq)
		}
		if !strings.HasPrefix(r.RawPath, "/") {
			return bizerr.New(bizerr.InvalidRequest, "raw path must start with /").
				WithInt("seq", r.Seq).With("raw_path", r.RawPath)
		}
		for _, h := range r.Headers {
			if !validToken(h.Name) || strings.ContainsAny(h.Value, "\r\n") {
				return bizerr.New(bizerr.InvalidRequest, "invalid request header").
					WithInt("seq", r.Seq).With("header", h.Name)
			}
		}
		for _, f := range r.FailurePlan {
			if !allowedFailures[f] {
				return bizerr.New(bizerr.InvalidRequest, "unknown failure plan reason").
					WithInt("seq", r.Seq).With("reason", f)
			}
		}
		for _, latency := range r.AttemptLatencyMS {
			if latency < 0 {
				return bizerr.New(bizerr.TimeoutOrder, "negative attempt latency inverts timeout window").
					WithInt("seq", r.Seq).WithInt64("latency_ms", latency)
			}
		}
	}
	if !sort.IntsAreSorted(seqs) {
		return bizerr.New(bizerr.InvalidRequest, "request sequence must be declared in ascending order")
	}
	return nil
}

func validateFixed(f FixedConfig) error {
	if f.PathWidthBits < 8 || f.PathWidthBits > 62 {
		return bizerr.New(bizerr.SchemaRequired, "path fixed width outside 8..62").
			WithInt("path_width_bits", f.PathWidthBits)
	}
	if f.WeightWidthBits < 16 || f.WeightWidthBits > 62 {
		return bizerr.New(bizerr.SchemaRequired, "weight fixed width outside 16..62").
			WithInt("weight_width_bits", f.WeightWidthBits)
	}
	if f.PathFracBits == 0 || int(f.PathFracBits) >= f.PathWidthBits-1 {
		return bizerr.New(bizerr.SchemaRequired, "path fraction bits leave no integer bits").
			WithInt("path_fraction_bits", int(f.PathFracBits))
	}
	if f.WeightFracBits == 0 || int(f.WeightFracBits) >= f.WeightWidthBits-1 {
		return bizerr.New(bizerr.SchemaRequired, "weight fraction bits leave no integer bits").
			WithInt("weight_fraction_bits", int(f.WeightFracBits))
	}
	if f.Tolerance < 0 {
		return bizerr.New(bizerr.SchemaRequired, "fixed tolerance must be non-negative").
			WithInt64("tolerance", f.Tolerance)
	}
	return nil
}

func validToken(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r > unicode.MaxASCII || r <= 32 || r >= 127 || strings.ContainsRune("()<>@,;:\\\"/[]?={}", r) {
			return false
		}
	}
	return true
}

func DebugProject(p *Project) string {
	if p == nil {
		return "<nil>"
	}
	return fmt.Sprintf("%s/%s listeners=%d upstreams=%d requests=%d",
		p.SourceSchema, p.Version, len(p.Listeners), len(p.Upstreams), len(p.Requests))
}
