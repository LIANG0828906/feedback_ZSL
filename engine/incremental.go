package engine

import (
	"fmt"
	"sort"

	"proxydiag/bizerr"
	"proxydiag/domain"
)

type EditKind string

const (
	EditHealth  EditKind = "health"
	EditWeight  EditKind = "weight"
	EditRewrite EditKind = "rewrite"
	EditAdd     EditKind = "add_request"
	EditDelete  EditKind = "delete_request"

	InvalidationHealth        = "I_HEALTH"
	InvalidationWeight        = "I_WEIGHT"
	InvalidationRewrite       = "I_REWRITE"
	InvalidationRequestAdd    = "I_REQUEST_ADD"
	InvalidationRequestDelete = "I_REQUEST_DELETE"
	InvalidationSnapshot      = "I_SNAPSHOT_REPLACE"
)

type Edit struct {
	Kind        EditKind
	AtSeq       int
	UpstreamID  string
	Healthy     bool
	WeightPPM   int64
	RouteID     string
	StripPrefix string
	AddPrefix   string
	Request     *domain.Request
	Seq         int
}

type InvalidationReport struct {
	Kind              EditKind `json:"kind"`
	Target            string   `json:"target"`
	AtSeq             int      `json:"at_seq"`
	Version           string   `json:"version"`
	Source            string   `json:"source"`
	FullReset         bool     `json:"full_reset"`
	Closure           []string `json:"closure"`
	Recomputed        []int    `json:"recomputed"`
	Reused            []int    `json:"reused"`
	BeforeFingerprint string   `json:"before_fingerprint"`
	AfterFingerprint  string   `json:"after_fingerprint"`
}

func (e *Engine) Apply(edit Edit) (*InvalidationReport, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	after := e.project.Clone()
	report := &InvalidationReport{
		Kind: edit.Kind, AtSeq: edit.AtSeq, Version: after.Version,
		BeforeFingerprint: e.fingerprint,
	}
	if err := applyEdit(after, edit); err != nil {
		return nil, err
	}
	if err := after.Validate(); err != nil {
		return nil, err
	}
	report.AfterFingerprint = after.CoreFingerprint()
	if edit.AtSeq <= 0 {
		report.AtSeq = minRequestSeq(after)
	}
	report.Target = editTarget(edit)
	report.Source, report.Closure = invalidationSource(edit, after)

	routeDeps := affectedRoutes(after, edit)
	recompute := map[int]bool{}
	switch edit.Kind {
	case EditAdd, EditDelete:
		for _, req := range after.Requests {
			if req.Seq >= report.AtSeq {
				recompute[req.Seq] = true
			}
		}
	case EditHealth, EditWeight:
		for _, req := range after.Requests {
			if req.Seq < report.AtSeq {
				continue
			}
			listener, _, ok := selectListener(after, pathOnly(req.RawPath))
			if !ok {
				// A previously unmatched request cannot depend on this upstream.
				continue
			}
			route, _, ok := selectRoute(listener, pathOnly(req.RawPath))
			if ok && routeDeps[route.ID] {
				recompute[req.Seq] = true
			}
		}
	case EditRewrite:
		for _, req := range after.Requests {
			if req.Seq < report.AtSeq {
				continue
			}
			listener, _, ok := selectListener(after, pathOnly(req.RawPath))
			if !ok {
				continue
			}
			route, _, ok := selectRoute(listener, pathOnly(req.RawPath))
			if ok && route.ID == edit.RouteID {
				recompute[req.Seq] = true
			}
		}
	default:
		return nil, bizerr.New(bizerr.InvalidEdit, "unknown incremental edit kind").
			With("kind", string(edit.Kind))
	}

	// Build the new cache. Unaffected entries are reused byte-for-byte. Edited
	// requests are evaluated against the new snapshot without the stale truth.
	newResults := make(map[int]Result, len(after.Requests))
	reused := make([]int, 0, len(e.results))
	for _, req := range after.Requests {
		if recompute[req.Seq] {
			continue
		}
		if old, ok := e.results[req.Seq]; ok {
			old.Cache.Reused = true
			newResults[req.Seq] = old
			reused = append(reused, req.Seq)
			continue
		}
		recompute[req.Seq] = true
	}
	recomputed := make([]int, 0, len(recompute))
	for _, req := range after.Requests {
		if !recompute[req.Seq] {
			continue
		}
		r := EvaluateRequest(after, req, nil)
		r.Cache.Reused = false
		r.Cache.InvalidationSource = []string{report.Source}
		newResults[req.Seq] = r
		recomputed = append(recomputed, req.Seq)
	}

	report.Reused = reused
	report.Recomputed = recomputed
	for _, seq := range recomputed {
		report.Closure = append(report.Closure, fmt.Sprintf("request:%d", seq))
	}
	report.Closure = uniqueSorted(report.Closure)
	sort.Ints(report.Reused)
	sort.Ints(report.Recomputed)

	e.project = after
	e.fingerprint = report.AfterFingerprint
	e.results = newResults
	e.truth = nil
	e.truthBySeq = map[int]domain.TruthRequest{}
	return report, nil
}

func applyEdit(p *domain.Project, edit Edit) error {
	if edit.Kind == EditAdd {
		if edit.Request == nil {
			return bizerr.New(bizerr.InvalidEdit, "add request requires a request value")
		}
		if _, ok := p.RequestBySeq(edit.Request.Seq); ok {
			return bizerr.New(bizerr.InvalidEdit, "request sequence already exists").
				WithInt("seq", edit.Request.Seq)
		}
		p.Requests = append(p.Requests, *edit.Request)
		sort.SliceStable(p.Requests, func(i, j int) bool { return p.Requests[i].Seq < p.Requests[j].Seq })
		return nil
	}
	if edit.Kind == EditDelete {
		for i := range p.Requests {
			if p.Requests[i].Seq == edit.Seq {
				p.Requests = append(p.Requests[:i], p.Requests[i+1:]...)
				return nil
			}
		}
		return bizerr.New(bizerr.InvalidEdit, "delete request target not found").
			WithInt("seq", edit.Seq)
	}
	if edit.Kind == EditHealth || edit.Kind == EditWeight {
		up, ok := p.UpstreamByID(edit.UpstreamID)
		if !ok {
			return bizerr.New(bizerr.InvalidEdit, "upstream edit target not found").
				With("upstream", edit.UpstreamID)
		}
		if edit.Kind == EditHealth {
			up.Healthy = edit.Healthy
		} else {
			up.WeightPPM = edit.WeightPPM
		}
		return nil
	}
	if edit.Kind == EditRewrite {
		route, _, ok := p.RouteByID(edit.RouteID)
		if !ok {
			return bizerr.New(bizerr.InvalidEdit, "rewrite edit target not found").
				With("route", edit.RouteID)
		}
		route.Rewrite.StripPrefix = edit.StripPrefix
		route.Rewrite.AddPrefix = edit.AddPrefix
		return nil
	}
	return bizerr.New(bizerr.InvalidEdit, "unknown edit kind").With("kind", string(edit.Kind))
}

func editTarget(edit Edit) string {
	switch edit.Kind {
	case EditHealth, EditWeight:
		return "upstream:" + edit.UpstreamID
	case EditRewrite:
		return "route:" + edit.RouteID
	case EditAdd:
		if edit.Request != nil {
			return fmt.Sprintf("request:%d", edit.Request.Seq)
		}
	case EditDelete:
		return fmt.Sprintf("request:%d", edit.Seq)
	}
	return "unknown"
}

func invalidationSource(edit Edit, p *domain.Project) (string, []string) {
	var source string
	var closure []string
	switch edit.Kind {
	case EditHealth:
		source = InvalidationHealth
		closure = append(closure, "upstream:"+edit.UpstreamID)
	case EditWeight:
		source = InvalidationWeight
		closure = append(closure, "upstream:"+edit.UpstreamID)
	case EditRewrite:
		source = InvalidationRewrite
		closure = append(closure, "route:"+edit.RouteID)
	case EditAdd:
		source = InvalidationRequestAdd
		closure = append(closure, fmt.Sprintf("request:%d", edit.Request.Seq))
	case EditDelete:
		source = InvalidationRequestDelete
		closure = append(closure, fmt.Sprintf("request:%d", edit.Seq))
	}
	for _, l := range p.Listeners {
		for _, r := range l.Routes {
			if (edit.Kind == EditHealth || edit.Kind == EditWeight) && contains(r.UpstreamIDs, edit.UpstreamID) {
				closure = append(closure, "route:"+r.ID, "listener:"+l.ID)
			}
			if edit.Kind == EditRewrite && r.ID == edit.RouteID {
				closure = append(closure, "listener:"+l.ID)
			}
		}
	}
	return source, closure
}

func affectedRoutes(p *domain.Project, edit Edit) map[string]bool {
	deps := map[string]bool{}
	if edit.Kind != EditHealth && edit.Kind != EditWeight {
		return deps
	}
	for _, l := range p.Listeners {
		for _, r := range l.Routes {
			if contains(r.UpstreamIDs, edit.UpstreamID) {
				deps[r.ID] = true
			}
		}
	}
	return deps
}

func contains(haystack []string, needle string) bool {
	for _, v := range haystack {
		if v == needle {
			return true
		}
	}
	return false
}

func minRequestSeq(p *domain.Project) int {
	if len(p.Requests) == 0 {
		return 1
	}
	return p.Requests[0].Seq
}

func pathOnly(raw string) string {
	for i := 0; i < len(raw); i++ {
		if raw[i] == '?' {
			return raw[:i]
		}
	}
	return raw
}

func uniqueSorted(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	sort.Strings(in)
	out := in[:0]
	for _, v := range in {
		if len(out) == 0 || out[len(out)-1] != v {
			out = append(out, v)
		}
	}
	return out
}

// ReplaceSnapshot is the only whole-project replacement entry point. Version
// equality is authoritative; content equality is never mistaken for identity.
func (e *Engine) ReplaceSnapshot(p *domain.Project, truth *domain.Truth) (*InvalidationReport, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := p.Validate(); err != nil {
		return nil, err
	}
	oldVersion := e.project.Version
	oldFP := e.fingerprint
	newFP := p.CoreFingerprint()
	report := &InvalidationReport{
		Kind: "snapshot", Target: "project", Version: p.Version,
		Source: InvalidationSnapshot, BeforeFingerprint: oldFP, AfterFingerprint: newFP,
	}
	if oldVersion == p.Version {
		if oldFP != newFP {
			return nil, bizerr.New(bizerr.SnapshotVersion, "snapshot content changed without a version change").
				With("version", p.Version)
		}
		e.replaceUnchecked(p, truth, false)
		for _, req := range e.project.Requests {
			if r, ok := e.results[req.Seq]; ok {
				r.Cache.Reused = true
				e.results[req.Seq] = r
				report.Reused = append(report.Reused, req.Seq)
			}
		}
		sort.Ints(report.Reused)
		return report, nil
	}
	e.replaceUnchecked(p, truth, true)
	results := evaluateProject(e.project, e.truthBySeq)
	for _, r := range results {
		e.results[r.Seq] = r
		report.Recomputed = append(report.Recomputed, r.Seq)
	}
	report.FullReset = true
	sort.Ints(report.Recomputed)
	return report, nil
}
