package loader

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"

	"graphlab/internal/model"
)

type Bundle struct {
	Projects    map[string]*model.Project
	CoreDigest  string
	Consistency []string
}

type rangeV1 struct {
	ID   string `json:"id"`
	Low  string `json:"start_key"`
	High string `json:"end_key"`
}

type rangeV2 struct {
	ID   string `json:"shardId"`
	Low  string `json:"low"`
	High string `json:"high"`
}

type rangeV3 struct {
	ID   string `json:"partition"`
	Low  string `json:"begin"`
	High string `json:"end"`
}

type clockRaw struct {
	Unit          string `json:"unit"`
	Value         int64  `json:"value"`
	MillisPerTick int64  `json:"millis_per_tick,omitempty"`
	TickMillis    int64  `json:"tick_ms,omitempty"`
}

type capacityRaw struct {
	Unit  string `json:"unit"`
	Value int64  `json:"value"`
}

type attrV1 struct {
	Kind  string          `json:"kind"`
	Scale int64           `json:"scale,omitempty"`
	Value json.RawMessage `json:"value"`
}

type attrV2 struct {
	Type  string          `json:"type"`
	Scale int64           `json:"scale,omitempty"`
	Raw   json.RawMessage `json:"raw"`
}

type attrV3 struct {
	Type  string          `json:"t"`
	Scale int64           `json:"scale,omitempty"`
	Raw   json.RawMessage `json:"n"`
}

type vertexV1 struct {
	ID        string            `json:"vertex_id"`
	DeclOrder int               `json:"decl_index"`
	Labels    []string          `json:"labels"`
	Attrs     map[string]attrV1 `json:"attrs"`
}

type edgeV1 struct {
	ID        string   `json:"edge_id"`
	From      string   `json:"source_vertex"`
	To        string   `json:"target_vertex"`
	Directed  *bool    `json:"directed"`
	Weight    int64    `json:"weight"`
	Levels    []string `json:"level_labels,omitempty"`
	DeclOrder int      `json:"decl_index"`
}

type vertexV2 struct {
	ID        string            `json:"vertexId"`
	DeclOrder int               `json:"declIndex"`
	Labels    []string          `json:"tags"`
	Attrs     map[string]attrV2 `json:"attributes"`
}

type edgeV2 struct {
	ID        string   `json:"edgeId"`
	From      string   `json:"from"`
	To        string   `json:"to"`
	Directed  *bool    `json:"directed"`
	Weight    int64    `json:"weight"`
	Levels    []string `json:"hierarchy,omitempty"`
	DeclOrder int      `json:"declIndex"`
}

type vertexV3 struct {
	ID        string            `json:"node_key"`
	DeclOrder int               `json:"decl_order"`
	Labels    []string          `json:"tags"`
	Attrs     map[string]attrV3 `json:"props"`
}

type edgeV3 struct {
	ID        string `json:"link_id"`
	From      string `json:"from_key"`
	To        string `json:"to_key"`
	Directed  *bool  `json:"directed"`
	Weight    int64  `json:"weight"`
	DeclOrder int    `json:"order"`
}

type ruleV1 struct {
	Attribute string `json:"attribute"`
	Kind      string `json:"kind"`
	Bucket    int64  `json:"bucket,omitempty"`
	PrefixLen int    `json:"prefix_len,omitempty"`
}

type ruleV2 struct {
	Attribute string `json:"attribute"`
	Kind      string `json:"type"`
	Bucket    int64  `json:"range,omitempty"`
	PrefixLen int    `json:"prefixLength,omitempty"`
}

type ruleV3 struct {
	Attribute string `json:"field"`
	Kind      string `json:"class"`
	Bucket    int64  `json:"span,omitempty"`
	PrefixLen int    `json:"prefix"`
}

type docV1 struct {
	Schema           string            `json:"schema"`
	ProjectID        string            `json:"project_id"`
	ProjectVersion   int               `json:"project_version"`
	Seed             int64             `json:"seed"`
	HierarchyEnabled *bool             `json:"hierarchy_enabled"`
	Clock            clockRaw          `json:"clock"`
	Capacity         capacityRaw       `json:"capacity"`
	VertexRanges     []rangeV1         `json:"vertex_ranges"`
	Vertices         []vertexV1        `json:"vertices"`
	Edges            []edgeV1          `json:"edges"`
	IndexRules       []ruleV1          `json:"index_rules"`
	Queries          []model.Query     `json:"queries"`
	Operations       []model.Operation `json:"operations"`
}

type docV2 struct {
	SchemaVersion    string                `json:"schemaVersion"`
	ProjectID        string                `json:"projectId"`
	ProjectVersion   int                   `json:"projectRevision"`
	Seed             int64                 `json:"seed"`
	HierarchyEnabled *bool                 `json:"hierarchyEnabled"`
	Clock            clockRaw              `json:"clock"`
	Capacity         capacityRaw           `json:"capacity"`
	VertexRanges     []rangeV2             `json:"vertexShards"`
	VerticesByLabel  map[string][]vertexV2 `json:"verticesByLabel"`
	EdgesByHierarchy map[string][]edgeV2   `json:"edgesByHierarchy"`
	IndexRules       []ruleV2              `json:"indexRules"`
	Queries          []model.Query         `json:"queries"`
	Operations       []model.Operation     `json:"operations"`
}

type docV3 struct {
	Schema         string            `json:"schema"`
	ProjectID      string            `json:"dataset_id"`
	ProjectVersion int               `json:"revision"`
	Seed           int64             `json:"seed"`
	Clock          clockRaw          `json:"clock"`
	Capacity       capacityRaw       `json:"capacity"`
	Partitions     []rangeV3         `json:"partitions"`
	Nodes          []vertexV3        `json:"nodes"`
	Links          []edgeV3          `json:"links"`
	IndexRules     []ruleV3          `json:"range_rules"`
	Queries        []model.Query     `json:"queries"`
	Operations     []model.Operation `json:"operations"`
}

func decodeEnvelope(data []byte) (map[string]json.RawMessage, error) {
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(data, &envelope); err != nil {
		return nil, model.Wrap(model.EInvalidJSON, "document is not a JSON object", err,
			map[string]any{"path": "$"})
	}
	if envelope == nil {
		return nil, model.Err(model.EInvalidJSON, "document is null", map[string]any{"path": "$"})
	}
	return envelope, nil
}

func normalizeVersionToken(token string) (string, bool) {
	t := strings.ToLower(strings.TrimSpace(token))
	if t == "" {
		return "", false
	}
	for _, v := range []string{model.SchemaV1, model.SchemaV2, model.SchemaV3} {
		if t == v || strings.HasSuffix(t, "-"+v) || strings.HasSuffix(t, "_"+v) ||
			strings.HasSuffix(t, "."+v) {
			return v, true
		}
	}
	switch t {
	case "1", "v1.0", "v1":
		return model.SchemaV1, true
	case "2", "v2.0", "v2":
		return model.SchemaV2, true
	case "3", "v3.0", "v3":
		return model.SchemaV3, true
	}
	return "", false
}

func detectVersion(envelope map[string]json.RawMessage) (string, error) {
	var markerValues []string
	var markerPaths []string
	for _, path := range []string{"schema", "schema_version", "schemaVersion"} {
		raw, ok := envelope[path]
		if !ok {
			continue
		}
		var token string
		if err := json.Unmarshal(raw, &token); err != nil {
			continue
		}
		v, ok := normalizeVersionToken(token)
		if !ok {
			return "", model.Err(model.ESchemaVersion, "unknown schema version token", map[string]any{
				"path": path, "value": token,
			})
		}
		markerValues = append(markerValues, v)
		markerPaths = append(markerPaths, path)
	}
	cues := make(map[string][]string)
	addCue := func(v, path string) {
		cues[v] = append(cues[v], path)
	}
	if _, ok := envelope["verticesByLabel"]; ok {
		addCue(model.SchemaV2, "verticesByLabel")
	}
	if _, ok := envelope["edgesByHierarchy"]; ok {
		addCue(model.SchemaV2, "edgesByHierarchy")
	}
	if _, ok := envelope["nodes"]; ok {
		addCue(model.SchemaV3, "nodes")
	}
	if _, ok := envelope["project_id"]; ok {
		addCue(model.SchemaV1, "project_id")
	}
	if _, ok := envelope["dataset_id"]; ok {
		addCue(model.SchemaV3, "dataset_id")
	}
	if _, ok := envelope["vertex_ranges"]; ok {
		addCue(model.SchemaV1, "vertex_ranges")
	}
	if _, ok := envelope["vertexShards"]; ok {
		addCue(model.SchemaV2, "vertexShards")
	}
	if _, ok := envelope["partitions"]; ok {
		addCue(model.SchemaV3, "partitions")
	}
	var candidates []string
	seen := make(map[string]bool)
	for _, v := range markerValues {
		if !seen[v] {
			seen[v] = true
			candidates = append(candidates, v)
		}
	}
	for v := range cues {
		if !seen[v] {
			seen[v] = true
			candidates = append(candidates, v)
		}
	}
	sort.Strings(candidates)
	if len(candidates) == 0 {
		return "", model.Err(model.ESchemaVersion, "document has no recognizable schema version",
			map[string]any{"path": "$", "expected": []string{model.SchemaV1, model.SchemaV2, model.SchemaV3}})
	}
	if len(candidates) > 1 {
		return "", model.Err(model.EAmbiguousField, "schema version indicators disagree",
			map[string]any{"markers": markerPaths, "candidates": candidates, "field_cues": cues})
	}
	return candidates[0], nil
}

func canonicalRaw(raw json.RawMessage) string {
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return string(raw)
	}
	return buf.String()
}

func checkAliases(envelope map[string]json.RawMessage, label string, paths ...string) error {
	var values []string
	var present []string
	for _, path := range paths {
		raw, ok := envelope[path]
		if !ok || string(raw) == "null" {
			continue
		}
		values = append(values, canonicalRaw(raw))
		present = append(present, path)
	}
	if len(values) <= 1 {
		return nil
	}
	first := values[0]
	for i := 1; i < len(values); i++ {
		if values[i] != first {
			return model.Err(model.EAmbiguousField, "conflicting aliases for "+label,
				map[string]any{"paths": present, "values": values})
		}
	}
	return nil
}

func Load(data []byte) (*model.Project, error) {
	envelope, err := decodeEnvelope(data)
	if err != nil {
		return nil, err
	}
	if err := checkAliases(envelope, "project id", "project_id", "projectId", "dataset_id"); err != nil {
		return nil, err
	}
	if err := checkAliases(envelope, "project revision", "project_version", "projectRevision", "revision"); err != nil {
		return nil, err
	}
	if err := checkAliases(envelope, "vertex list", "vertices", "verticesByLabel", "nodes"); err != nil {
		return nil, err
	}
	if err := checkAliases(envelope, "edge list", "edges", "edgesByHierarchy", "links"); err != nil {
		return nil, err
	}
	if err := checkAliases(envelope, "shard list", "vertex_ranges", "vertexShards", "partitions"); err != nil {
		return nil, err
	}
	version, err := detectVersion(envelope)
	if err != nil {
		return nil, err
	}
	switch version {
	case model.SchemaV1:
		return loadV1(data)
	case model.SchemaV2:
		return loadV2(data)
	case model.SchemaV3:
		return loadV3(data)
	default:
		return nil, model.Err(model.ESchemaVersion, "unsupported schema version",
			map[string]any{"version": version})
	}
}

func LoadBundle(data map[string][]byte) (*Bundle, error) {
	projects := make(map[string]*model.Project, 3)
	for _, v := range []string{model.SchemaV1, model.SchemaV2, model.SchemaV3} {
		doc, ok := data[v]
		if !ok {
			return nil, model.Err(model.EInvalidSchema, "schema document is missing from bundle",
				map[string]any{"schema": v})
		}
		p, err := Load(doc)
		if err != nil {
			return nil, err
		}
		if p.SchemaVersion != v {
			return nil, model.Err(model.EAmbiguousField, "document loaded under an unexpected schema",
				map[string]any{"expected": v, "observed": p.SchemaVersion})
		}
		projects[v] = p
	}
	base := projects[model.SchemaV1]
	var diffs []string
	for _, v := range []string{model.SchemaV2, model.SchemaV3} {
		diffs = append(diffs, prefixDiffs(v, model.CompareCore(base, projects[v]))...)
	}
	if len(diffs) > 0 {
		return nil, model.Err(model.EAmbiguousField, "schema versions do not describe the same core graph",
			map[string]any{"differences": diffs})
	}
	return &Bundle{
		Projects: projects, CoreDigest: model.CoreDigest(base), Consistency: diffs,
	}, nil
}

func prefixDiffs(version string, diffs []string) []string {
	out := make([]string, 0, len(diffs))
	for _, d := range diffs {
		out = append(out, version+": "+d)
	}
	return out
}

func loadV1(data []byte) (*model.Project, error) {
	var doc docV1
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, model.Wrap(model.EInvalidJSON, "cannot decode v1 document", err,
			map[string]any{"path": "$", "schema": model.SchemaV1})
	}
	if doc.Schema == "" || doc.ProjectID == "" || doc.ProjectVersion <= 0 {
		return nil, model.Err(model.EInvalidSchema, "v1 document is missing required project fields",
			map[string]any{"schema": doc.Schema, "project_id": doc.ProjectID, "version": doc.ProjectVersion})
	}
	p := &model.Project{
		ID: doc.ProjectID, Version: doc.ProjectVersion, Seed: doc.Seed,
		SchemaVersion: model.SchemaV1, ClockUnit: doc.Clock.Unit, CapacityUnit: doc.Capacity.Unit,
		HierarchyDegraded: doc.HierarchyEnabled == nil,
	}
	if doc.HierarchyEnabled != nil {
		p.HierarchyEnabled = *doc.HierarchyEnabled
	} else {
		p.DegradationNotes = append(p.DegradationNotes, "optional hierarchy field absent; marked disabled")
	}
	if err := applyUnits(p, doc.Clock, doc.Capacity); err != nil {
		return nil, err
	}
	var err error
	p.Shards, err = convertRangesV1(doc.VertexRanges)
	if err != nil {
		return nil, err
	}
	for i, raw := range doc.Vertices {
		v, err := convertVertexV1(raw, model.SchemaV1)
		if err != nil {
			return nil, withVertexContext(err, raw.ID, i)
		}
		p.Vertices = append(p.Vertices, v)
	}
	for i, raw := range doc.Edges {
		e, err := convertEdgeV1(raw, p.HierarchyEnabled)
		if err != nil {
			return nil, withEdgeContext(err, raw.ID, i)
		}
		p.Edges = append(p.Edges, e)
	}
	p.IndexRules = convertRulesV1(doc.IndexRules)
	p.Queries = append([]model.Query(nil), doc.Queries...)
	p.Operations = normalizeOperations(doc.Operations, p.HierarchyEnabled)
	if err := finalizeProject(p); err != nil {
		return nil, err
	}
	return p, nil
}

func loadV2(data []byte) (*model.Project, error) {
	var doc docV2
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, model.Wrap(model.EInvalidJSON, "cannot decode v2 document", err,
			map[string]any{"path": "$", "schema": model.SchemaV2})
	}
	if doc.SchemaVersion == "" || doc.ProjectID == "" || doc.ProjectVersion <= 0 {
		return nil, model.Err(model.EInvalidSchema, "v2 document is missing required project fields",
			map[string]any{"schema": doc.SchemaVersion, "project_id": doc.ProjectID, "version": doc.ProjectVersion})
	}
	p := &model.Project{
		ID: doc.ProjectID, Version: doc.ProjectVersion, Seed: doc.Seed,
		SchemaVersion: model.SchemaV2, ClockUnit: doc.Clock.Unit, CapacityUnit: doc.Capacity.Unit,
		HierarchyDegraded: doc.HierarchyEnabled == nil,
	}
	if doc.HierarchyEnabled != nil {
		p.HierarchyEnabled = *doc.HierarchyEnabled
	} else {
		p.DegradationNotes = append(p.DegradationNotes, "optional hierarchy field absent; marked disabled")
	}
	if err := applyUnits(p, doc.Clock, doc.Capacity); err != nil {
		return nil, err
	}
	var err error
	p.Shards, err = convertRangesV2(doc.VertexRanges)
	if err != nil {
		return nil, err
	}
	vertices, err := flattenV2Vertices(doc.VerticesByLabel, model.SchemaV2)
	if err != nil {
		return nil, err
	}
	p.Vertices = vertices
	edges, err := flattenV2Edges(doc.EdgesByHierarchy, p.HierarchyEnabled)
	if err != nil {
		return nil, err
	}
	p.Edges = edges
	p.IndexRules = convertRulesV2(doc.IndexRules)
	p.Queries = append([]model.Query(nil), doc.Queries...)
	p.Operations = normalizeOperations(doc.Operations, p.HierarchyEnabled)
	if err := finalizeProject(p); err != nil {
		return nil, err
	}
	return p, nil
}

func loadV3(data []byte) (*model.Project, error) {
	var doc docV3
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, model.Wrap(model.EInvalidJSON, "cannot decode v3 document", err,
			map[string]any{"path": "$", "schema": model.SchemaV3})
	}
	if doc.Schema == "" || doc.ProjectID == "" || doc.ProjectVersion <= 0 {
		return nil, model.Err(model.EInvalidSchema, "v3 document is missing required project fields",
			map[string]any{"schema": doc.Schema, "project_id": doc.ProjectID, "version": doc.ProjectVersion})
	}
	p := &model.Project{
		ID: doc.ProjectID, Version: doc.ProjectVersion, Seed: doc.Seed,
		SchemaVersion: model.SchemaV3, ClockUnit: doc.Clock.Unit, CapacityUnit: doc.Capacity.Unit,
		HierarchyEnabled: false, HierarchyDegraded: true,
		DegradationNotes: []string{"optional hierarchy field absent; marked disabled"},
	}
	if err := applyUnits(p, doc.Clock, doc.Capacity); err != nil {
		return nil, err
	}
	var err error
	p.Shards, err = convertRangesV3(doc.Partitions)
	if err != nil {
		return nil, err
	}
	for i, raw := range doc.Nodes {
		v, err := convertVertexV3(raw, model.SchemaV3)
		if err != nil {
			return nil, withVertexContext(err, raw.ID, i)
		}
		p.Vertices = append(p.Vertices, v)
	}
	for i, raw := range doc.Links {
		e, err := convertEdgeV3(raw, false)
		if err != nil {
			return nil, withEdgeContext(err, raw.ID, i)
		}
		p.Edges = append(p.Edges, e)
	}
	p.IndexRules = convertRulesV3(doc.IndexRules)
	p.Queries = append([]model.Query(nil), doc.Queries...)
	p.Operations = normalizeOperations(doc.Operations, false)
	if err := finalizeProject(p); err != nil {
		return nil, err
	}
	return p, nil
}

func finalizeProject(p *model.Project) error {
	if p.ClockUnit == "" || p.CapacityUnit == "" {
		return model.Err(model.EInvalidSchema, "clock or capacity unit is missing",
			map[string]any{"clock": p.ClockUnit, "capacity": p.CapacityUnit})
	}
	queryIDs := make(map[string]bool, len(p.Queries))
	for _, q := range p.Queries {
		if queryIDs[q.ID] {
			return model.Err(model.EDuplicateID, "duplicate query id", map[string]any{"query_id": q.ID})
		}
		queryIDs[q.ID] = true
	}
	seqs := make(map[int]bool, len(p.Operations))
	for i, op := range p.Operations {
		if op.Seq <= 0 {
			return model.Err(model.EInvalidOperation, "operation sequence must be positive",
				map[string]any{"operation_index": i, "seq": op.Seq})
		}
		if seqs[op.Seq] {
			return model.Err(model.EDuplicateID, "duplicate operation sequence",
				map[string]any{"seq": op.Seq})
		}
		seqs[op.Seq] = true
	}
	if err := model.AssignShards(p); err != nil {
		return err
	}
	model.SortProject(p)
	return nil
}

func applyUnits(p *model.Project, clock clockRaw, capacity capacityRaw) error {
	if clock.Unit == "" {
		return model.Err(model.EUnsupportedUnit, "clock unit is missing", map[string]any{"path": "$.clock.unit"})
	}
	if clock.Value < 0 {
		return model.Err(model.EUnsupportedUnit, "clock value is negative",
			map[string]any{"path": "$.clock.value", "value": clock.Value})
	}
	tickMillis := clock.MillisPerTick
	if tickMillis == 0 {
		tickMillis = clock.TickMillis
	}
	unit := strings.ToLower(clock.Unit)
	switch unit {
	case "ms", "millisecond", "milliseconds":
		p.ObservedAtMillis = clock.Value
	case "tick", "ticks":
		if tickMillis <= 0 {
			return model.Err(model.EUnsupportedUnit, "tick clock requires a positive millisecond conversion",
				map[string]any{"path": "$.clock", "unit": clock.Unit, "value": clock.Value})
		}
		if clock.Value > math.MaxInt64/tickMillis {
			return model.Err(model.EUnsupportedUnit, "clock conversion overflows",
				map[string]any{"path": "$.clock.value", "value": clock.Value, "multiplier": tickMillis})
		}
		p.ObservedAtMillis = clock.Value * tickMillis
	default:
		return model.Err(model.EUnsupportedUnit, "unsupported clock unit",
			map[string]any{"path": "$.clock.unit", "unit": clock.Unit})
	}
	if capacity.Unit == "" {
		return model.Err(model.EUnsupportedUnit, "capacity unit is missing",
			map[string]any{"path": "$.capacity.unit"})
	}
	if capacity.Value < 0 {
		return model.Err(model.EUnsupportedUnit, "capacity value is negative",
			map[string]any{"path": "$.capacity.value", "value": capacity.Value})
	}
	unit = strings.ToLower(capacity.Unit)
	switch unit {
	case "b", "byte", "bytes":
		p.CacheCapacityByte = capacity.Value
	case "kib", "kibibyte", "kibibytes":
		if capacity.Value > math.MaxInt64/1024 {
			return model.Err(model.EUnsupportedUnit, "capacity conversion overflows",
				map[string]any{"path": "$.capacity.value", "value": capacity.Value, "multiplier": 1024})
		}
		p.CacheCapacityByte = capacity.Value * 1024
	default:
		return model.Err(model.EUnsupportedUnit, "unsupported capacity unit",
			map[string]any{"path": "$.capacity.unit", "unit": capacity.Unit})
	}
	return nil
}

func convertRangesV1(in []rangeV1) ([]model.Shard, error) {
	out := make([]model.Shard, 0, len(in))
	for i, r := range in {
		if r.ID == "" || r.Low == "" || r.High == "" {
			return nil, model.Err(model.EEmptyShardKeyspace, "v1 shard range is incomplete",
				map[string]any{"path": fmt.Sprintf("$.vertex_ranges[%d]", i), "shard_id": r.ID})
		}
		out = append(out, model.Shard{ID: r.ID, Low: r.Low, High: r.High})
	}
	return out, nil
}

func convertRangesV2(in []rangeV2) ([]model.Shard, error) {
	out := make([]model.Shard, 0, len(in))
	for i, r := range in {
		if r.ID == "" || r.Low == "" || r.High == "" {
			return nil, model.Err(model.EEmptyShardKeyspace, "v2 shard range is incomplete",
				map[string]any{"path": fmt.Sprintf("$.vertexShards[%d]", i), "shard_id": r.ID})
		}
		out = append(out, model.Shard{ID: r.ID, Low: r.Low, High: r.High})
	}
	return out, nil
}

func convertRangesV3(in []rangeV3) ([]model.Shard, error) {
	out := make([]model.Shard, 0, len(in))
	for i, r := range in {
		if r.ID == "" || r.Low == "" || r.High == "" {
			return nil, model.Err(model.EEmptyShardKeyspace, "v3 partition range is incomplete",
				map[string]any{"path": fmt.Sprintf("$.partitions[%d]", i), "partition": r.ID})
		}
		out = append(out, model.Shard{ID: r.ID, Low: r.Low, High: r.High})
	}
	return out, nil
}

func convertVertexV1(v vertexV1, schema string) (model.Vertex, error) {
	if v.ID == "" {
		return model.Vertex{}, model.Err(model.EInvalidSchema, "vertex id is empty",
			map[string]any{"path": "$.vertices[].vertex_id"})
	}
	attrs, err := convertAttrsV1(v.Attrs)
	if err != nil {
		return model.Vertex{}, err
	}
	return model.Vertex{ID: v.ID, DeclOrder: v.DeclOrder, Labels: append([]string(nil), v.Labels...), Attrs: attrs}, nil
}

func convertVertexV2(v vertexV2, schema string) (model.Vertex, error) {
	attrs, err := convertAttrsV2(v.Attrs)
	if err != nil {
		return model.Vertex{}, err
	}
	return model.Vertex{ID: v.ID, DeclOrder: v.DeclOrder, Labels: append([]string(nil), v.Labels...), Attrs: attrs}, nil
}

func convertVertexV3(v vertexV3, schema string) (model.Vertex, error) {
	attrs, err := convertAttrsV3(v.Attrs)
	if err != nil {
		return model.Vertex{}, err
	}
	return model.Vertex{ID: v.ID, DeclOrder: v.DeclOrder, Labels: append([]string(nil), v.Labels...), Attrs: attrs}, nil
}

func convertAttrsV1(in map[string]attrV1) (map[string]model.AttrValue, error) {
	out := make(map[string]model.AttrValue, len(in))
	for _, name := range sortedMapKeys(in) {
		a := in[name]
		kind := strings.ToLower(a.Kind)
		switch kind {
		case model.AttrInt:
			n, err := parseIntRaw(a.Value, "$.attrs."+name+".value")
			if err != nil {
				return nil, err
			}
			out[name] = model.AttrValue{Kind: model.AttrInt, Int: n}
		case model.AttrFixed:
			n, err := parseFixedRaw(a.Value, a.Scale, 1000, name, "$.attrs."+name)
			if err != nil {
				return nil, err
			}
			out[name] = model.AttrValue{Kind: model.AttrFixed, FixedMicros: n}
		case model.AttrString:
			s, err := parseStringRaw(a.Value, "$.attrs."+name+".value")
			if err != nil {
				return nil, err
			}
			out[name] = model.AttrValue{Kind: model.AttrString, String: s}
		default:
			return nil, model.Err(model.EUnsupportedAttributeKind, "unsupported v1 attribute kind",
				map[string]any{"attribute": name, "kind": a.Kind})
		}
	}
	return out, nil
}

func convertAttrsV2(in map[string]attrV2) (map[string]model.AttrValue, error) {
	out := make(map[string]model.AttrValue, len(in))
	for _, name := range sortedMapKeys(in) {
		a := in[name]
		kind := strings.ToLower(a.Type)
		switch kind {
		case "int":
			n, err := parseIntRaw(a.Raw, "$.attributes."+name+".raw")
			if err != nil {
				return nil, err
			}
			out[name] = model.AttrValue{Kind: model.AttrInt, Int: n}
		case "decimal", "fixed":
			n, err := parseFixedRaw(a.Raw, a.Scale, 10000, name, "$.attributes."+name)
			if err != nil {
				return nil, err
			}
			out[name] = model.AttrValue{Kind: model.AttrFixed, FixedMicros: n}
		case "text", "string":
			s, err := parseStringRaw(a.Raw, "$.attributes."+name+".raw")
			if err != nil {
				return nil, err
			}
			out[name] = model.AttrValue{Kind: model.AttrString, String: s}
		default:
			return nil, model.Err(model.EUnsupportedAttributeKind, "unsupported v2 attribute type",
				map[string]any{"attribute": name, "kind": a.Type})
		}
	}
	return out, nil
}

func convertAttrsV3(in map[string]attrV3) (map[string]model.AttrValue, error) {
	out := make(map[string]model.AttrValue, len(in))
	for _, name := range sortedMapKeys(in) {
		a := in[name]
		kind := strings.ToLower(a.Type)
		switch kind {
		case "i", "int":
			n, err := parseIntRaw(a.Raw, "$.props."+name+".n")
			if err != nil {
				return nil, err
			}
			out[name] = model.AttrValue{Kind: model.AttrInt, Int: n}
		case "d", "decimal", "fixed":
			n, err := parseFixedRaw(a.Raw, a.Scale, 1000, name, "$.props."+name)
			if err != nil {
				return nil, err
			}
			out[name] = model.AttrValue{Kind: model.AttrFixed, FixedMicros: n}
		case "s", "string":
			s, err := parseStringRaw(a.Raw, "$.props."+name+".n")
			if err != nil {
				return nil, err
			}
			out[name] = model.AttrValue{Kind: model.AttrString, String: s}
		default:
			return nil, model.Err(model.EUnsupportedAttributeKind, "unsupported v3 property type",
				map[string]any{"attribute": name, "kind": a.Type})
		}
	}
	return out, nil
}

func sortedMapKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func parseIntRaw(raw json.RawMessage, path string) (int64, error) {
	t := strings.TrimSpace(string(raw))
	if t == "" || t == "null" {
		return 0, model.Err(model.EInvalidSchema, "integer attribute value is missing",
			map[string]any{"path": path})
	}
	n, err := strconv.ParseInt(t, 10, 64)
	if err != nil {
		return 0, model.Wrap(model.EInvalidSchema, "integer attribute value is not an int64", err,
			map[string]any{"path": path, "raw": t})
	}
	return n, nil
}

func parseStringRaw(raw json.RawMessage, path string) (string, error) {
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", model.Wrap(model.EInvalidSchema, "string attribute value is not a string", err,
			map[string]any{"path": path})
	}
	return s, nil
}

func parseFixedRaw(raw json.RawMessage, scale, expectedScale int64, attr, path string) (int64, error) {
	if scale <= 0 || scale > 1_000_000_000 {
		return 0, model.Err(model.EBadAttributeScale, "fixed-point scale is outside the allowed range",
			map[string]any{"path": path + ".scale", "attribute": attr, "scale": scale, "min": 1, "max": 1_000_000_000})
	}
	if scale != expectedScale {
		return 0, model.Err(model.EBadAttributeScale, "fixed-point scale does not match this schema version",
			map[string]any{"path": path + ".scale", "attribute": attr, "scale": scale, "expected": expectedScale})
	}
	t := strings.TrimSpace(string(raw))
	n, err := strconv.ParseInt(t, 10, 64)
	if err != nil {
		return 0, model.Wrap(model.EBadAttributeScale, "fixed-point raw value is not an int64", err,
			map[string]any{"path": path, "attribute": attr, "raw": t})
	}
	const microsPerUnit = int64(1_000_000)
	if n > math.MaxInt64/microsPerUnit || n < math.MinInt64/microsPerUnit {
		return 0, model.Err(model.EBadAttributeScale, "fixed-point conversion overflows int64",
			map[string]any{"path": path, "attribute": attr, "raw": n, "scale": scale})
	}
	product := n * microsPerUnit
	if product%scale != 0 {
		return 0, model.Err(model.EBadAttributeScale, "fixed-point value cannot be represented losslessly",
			map[string]any{"path": path, "attribute": attr, "raw": n, "scale": scale, "canonical_scale": 1_000_000})
	}
	return product / scale, nil
}

func convertEdgeV1(e edgeV1, hierarchy bool) (model.Edge, error) {
	if e.ID == "" || e.From == "" || e.To == "" || e.Directed == nil {
		return model.Edge{}, model.Err(model.EInvalidEdge, "v1 edge is missing required fields",
			map[string]any{"path": "$.edges", "edge_id": e.ID})
	}
	levels := []string(nil)
	if hierarchy {
		levels = append([]string(nil), e.Levels...)
	}
	return model.Edge{
		ID: e.ID, From: e.From, To: e.To, Directed: *e.Directed,
		Weight: e.Weight, Levels: levels, DeclOrder: e.DeclOrder,
	}, nil
}

func convertEdgeV2(e edgeV2, hierarchy bool) (model.Edge, error) {
	if e.ID == "" || e.From == "" || e.To == "" || e.Directed == nil {
		return model.Edge{}, model.Err(model.EInvalidEdge, "v2 edge is missing required fields",
			map[string]any{"path": "$.edges", "edge_id": e.ID})
	}
	levels := []string(nil)
	if hierarchy {
		levels = append([]string(nil), e.Levels...)
	}
	return model.Edge{
		ID: e.ID, From: e.From, To: e.To, Directed: *e.Directed,
		Weight: e.Weight, Levels: levels, DeclOrder: e.DeclOrder,
	}, nil
}

func convertEdgeV3(e edgeV3, hierarchy bool) (model.Edge, error) {
	if e.ID == "" || e.From == "" || e.To == "" || e.Directed == nil {
		return model.Edge{}, model.Err(model.EInvalidEdge, "v3 link is missing required fields",
			map[string]any{"path": "$.links", "edge_id": e.ID})
	}
	return model.Edge{
		ID: e.ID, From: e.From, To: e.To, Directed: *e.Directed,
		Weight: e.Weight, DeclOrder: e.DeclOrder,
	}, nil
}

func flattenV2Vertices(tree map[string][]vertexV2, schema string) ([]model.Vertex, error) {
	groupKeys := make([]string, 0, len(tree))
	for label := range tree {
		groupKeys = append(groupKeys, label)
	}
	sort.Strings(groupKeys)
	type seenVertex struct {
		raw       vertexV2
		firstPath string
	}
	seen := make(map[string]seenVertex)
	for _, group := range groupKeys {
		for i, raw := range tree[group] {
			path := fmt.Sprintf("$.verticesByLabel[%q][%d]", group, i)
			if raw.ID == "" {
				return nil, model.Err(model.EInvalidSchema, "nested vertex has an empty id",
					map[string]any{"path": path})
			}
			foundLabel := false
			for _, tag := range raw.Labels {
				if tag == group {
					foundLabel = true
					break
				}
			}
			if !foundLabel {
				return nil, model.Err(model.EAmbiguousField,
					"nested vertex group label is absent from the vertex tag set", map[string]any{
						"path": path, "vertex_id": raw.ID, "group_label": group, "tags": raw.Labels,
					})
			}
			if prior, ok := seen[raw.ID]; ok {
				canonicalNow, _ := json.Marshal(raw)
				canonicalPrior, _ := json.Marshal(prior.raw)
				if string(canonicalNow) != string(canonicalPrior) {
					return nil, model.Err(model.EAmbiguousField,
						"nested tree contains conflicting declarations for one vertex", map[string]any{
							"vertex_id": raw.ID, "first_path": prior.firstPath, "second_path": path,
						})
				}
				continue
			}
			seen[raw.ID] = seenVertex{raw: raw, firstPath: path}
		}
	}
	vertices := make([]model.Vertex, 0, len(seen))
	for _, item := range seen {
		v, err := convertVertexV2(item.raw, schema)
		if err != nil {
			return nil, withVertexContext(err, item.raw.ID, item.firstPath)
		}
		vertices = append(vertices, v)
	}
	sort.Slice(vertices, func(i, j int) bool {
		if vertices[i].DeclOrder == vertices[j].DeclOrder {
			return vertices[i].ID < vertices[j].ID
		}
		return vertices[i].DeclOrder < vertices[j].DeclOrder
	})
	return vertices, nil
}

func flattenV2Edges(tree map[string][]edgeV2, hierarchy bool) ([]model.Edge, error) {
	groupKeys := sortedMapKeys(tree)
	type seenEdge struct {
		raw       edgeV2
		firstPath string
	}
	seen := make(map[string]seenEdge)
	for _, group := range groupKeys {
		for i, raw := range tree[group] {
			path := fmt.Sprintf("$.edgesByHierarchy[%q][%d]", group, i)
			if raw.ID == "" {
				return nil, model.Err(model.EInvalidEdge, "nested edge has an empty id",
					map[string]any{"path": path})
			}
			if hierarchy && group != "_unlabeled" {
				foundLabel := false
				for _, level := range raw.Levels {
					if level == group {
						foundLabel = true
						break
					}
				}
				if !foundLabel {
					return nil, model.Err(model.EAmbiguousField,
						"nested edge group label is absent from the edge hierarchy", map[string]any{
							"path": path, "edge_id": raw.ID, "group_label": group, "hierarchy": raw.Levels,
						})
				}
			}
			if prior, ok := seen[raw.ID]; ok {
				canonicalNow, _ := json.Marshal(raw)
				canonicalPrior, _ := json.Marshal(prior.raw)
				if string(canonicalNow) != string(canonicalPrior) {
					return nil, model.Err(model.EAmbiguousField,
						"nested edge tree contains conflicting declarations for one edge", map[string]any{
							"edge_id": raw.ID, "first_path": prior.firstPath, "second_path": path,
						})
				}
				continue
			}
			seen[raw.ID] = seenEdge{raw: raw, firstPath: path}
		}
	}
	edges := make([]model.Edge, 0, len(seen))
	for _, item := range seen {
		e, err := convertEdgeV2(item.raw, hierarchy)
		if err != nil {
			return nil, withEdgeContext(err, item.raw.ID, item.firstPath)
		}
		edges = append(edges, e)
	}
	sort.Slice(edges, func(i, j int) bool {
		if edges[i].DeclOrder == edges[j].DeclOrder {
			return edges[i].ID < edges[j].ID
		}
		return edges[i].DeclOrder < edges[j].DeclOrder
	})
	return edges, nil
}

func convertRulesV1(in []ruleV1) []model.IndexRule {
	out := make([]model.IndexRule, 0, len(in))
	for _, r := range in {
		out = append(out, model.IndexRule{Attribute: r.Attribute, Kind: strings.ToLower(r.Kind), Bucket: r.Bucket, PrefixLen: r.PrefixLen})
	}
	return out
}

func convertRulesV2(in []ruleV2) []model.IndexRule {
	out := make([]model.IndexRule, 0, len(in))
	for _, r := range in {
		kind := strings.ToLower(r.Kind)
		switch kind {
		case "decimal":
			kind = model.AttrFixed
		case "text":
			kind = model.AttrString
		}
		out = append(out, model.IndexRule{Attribute: r.Attribute, Kind: kind, Bucket: r.Bucket, PrefixLen: r.PrefixLen})
	}
	return out
}

func convertRulesV3(in []ruleV3) []model.IndexRule {
	out := make([]model.IndexRule, 0, len(in))
	for _, r := range in {
		kind := strings.ToLower(r.Kind)
		switch kind {
		case "i":
			kind = model.AttrInt
		case "d":
			kind = model.AttrFixed
		case "s":
			kind = model.AttrString
		}
		out = append(out, model.IndexRule{Attribute: r.Attribute, Kind: kind, Bucket: r.Bucket, PrefixLen: r.PrefixLen})
	}
	return out
}

func normalizeOperations(in []model.Operation, hierarchy bool) []model.Operation {
	out := append([]model.Operation(nil), in...)
	sort.Slice(out, func(i, j int) bool { return out[i].Seq < out[j].Seq })
	for i := range out {
		if out[i].Vertex != nil {
			v := model.CloneVertex(*out[i].Vertex)
			normalizeVertexLabels(&v)
			out[i].Vertex = &v
		}
		if out[i].Edge != nil {
			e := model.CloneEdge(*out[i].Edge)
			if !hierarchy {
				e.Levels = nil
			} else {
				sort.Strings(e.Levels)
			}
			out[i].Edge = &e
		}
	}
	return out
}

func normalizeVertexLabels(v *model.Vertex) {
	sort.Strings(v.Labels)
	out := v.Labels[:0]
	for i, l := range v.Labels {
		if i == 0 || l != out[len(out)-1] {
			out = append(out, l)
		}
	}
	v.Labels = out
}

func withVertexContext(err error, id string, location any) error {
	if be, ok := err.(*model.BizError); ok {
		if be.Context == nil {
			be.Context = make(map[string]any)
		}
		if _, exists := be.Context["vertex_id"]; !exists {
			be.Context["vertex_id"] = id
		}
		if _, exists := be.Context["location"]; !exists {
			be.Context["location"] = location
		}
	}
	return err
}

func withEdgeContext(err error, id string, location any) error {
	if be, ok := err.(*model.BizError); ok {
		if be.Context == nil {
			be.Context = make(map[string]any)
		}
		if _, exists := be.Context["edge_id"]; !exists {
			be.Context["edge_id"] = id
		}
		if _, exists := be.Context["location"]; !exists {
			be.Context["location"] = location
		}
	}
	return err
}
