package engine

import (
	"encoding/json"
	"fmt"
	"hash/fnv"
	"math"
	"sort"
	"strings"

	"proxydiag/bizerr"
	"proxydiag/domain"
	"proxydiag/fixed"
)

const (
	DecisionListenerLongest = "D01"
	DecisionListenerTie     = "D02"
	DecisionRouteLongest    = "D03"
	DecisionRouteTie        = "D04"

	TerminalSuccess           = "T_SUCCESS"
	TerminalRetryExhausted    = "T_RETRY_EXHAUSTED"
	TerminalNoHealthyUpstream = "T_NO_HEALTHY_UPSTREAM"
	TerminalNoListener        = "T_NO_LISTENER"
	TerminalNoRoute           = "T_NO_ROUTE"
	TerminalRejected          = "T_REJECTED"
	TerminalBackpressure      = "T_BACKPRESSURE"

	ReasonTimeout   = "R_TIMEOUT"
	ReasonConnect   = "R_CONNECT"
	ReasonHTTP503   = "R_HTTP_503"
	ReasonNoHealthy = "R_NO_HEALTHY_UPSTREAM"
)

type TraceHop struct {
	Seq             int    `json:"seq"`
	Name            string `json:"name"`
	Formula         string `json:"formula"`
	WidthBits       int    `json:"width_bits"`
	FracBits        uint   `json:"frac_bits"`
	Value           int64  `json:"value"`
	Truth           int64  `json:"truth"`
	Delta           int64  `json:"delta"`
	Tolerance       int64  `json:"tolerance"`
	WithinTolerance bool   `json:"within_tolerance"`
	Compared        bool   `json:"compared"`
	Saturated       bool   `json:"saturated,omitempty"`
}

type Attempt struct {
	Index          int    `json:"index"`
	UpstreamID     string `json:"upstream_id"`
	StartMS        int64  `json:"start_ms"`
	EndMS          int64  `json:"end_ms"`
	Outcome        string `json:"outcome"`
	TriggerReason  string `json:"trigger_reason,omitempty"`
	RetryScheduled bool   `json:"retry_scheduled"`
}

type CacheInfo struct {
	Reused             bool     `json:"reused"`
	InvalidationSource []string `json:"invalidation_sources,omitempty"`
}

type Result struct {
	Seq                  int               `json:"seq"`
	ArrivalMS            int64             `json:"arrival_ms"`
	Method               string            `json:"method"`
	RawPath              string            `json:"raw_path"`
	ListenerID           string            `json:"listener_id,omitempty"`
	RouteID              string            `json:"route_id,omitempty"`
	DecisionBasis        []string          `json:"decision_basis,omitempty"`
	RewrittenRequestLine string            `json:"rewritten_request_line,omitempty"`
	Headers              []domain.Header   `json:"headers"`
	SelectedUpstreamID   string            `json:"selected_upstream_id,omitempty"`
	Attempts             []Attempt         `json:"attempts,omitempty"`
	Terminal             string            `json:"terminal"`
	TerminalReason       string            `json:"terminal_reason,omitempty"`
	Trace                []TraceHop        `json:"trace,omitempty"`
	MaxAbsDelta          int64             `json:"max_abs_delta"`
	WithinTolerance      bool              `json:"within_tolerance"`
	TruthMatched         bool              `json:"truth_matched"`
	Mismatches           []string          `json:"mismatches,omitempty"`
	ErrorCode            string            `json:"error_code,omitempty"`
	ErrorMessage         string            `json:"error_message,omitempty"`
	ErrorContext         map[string]string `json:"error_context,omitempty"`
	Cache                CacheInfo         `json:"cache"`
}

func (r Result) Err() error {
	if r.ErrorCode == "" {
		return nil
	}
	e := bizerr.New(r.ErrorCode, r.ErrorMessage)
	for k, v := range r.ErrorContext {
		e = e.With(k, v)
	}
	return e
}

// EvaluateRequest is pure: it does not mutate the project or request.
func EvaluateRequest(p *domain.Project, r domain.Request, truth *domain.TruthRequest) Result {
	res := Result{
		Seq: r.Seq, ArrivalMS: r.ArrivalMS, Method: r.Method, RawPath: r.RawPath,
		Headers: []domain.Header{}, Attempts: []Attempt{}, Trace: []TraceHop{},
		Terminal: TerminalRejected, WithinTolerance: true, TruthMatched: truth == nil,
	}
	pathPart, queryPart := splitPath(r.RawPath)
	listener, listenerBasis, ok := selectListener(p, pathPart)
	if !ok {
		res.Terminal = TerminalNoListener
		res.TerminalReason = "no listener prefix matched"
		return res
	}
	res.ListenerID = listener.ID
	res.DecisionBasis = append(res.DecisionBasis, listenerBasis...)
	route, routeBasis, ok := selectRoute(listener, pathPart)
	if !ok {
		res.Terminal = TerminalNoRoute
		res.TerminalReason = "no route prefix matched within listener"
		return res
	}
	res.RouteID = route.ID
	res.DecisionBasis = append(res.DecisionBasis, routeBasis...)

	rewrittenPath, err := rewritePath(pathPart, route.Rewrite)
	if err != nil {
		return rejectWithError(res, err)
	}
	if rewrittenPath == "" || !strings.HasPrefix(rewrittenPath, "/") ||
		strings.ContainsAny(rewrittenPath, " \t\r\n") {
		return rejectWithError(res, bizerr.New(bizerr.RewriteEmpty, "rewrite produced an empty or invalid request line").
			With("route", route.ID).With("raw_path", pathPart).With("rewritten_path", rewrittenPath))
	}
	finalPath := rewrittenPath + queryPart
	res.RewrittenRequestLine = fmt.Sprintf("%s %s HTTP/1.1", r.Method, finalPath)
	res.Headers = rewriteHeaders(r.Headers, route.Rewrite)
	if err := appendPathHop(p, &res, pathPart, rewrittenPath); err != nil {
		return rejectWithError(res, err)
	}

	upstream, candidates, totalWeight, err := selectUpstream(p, r, route, res.RewrittenRequestLine)
	if err != nil {
		return rejectWithError(res, err)
	}
	if upstream == nil {
		res.Terminal = TerminalNoHealthyUpstream
		res.TerminalReason = ReasonNoHealthy
		return res
	}
	res.SelectedUpstreamID = upstream.ID
	if err := appendWeightHops(p, &res, candidates, totalWeight, r, res.RewrittenRequestLine, upstream); err != nil {
		return rejectWithError(res, err)
	}

	if err := runAttempts(p, listener, upstream, r, &res); err != nil {
		return rejectWithError(res, err)
	}
	if truth != nil {
		compareTruth(p, &res, truth)
	}
	return res
}

func rejectWithError(res Result, err error) Result {
	res.Terminal = TerminalRejected
	if e, ok := bizerr.As(err); ok {
		res.ErrorCode = e.Code
		res.ErrorMessage = e.Message
		res.ErrorContext = e.Context
	} else {
		res.ErrorCode = bizerr.Internal
		res.ErrorMessage = err.Error()
		res.ErrorContext = map[string]string{}
	}
	res.TerminalReason = res.ErrorCode
	return res
}

func splitPath(raw string) (string, string) {
	if i := strings.IndexByte(raw, '?'); i >= 0 {
		return raw[:i], raw[i:]
	}
	return raw, ""
}

func selectListener(p *domain.Project, path string) (*domain.Listener, []string, bool) {
	bestIdx := -1
	bestLen := -1
	tie := false
	for i := range p.Listeners {
		if !strings.HasPrefix(path, p.Listeners[i].Prefix) {
			continue
		}
		n := len(p.Listeners[i].Prefix)
		switch {
		case n > bestLen:
			bestIdx, bestLen, tie = i, n, false
		case n == bestLen:
			tie = true
		}
	}
	if bestIdx < 0 {
		return nil, nil, false
	}
	basis := []string{DecisionListenerLongest}
	if tie {
		basis = append(basis, DecisionListenerTie)
	}
	return &p.Listeners[bestIdx], basis, true
}

func effectiveRoutePrefix(listener domain.Listener, route domain.Route) string {
	if route.Prefix == "/" || route.Prefix == "" {
		return listener.Prefix
	}
	return strings.TrimSuffix(listener.Prefix, "/") + "/" + strings.TrimPrefix(route.Prefix, "/")
}

func selectRoute(listener *domain.Listener, path string) (*domain.Route, []string, bool) {
	bestIdx := -1
	bestLen := -1
	tie := false
	for i := range listener.Routes {
		effective := effectiveRoutePrefix(*listener, listener.Routes[i])
		if !strings.HasPrefix(path, effective) {
			continue
		}
		n := len(effective)
		switch {
		case n > bestLen:
			bestIdx, bestLen, tie = i, n, false
		case n == bestLen:
			tie = true
		}
	}
	if bestIdx < 0 {
		return nil, nil, false
	}
	basis := []string{DecisionRouteLongest}
	if tie {
		basis = append(basis, DecisionRouteTie)
	}
	return &listener.Routes[bestIdx], basis, true
}

func rewritePath(path string, rw domain.Rewrite) (string, error) {
	out := path
	if rw.StripPrefix != "" {
		if !strings.HasPrefix(out, rw.StripPrefix) {
			return "", bizerr.New(bizerr.RewriteStrip, "rewrite strip prefix does not match request path").
				With("strip_prefix", rw.StripPrefix).With("path", path)
		}
		out = strings.TrimPrefix(out, rw.StripPrefix)
	}
	out = rw.AddPrefix + out
	return out, nil
}

func rewriteHeaders(in []domain.Header, rw domain.Rewrite) []domain.Header {
	removed := make(map[string]bool, len(rw.RemoveHeaders))
	for _, name := range rw.RemoveHeaders {
		removed[strings.ToLower(name)] = true
	}
	out := make([]domain.Header, 0, len(in)+len(rw.SetHeaders))
	for _, h := range in {
		if removed[strings.ToLower(h.Name)] {
			continue
		}
		out = append(out, h)
	}
	for _, h := range rw.SetHeaders {
		replaced := false
		for i := range out {
			if strings.EqualFold(out[i].Name, h.Name) {
				out[i] = h
				replaced = true
				break
			}
		}
		if !replaced {
			out = append(out, h)
		}
	}
	return out
}

func appendPathHop(p *domain.Project, res *Result, rawPath, rewritten string) error {
	rawLen := len(rawPath)
	if rawLen == 0 {
		rawLen = 1
	}
	r, err := fixed.MulDiv(int64(len(rewritten)), fixed.Unit(p.Fixed.PathFracBits),
		int64(rawLen), p.Fixed.PathWidthBits)
	if err != nil {
		return err
	}
	if r.Overflow {
		return bizerr.New(bizerr.FixedOverflow, "path recomposition exceeded declared width").
			With("route", res.RouteID).WithInt("raw_len", rawLen).
			WithInt("rewritten_len", len(rewritten)).WithInt("width_bits", p.Fixed.PathWidthBits)
	}
	appendHop(res, TraceHop{
		Name: "path.recombine", Formula: "rewritten_len * 2^frac / max(raw_len,1)",
		WidthBits: p.Fixed.PathWidthBits, FracBits: p.Fixed.PathFracBits,
		Value: r.Value, Tolerance: p.Fixed.Tolerance, WithinTolerance: true,
	})
	return nil
}

func selectUpstream(p *domain.Project, req domain.Request, route *domain.Route, requestLine string) (*domain.Upstream, []*domain.Upstream, int64, error) {
	var candidates []*domain.Upstream
	var total int64
	for _, uid := range route.UpstreamIDs {
		up, ok := p.UpstreamByID(uid)
		if !ok {
			return nil, nil, 0, bizerr.New(bizerr.UpstreamMissing, "route references a missing upstream").
				With("route", route.ID).With("upstream", uid)
		}
		if !up.Healthy {
			continue
		}
		candidates = append(candidates, up)
		sum, err := fixed.Add(total, up.WeightPPM, p.Fixed.WeightWidthBits)
		if err != nil {
			return nil, nil, 0, err
		}
		if sum.Overflow {
			return nil, nil, 0, bizerr.New(bizerr.FixedOverflow, "upstream weight sum exceeded declared width").
				With("route", route.ID).WithInt("weight_width_bits", p.Fixed.WeightWidthBits)
		}
		total = sum.Value
	}
	if len(candidates) == 0 || total == 0 {
		return nil, candidates, total, nil
	}
	h := fnv.New64a()
	_, _ = h.Write([]byte(p.Version))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(fmt.Sprintf("%d|%s|%s", req.Seq, route.ID, requestLine)))
	slot := int64(h.Sum64() % uint64(total))
	var cumulative int64
	var selected *domain.Upstream
	for _, up := range candidates {
		cumulative += up.WeightPPM
		if slot < cumulative {
			selected = up
			break
		}
	}
	return selected, candidates, total, nil
}

func appendWeightHops(p *domain.Project, res *Result, candidates []*domain.Upstream,
	total int64, req domain.Request, requestLine string, selected *domain.Upstream) error {
	appendHop(res, TraceHop{
		Name: "weight.total", Formula: "sum(healthy_weights_ppm)",
		WidthBits: p.Fixed.WeightWidthBits, FracBits: 0,
		Value: total, Tolerance: p.Fixed.Tolerance, WithinTolerance: true,
	})
	for _, up := range candidates {
		n, err := fixed.Normalize(up.WeightPPM, total, p.Fixed.WeightWidthBits, p.Fixed.WeightFracBits)
		if err != nil {
			return err
		}
		if n.Overflow {
			return bizerr.New(bizerr.FixedOverflow, "weight normalization exceeded declared width").
				With("upstream", up.ID).WithInt("width_bits", p.Fixed.WeightWidthBits)
		}
		appendHop(res, TraceHop{
			Name: "weight.normalize." + up.ID, Formula: "weight_ppm * 2^frac / total_weight_ppm",
			WidthBits: p.Fixed.WeightWidthBits, FracBits: p.Fixed.WeightFracBits,
			Value: n.Value, Tolerance: p.Fixed.Tolerance, WithinTolerance: true,
		})
	}
	h := fnv.New64a()
	_, _ = h.Write([]byte(p.Version))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(fmt.Sprintf("%d|%s|%s", req.Seq, res.RouteID, requestLine)))
	slot := int64(h.Sum64() % uint64(total))
	target, err := fixed.Normalize(slot, total, p.Fixed.WeightWidthBits, p.Fixed.WeightFracBits)
	if err != nil {
		return err
	}
	if target.Overflow {
		return bizerr.New(bizerr.FixedOverflow, "weight target exceeded declared width").
			With("route", res.RouteID)
	}
	appendHop(res, TraceHop{
		Name: "weight.target", Formula: "hash_slot * 2^frac / total_weight_ppm",
		WidthBits: p.Fixed.WeightWidthBits, FracBits: p.Fixed.WeightFracBits,
		Value: target.Value, Tolerance: p.Fixed.Tolerance, WithinTolerance: true,
	})
	selectedNorm, err := fixed.Normalize(selected.WeightPPM, total, p.Fixed.WeightWidthBits, p.Fixed.WeightFracBits)
	if err != nil {
		return err
	}
	appendHop(res, TraceHop{
		Name: "weight.selected", Formula: "selected_weight_ppm * 2^frac / total_weight_ppm",
		WidthBits: p.Fixed.WeightWidthBits, FracBits: p.Fixed.WeightFracBits,
		Value: selectedNorm.Value, Tolerance: p.Fixed.Tolerance, WithinTolerance: true,
	})
	return nil
}

func runAttempts(p *domain.Project, listener *domain.Listener, upstream *domain.Upstream,
	req domain.Request, res *Result) error {
	maxAttempts := upstream.MaxRetries + 1
	var previousEnd int64
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		start := req.ArrivalMS
		if attempt > 1 {
			delay, err := backoffMS(p, attempt-1)
			if err != nil {
				return err
			}
			delayNorm, err := fixed.Normalize(delay, 1000, p.Fixed.WeightWidthBits, p.Fixed.WeightFracBits)
			if err != nil {
				return err
			}
			appendHop(res, TraceHop{
				Name:      fmt.Sprintf("retry.backoff.%d", attempt-1),
				Formula:   "retry_tick_ms * backoff_permille * growth * 2^frac / 1e6",
				WidthBits: p.Fixed.WeightWidthBits, FracBits: p.Fixed.WeightFracBits,
				Value: delayNorm.Value, Tolerance: p.Fixed.Tolerance, WithinTolerance: true,
			})
			start, err = checkedAdd(previousEnd, delay)
			if err != nil {
				return err
			}
			if start < previousEnd {
				return bizerr.New(bizerr.TimeoutOrder, "retry start precedes prior attempt end").
					WithInt("seq", req.Seq).WithInt("attempt", attempt)
			}
		}

		nominal := int64(4)
		if attempt-1 < len(req.AttemptLatencyMS) {
			nominal = req.AttemptLatencyMS[attempt-1]
		}
		planned := ""
		if attempt-1 < len(req.FailurePlan) {
			planned = req.FailurePlan[attempt-1]
		}
		timedOut := nominal >= listener.TimeoutMS
		end, err := checkedAdd(start, nominal)
		if err != nil {
			return err
		}
		outcome := "success"
		reason := ""
		if timedOut {
			end, err = checkedAdd(start, listener.TimeoutMS)
			if err != nil {
				return err
			}
			outcome = "timeout"
			reason = ReasonTimeout
		} else if planned != "" {
			outcome = planned
			switch planned {
			case "timeout":
				outcome = "timeout"
				reason = ReasonTimeout
				end, err = checkedAdd(start, listener.TimeoutMS)
				if err != nil {
					return err
				}
				timedOut = true
			case "connect":
				reason = ReasonConnect
			case "http_503":
				reason = ReasonHTTP503
			}
		}
		if end < start {
			return bizerr.New(bizerr.TimeoutOrder, "attempt end precedes start").
				WithInt("seq", req.Seq).WithInt("attempt", attempt)
		}
		failed := outcome != "success"
		retry := failed && attempt < maxAttempts
		res.Attempts = append(res.Attempts, Attempt{
			Index: attempt, UpstreamID: upstream.ID, StartMS: start, EndMS: end,
			Outcome: outcome, TriggerReason: reason, RetryScheduled: retry,
		})
		windowNorm, err := fixed.Normalize(end-start, 1000, p.Fixed.WeightWidthBits, p.Fixed.WeightFracBits)
		if err != nil {
			return err
		}
		appendHop(res, TraceHop{
			Name:      fmt.Sprintf("attempt.window.%d", attempt),
			Formula:   "(end_ms - start_ms) * 2^frac / 1000",
			WidthBits: p.Fixed.WeightWidthBits, FracBits: p.Fixed.WeightFracBits,
			Value: windowNorm.Value, Tolerance: p.Fixed.Tolerance, WithinTolerance: true,
		})
		if !retry {
			if failed {
				res.Terminal = TerminalRetryExhausted
			} else {
				res.Terminal = TerminalSuccess
			}
			res.TerminalReason = reason
			return nil
		}
		previousEnd = end
	}
	return nil
}

func backoffMS(p *domain.Project, retryNumber int) (int64, error) {
	base, err := fixed.MulDiv(p.RetryTickMS, p.BackoffPermille, 1000, p.Fixed.WeightWidthBits)
	if err != nil {
		return 0, err
	}
	if base.Overflow {
		return 0, bizerr.New(bizerr.FixedOverflow, "retry base exceeded declared width").
			WithInt64("retry_tick_ms", p.RetryTickMS).WithInt64("backoff_permille", p.BackoffPermille)
	}
	growth := int64(1000 + (retryNumber-1)*250)
	delay, err := fixed.MulDiv(base.Value, growth, 1000, p.Fixed.WeightWidthBits)
	if err != nil {
		return 0, err
	}
	if delay.Overflow {
		return 0, bizerr.New(bizerr.FixedOverflow, "retry backoff exceeded declared width").
			WithInt("attempt", retryNumber)
	}
	return delay.Value, nil
}

func checkedAdd(a, b int64) (int64, error) {
	if a > math.MaxInt64-b {
		return 0, bizerr.New(bizerr.TimeoutOrder, "attempt time overflow").
			WithInt64("base_ms", a).WithInt64("delta_ms", b)
	}
	return a + b, nil
}

func appendHop(res *Result, hop TraceHop) {
	hop.Seq = len(res.Trace) + 1
	res.Trace = append(res.Trace, hop)
}

func compareTruth(p *domain.Project, res *Result, truth *domain.TruthRequest) {
	res.TruthMatched = true
	res.Mismatches = nil
	if res.ListenerID != truth.ListenerID {
		res.TruthMatched = false
		res.Mismatches = append(res.Mismatches, "listener_id")
	}
	if res.RouteID != truth.RouteID {
		res.TruthMatched = false
		res.Mismatches = append(res.Mismatches, "route_id")
	}
	if res.RewrittenRequestLine != truth.RewrittenRequestLine {
		res.TruthMatched = false
		res.Mismatches = append(res.Mismatches, "rewritten_request_line")
	}
	if !headersEqual(res.Headers, truth.Headers) {
		res.TruthMatched = false
		res.Mismatches = append(res.Mismatches, "headers")
	}
	if !stringSlicesEqual(res.DecisionBasis, truth.DecisionBasis) {
		res.TruthMatched = false
		res.Mismatches = append(res.Mismatches, "decision_basis")
	}
	if res.SelectedUpstreamID != truth.SelectedUpstreamID {
		res.TruthMatched = false
		res.Mismatches = append(res.Mismatches, "selected_upstream_id")
	}
	if res.Terminal != truth.Terminal {
		res.TruthMatched = false
		res.Mismatches = append(res.Mismatches, "terminal")
	}
	if res.TerminalReason != truth.TerminalReason {
		res.TruthMatched = false
		res.Mismatches = append(res.Mismatches, "terminal_reason")
	}
	if len(res.Attempts) != len(truth.Attempts) {
		res.TruthMatched = false
		res.Mismatches = append(res.Mismatches, "attempt_count")
	} else {
		for i := range res.Attempts {
			a, t := res.Attempts[i], truth.Attempts[i]
			if a.Index != t.Index || a.UpstreamID != t.UpstreamID || a.StartMS != t.StartMS ||
				a.EndMS != t.EndMS || a.Outcome != t.Outcome ||
				a.TriggerReason != t.TriggerReason || a.RetryScheduled != t.RetryScheduled {
				res.TruthMatched = false
				res.Mismatches = append(res.Mismatches, fmt.Sprintf("attempt.%d", i+1))
			}
		}
	}
	if len(res.Trace) != len(truth.Hops) {
		res.TruthMatched = false
		res.Mismatches = append(res.Mismatches, "hop_count")
	}
	tol := p.Fixed.Tolerance
	var maxAbs int64
	for i := range res.Trace {
		hop := &res.Trace[i]
		if i >= len(truth.Hops) {
			hop.Compared = false
			hop.WithinTolerance = false
			continue
		}
		th := truth.Hops[i]
		hop.Compared = true
		hop.Truth = th.Value
		if hop.Name != th.Name {
			res.TruthMatched = false
			res.Mismatches = append(res.Mismatches, fmt.Sprintf("hop.%d.name", i+1))
		}
		hop.Delta = hop.Value - th.Value
		abs := hop.Delta
		if abs < 0 {
			abs = -abs
		}
		if abs > maxAbs {
			maxAbs = abs
		}
		hop.WithinTolerance = abs <= tol && !hop.Saturated
		if !hop.WithinTolerance {
			res.TruthMatched = false
			res.Mismatches = append(res.Mismatches, fmt.Sprintf("hop.%d.tolerance", i+1))
		}
	}
	res.MaxAbsDelta = maxAbs
	res.WithinTolerance = maxAbs <= tol
	if res.ErrorCode != "" {
		res.TruthMatched = false
		res.Mismatches = append(res.Mismatches, "error_code")
	}
}

func headersEqual(a, b []domain.Header) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func stringSlicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func ResultsJSON(results []Result) ([]byte, error) {
	sorted := append([]Result(nil), results...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Seq < sorted[j].Seq })
	return json.MarshalIndent(sorted, "", "  ")
}
