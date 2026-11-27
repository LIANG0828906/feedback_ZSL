package engine

import (
	"fmt"
	"sort"
	"sync"
	"time"

	"proxydiag/bizerr"
	"proxydiag/domain"
)

type ConcurrencyConfig struct {
	Workers int
	// ListenerBufferOverride can only reduce a listener's declared limit.
	// A zero override intentionally models a zero-capacity buffer.
	ListenerBufferOverride map[string]int
}

type Backpressure struct {
	Seq        int               `json:"seq"`
	ListenerID string            `json:"listener_id"`
	Code       string            `json:"code"`
	Workers    int               `json:"workers"`
	Limit      int               `json:"limit,omitempty"`
	GroupSize  int               `json:"group_size"`
	Context    map[string]string `json:"context,omitempty"`
}

type ListenerTiming struct {
	ListenerID string `json:"listener_id"`
	Tasks      int    `json:"tasks"`
	ElapsedNS  int64  `json:"elapsed_ns"`
	TotalNS    int64  `json:"total_ns"`
}

type ConcurrentReport struct {
	Workers         int              `json:"workers"`
	Groups          int              `json:"groups"`
	Results         []Result         `json:"results"`
	Backpressure    []Backpressure   `json:"backpressure,omitempty"`
	ListenerTimings []ListenerTiming `json:"listener_timings"`
}

// RunConcurrent uses logical arrival batches for deterministic admission.
// Requests in a batch are the concurrency unit. A batch larger than a
// listener's buffer or the worker budget produces explicit backpressure
// results; no request is silently dropped.
func (e *Engine) RunConcurrent(cfg ConcurrencyConfig) ConcurrentReport {
	e.mu.Lock()
	defer e.mu.Unlock()
	report := ConcurrentReport{Workers: cfg.Workers, Results: []Result{}}
	if cfg.Workers < 0 {
		cfg.Workers = 0
	}
	groups := groupByArrival(e.project.Requests)
	report.Groups = len(groups)
	timings := map[string]*ListenerTiming{}
	var timingMu sync.Mutex
	all := make([]Result, 0, len(e.project.Requests))
	if cfg.Workers == 0 {
		for _, req := range e.project.Requests {
			listenerID := ""
			if l, _, ok := selectListener(e.project, pathOnly(req.RawPath)); ok {
				listenerID = l.ID
			}
			bp := Backpressure{
				Seq: req.Seq, ListenerID: listenerID, Code: bizerr.BackpressureWorkers,
				Workers: 0, GroupSize: 1,
				Context: map[string]string{"reason": "declared worker budget is zero"},
			}
			report.Backpressure = append(report.Backpressure, bp)
			all = append(all, backpressureResult(req, listenerID, bp))
		}
		sort.Slice(all, func(i, j int) bool { return all[i].Seq < all[j].Seq })
		report.Results = all
		return report
	}

	for _, group := range groups {
		accepted, rejected := admit(e.project, group, cfg)
		report.Backpressure = append(report.Backpressure, rejected...)
		for _, req := range group {
			if _, bad := rejectedSeq(rejected, req.Seq); bad {
				listenerID := listenerFor(e.project, req)
				all = append(all, backpressureResult(req, listenerID, findBackpressure(rejected, req.Seq)))
			}
		}
		if len(accepted) == 0 {
			continue
		}
		groupStart := time.Now()
		results := make([]Result, len(accepted))
		var wg sync.WaitGroup
		wg.Add(len(accepted))
		for i := range accepted {
			go func(i int) {
				defer wg.Done()
				start := time.Now()
				req := accepted[i]
				var tr *domain.TruthRequest
				if t, ok := e.truthBySeq[req.Seq]; ok {
					tcopy := t
					tr = &tcopy
				}
				results[i] = EvaluateRequest(e.project, req, tr)
				dur := time.Since(start).Nanoseconds()
				listenerID := results[i].ListenerID
				if listenerID == "" {
					listenerID = listenerFor(e.project, req)
				}
				timingMu.Lock()
				timing := timings[listenerID]
				if timing == nil {
					timing = &ListenerTiming{ListenerID: listenerID}
					timings[listenerID] = timing
				}
				timing.Tasks++
				timing.TotalNS += dur
				if dur > timing.ElapsedNS {
					timing.ElapsedNS = dur
				}
				timingMu.Unlock()
			}(i)
		}
		wg.Wait()
		_ = groupStart
		for _, r := range results {
			all = append(all, r)
			e.results[r.Seq] = r
		}
	}
	sort.Slice(all, func(i, j int) bool { return all[i].Seq < all[j].Seq })
	report.Results = all
	ids := make([]string, 0, len(timings))
	for id := range timings {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		report.ListenerTimings = append(report.ListenerTimings, *timings[id])
	}
	return report
}

func groupByArrival(reqs []domain.Request) [][]domain.Request {
	var groups [][]domain.Request
	for _, req := range reqs {
		if len(groups) == 0 || groups[len(groups)-1][0].ArrivalMS != req.ArrivalMS {
			groups = append(groups, []domain.Request{req})
		} else {
			groups[len(groups)-1] = append(groups[len(groups)-1], req)
		}
	}
	return groups
}

func admit(p *domain.Project, group []domain.Request, cfg ConcurrencyConfig) ([]domain.Request, []Backpressure) {
	limits := map[string]int{}
	counts := map[string]int{}
	for _, l := range p.Listeners {
		limit := l.InflightLimit
		if v, ok := cfg.ListenerBufferOverride[l.ID]; ok && v < limit {
			limit = v
		}
		if limit < 0 {
			limit = 0
		}
		limits[l.ID] = limit
	}
	accepted := make([]domain.Request, 0, len(group))
	rejected := make([]Backpressure, 0)
	for _, req := range group {
		listenerID := listenerFor(p, req)
		if _, ok := limits[listenerID]; !ok {
			limits[listenerID] = len(group)
		}
		counts[listenerID]++
		if counts[listenerID] > limits[listenerID] {
			rejected = append(rejected, Backpressure{
				Seq: req.Seq, ListenerID: listenerID, Code: bizerr.BackpressureBuffer,
				Workers: cfg.Workers, Limit: limits[listenerID], GroupSize: len(group),
				Context: map[string]string{
					"reason": "arrival burst exceeds listener in-flight buffer",
				},
			})
			continue
		}
		accepted = append(accepted, req)
	}
	if len(accepted) > cfg.Workers {
		overflow := accepted[cfg.Workers:]
		accepted = accepted[:cfg.Workers]
		for _, req := range overflow {
			rejected = append(rejected, Backpressure{
				Seq: req.Seq, ListenerID: listenerFor(p, req), Code: bizerr.BackpressureWorkers,
				Workers: cfg.Workers, GroupSize: len(group),
				Context: map[string]string{
					"reason": "arrival burst exceeds worker budget",
				},
			})
		}
	}
	return accepted, rejected
}

func backpressureResult(req domain.Request, listenerID string, bp Backpressure) Result {
	res := Result{
		Seq: req.Seq, ArrivalMS: req.ArrivalMS, Method: req.Method, RawPath: req.RawPath,
		ListenerID: listenerID, Terminal: TerminalBackpressure,
		ErrorCode: bp.Code, TerminalReason: bp.Code, ErrorMessage: bp.Context["reason"],
		ErrorContext: bp.Context, WithinTolerance: true, TruthMatched: false,
	}
	return res
}

func listenerFor(p *domain.Project, req domain.Request) string {
	if l, _, ok := selectListener(p, pathOnly(req.RawPath)); ok {
		return l.ID
	}
	return "unmatched"
}

func rejectedSeq(rejected []Backpressure, seq int) (Backpressure, bool) {
	for _, bp := range rejected {
		if bp.Seq == seq {
			return bp, true
		}
	}
	return Backpressure{}, false
}

func findBackpressure(rejected []Backpressure, seq int) Backpressure {
	bp, _ := rejectedSeq(rejected, seq)
	return bp
}

func (r ConcurrentReport) BackpressureCodes() []string {
	seen := map[string]bool{}
	var out []string
	for _, bp := range r.Backpressure {
		if !seen[bp.Code] {
			seen[bp.Code] = true
			out = append(out, bp.Code)
		}
	}
	sort.Strings(out)
	return out
}

func (r ConcurrentReport) Summary() string {
	return fmt.Sprintf("workers=%d groups=%d results=%d backpressure=%d codes=%v",
		r.Workers, r.Groups, len(r.Results), len(r.Backpressure), r.BackpressureCodes())
}

var _ = domain.Header{}
