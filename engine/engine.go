package engine

import (
	"sort"
	"sync"

	"proxydiag/domain"
)

// Engine owns a committed project snapshot and its per-request decision cache.
// Project values are cloned on every mutation; callers cannot mutate engine
// state through an alias.
type Engine struct {
	mu          sync.RWMutex
	project     *domain.Project
	truth       *domain.Truth
	truthBySeq  map[int]domain.TruthRequest
	results     map[int]Result
	fingerprint string
}

func New(p *domain.Project, truth *domain.Truth) *Engine {
	e := &Engine{}
	e.replaceUnchecked(p, truth, true)
	return e
}

func (e *Engine) replaceUnchecked(p *domain.Project, truth *domain.Truth, clear bool) {
	e.project = p.Clone()
	e.fingerprint = p.CoreFingerprint()
	e.truth = nil
	e.truthBySeq = map[int]domain.TruthRequest{}
	if truth != nil && truth.Version == p.Version {
		copyTruth := *truth
		copyTruth.Requests = append([]domain.TruthRequest(nil), truth.Requests...)
		e.truth = &copyTruth
		for _, tr := range truth.Requests {
			e.truthBySeq[tr.Seq] = tr
		}
	}
	if clear {
		e.results = map[int]Result{}
	}
}

func (e *Engine) Project() *domain.Project {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.project.Clone()
}

func (e *Engine) Version() string {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.project.Version
}

func (e *Engine) Truth() *domain.Truth {
	e.mu.RLock()
	defer e.mu.RUnlock()
	if e.truth == nil {
		return nil
	}
	t := *e.truth
	t.Requests = append([]domain.TruthRequest(nil), e.truth.Requests...)
	return &t
}

func (e *Engine) truthFor(seq int) *domain.TruthRequest {
	if t, ok := e.truthBySeq[seq]; ok {
		return &t
	}
	return nil
}

func (e *Engine) RunAll() []Result {
	e.mu.Lock()
	defer e.mu.Unlock()
	results := evaluateProject(e.project, e.truthBySeq)
	e.results = make(map[int]Result, len(results))
	for _, r := range results {
		e.results[r.Seq] = r
	}
	return cloneResults(results)
}

func evaluateProject(p *domain.Project, truthBySeq map[int]domain.TruthRequest) []Result {
	out := make([]Result, 0, len(p.Requests))
	for _, req := range p.Requests {
		var tr *domain.TruthRequest
		if t, ok := truthBySeq[req.Seq]; ok {
			tcopy := t
			tr = &tcopy
		}
		out = append(out, EvaluateRequest(p, req, tr))
	}
	return out
}

// EvaluateAll is a deterministic full recomputation used for equivalence
// checks and diagnostics. It does not touch an Engine cache.
func EvaluateAll(p *domain.Project, truth *domain.Truth) []Result {
	bySeq := map[int]domain.TruthRequest{}
	if truth != nil && truth.Version == p.Version {
		for _, tr := range truth.Requests {
			bySeq[tr.Seq] = tr
		}
	}
	return evaluateProject(p, bySeq)
}

func (e *Engine) Results() []Result {
	e.mu.RLock()
	defer e.mu.RUnlock()
	out := make([]Result, 0, len(e.results))
	for _, r := range e.results {
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Seq < out[j].Seq })
	return cloneResults(out)
}

func (e *Engine) Result(seq int) (Result, bool) {
	e.mu.RLock()
	defer e.mu.RUnlock()
	r, ok := e.results[seq]
	return r, ok
}

func cloneResults(in []Result) []Result {
	out := make([]Result, len(in))
	copy(out, in)
	for i := range out {
		out[i].Headers = append([]domain.Header(nil), in[i].Headers...)
		out[i].Attempts = append([]Attempt(nil), in[i].Attempts...)
		out[i].Trace = append([]TraceHop(nil), in[i].Trace...)
		out[i].Mismatches = append([]string(nil), in[i].Mismatches...)
		out[i].DecisionBasis = append([]string(nil), in[i].DecisionBasis...)
		out[i].Cache.InvalidationSource = append([]string(nil), in[i].Cache.InvalidationSource...)
		if in[i].ErrorContext != nil {
			out[i].ErrorContext = make(map[string]string, len(in[i].ErrorContext))
			for k, v := range in[i].ErrorContext {
				out[i].ErrorContext[k] = v
			}
		}
	}
	return out
}
