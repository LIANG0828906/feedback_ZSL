package engine

import (
	"dnslab/codes"
	"dnslab/fixedint"
)

// HopTrace records the deterministic virtual forwarding path.
type HopTrace struct {
	Path             []int `json:"path"`
	VirtualElapsedMS int64 `json:"virtual_elapsed_ms"`
	TerminalIndex    int   `json:"terminal_index,omitempty"`
}

func (e *Engine) forward(startIndex int) (HopTrace, *codes.Error) {
	trace := HopTrace{Path: []int{}}
	position := map[int]int{}
	current := startIndex

	for {
		if at, ok := position[current]; ok {
			cyclePath := append(append([]int{}, trace.Path...), current)
			return trace, codes.New(codes.EUpstreamCycle, "forwarding path revisits an upstream").
				WithContext("upstream_index", current).
				WithContext("first_visit_position", at).
				WithContext("path", cyclePath).
				WithContext("hop_count", len(trace.Path))
		}
		if len(trace.Path) >= e.project.MaxHops {
			return trace, codes.New(codes.EUpstreamHopsExceeded, "per-query forwarding hop budget exhausted").
				WithContext("hop_count", len(trace.Path)).
				WithContext("max_hops", e.project.MaxHops).
				WithContext("next_upstream_index", current).
				WithContext("path", trace.Path)
		}
		u, ok := e.upstreams[current]
		if !ok {
			return trace, codes.New(codes.EUpstreamReference, "forward path references undeclared upstream").
				WithContext("upstream_index", current).
				WithContext("path", trace.Path)
		}

		position[current] = len(trace.Path)
		trace.Path = append(trace.Path, current)
		elapsed := fixedint.Add64(trace.VirtualElapsedMS, u.LatencyMS)
		if elapsed.Saturated {
			return trace, codes.New(codes.EIntOverflow, "virtual forwarding elapsed time saturated").
				WithContext("path", trace.Path)
		}
		trace.VirtualElapsedMS = elapsed.Value

		if u.LatencyMS > u.TimeoutMS {
			return trace, codes.New(codes.EUpstreamTimeout, "declared upstream latency exceeds normalized integer timeout").
				WithContext("upstream_index", u.Index).
				WithContext("timeout_ms", u.TimeoutMS).
				WithContext("latency_ms", u.LatencyMS).
				WithContext("virtual_elapsed_ms", trace.VirtualElapsedMS).
				WithContext("wall_clock_wait", false)
		}
		if u.ForwardTo == 0 {
			trace.TerminalIndex = u.Index
			return trace, nil
		}
		current = u.ForwardTo
	}
}
