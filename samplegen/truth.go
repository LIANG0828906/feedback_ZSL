package samplegen

import (
	"fmt"
	"hash/fnv"
	"strings"

	"proxydiag/bizerr"
	"proxydiag/domain"
	"proxydiag/fixed"
)

const (
	truthDecisionListener = "D01"
	truthDecisionRoute    = "D03"

	truthSuccess         = "T_SUCCESS"
	truthRetryExhausted  = "T_RETRY_EXHAUSTED"
	truthNoHealthy       = "T_NO_HEALTHY_UPSTREAM"
	truthNoListener      = "T_NO_LISTENER"
	truthNoRoute         = "T_NO_ROUTE"
	truthReasonTimeout   = "R_TIMEOUT"
	truthReasonConnect   = "R_CONNECT"
	truthReasonHTTP503   = "R_HTTP_503"
	truthReasonNoHealthy = "R_NO_HEALTHY_UPSTREAM"
)

// BuildTruth derives expected values directly from the canonical construction.
// It intentionally does not call the runtime engine.
func BuildTruth(p *domain.Project, seed int64) (*domain.Truth, error) {
	t := &domain.Truth{Version: p.Version, Seed: seed}
	for _, req := range p.Requests {
		tr, err := referenceRequest(p, req)
		if err != nil {
			return nil, err
		}
		t.Requests = append(t.Requests, tr)
	}
	return t, nil
}

func referenceRequest(p *domain.Project, req domain.Request) (domain.TruthRequest, error) {
	out := domain.TruthRequest{Seq: req.Seq}
	path, query := splitQuery(req.RawPath)
	var listener *domain.Listener
	bestLen := -1
	for i := range p.Listeners {
		l := &p.Listeners[i]
		if strings.HasPrefix(path, l.Prefix) && len(l.Prefix) > bestLen {
			listener, bestLen = l, len(l.Prefix)
		}
	}
	if listener == nil {
		out.Terminal = truthNoListener
		out.TerminalReason = "no listener prefix matched"
		return out, nil
	}
	out.ListenerID = listener.ID
	out.DecisionBasis = append(out.DecisionBasis, truthDecisionListener)
	var route *domain.Route
	bestLen = -1
	for i := range listener.Routes {
		r := &listener.Routes[i]
		effective := referenceEffectivePrefix(*listener, *r)
		if strings.HasPrefix(path, effective) && len(effective) > bestLen {
			route, bestLen = r, len(effective)
		}
	}
	if route == nil {
		out.Terminal = truthNoRoute
		out.TerminalReason = "no route prefix matched within listener"
		return out, nil
	}
	out.RouteID = route.ID
	out.DecisionBasis = append(out.DecisionBasis, truthDecisionRoute)

	rewritten, err := referenceRewrite(path, route.Rewrite)
	if err != nil {
		return out, err
	}
	out.RewrittenRequestLine = fmt.Sprintf("%s %s%s HTTP/1.1", req.Method, rewritten, query)
	out.Headers = referenceHeaders(req.Headers, route.Rewrite)
	pathHop, err := referenceMulDiv(int64(len(rewritten)), fixed.Unit(p.Fixed.PathFracBits),
		int64(max1(len(path))), p.Fixed.PathWidthBits)
	if err != nil {
		return out, err
	}
	out.Hops = append(out.Hops, domain.TruthHop{Name: "path.recombine", Value: pathHop})

	upstream, candidates, total, err := referenceUpstream(p, req, route, out.RewrittenRequestLine)
	if err != nil {
		return out, err
	}
	if upstream == nil {
		out.Terminal = truthNoHealthy
		out.TerminalReason = truthReasonNoHealthy
		return out, nil
	}
	out.SelectedUpstreamID = upstream.ID
	out.Hops = append(out.Hops, domain.TruthHop{Name: "weight.total", Value: total})
	for _, up := range candidates {
		v, err := referenceMulDiv(up.WeightPPM, fixed.Unit(p.Fixed.WeightFracBits),
			total, p.Fixed.WeightWidthBits)
		if err != nil {
			return out, err
		}
		out.Hops = append(out.Hops, domain.TruthHop{Name: "weight.normalize." + up.ID, Value: v})
	}
	h := fnv.New64a()
	_, _ = h.Write([]byte(p.Version))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(fmt.Sprintf("%d|%s|%s", req.Seq, route.ID, out.RewrittenRequestLine)))
	slot := int64(h.Sum64() % uint64(total))
	target, err := referenceMulDiv(slot, fixed.Unit(p.Fixed.WeightFracBits), total, p.Fixed.WeightWidthBits)
	if err != nil {
		return out, err
	}
	out.Hops = append(out.Hops, domain.TruthHop{Name: "weight.target", Value: target})
	selected, err := referenceMulDiv(upstream.WeightPPM, fixed.Unit(p.Fixed.WeightFracBits),
		total, p.Fixed.WeightWidthBits)
	if err != nil {
		return out, err
	}
	out.Hops = append(out.Hops, domain.TruthHop{Name: "weight.selected", Value: selected})

	var previousEnd int64
	maxAttempts := upstream.MaxRetries + 1
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		start := req.ArrivalMS
		if attempt > 1 {
			delay, err := referenceBackoff(p, attempt-1)
			if err != nil {
				return out, err
			}
			delayNorm, err := referenceMulDiv(delay, fixed.Unit(p.Fixed.WeightFracBits),
				1000, p.Fixed.WeightWidthBits)
			if err != nil {
				return out, err
			}
			out.Hops = append(out.Hops, domain.TruthHop{
				Name: fmt.Sprintf("retry.backoff.%d", attempt-1), Value: delayNorm,
			})
			start = previousEnd + delay
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
		end := start + nominal
		outcome, reason := "success", ""
		if timedOut {
			end = start + listener.TimeoutMS
			outcome, reason = "timeout", truthReasonTimeout
		} else if planned != "" {
			outcome = planned
			switch planned {
			case "timeout":
				outcome, reason = "timeout", truthReasonTimeout
				end = start + listener.TimeoutMS
				timedOut = true
			case "connect":
				reason = truthReasonConnect
			case "http_503":
				reason = truthReasonHTTP503
			}
		}
		failed := outcome != "success"
		retry := failed && attempt < maxAttempts
		out.Attempts = append(out.Attempts, domain.TruthAttempt{
			Index: attempt, UpstreamID: upstream.ID, StartMS: start, EndMS: end,
			Outcome: outcome, TriggerReason: reason, RetryScheduled: retry,
		})
		window, err := referenceMulDiv(end-start, fixed.Unit(p.Fixed.WeightFracBits),
			1000, p.Fixed.WeightWidthBits)
		if err != nil {
			return out, err
		}
		out.Hops = append(out.Hops, domain.TruthHop{
			Name: fmt.Sprintf("attempt.window.%d", attempt), Value: window,
		})
		if !retry {
			out.TerminalReason = reason
			if failed {
				out.Terminal = truthRetryExhausted
			} else {
				out.Terminal = truthSuccess
			}
			return out, nil
		}
		previousEnd = end
	}
	return out, bizerr.New(bizerr.Internal, "reference attempt loop did not terminate")
}

func splitQuery(raw string) (string, string) {
	if i := strings.IndexByte(raw, '?'); i >= 0 {
		return raw[:i], raw[i:]
	}
	return raw, ""
}

func referenceEffectivePrefix(l domain.Listener, r domain.Route) string {
	if r.Prefix == "" || r.Prefix == "/" {
		return l.Prefix
	}
	return strings.TrimSuffix(l.Prefix, "/") + "/" + strings.TrimPrefix(r.Prefix, "/")
}

func referenceRewrite(path string, rw domain.Rewrite) (string, error) {
	out := path
	if rw.StripPrefix != "" {
		if !strings.HasPrefix(out, rw.StripPrefix) {
			return "", fmt.Errorf("reference strip mismatch")
		}
		out = strings.TrimPrefix(out, rw.StripPrefix)
	}
	return rw.AddPrefix + out, nil
}

func referenceHeaders(in []domain.Header, rw domain.Rewrite) []domain.Header {
	removed := map[string]bool{}
	for _, h := range rw.RemoveHeaders {
		removed[strings.ToLower(h)] = true
	}
	out := make([]domain.Header, 0, len(in)+len(rw.SetHeaders))
	for _, h := range in {
		if !removed[strings.ToLower(h.Name)] {
			out = append(out, h)
		}
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

func referenceUpstream(p *domain.Project, req domain.Request, route *domain.Route,
	line string) (*domain.Upstream, []*domain.Upstream, int64, error) {
	var candidates []*domain.Upstream
	var total int64
	for _, uid := range route.UpstreamIDs {
		up, ok := p.UpstreamByID(uid)
		if !ok {
			return nil, nil, 0, fmt.Errorf("reference missing upstream %s", uid)
		}
		if up.Healthy {
			candidates = append(candidates, up)
			total += up.WeightPPM
		}
	}
	if total == 0 {
		return nil, candidates, 0, nil
	}
	h := fnv.New64a()
	_, _ = h.Write([]byte(p.Version))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(fmt.Sprintf("%d|%s|%s", req.Seq, route.ID, line)))
	slot := int64(h.Sum64() % uint64(total))
	var cumulative int64
	for _, up := range candidates {
		cumulative += up.WeightPPM
		if slot < cumulative {
			return up, candidates, total, nil
		}
	}
	return nil, candidates, total, fmt.Errorf("reference weighted selection failed")
}

func referenceBackoff(p *domain.Project, retry int) (int64, error) {
	base, err := referenceMulDiv(p.RetryTickMS, p.BackoffPermille, 1000, p.Fixed.WeightWidthBits)
	if err != nil {
		return 0, err
	}
	growth := int64(1000 + (retry-1)*250)
	return referenceMulDiv(base, growth, 1000, p.Fixed.WeightWidthBits)
}

func referenceMulDiv(a, b, c int64, width int) (int64, error) {
	out, _ := fixed.MulDiv(a, b, c, width)
	if out.Overflow {
		return 0, fmt.Errorf("reference fixed overflow")
	}
	return out.Value, nil
}

func max1(v int) int {
	if v < 1 {
		return 1
	}
	return v
}
