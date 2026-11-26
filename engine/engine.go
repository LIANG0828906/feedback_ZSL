// Package engine resolves deterministic DNS queries against canonical
// engineering data, including route adjudication, cache TTL state, simulated
// forwarding, and incremental cache segments.
package engine

import (
	"sort"
	"sync"

	"dnslab/codes"
	"dnslab/fixedint"
	"dnslab/spec"
)

// Segment is one independently invalidatable regional cache segment.
type Segment struct {
	RegionID  string                 `json:"region_id"`
	Revision  int64                  `json:"revision"`
	BuiltAtMS int64                  `json:"built_at_ms"`
	Entries   map[string]*CacheEntry `json:"entries"`
}

// CacheEntry stores normalized absolute expiry values.
type CacheEntry struct {
	Key                 string        `json:"key"`
	QName               string        `json:"qname,omitempty"`
	QType               string        `json:"qtype,omitempty"`
	RegionID            string        `json:"region_id"`
	Negative            bool          `json:"negative"`
	Records             []spec.Record `json:"records,omitempty"`
	NegativeExpiresAtMS int64         `json:"negative_expires_at_ms,omitempty"`
	CreatedAtMS         int64         `json:"created_at_ms"`
	Source              string        `json:"source"`
	DependencyRules     []string      `json:"dependency_rules,omitempty"`
	DependencyUpstreams []int         `json:"dependency_upstreams,omitempty"`
}

// Result is the per-query deterministic diagnostic output.
type Result struct {
	Sequence         int            `json:"sequence"`
	QueryID          string         `json:"query_id"`
	Code             string         `json:"code"`
	Matched          bool           `json:"matched"`
	RuleNumber       string         `json:"rule_number,omitempty"`
	RuleOrdinal      int            `json:"rule_ordinal,omitempty"`
	DecisionBasis    string         `json:"decision_basis,omitempty"`
	CandidateRules   []string       `json:"candidate_rules,omitempty"`
	UpstreamIndex    int            `json:"upstream_index,omitempty"`
	HopPath          []int          `json:"hop_path,omitempty"`
	HopCount         int            `json:"hop_count"`
	Source           string         `json:"source,omitempty"`
	Negative         bool           `json:"negative,omitempty"`
	Records          []spec.Record  `json:"records,omitempty"`
	RemainingTTLMS   []int64        `json:"remaining_ttl_ms,omitempty"`
	EntryRemainingMS int64          `json:"entry_remaining_ms,omitempty"`
	ExpiryBasis      string         `json:"expiry_basis,omitempty"`
	VirtualElapsedMS int64          `json:"virtual_elapsed_ms"`
	Saturated        bool           `json:"saturated,omitempty"`
	RegionID         string         `json:"region_id,omitempty"`
	SegmentRevision  int64          `json:"segment_revision,omitempty"`
	ErrorContext     map[string]any `json:"error_context,omitempty"`
}

// Engine is safe for concurrent use because cache mutation is serialized.
type Engine struct {
	mu        sync.Mutex
	project   *spec.Project
	segments  map[string]*Segment
	answers   map[string]spec.AnswerSet
	upstreams map[int]spec.Upstream
}

// NewEngine loads declared cache seeds into regional segments.
func NewEngine(project *spec.Project) *Engine {
	p := project.Clone()
	e := &Engine{
		project:   p,
		segments:  map[string]*Segment{},
		answers:   map[string]spec.AnswerSet{},
		upstreams: map[int]spec.Upstream{},
	}
	for _, u := range p.Upstreams {
		e.upstreams[u.Index] = u
	}
	for _, a := range p.AnswerSets {
		e.answers[a.CacheKey] = a
	}
	for _, seed := range p.CacheSeeds {
		seg := e.segmentFor(seed.RegionID)
		entry := &CacheEntry{
			Key:                 seed.CacheKey,
			QName:               seed.QName,
			QType:               seed.QType,
			RegionID:            seed.RegionID,
			Negative:            seed.Negative,
			Records:             append([]spec.Record(nil), seed.Records...),
			NegativeExpiresAtMS: seed.NegativeExpiresAtMS,
			CreatedAtMS:         p.ReferenceNowMS,
			Source:              spec.SourcePositiveSeed,
		}
		if seed.Negative {
			entry.Source = spec.SourceNegativeSeed
		}
		seg.Entries[seed.CacheKey] = entry
	}
	e.rebuildDependenciesLocked()
	return e
}

func (e *Engine) segmentFor(regionID string) *Segment {
	seg, ok := e.segments[regionID]
	if !ok {
		seg = &Segment{RegionID: regionID, Revision: 1, BuiltAtMS: e.project.ReferenceNowMS, Entries: map[string]*CacheEntry{}}
		e.segments[regionID] = seg
	}
	return seg
}

// Project returns a deep copy so callers cannot mutate source state.
func (e *Engine) Project() *spec.Project { return e.project.Clone() }

// SegmentSnapshots returns checkpoint-safe copies of all cache segments.
func (e *Engine) SegmentSnapshots() map[string]Segment {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make(map[string]Segment, len(e.segments))
	for id, seg := range e.segments {
		cp := Segment{RegionID: seg.RegionID, Revision: seg.Revision, BuiltAtMS: seg.BuiltAtMS, Entries: map[string]*CacheEntry{}}
		for k, ent := range seg.Entries {
			ecp := *ent
			ecp.Records = append([]spec.Record(nil), ent.Records...)
			ecp.DependencyRules = append([]string(nil), ent.DependencyRules...)
			ecp.DependencyUpstreams = append([]int(nil), ent.DependencyUpstreams...)
			cp.Entries[k] = &ecp
		}
		out[id] = cp
	}
	return out
}

// RestoreSegment replaces a segment during checkpoint recovery.
func (e *Engine) RestoreSegment(seg Segment) {
	e.mu.Lock()
	defer e.mu.Unlock()
	cp := seg
	cp.Entries = map[string]*CacheEntry{}
	for k, ent := range seg.Entries {
		ecp := *ent
		ecp.Records = append([]spec.Record(nil), ent.Records...)
		cp.Entries[k] = &ecp
	}
	e.segments[seg.RegionID] = &cp
}

func (e *Engine) rebuildDependenciesLocked() {
	for _, seg := range e.segments {
		for _, ent := range seg.Entries {
			decision := routeWithProject(e.project, ent.QName)
			if decision.Matched {
				ent.DependencyRules = []string{decision.Rule.Number}
				ent.DependencyUpstreams = []int{decision.Rule.UpstreamIndex}
			}
		}
	}
}

// Route exposes deterministic rule adjudication.
func (e *Engine) Route(qname string) RouteDecision {
	return routeWithProject(e.project, qname)
}

// ProcessQuery resolves one query at its declared logical timestamp.
func (e *Engine) ProcessQuery(q spec.Query) Result {
	result := Result{Sequence: q.Sequence, QueryID: q.ID}
	decision := routeWithProject(e.project, q.Name)
	result.Matched = decision.Matched
	result.CandidateRules = decision.CandidateNumbers()
	result.DecisionBasis = decision.Basis
	if !decision.Matched {
		result.Code = codes.ERouteNoMatch
		result.Source = spec.SourceError
		result.ErrorContext = map[string]any{"qname": q.Name, "qtype": q.QType}
		return result
	}
	rule := decision.Rule
	result.RuleNumber = rule.Number
	result.RuleOrdinal = rule.Ordinal
	result.UpstreamIndex = rule.UpstreamIndex
	result.RegionID = rule.RegionID

	e.mu.Lock()
	seg := e.segmentFor(rule.RegionID)
	result.SegmentRevision = seg.Revision
	entry := seg.Entries[spec.Key(q.Name, q.QType)]
	if entry != nil {
		if entry.Negative {
			remaining := fixedint.Remaining(entry.NegativeExpiresAtMS, q.AtMS)
			result.EntryRemainingMS = remaining.Value
			if remaining.Value > 0 {
				e.mu.Unlock()
				result.Code = "OK_NEGATIVE"
				result.Negative = true
				result.Source = spec.SourceNegativeSeed
				result.ExpiryBasis = "negative_expires_at_ms>logical_now_ms"
				return result
			}
			delete(seg.Entries, entry.Key)
			result.ExpiryBasis = "negative_expires_at_ms<=logical_now_ms"
		} else if fresh, basis, minRemaining, remainings := freshPositive(entry.Records, q.AtMS); fresh {
			e.mu.Unlock()
			result.Code = "OK"
			result.Source = spec.SourcePositiveSeed
			result.Records = spec.SortedRecords(entry.Records)
			result.RemainingTTLMS = remainings
			result.EntryRemainingMS = minRemaining
			result.ExpiryBasis = basis
			return result
		} else {
			delete(seg.Entries, entry.Key)
			result.ExpiryBasis = basis
		}
	}
	hadExpired := entry != nil
	e.mu.Unlock()

	hop, bizErr := e.forward(rule.UpstreamIndex)
	result.HopPath = hop.Path
	result.HopCount = len(hop.Path)
	result.VirtualElapsedMS = hop.VirtualElapsedMS
	if bizErr != nil {
		result.Code = bizErr.Code
		result.Source = spec.SourceError
		result.ErrorContext = bizErr.Context
		if result.ErrorContext == nil {
			result.ErrorContext = map[string]any{}
		}
		result.ErrorContext["field"] = bizErr.Field
		return result
	}

	answer, ok := e.answers[spec.Key(q.Name, q.QType)]
	if !ok {
		biz := codes.New(codes.EAnswerNotDeclared, "no deterministic local answer set is declared for query").
			WithContext("qname", q.Name).
			WithContext("qtype", q.QType).
			WithContext("rule_number", rule.Number)
		result.Code = biz.Code
		result.Source = spec.SourceError
		result.ErrorContext = biz.Context
		return result
	}

	records := make([]spec.Record, 0, len(answer.Records))
	remaining := make([]int64, 0, len(answer.Records))
	var saturated bool
	for _, template := range answer.Records {
		exp := fixedint.Add64(q.AtMS, template.TTLMS)
		saturated = saturated || exp.Saturated
		rec := template
		rec.ExpiresAtMS = exp.Value
		records = append(records, rec)
		remaining = append(remaining, template.TTLMS)
	}
	records = spec.SortedRecords(records)
	remaining = remainingForSorted(records, q.AtMS)

	e.mu.Lock()
	seg = e.segmentFor(rule.RegionID)
	seg.Entries[answer.CacheKey] = &CacheEntry{
		Key: answer.CacheKey, QName: answer.QName, QType: answer.QType,
		RegionID: rule.RegionID, Records: records,
		CreatedAtMS: q.AtMS, Source: spec.SourceUpstream,
		DependencyRules: []string{rule.Number}, DependencyUpstreams: []int{rule.UpstreamIndex},
	}
	result.SegmentRevision = seg.Revision
	e.mu.Unlock()

	result.Code = "OK"
	result.Records = records
	result.RemainingTTLMS = remaining
	result.EntryRemainingMS = minInt64(remaining)
	result.Saturated = saturated
	if hadExpired {
		result.Source = spec.SourcePositiveRefresh
		if result.ExpiryBasis == "" {
			result.ExpiryBasis = "expires_at_ms<=logical_now_ms"
		}
		result.ExpiryBasis += "; refresh ttl_ms starts at logical_now_ms"
	} else {
		result.Source = spec.SourceUpstream
		result.ExpiryBasis = "cache_miss; expires_at_ms=logical_now_ms+ttl_ms"
	}
	return result
}

func freshPositive(records []spec.Record, nowMS int64) (bool, string, int64, []int64) {
	if len(records) == 0 {
		return false, "empty positive entry requires refresh", 0, nil
	}
	sorted := spec.SortedRecords(records)
	remainings := remainingForSorted(sorted, nowMS)
	min := minInt64(remainings)
	if min <= 0 {
		return false, "min(record.expires_at_ms)<=logical_now_ms", min, remainings
	}
	return true, "all record expires_at_ms>logical_now_ms", min, remainings
}

func remainingForSorted(records []spec.Record, nowMS int64) []int64 {
	out := make([]int64, 0, len(records))
	for _, r := range records {
		out = append(out, fixedint.Remaining(r.ExpiresAtMS, nowMS).Value)
	}
	return out
}

func minInt64(xs []int64) int64 {
	if len(xs) == 0 {
		return 0
	}
	m := xs[0]
	for _, x := range xs[1:] {
		if x < m {
			m = x
		}
	}
	return m
}

// SortedSegmentIDs returns stable segment ordering for reports.
func (e *Engine) SortedSegmentIDs() []string {
	e.mu.Lock()
	defer e.mu.Unlock()
	ids := make([]string, 0, len(e.segments))
	for id := range e.segments {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}
