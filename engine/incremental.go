package engine

import (
	"sort"

	"dnslab/spec"
)

// InvalidationSource identifies one edit and the regions it directly touches.
type InvalidationSource struct {
	Kind    string   `json:"kind"`
	Ref     string   `json:"ref"`
	Field   string   `json:"field,omitempty"`
	Before  any      `json:"before,omitempty"`
	After   any      `json:"after,omitempty"`
	Reason  string   `json:"reason,omitempty"`
	Regions []string `json:"regions,omitempty"`
}

// QueryImpact explains the invalidation decision for one query sample.
type QueryImpact struct {
	Sequence           int      `json:"sequence"`
	QueryID            string   `json:"query_id"`
	RuleNumber         string   `json:"rule_number,omitempty"`
	RegionID           string   `json:"region_id,omitempty"`
	Action             string   `json:"action"`
	Invalidated        bool     `json:"invalidated"`
	Sources            []string `json:"sources,omitempty"`
	PropagationClosure []string `json:"propagation_closure,omitempty"`
	RevisionBefore     int64    `json:"revision_before,omitempty"`
	RevisionAfter      int64    `json:"revision_after,omitempty"`
}

// InvalidationReport is the auditable result of an incremental reconcile.
type InvalidationReport struct {
	FromVersion        string               `json:"from_version"`
	ToVersion          string               `json:"to_version"`
	SchemaBefore       string               `json:"schema_before,omitempty"`
	SchemaAfter        string               `json:"schema_after,omitempty"`
	SchemaOnly         bool                 `json:"schema_only"`
	SemanticChange     bool                 `json:"semantic_change"`
	Sources            []InvalidationSource `json:"sources"`
	DirectRegions      []string             `json:"direct_regions"`
	PropagationClosure []string             `json:"propagation_closure"`
	RebuiltRegions     []string             `json:"rebuilt_regions"`
	ReusedRegions      []string             `json:"reused_regions"`
	BeforeRevisions    map[string]int64     `json:"before_revisions"`
	AfterRevisions     map[string]int64     `json:"after_revisions"`
	Queries            []QueryImpact        `json:"queries,omitempty"`
}

// Reconcile preserves segments outside the invalidation closure and rebuilds
// segments inside it from the new canonical project.
func Reconcile(old *Engine, next *spec.Project, schemaBefore, schemaAfter string, sources []InvalidationSource) (*Engine, *InvalidationReport) {
	oldProject := old.Project()
	before := old.SegmentSnapshots()
	if sources == nil {
		sources = DiffProjects(oldProject, next)
	}

	report := &InvalidationReport{
		FromVersion: oldProject.Version, ToVersion: next.Version,
		SchemaBefore: schemaBefore, SchemaAfter: schemaAfter,
		Sources: sources, BeforeRevisions: map[string]int64{}, AfterRevisions: map[string]int64{},
	}
	for id, seg := range before {
		report.BeforeRevisions[id] = seg.Revision
	}
	if oldProject.Fingerprint == next.Fingerprint {
		report.SchemaOnly = schemaBefore != schemaAfter
		report.SemanticChange = false
	} else {
		report.SemanticChange = true
	}

	directSet := map[string]bool{}
	for _, src := range sources {
		for _, r := range src.Regions {
			directSet[r] = true
		}
	}
	report.DirectRegions = sortedKeys(directSet)

	edges := dependencyEdges(oldProject, before)
	closureSet := reachable(directSet, edges)
	for r := range directSet {
		closureSet[r] = true
	}
	report.PropagationClosure = sortedKeys(closureSet)

	rebuiltSet := map[string]bool{}
	reusedSet := map[string]bool{}
	newEngine := NewEngine(next)

	for id, oldSeg := range before {
		if closureSet[id] {
			if _, ok := newEngine.segments[id]; !ok {
				newEngine.segments[id] = &Segment{
					RegionID: id, Revision: oldSeg.Revision + 1,
					BuiltAtMS: next.ReferenceNowMS, Entries: map[string]*CacheEntry{},
				}
			} else {
				newEngine.segments[id].Revision = oldSeg.Revision + 1
			}
			rebuiltSet[id] = true
		} else {
			newEngine.RestoreSegment(oldSeg)
			reusedSet[id] = true
		}
	}
	for id := range newEngine.segments {
		if closureSet[id] {
			rebuiltSet[id] = true
		}
	}

	report.RebuiltRegions = sortedKeys(rebuiltSet)
	report.ReusedRegions = sortedKeys(reusedSet)
	for id, seg := range newEngine.SegmentSnapshots() {
		report.AfterRevisions[id] = seg.Revision
	}
	return newEngine, report
}

// AnnotateQueries fills per-query invalidation basis using the reconciled engine.
func (r *InvalidationReport) AnnotateQueries(e *Engine, queries []spec.Query) {
	r.Queries = make([]QueryImpact, 0, len(queries))
	closure := map[string]bool{}
	for _, id := range r.PropagationClosure {
		closure[id] = true
	}
	sourceRefs := make([]string, 0, len(r.Sources))
	for _, s := range r.Sources {
		ref := s.Kind
		if s.Ref != "" {
			ref += ":" + s.Ref
		}
		if s.Field != "" {
			ref += "." + s.Field
		}
		sourceRefs = append(sourceRefs, ref)
	}
	for _, q := range queries {
		d := e.Route(q.Name)
		impact := QueryImpact{
			Sequence: q.Sequence, QueryID: q.ID,
			PropagationClosure: append([]string{}, r.PropagationClosure...),
			Sources:            []string{},
		}
		if d.Matched {
			impact.RuleNumber = d.Rule.Number
			impact.RegionID = d.Rule.RegionID
			impact.RevisionBefore = r.BeforeRevisions[d.Rule.RegionID]
			impact.RevisionAfter = r.AfterRevisions[d.Rule.RegionID]
			if closure[d.Rule.RegionID] {
				impact.Action = "rebuilt"
				impact.Invalidated = true
				impact.Sources = append(impact.Sources, sourceRefs...)
			} else if impact.RevisionBefore != 0 {
				impact.Action = "reused"
			} else {
				impact.Action = "new_segment"
			}
		} else {
			impact.Action = "no_route"
		}
		r.Queries = append(r.Queries, impact)
	}
}

func dependencyEdges(p *spec.Project, segments map[string]Segment) map[string]map[string]bool {
	edges := map[string]map[string]bool{}
	add := func(from, to string) {
		if from == "" || to == "" {
			return
		}
		if edges[from] == nil {
			edges[from] = map[string]bool{}
		}
		edges[from][to] = true
	}
	for segmentID, seg := range segments {
		for _, ent := range seg.Entries {
			qname := ent.QName
			if qname == "" {
				qname = ent.Key
			}
			decision := routeWithProject(p, qname)
			if !decision.Matched {
				continue
			}
			for _, rule := range decision.Candidates {
				add(rule.RegionID, segmentID)
			}
		}
		// The winning segment itself remains connected even before entries exist.
		add(segmentID, segmentID)
	}
	return edges
}

func reachable(starts map[string]bool, edges map[string]map[string]bool) map[string]bool {
	seen := map[string]bool{}
	queue := make([]string, 0, len(starts))
	for k := range starts {
		if !seen[k] {
			seen[k] = true
			queue = append(queue, k)
		}
	}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for next := range edges[cur] {
			if !seen[next] {
				seen[next] = true
				queue = append(queue, next)
			}
		}
	}
	return seen
}

func sortedKeys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// DiffProjects identifies semantic differences and directly touched regions.
func DiffProjects(before, after *spec.Project) []InvalidationSource {
	var sources []InvalidationSource
	beforeRules := map[string]spec.Rule{}
	afterRules := map[string]spec.Rule{}
	for _, r := range before.Rules {
		beforeRules[r.Number] = r
	}
	for _, r := range after.Rules {
		afterRules[r.Number] = r
	}
	for _, nr := range after.Rules {
		br, ok := beforeRules[nr.Number]
		if !ok {
			sources = append(sources, InvalidationSource{
				Kind: "rule", Ref: nr.Number, Field: "added", After: nr,
				Regions: []string{nr.RegionID}, Reason: "rule added",
			})
			continue
		}
		var fields []struct {
			name string
			b, a any
		}
		if br.Suffix != nr.Suffix {
			fields = append(fields, struct {
				name string
				b, a any
			}{"suffix", br.Suffix, nr.Suffix})
		}
		if br.Priority != nr.Priority {
			fields = append(fields, struct {
				name string
				b, a any
			}{"priority", br.Priority, nr.Priority})
		}
		if br.UpstreamIndex != nr.UpstreamIndex {
			fields = append(fields, struct {
				name string
				b, a any
			}{"upstream_index", br.UpstreamIndex, nr.UpstreamIndex})
		}
		if br.RegionID != nr.RegionID {
			fields = append(fields, struct {
				name string
				b, a any
			}{"region_id", br.RegionID, nr.RegionID})
		}
		for _, f := range fields {
			regions := []string{br.RegionID, nr.RegionID}
			sources = append(sources, InvalidationSource{
				Kind: "rule", Ref: nr.Number, Field: f.name, Before: f.b, After: f.a,
				Regions: uniqueStrings(regions), Reason: "rule declaration changed",
			})
		}
	}
	for _, br := range before.Rules {
		if _, ok := afterRules[br.Number]; !ok {
			sources = append(sources, InvalidationSource{
				Kind: "rule", Ref: br.Number, Field: "removed", Before: br,
				Regions: []string{br.RegionID}, Reason: "rule removed",
			})
		}
	}

	beforeUp := map[int]spec.Upstream{}
	for _, u := range before.Upstreams {
		beforeUp[u.Index] = u
	}
	for _, ua := range after.Upstreams {
		ub, ok := beforeUp[ua.Index]
		if !ok || ub.Literal != ua.Literal || ub.TimeoutMS != ua.TimeoutMS ||
			ub.LatencyMS != ua.LatencyMS || ub.ForwardTo != ua.ForwardTo {
			regions := referencingRegions(after, ua.Index)
			regions = append(regions, referencingRegions(before, ua.Index)...)
			sources = append(sources, InvalidationSource{
				Kind: "upstream", Ref: "U-" + itoaInt(ua.Index), Field: "declaration",
				Before: ub, After: ua, Regions: uniqueStrings(regions),
				Reason: "upstream list entry changed",
			})
		}
	}

	if before.NegativeCacheEnabled != after.NegativeCacheEnabled ||
		before.NegativeDefaultTTLMS != after.NegativeDefaultTTLMS {
		regions := negativeRegions(before)
		regions = append(regions, negativeRegions(after)...)
		sources = append(sources, InvalidationSource{
			Kind: "negative_switch", Ref: "negative_cache", Field: "enabled_or_ttl",
			Before:  map[string]any{"enabled": before.NegativeCacheEnabled, "ttl_ms": before.NegativeDefaultTTLMS},
			After:   map[string]any{"enabled": after.NegativeCacheEnabled, "ttl_ms": after.NegativeDefaultTTLMS},
			Regions: uniqueStrings(regions), Reason: "negative cache policy changed",
		})
	}

	if before.Fingerprint != after.Fingerprint && len(sources) == 0 {
		all := make([]string, 0, len(after.Regions))
		for _, r := range after.Regions {
			all = append(all, r.ID)
		}
		sources = append(sources, InvalidationSource{
			Kind: "snapshot_replace", Ref: after.Version, Field: "canonical_project",
			Before: before.Version, After: after.Version, Regions: all,
			Reason: "whole snapshot changed without a more specific structural diff",
		})
	}
	return sources
}

func referencingRegions(p *spec.Project, upstreamIndex int) []string {
	var out []string
	for _, r := range p.Rules {
		if r.UpstreamIndex == upstreamIndex {
			out = append(out, r.RegionID)
		}
	}
	return out
}

func negativeRegions(p *spec.Project) []string {
	set := map[string]bool{}
	for _, s := range p.CacheSeeds {
		if s.Negative {
			set[s.RegionID] = true
		}
	}
	return sortedKeys(set)
}

func uniqueStrings(in []string) []string {
	set := map[string]bool{}
	for _, s := range in {
		if s != "" {
			set[s] = true
		}
	}
	return sortedKeys(set)
}

func itoaInt(i int) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var b [20]byte
	n := len(b)
	for i > 0 {
		n--
		b[n] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		n--
		b[n] = '-'
	}
	return string(b[n:])
}
