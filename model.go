package thermalcache

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
)

type Tier string

const (
	TierHot  Tier = "hot"
	TierCold Tier = "cold"
)

const (
	SchemaV1 = "v1"
	SchemaV2 = "v2"
	SchemaV3 = "v3"
)

type BoundaryPolicy string

const (
	// PromoteAtBoundary is the declared default: score >= promote threshold
	// promotes, score <= demote threshold demotes. If both thresholds are equal,
	// promotion wins for an object currently in cold storage.
	PromoteAtBoundary BoundaryPolicy = "promote_inclusive"
	// DemoteAtBoundary is the alternative: score > promote threshold promotes,
	// score <= demote threshold demotes.
	DemoteAtBoundary BoundaryPolicy = "demote_inclusive"
)

type OpKind string

const (
	OpRead  OpKind = "read"
	OpWrite OpKind = "write"
)

// ScoreConfig is a fully deterministic signed fixed-point configuration.
// Every score is an integer measured in 1/Scale units.
type ScoreConfig struct {
	Bits             int            `json:"bits"`
	Scale            int64          `json:"scale"`
	CountWeight      int64          `json:"count_weight"`
	PromoteThreshold int64          `json:"promote_threshold"`
	DemoteThreshold  int64          `json:"demote_threshold"`
	Boundary         BoundaryPolicy `json:"boundary"`
}

type Limits struct {
	MaxBatchObjects int   `json:"max_batch_objects"`
	MaxBatchBytes   int64 `json:"max_batch_bytes"`
	WorkerBudget    int   `json:"worker_budget"`
	PerLayerBuffer  int   `json:"per_layer_buffer"`
	CommitDelayMs   int64 `json:"commit_delay_ms"`
	MaxWorkers      int   `json:"max_workers"`
}

type Node struct {
	ID             string   `json:"id"`
	Tier           Tier     `json:"tier"`
	Rack           string   `json:"rack"`
	CapacityBytes  int64    `json:"capacity_bytes"`
	AllowedObjects []string `json:"allowed_objects,omitempty"`
	EvictionTick   int64    `json:"eviction_tick"`
}

type Object struct {
	ID           string  `json:"id"`
	SizeBytes    int64   `json:"size_bytes"`
	InitialTier  Tier    `json:"initial_tier"`
	AccessCounts []int64 `json:"access_counts"`
	LastAccessMs int64   `json:"last_access_ms"`
	TTLMs        int64   `json:"ttl_ms"`
	Priority     int     `json:"priority"`
	// Watermark is an optional object-specific promote threshold. A nil value
	// inherits ScoreConfig.PromoteThreshold.
	Watermark *int64 `json:"watermark,omitempty"`
}

type AccessEvent struct {
	AtMs     int64  `json:"at_ms"`
	ObjectID string `json:"object_id"`
	Count    int64  `json:"count"`
}

type OpEvent struct {
	AtMs          int64  `json:"at_ms"`
	ObjectID      string `json:"object_id"`
	Kind          OpKind `json:"kind"`
	RequestedTier Tier   `json:"requested_tier,omitempty"`
}

type CapacityEvent struct {
	AtMs          int64  `json:"at_ms"`
	NodeID        string `json:"node_id"`
	CapacityBytes *int64 `json:"capacity_bytes,omitempty"`
	Delete        bool   `json:"delete,omitempty"`
}

type MigrationCommand struct {
	AtMs         int64  `json:"at_ms"`
	ObjectID     string `json:"object_id"`
	FromTier     Tier   `json:"from_tier"`
	ToTier       Tier   `json:"to_tier"`
	TargetNodeID string `json:"target_node_id,omitempty"`
}

// Project is the single normalized model produced by all three schema
// loaders. Slice order is canonical: hot before cold, then rack/id for nodes,
// and object id for objects.
type Project struct {
	ID             string             `json:"id"`
	SchemaVersion  string             `json:"schema_version"`
	TickMs         int64              `json:"tick_ms"`
	Score          ScoreConfig        `json:"score"`
	Limits         Limits             `json:"limits"`
	Nodes          []Node             `json:"nodes"`
	Objects        []Object           `json:"objects"`
	AccessEvents   []AccessEvent      `json:"access_events,omitempty"`
	OpEvents       []OpEvent          `json:"op_events,omitempty"`
	CapacityEvents []CapacityEvent    `json:"capacity_events,omitempty"`
	Migrations     []MigrationCommand `json:"migrations,omitempty"`
}

func (p *Project) Clone() *Project {
	if p == nil {
		return nil
	}
	out := *p
	out.Nodes = append([]Node(nil), p.Nodes...)
	for i := range out.Nodes {
		out.Nodes[i].AllowedObjects = append([]string(nil), p.Nodes[i].AllowedObjects...)
	}
	out.Objects = append([]Object(nil), p.Objects...)
	for i := range out.Objects {
		out.Objects[i].AccessCounts = append([]int64(nil), p.Objects[i].AccessCounts...)
		if p.Objects[i].Watermark != nil {
			v := *p.Objects[i].Watermark
			out.Objects[i].Watermark = &v
		}
	}
	out.AccessEvents = append([]AccessEvent(nil), p.AccessEvents...)
	out.OpEvents = append([]OpEvent(nil), p.OpEvents...)
	out.CapacityEvents = append([]CapacityEvent(nil), p.CapacityEvents...)
	for i := range out.CapacityEvents {
		if p.CapacityEvents[i].CapacityBytes != nil {
			v := *p.CapacityEvents[i].CapacityBytes
			out.CapacityEvents[i].CapacityBytes = &v
		}
	}
	out.Migrations = append([]MigrationCommand(nil), p.Migrations...)
	return &out
}

// Fingerprint hashes only normalized domain content, excluding the schema
// version so callers can prove the three encodings are semantically equal.
func (p *Project) Fingerprint() string {
	if p == nil {
		return ""
	}
	cp := p.Clone()
	cp.SchemaVersion = ""
	b, _ := json.Marshal(cp)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func (p *Project) objectIndex() map[string]*Object {
	out := make(map[string]*Object, len(p.Objects))
	for i := range p.Objects {
		out[p.Objects[i].ID] = &p.Objects[i]
	}
	return out
}

func validateTier(t Tier) bool {
	return t == TierHot || t == TierCold
}

func canonicalize(p *Project) {
	sort.SliceStable(p.Nodes, func(i, j int) bool {
		a, b := p.Nodes[i], p.Nodes[j]
		if a.Tier != b.Tier {
			return a.Tier == TierHot
		}
		if a.Rack != b.Rack {
			return a.Rack < b.Rack
		}
		return a.ID < b.ID
	})
	for i := range p.Nodes {
		sort.Strings(p.Nodes[i].AllowedObjects)
	}
	sort.SliceStable(p.Objects, func(i, j int) bool {
		return p.Objects[i].ID < p.Objects[j].ID
	})
	sort.SliceStable(p.AccessEvents, func(i, j int) bool {
		a, b := p.AccessEvents[i], p.AccessEvents[j]
		if a.AtMs != b.AtMs {
			return a.AtMs < b.AtMs
		}
		if a.ObjectID != b.ObjectID {
			return a.ObjectID < b.ObjectID
		}
		return a.Count < b.Count
	})
	sort.SliceStable(p.OpEvents, func(i, j int) bool {
		a, b := p.OpEvents[i], p.OpEvents[j]
		if a.AtMs != b.AtMs {
			return a.AtMs < b.AtMs
		}
		if a.ObjectID != b.ObjectID {
			return a.ObjectID < b.ObjectID
		}
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		return a.RequestedTier < b.RequestedTier
	})
	sort.SliceStable(p.CapacityEvents, func(i, j int) bool {
		a, b := p.CapacityEvents[i], p.CapacityEvents[j]
		if a.AtMs != b.AtMs {
			return a.AtMs < b.AtMs
		}
		return a.NodeID < b.NodeID
	})
	sort.SliceStable(p.Migrations, func(i, j int) bool {
		a, b := p.Migrations[i], p.Migrations[j]
		if a.AtMs != b.AtMs {
			return a.AtMs < b.AtMs
		}
		if a.ObjectID != b.ObjectID {
			return a.ObjectID < b.ObjectID
		}
		return a.ToTier < b.ToTier
	})
}

func (p *Project) Validate() error {
	if p == nil {
		return biz(CodeSchemaInvalid, "project is nil", map[string]any{"field": "project"})
	}
	if p.ID == "" {
		return biz(CodeSchemaInvalid, "project id is required", map[string]any{"field": "id"})
	}
	if p.TickMs <= 0 {
		return biz(CodeSchemaInvalid, "tick milliseconds must be positive", map[string]any{"field": "tick_ms", "value": p.TickMs})
	}
	if p.Score.Bits < 2 || p.Score.Bits > 63 {
		return biz(CodeSchemaInvalid, "score bits must be in [2,63]", map[string]any{"field": "score.bits", "value": p.Score.Bits})
	}
	if p.Score.Scale <= 0 || p.Score.CountWeight < 0 {
		return biz(CodeSchemaInvalid, "invalid fixed-point weights", map[string]any{
			"scale": p.Score.Scale, "count_weight": p.Score.CountWeight,
		})
	}
	if p.Score.PromoteThreshold < 0 || p.Score.DemoteThreshold < 0 {
		return biz(CodeSchemaInvalid, "heat thresholds must not be negative", map[string]any{
			"promote": p.Score.PromoteThreshold, "demote": p.Score.DemoteThreshold,
		})
	}
	scoreMax := signedMax(p.Score.Bits)
	if p.Score.PromoteThreshold > scoreMax || p.Score.DemoteThreshold > scoreMax {
		return biz(CodeSchemaInvalid, "heat threshold exceeds declared score width", map[string]any{
			"bits": p.Score.Bits, "max_score": scoreMax,
			"promote": p.Score.PromoteThreshold, "demote": p.Score.DemoteThreshold,
		})
	}
	if p.Score.PromoteThreshold < p.Score.DemoteThreshold {
		return biz(CodeSchemaInvalid, "promote threshold is below demote threshold", map[string]any{
			"promote": p.Score.PromoteThreshold, "demote": p.Score.DemoteThreshold,
		})
	}
	if p.Score.Boundary == "" {
		p.Score.Boundary = PromoteAtBoundary
	}
	if p.Score.Boundary != PromoteAtBoundary && p.Score.Boundary != DemoteAtBoundary {
		return biz(CodeSchemaInvalid, "unknown boundary policy", map[string]any{"boundary": p.Score.Boundary})
	}
	if p.Limits.MaxBatchObjects == 0 {
		return biz(CodeBatchLimitZero, "max batch objects is zero", map[string]any{"field": "limits.max_batch_objects"})
	}
	if p.Limits.MaxBatchBytes == 0 {
		return biz(CodeBatchLimitZero, "max batch bytes is zero", map[string]any{"field": "limits.max_batch_bytes"})
	}
	if p.Limits.MaxBatchObjects < 0 || p.Limits.MaxBatchBytes < 0 {
		return biz(CodeBatchLimitZero, "batch limits must not be negative", map[string]any{
			"max_objects": p.Limits.MaxBatchObjects, "max_bytes": p.Limits.MaxBatchBytes,
		})
	}
	if p.Limits.MaxWorkers == 0 {
		p.Limits.MaxWorkers = 16
	}
	if p.Limits.WorkerBudget == 0 {
		p.Limits.WorkerBudget = 1
	}
	if p.Limits.WorkerBudget < 1 || p.Limits.WorkerBudget > p.Limits.MaxWorkers {
		return biz(CodeThreadBudget, "worker budget is outside [1,max_workers]", map[string]any{
			"worker_budget": p.Limits.WorkerBudget, "max_workers": p.Limits.MaxWorkers,
		})
	}
	if p.Limits.PerLayerBuffer == 0 {
		return biz(CodeBufferBudget, "per-layer migration buffer is zero", map[string]any{"field": "limits.per_layer_buffer"})
	}
	if p.Limits.PerLayerBuffer < 0 {
		return biz(CodeBufferBudget, "per-layer migration buffer is negative", map[string]any{
			"per_layer_buffer": p.Limits.PerLayerBuffer,
		})
	}
	if p.Limits.CommitDelayMs < 0 {
		return biz(CodeSchemaInvalid, "commit delay must not be negative", map[string]any{"commit_delay_ms": p.Limits.CommitDelayMs})
	}

	nodeIDs := make(map[string]struct{}, len(p.Nodes))
	for i := range p.Nodes {
		n := &p.Nodes[i]
		if n.ID == "" {
			return biz(CodeSchemaInvalid, "node id is required", map[string]any{"node_index": i})
		}
		if _, ok := nodeIDs[n.ID]; ok {
			return biz(CodeDuplicateNode, "duplicate node id", map[string]any{"node_id": n.ID, "node_index": i})
		}
		nodeIDs[n.ID] = struct{}{}
		if !validateTier(n.Tier) {
			return biz(CodeSchemaInvalid, "unknown node tier", map[string]any{"node_id": n.ID, "tier": n.Tier})
		}
		if n.CapacityBytes < 0 {
			return biz(CodeCapacityNegative, "node capacity is negative", map[string]any{
				"node_id": n.ID, "capacity_bytes": n.CapacityBytes,
			})
		}
		if n.EvictionTick < 0 {
			return biz(CodeSchemaInvalid, "negative node eviction tick", map[string]any{
				"node_id": n.ID, "eviction_tick": n.EvictionTick,
			})
		}
	}

	objectIDs := make(map[string]struct{}, len(p.Objects))
	for i := range p.Objects {
		o := &p.Objects[i]
		if o.ID == "" {
			return biz(CodeSchemaInvalid, "object id is required", map[string]any{"object_index": i})
		}
		if _, ok := objectIDs[o.ID]; ok {
			return biz(CodeDuplicateObject, "duplicate object id", map[string]any{"object_id": o.ID, "object_index": i})
		}
		objectIDs[o.ID] = struct{}{}
		if !validateTier(o.InitialTier) {
			return biz(CodeSchemaInvalid, "unknown object tier", map[string]any{"object_id": o.ID, "tier": o.InitialTier})
		}
		if o.SizeBytes < 0 {
			return biz(CodeSchemaInvalid, "object size is negative", map[string]any{"object_id": o.ID, "size_bytes": o.SizeBytes})
		}
		if o.LastAccessMs < 0 || o.TTLMs < 0 {
			return biz(CodeSchemaInvalid, "negative object time fields", map[string]any{
				"object_id": o.ID, "last_access_ms": o.LastAccessMs, "ttl_ms": o.TTLMs,
			})
		}
		for j, c := range o.AccessCounts {
			if c < 0 {
				return biz(CodeSchemaInvalid, "negative access count", map[string]any{
					"object_id": o.ID, "count_index": j, "count": c,
				})
			}
		}
		if o.Watermark != nil && *o.Watermark < 0 {
			return biz(CodeSchemaInvalid, "negative heat watermark", map[string]any{"object_id": o.ID, "watermark": *o.Watermark})
		}
		if o.Watermark != nil && *o.Watermark > scoreMax {
			return biz(CodeSchemaInvalid, "heat watermark exceeds declared score width", map[string]any{
				"object_id": o.ID, "watermark": *o.Watermark, "max_score": scoreMax,
			})
		}
		eligible := false
		for _, n := range p.Nodes {
			if n.Tier != o.InitialTier {
				continue
			}
			if len(n.AllowedObjects) == 0 || containsString(n.AllowedObjects, o.ID) {
				eligible = true
				break
			}
		}
		if !eligible {
			return biz(CodeSchemaInvalid, "object is not placeable in its initial tier", map[string]any{
				"object_id": o.ID, "tier": o.InitialTier,
			})
		}
	}
	for _, n := range p.Nodes {
		for _, objectID := range n.AllowedObjects {
			if _, ok := objectIDs[objectID]; !ok {
				return biz(CodeObjectNotFound, "node allowed-object set references unknown object", map[string]any{
					"node_id": n.ID, "object_id": objectID,
				})
			}
		}
	}
	for i, ev := range p.AccessEvents {
		if ev.AtMs < 0 || ev.Count < 0 {
			return biz(CodeSchemaInvalid, "invalid access event", map[string]any{"event_index": i})
		}
		if _, ok := objectIDs[ev.ObjectID]; !ok {
			return biz(CodeObjectNotFound, "access event references unknown object", map[string]any{
				"event_index": i, "object_id": ev.ObjectID,
			})
		}
	}
	for i, ev := range p.OpEvents {
		if ev.AtMs < 0 || (ev.Kind != OpRead && ev.Kind != OpWrite) {
			return biz(CodeSchemaInvalid, "invalid operation event", map[string]any{"event_index": i})
		}
		if _, ok := objectIDs[ev.ObjectID]; !ok {
			return biz(CodeObjectNotFound, "operation event references unknown object", map[string]any{
				"event_index": i, "object_id": ev.ObjectID,
			})
		}
	}
	for i, ev := range p.CapacityEvents {
		if ev.AtMs < 0 {
			return biz(CodeSchemaInvalid, "capacity event has negative time", map[string]any{"event_index": i})
		}
		if _, ok := nodeIDs[ev.NodeID]; !ok {
			return biz(CodeNodeNotFound, "capacity event references unknown node", map[string]any{
				"event_index": i, "node_id": ev.NodeID,
			})
		}
		if !ev.Delete && ev.CapacityBytes == nil {
			return biz(CodeSchemaInvalid, "capacity event has no capacity", map[string]any{"event_index": i})
		}
		if ev.CapacityBytes != nil && *ev.CapacityBytes < 0 {
			return biz(CodeCapacityNegative, "capacity event is negative", map[string]any{
				"event_index": i, "node_id": ev.NodeID, "capacity_bytes": *ev.CapacityBytes,
			})
		}
	}
	for i, m := range p.Migrations {
		if m.AtMs < 0 || !validateTier(m.FromTier) || !validateTier(m.ToTier) || m.FromTier == m.ToTier {
			return biz(CodeSchemaInvalid, "invalid migration command", map[string]any{"command_index": i})
		}
		if _, ok := objectIDs[m.ObjectID]; !ok {
			return biz(CodeObjectNotFound, "migration command references unknown object", map[string]any{
				"command_index": i, "object_id": m.ObjectID,
			})
		}
	}
	return nil
}

func (p *Project) layerCapacity(t Tier) (int64, error) {
	var cap int64
	for _, n := range p.Nodes {
		if n.Tier != t {
			continue
		}
		if n.CapacityBytes > 0 && cap > int64(^uint64(0)>>1)-n.CapacityBytes {
			return 0, biz(CodeCapacityOverflow, "layer capacity overflows int64", map[string]any{"tier": t})
		}
		cap += n.CapacityBytes
	}
	return cap, nil
}

func (p *Project) nodeByID(id string) (Node, bool) {
	for _, n := range p.Nodes {
		if n.ID == id {
			return n, true
		}
	}
	return Node{}, false
}

func (p *Project) objectByID(id string) *Object {
	for i := range p.Objects {
		if p.Objects[i].ID == id {
			return &p.Objects[i]
		}
	}
	return nil
}

func (p *Project) String() string {
	return fmt.Sprintf("Project(%s,%s,objects=%d,nodes=%d)", p.ID, p.SchemaVersion, len(p.Objects), len(p.Nodes))
}
