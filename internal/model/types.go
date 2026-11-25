package model

import (
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

const (
	SchemaV1 = "v1"
	SchemaV2 = "v2"
	SchemaV3 = "v3"

	AttrInt    = "int"
	AttrFixed  = "fixed"
	AttrString = "string"

	QueryOneHop       = "one_hop"
	QueryTwoHop       = "two_hop"
	QueryShortestPath = "shortest_path"

	OutcomeOK                = "ok"
	OutcomeStartMissing      = "start_missing"
	OutcomeDepthZero         = "depth_zero"
	OutcomeTargetUnreachable = "target_unreachable"

	OpVertexAdd    = "vertex_add"
	OpVertexUpdate = "vertex_update"
	OpVertexDelete = "vertex_delete"
	OpEdgeAdd      = "edge_add"
	OpEdgeUpdate   = "edge_update"
	OpEdgeDelete   = "edge_delete"
	OpReshard      = "reshard"
)

// AttrValue stores all fixed-point attribute values in canonical millionths.
// It is intentionally lossless for the generator's declared scales (1/1000 and
// 1/10000) and avoids floating-point comparison in every later stage.
type AttrValue struct {
	Kind        string `json:"kind"`
	Int         int64  `json:"int,omitempty"`
	FixedMicros int64  `json:"fixed_micros,omitempty"`
	String      string `json:"string,omitempty"`
}

func (v AttrValue) Canonical() string {
	switch v.Kind {
	case AttrInt:
		return "i:" + strconv.FormatInt(v.Int, 10)
	case AttrFixed:
		return "f:" + strconv.FormatInt(v.FixedMicros, 10)
	case AttrString:
		return "s:" + v.String
	default:
		return "?:" + v.Kind
	}
}

type Vertex struct {
	ID        string               `json:"id"`
	ShardID   string               `json:"shard_id"`
	DeclOrder int                  `json:"decl_order"`
	Labels    []string             `json:"labels"`
	Attrs     map[string]AttrValue `json:"attrs"`
}

type Edge struct {
	ID        string   `json:"id"`
	From      string   `json:"from"`
	To        string   `json:"to"`
	Directed  bool     `json:"directed"`
	Weight    int64    `json:"weight"`
	Levels    []string `json:"levels"`
	ShardID   string   `json:"shard_id"`
	DeclOrder int      `json:"decl_order"`
}

type Shard struct {
	ID   string `json:"id"`
	Low  string `json:"low"`
	High string `json:"high"`
}

type IndexRule struct {
	Attribute string `json:"attribute"`
	Kind      string `json:"kind"`
	Bucket    int64  `json:"bucket,omitempty"`
	PrefixLen int    `json:"prefix_len,omitempty"`
}

type Query struct {
	ID       string   `json:"id"`
	Kind     string   `json:"kind"`
	Start    string   `json:"start"`
	Labels   []string `json:"labels,omitempty"`
	MaxDepth int      `json:"max_depth"`
	Target   string   `json:"target,omitempty"`
}

func (q Query) Canonical() string {
	labels := append([]string(nil), q.Labels...)
	sort.Strings(labels)
	return strings.Join([]string{q.ID, q.Kind, q.Start, strings.Join(labels, ","),
		strconv.Itoa(q.MaxDepth), q.Target}, "|")
}

// CacheID is a stable function of the logical query rather than of thread
// scheduling, shard order, or build batches.
func (q Query) CacheID() string {
	sum := sha1.Sum([]byte(q.Canonical()))
	return "C-" + hex.EncodeToString(sum[:])[:12]
}

type Operation struct {
	Seq      int     `json:"seq"`
	Kind     string  `json:"kind"`
	VertexID string  `json:"vertex_id,omitempty"`
	EdgeID   string  `json:"edge_id,omitempty"`
	Vertex   *Vertex `json:"vertex,omitempty"`
	Edge     *Edge   `json:"edge,omitempty"`
	Shards   []Shard `json:"shards,omitempty"`
	Note     string  `json:"note,omitempty"`
}

type Project struct {
	ID                string      `json:"id"`
	Version           int         `json:"version"`
	Seed              int64       `json:"seed"`
	SchemaVersion     string      `json:"schema_version,omitempty"`
	HierarchyEnabled  bool        `json:"hierarchy_enabled"`
	HierarchyDegraded bool        `json:"hierarchy_degraded,omitempty"`
	DegradationNotes  []string    `json:"degradation_notes,omitempty"`
	ClockUnit         string      `json:"clock_unit"`
	CapacityUnit      string      `json:"capacity_unit"`
	ObservedAtMillis  int64       `json:"observed_at_ms"`
	CacheCapacityByte int64       `json:"cache_capacity_bytes"`
	IndexRules        []IndexRule `json:"index_rules"`
	Shards            []Shard     `json:"shards"`
	Vertices          []Vertex    `json:"vertices"`
	Edges             []Edge      `json:"edges"`
	Queries           []Query     `json:"queries"`
	Operations        []Operation `json:"operations"`
}

type QueryResult struct {
	QueryID         string   `json:"query_id"`
	Status          string   `json:"status"`
	VisitedVertices []string `json:"visited_vertices"`
	VisitedEdges    []string `json:"visited_edges"`
	ResultVertices  []string `json:"result_vertices"`
	PathVertices    []string `json:"path_vertices,omitempty"`
	PathEdges       []string `json:"path_edges,omitempty"`
	IndexKeys       []string `json:"index_keys"`
	Dependencies    []string `json:"dependencies"`
	CacheID         string   `json:"cache_id"`
	ProjectVersion  int      `json:"project_version"`
}

type ShardTruth struct {
	VertexOrder []string `json:"vertex_order"`
	EdgeOrder   []string `json:"edge_order"`
	IndexKeys   []string `json:"index_keys"`
}

type QueryTruth struct {
	Status          string   `json:"status"`
	VisitedVertices []string `json:"visited_vertices"`
	VisitedEdges    []string `json:"visited_edges"`
	ResultVertices  []string `json:"result_vertices"`
	PathVertices    []string `json:"path_vertices,omitempty"`
	PathEdges       []string `json:"path_edges,omitempty"`
}

type OperationTruth struct {
	Seq                 int                   `json:"seq"`
	Kind                string                `json:"kind"`
	ProjectVersion      int                   `json:"project_version"`
	AffectedShards      []string              `json:"affected_shards"`
	ChangedEntities     []string              `json:"changed_entities"`
	AffectedIndexKeys   []string              `json:"affected_index_keys"`
	InvalidatedCacheIDs []string              `json:"invalidated_cache_ids"`
	ReusedCacheIDs      []string              `json:"reused_cache_ids"`
	Queries             map[string]QueryTruth `json:"queries"`
}

type Truth struct {
	Seed           int64                 `json:"seed"`
	ProjectID      string                `json:"project_id"`
	InitialVersion int                   `json:"initial_version"`
	CoreDigest     string                `json:"core_digest"`
	Shards         map[string]ShardTruth `json:"shards"`
	Queries        map[string]QueryTruth `json:"queries"`
	Operations     []OperationTruth      `json:"operations"`
}

func CloneProject(p *Project) *Project {
	if p == nil {
		return nil
	}
	b, _ := json.Marshal(p)
	var out Project
	_ = json.Unmarshal(b, &out)
	return &out
}

func CloneVertex(v Vertex) Vertex {
	out := v
	out.Labels = append([]string(nil), v.Labels...)
	out.Attrs = make(map[string]AttrValue, len(v.Attrs))
	for k, val := range v.Attrs {
		out.Attrs[k] = val
	}
	return out
}

func CloneEdge(e Edge) Edge {
	out := e
	out.Levels = append([]string(nil), e.Levels...)
	return out
}

func SortStrings(values []string) {
	sort.Strings(values)
}

func SortProject(p *Project) {
	sort.Slice(p.Shards, func(i, j int) bool { return p.Shards[i].ID < p.Shards[j].ID })
	sort.Slice(p.Vertices, func(i, j int) bool {
		if p.Vertices[i].DeclOrder == p.Vertices[j].DeclOrder {
			return p.Vertices[i].ID < p.Vertices[j].ID
		}
		return p.Vertices[i].DeclOrder < p.Vertices[j].DeclOrder
	})
	sort.Slice(p.Edges, func(i, j int) bool {
		if p.Edges[i].DeclOrder == p.Edges[j].DeclOrder {
			return p.Edges[i].ID < p.Edges[j].ID
		}
		return p.Edges[i].DeclOrder < p.Edges[j].DeclOrder
	})
	sort.Slice(p.Queries, func(i, j int) bool { return p.Queries[i].ID < p.Queries[j].ID })
	for i := range p.Vertices {
		v := &p.Vertices[i]
		sort.Strings(v.Labels)
		v.Labels = uniqueStrings(v.Labels)
	}
	for i := range p.Edges {
		sort.Strings(p.Edges[i].Levels)
		p.Edges[i].Levels = uniqueStrings(p.Edges[i].Levels)
	}
	sort.Slice(p.IndexRules, func(i, j int) bool {
		if p.IndexRules[i].Attribute == p.IndexRules[j].Attribute {
			return p.IndexRules[i].Kind < p.IndexRules[j].Kind
		}
		return p.IndexRules[i].Attribute < p.IndexRules[j].Attribute
	})
}

func uniqueStrings(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	out := in[:0]
	var prev string
	for i, s := range in {
		if i == 0 || s != prev {
			out = append(out, s)
			prev = s
		}
	}
	return out
}

func ProjectByID(p *Project) map[string]Vertex {
	m := make(map[string]Vertex, len(p.Vertices))
	for _, v := range p.Vertices {
		m[v.ID] = v
	}
	return m
}

func ShardIDs(p *Project) []string {
	ids := make([]string, len(p.Shards))
	for i := range p.Shards {
		ids[i] = p.Shards[i].ID
	}
	sort.Strings(ids)
	return ids
}

// CoreDigest intentionally excludes schema vocabulary and optional hierarchy
// levels when hierarchy has been declared disabled. It covers every field that
// must survive translation between the three schema versions.
func CoreDigest(p *Project) string {
	type coreVertex struct {
		ID, ShardID string
		DeclOrder   int
		Labels      []string
		Attrs       map[string]AttrValue
	}
	type coreEdge struct {
		ID, From, To, ShardID string
		Directed              bool
		Weight                int64
		DeclOrder             int
		Levels                []string
	}
	type coreProject struct {
		ID               string
		Version          int
		Seed             int64
		ObservedAtMillis int64
		Capacity         int64
		IndexRules       []IndexRule
		Shards           []Shard
		Vertices         []coreVertex
		Edges            []coreEdge
	}
	cp := coreProject{
		ID: p.ID, Version: p.Version, Seed: p.Seed,
		ObservedAtMillis: p.ObservedAtMillis,
		Capacity:         p.CacheCapacityByte,
		IndexRules:       append([]IndexRule(nil), p.IndexRules...),
		Shards:           append([]Shard(nil), p.Shards...),
	}
	sort.Slice(cp.Shards, func(i, j int) bool { return cp.Shards[i].ID < cp.Shards[j].ID })
	for _, v := range p.Vertices {
		cp.Vertices = append(cp.Vertices, coreVertex{
			ID: v.ID, ShardID: v.ShardID, DeclOrder: v.DeclOrder,
			Labels: append([]string(nil), v.Labels...),
			Attrs:  v.Attrs,
		})
	}
	for _, e := range p.Edges {
		ce := coreEdge{
			ID: e.ID, From: e.From, To: e.To, ShardID: e.ShardID,
			Directed: e.Directed, Weight: e.Weight, DeclOrder: e.DeclOrder,
		}
		// Optional hierarchy levels are deliberately outside the core digest:
		// one schema version may omit the optional field by declaration.
		cp.Edges = append(cp.Edges, ce)
	}
	b, _ := json.Marshal(cp)
	sum := sha1.Sum(b)
	return hex.EncodeToString(sum[:])
}

func CompareCore(a, b *Project) []string {
	var diffs []string
	if a.ID != b.ID {
		diffs = append(diffs, fmt.Sprintf("project id: %q != %q", a.ID, b.ID))
	}
	if a.Version != b.Version {
		diffs = append(diffs, fmt.Sprintf("project version: %d != %d", a.Version, b.Version))
	}
	if a.Seed != b.Seed {
		diffs = append(diffs, fmt.Sprintf("seed: %d != %d", a.Seed, b.Seed))
	}
	if a.ObservedAtMillis != b.ObservedAtMillis {
		diffs = append(diffs, fmt.Sprintf("observed time: %d != %d", a.ObservedAtMillis, b.ObservedAtMillis))
	}
	if a.CacheCapacityByte != b.CacheCapacityByte {
		diffs = append(diffs, fmt.Sprintf("capacity: %d != %d", a.CacheCapacityByte, b.CacheCapacityByte))
	}
	if len(a.Shards) != len(b.Shards) {
		diffs = append(diffs, "shard count differs")
	}
	for i := 0; i < len(a.Shards) && i < len(b.Shards); i++ {
		if a.Shards[i] != b.Shards[i] {
			diffs = append(diffs, fmt.Sprintf("shard %d differs", i))
		}
	}
	if len(a.Vertices) != len(b.Vertices) {
		diffs = append(diffs, "vertex count differs")
	}
	for i := 0; i < len(a.Vertices) && i < len(b.Vertices); i++ {
		av, bv := a.Vertices[i], b.Vertices[i]
		if av.ID != bv.ID || av.ShardID != bv.ShardID || av.DeclOrder != bv.DeclOrder ||
			!equalStringSet(av.Labels, bv.Labels) {
			diffs = append(diffs, "vertex declaration differs at "+av.ID)
			continue
		}
		if len(av.Attrs) != len(bv.Attrs) {
			diffs = append(diffs, "vertex attrs differ at "+av.ID)
			continue
		}
		for k, v := range av.Attrs {
			if bv.Attrs[k] != v {
				diffs = append(diffs, "vertex attr "+k+" differs at "+av.ID)
			}
		}
	}
	if len(a.Edges) != len(b.Edges) {
		diffs = append(diffs, "edge count differs")
	}
	for i := 0; i < len(a.Edges) && i < len(b.Edges); i++ {
		ae, be := a.Edges[i], b.Edges[i]
		if ae.ID != be.ID || ae.From != be.From || ae.To != be.To ||
			ae.Directed != be.Directed || ae.Weight != be.Weight ||
			ae.ShardID != be.ShardID || ae.DeclOrder != be.DeclOrder {
			diffs = append(diffs, "edge declaration differs at "+ae.ID)
			continue
		}
		if a.HierarchyEnabled && b.HierarchyEnabled && !equalStringSet(ae.Levels, be.Levels) {
			diffs = append(diffs, "edge hierarchy differs at "+ae.ID)
		}
	}
	if !equalIndexRules(a.IndexRules, b.IndexRules) {
		diffs = append(diffs, "index rules differ")
	}
	if len(a.Queries) != len(b.Queries) {
		diffs = append(diffs, "query count differs")
	}
	for i := 0; i < len(a.Queries) && i < len(b.Queries); i++ {
		if a.Queries[i].Canonical() != b.Queries[i].Canonical() {
			diffs = append(diffs, "query differs at "+a.Queries[i].ID)
		}
	}
	if len(a.Operations) != len(b.Operations) {
		diffs = append(diffs, "operation count differs")
	}
	sharedHierarchy := a.HierarchyEnabled && b.HierarchyEnabled
	for i := 0; i < len(a.Operations) && i < len(b.Operations); i++ {
		ab, _ := json.Marshal(canonicalOperation(a.Operations[i], sharedHierarchy))
		bb, _ := json.Marshal(canonicalOperation(b.Operations[i], sharedHierarchy))
		if string(ab) != string(bb) {
			diffs = append(diffs, fmt.Sprintf("operation differs at sequence %d", a.Operations[i].Seq))
		}
	}
	return diffs
}

func canonicalOperation(op Operation, includeHierarchy bool) Operation {
	cp := op
	if op.Vertex != nil {
		v := CloneVertex(*op.Vertex)
		cp.Vertex = &v
	}
	if op.Edge != nil {
		e := CloneEdge(*op.Edge)
		if !includeHierarchy {
			e.Levels = nil
		}
		cp.Edge = &e
	}
	cp.Shards = append([]Shard(nil), op.Shards...)
	return cp
}

func equalStringSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	aa := append([]string(nil), a...)
	bb := append([]string(nil), b...)
	sort.Strings(aa)
	sort.Strings(bb)
	for i := range aa {
		if aa[i] != bb[i] {
			return false
		}
	}
	return true
}

func equalIndexRules(a, b []IndexRule) bool {
	if len(a) != len(b) {
		return false
	}
	aa := append([]IndexRule(nil), a...)
	bb := append([]IndexRule(nil), b...)
	sort.Slice(aa, func(i, j int) bool { return aa[i].Attribute+aa[i].Kind < aa[j].Attribute+aa[j].Kind })
	sort.Slice(bb, func(i, j int) bool { return bb[i].Attribute+bb[i].Kind < bb[j].Attribute+bb[j].Kind })
	for i := range aa {
		if aa[i] != bb[i] {
			return false
		}
	}
	return true
}
