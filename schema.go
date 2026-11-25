package thermalcache

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"strings"
)

// LoadProject decodes one of the three declared encodings and returns the
// normalized domain model. The input byte slice is never modified.
func LoadProject(data []byte) (*Project, error) {
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(data, &envelope); err != nil {
		return nil, biz(CodeSchemaInvalid, "invalid JSON document", map[string]any{"error": err.Error()})
	}
	hasSchema := envelope["schema"] != nil
	hasSchemaVersion := envelope["schema_version"] != nil
	hasCamelVersion := envelope["schemaVersion"] != nil
	versionKeys := 0
	for _, ok := range []bool{hasSchema, hasSchemaVersion, hasCamelVersion} {
		if ok {
			versionKeys++
		}
	}
	if versionKeys == 0 {
		return nil, biz(CodeSchemaUnsupported, "schema discriminator is missing", map[string]any{
			"expected": []string{"schema", "schema_version", "schemaVersion"},
		})
	}
	if versionKeys > 1 {
		return nil, biz(CodeSchemaUnknownField, "multiple schema discriminators are ambiguous", map[string]any{
			"schema": hasSchema, "schema_version": hasSchemaVersion, "schemaVersion": hasCamelVersion,
		})
	}

	var p *Project
	var err error
	switch {
	case hasSchema:
		p, err = loadV1(data)
	case hasSchemaVersion:
		p, err = loadV2(data)
	case hasCamelVersion:
		p, err = loadV3(data)
	}
	if err != nil {
		return nil, err
	}
	if err := p.Validate(); err != nil {
		return nil, err
	}
	canonicalize(p)
	return p, nil
}

func strictDecode(data []byte, out any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(out); err != nil {
		if strings.Contains(err.Error(), "unknown field") {
			return biz(CodeSchemaUnknownField, "unrecognized field combination", map[string]any{"error": err.Error()})
		}
		return biz(CodeSchemaInvalid, "schema decoding failed", map[string]any{"error": err.Error()})
	}
	if err := dec.Decode(&struct{}{}); err != io.EOF {
		return biz(CodeSchemaInvalid, "trailing JSON content", map[string]any{"offset": dec.InputOffset()})
	}
	return nil
}

func checkedMul(a, b int64, field string) (int64, error) {
	if a == 0 || b == 0 {
		return 0, nil
	}
	if a > math.MaxInt64/b || a < math.MinInt64/b {
		return 0, biz(CodeCapacityOverflow, "integer normalization overflows", map[string]any{"field": field, "a": a, "b": b})
	}
	return a * b, nil
}

func ratioToBytes(kib int64, field string) (int64, error) {
	if kib < 0 {
		return 0, biz(CodeCapacityNegative, "KiB capacity is negative", map[string]any{"field": field, "capacity_kib": kib})
	}
	return checkedMul(kib, 1024, field)
}

// ---------------------------------------------------------------------------
// v1: compact/tick/flat

type v1Doc struct {
	Schema         string            `json:"schema"`
	ProjectID      string            `json:"projectId"`
	TickMs         int64             `json:"tickMs"`
	Score          v1Score           `json:"score"`
	Limits         v1Limits          `json:"limits"`
	Nodes          []v1Node          `json:"nodes"`
	Objects        []v1Object        `json:"objects"`
	AccessEvents   []v1AccessEvent   `json:"accessEvents"`
	OpEvents       []v1OpEvent       `json:"opEvents"`
	CapacityEvents []v1CapacityEvent `json:"capacityEvents"`
	Migrations     []v1Migration     `json:"migrations"`
}

type v1Score struct {
	Bits             int            `json:"bits"`
	Scale            int64          `json:"scale"`
	CountWeight      int64          `json:"countWeight"`
	PromoteThreshold int64          `json:"promoteThreshold"`
	DemoteThreshold  int64          `json:"demoteThreshold"`
	Boundary         BoundaryPolicy `json:"boundary"`
}

type v1Limits struct {
	MaxBatchObjects  int   `json:"maxBatchObjects"`
	MaxBatchBytes    int64 `json:"maxBatchBytes"`
	WorkerBudget     int   `json:"workerBudget"`
	PerLayerBuffer   int   `json:"perLayerBuffer"`
	CommitDelayTicks int64 `json:"commitDelayTicks"`
	MaxWorkers       int   `json:"maxWorkers"`
}

type v1Node struct {
	ID            string   `json:"id"`
	Tier          Tier     `json:"tier"`
	Rack          string   `json:"rack"`
	CapacityBytes int64    `json:"capacityBytes"`
	Objects       []string `json:"objects"`
	EvictionTick  int64    `json:"evictionTick"`
}

type v1Object struct {
	ID             string  `json:"id"`
	SizeBytes      int64   `json:"sizeBytes"`
	Tier           Tier    `json:"tier"`
	AccessCounts   []int64 `json:"accessCounts"`
	LastAccessTick int64   `json:"lastAccessTick"`
	TTLTicks       int64   `json:"ttlTicks"`
	Priority       int     `json:"priority"`
	HeatWatermark  *int64  `json:"heatWatermark"`
}

type v1AccessEvent struct {
	AtTick   int64  `json:"atTick"`
	ObjectID string `json:"objectId"`
	Count    int64  `json:"count"`
}

type v1OpEvent struct {
	AtTick        int64  `json:"atTick"`
	ObjectID      string `json:"objectId"`
	Kind          OpKind `json:"kind"`
	RequestedTier Tier   `json:"requestedTier"`
}

type v1CapacityEvent struct {
	AtTick        int64  `json:"atTick"`
	NodeID        string `json:"nodeId"`
	CapacityBytes *int64 `json:"capacityBytes"`
	Delete        bool   `json:"delete"`
}

type v1Migration struct {
	AtTick       int64  `json:"atTick"`
	ObjectID     string `json:"objectId"`
	From         Tier   `json:"from"`
	To           Tier   `json:"to"`
	TargetNodeID string `json:"targetNodeId"`
}

func loadV1(data []byte) (*Project, error) {
	var d v1Doc
	if err := strictDecode(data, &d); err != nil {
		return nil, err
	}
	if d.Schema != "compact/v1" && d.Schema != "v1" {
		return nil, biz(CodeSchemaUnsupported, "unsupported v1 schema", map[string]any{"schema": d.Schema})
	}
	p := &Project{
		ID:            d.ProjectID,
		SchemaVersion: SchemaV1,
		TickMs:        d.TickMs,
		Score: ScoreConfig{
			Bits: d.Score.Bits, Scale: d.Score.Scale, CountWeight: d.Score.CountWeight,
			PromoteThreshold: d.Score.PromoteThreshold, DemoteThreshold: d.Score.DemoteThreshold,
			Boundary: d.Score.Boundary,
		},
		Limits: Limits{
			MaxBatchObjects: d.Limits.MaxBatchObjects, MaxBatchBytes: d.Limits.MaxBatchBytes,
			WorkerBudget: d.Limits.WorkerBudget, PerLayerBuffer: d.Limits.PerLayerBuffer,
			MaxWorkers: d.Limits.MaxWorkers,
		},
	}
	delay, err := checkedMul(d.Limits.CommitDelayTicks, d.TickMs, "limits.commitDelayTicks")
	if err != nil {
		return nil, err
	}
	p.Limits.CommitDelayMs = delay
	for i, n := range d.Nodes {
		p.Nodes = append(p.Nodes, Node{
			ID: n.ID, Tier: n.Tier, Rack: n.Rack, CapacityBytes: n.CapacityBytes,
			AllowedObjects: append([]string(nil), n.Objects...), EvictionTick: n.EvictionTick,
		})
		if n.CapacityBytes < 0 {
			return nil, biz(CodeCapacityNegative, "node capacity is negative", map[string]any{"node_index": i, "node_id": n.ID})
		}
	}
	for _, o := range d.Objects {
		last, err := checkedMul(o.LastAccessTick, d.TickMs, "objects.lastAccessTick")
		if err != nil {
			return nil, err
		}
		ttl, err := checkedMul(o.TTLTicks, d.TickMs, "objects.ttlTicks")
		if err != nil {
			return nil, err
		}
		var watermark *int64
		if o.HeatWatermark != nil {
			v := *o.HeatWatermark
			watermark = &v
		}
		p.Objects = append(p.Objects, Object{
			ID: o.ID, SizeBytes: o.SizeBytes, InitialTier: o.Tier,
			AccessCounts: append([]int64(nil), o.AccessCounts...), LastAccessMs: last,
			TTLMs: ttl, Priority: o.Priority, Watermark: watermark,
		})
	}
	for _, e := range d.AccessEvents {
		at, err := checkedMul(e.AtTick, d.TickMs, "accessEvents.atTick")
		if err != nil {
			return nil, err
		}
		p.AccessEvents = append(p.AccessEvents, AccessEvent{AtMs: at, ObjectID: e.ObjectID, Count: e.Count})
	}
	for _, e := range d.OpEvents {
		at, err := checkedMul(e.AtTick, d.TickMs, "opEvents.atTick")
		if err != nil {
			return nil, err
		}
		p.OpEvents = append(p.OpEvents, OpEvent{AtMs: at, ObjectID: e.ObjectID, Kind: e.Kind, RequestedTier: e.RequestedTier})
	}
	for _, e := range d.CapacityEvents {
		at, err := checkedMul(e.AtTick, d.TickMs, "capacityEvents.atTick")
		if err != nil {
			return nil, err
		}
		var capacity *int64
		if e.CapacityBytes != nil {
			v := *e.CapacityBytes
			capacity = &v
		}
		p.CapacityEvents = append(p.CapacityEvents, CapacityEvent{AtMs: at, NodeID: e.NodeID, CapacityBytes: capacity, Delete: e.Delete})
	}
	for _, m := range d.Migrations {
		at, err := checkedMul(m.AtTick, d.TickMs, "migrations.atTick")
		if err != nil {
			return nil, err
		}
		p.Migrations = append(p.Migrations, MigrationCommand{
			AtMs: at, ObjectID: m.ObjectID, FromTier: m.From, ToTier: m.To, TargetNodeID: m.TargetNodeID,
		})
	}
	return p, nil
}

// ---------------------------------------------------------------------------
// v2: normalized/snake_case/millisecond/flat

type v2Doc struct {
	SchemaVersion  string            `json:"schema_version"`
	ProjectID      string            `json:"project_id"`
	TickMs         int64             `json:"tick_ms"`
	Score          v2Score           `json:"score"`
	Limits         v2Limits          `json:"limits"`
	Nodes          []v2Node          `json:"nodes"`
	Objects        []v2Object        `json:"objects"`
	AccessEvents   []v2AccessEvent   `json:"access_events"`
	OpEvents       []v2OpEvent       `json:"op_events"`
	CapacityEvents []v2CapacityEvent `json:"capacity_events"`
	Migrations     []v2Migration     `json:"migrations"`
}

type v2Score struct {
	Bits             int            `json:"bits"`
	Scale            int64          `json:"scale"`
	CountWeight      int64          `json:"count_weight"`
	PromoteThreshold int64          `json:"promote_threshold"`
	DemoteThreshold  int64          `json:"demote_threshold"`
	Boundary         BoundaryPolicy `json:"boundary"`
}

type v2Limits struct {
	MaxBatchObjects int   `json:"max_batch_objects"`
	MaxBatchBytes   int64 `json:"max_batch_bytes"`
	WorkerBudget    int   `json:"worker_budget"`
	PerLayerBuffer  int   `json:"per_layer_buffer"`
	CommitDelayMs   int64 `json:"commit_delay_ms"`
	MaxWorkers      int   `json:"max_workers"`
}

type v2Node struct {
	ID           string   `json:"id"`
	Tier         Tier     `json:"tier"`
	Rack         string   `json:"rack"`
	CapacityKiB  int64    `json:"capacity_kib"`
	Objects      []string `json:"objects"`
	EvictionTick int64    `json:"eviction_tick"`
}

type v2Object struct {
	ID            string  `json:"id"`
	SizeBytes     int64   `json:"size_bytes"`
	Tier          Tier    `json:"tier"`
	AccessCounts  []int64 `json:"access_counts"`
	LastAccessMs  int64   `json:"last_access_ms"`
	TTLMs         int64   `json:"ttl_ms"`
	Priority      int     `json:"priority"`
	HeatWatermark *int64  `json:"heat_watermark"`
}

type v2AccessEvent struct {
	AtMs     int64  `json:"at_ms"`
	ObjectID string `json:"object_id"`
	Count    int64  `json:"count"`
}

type v2OpEvent struct {
	AtMs          int64  `json:"at_ms"`
	ObjectID      string `json:"object_id"`
	Kind          OpKind `json:"kind"`
	RequestedTier Tier   `json:"requested_tier"`
}

type v2CapacityEvent struct {
	AtMs        int64  `json:"at_ms"`
	NodeID      string `json:"node_id"`
	CapacityKiB *int64 `json:"capacity_kib"`
	Delete      bool   `json:"delete"`
}

type v2Migration struct {
	AtMs         int64  `json:"at_ms"`
	ObjectID     string `json:"object_id"`
	From         Tier   `json:"from_tier"`
	To           Tier   `json:"to_tier"`
	TargetNodeID string `json:"target_node_id"`
}

func loadV2(data []byte) (*Project, error) {
	var d v2Doc
	if err := strictDecode(data, &d); err != nil {
		return nil, err
	}
	if d.SchemaVersion != "normalized/v2" && d.SchemaVersion != "v2" {
		return nil, biz(CodeSchemaUnsupported, "unsupported v2 schema", map[string]any{"schema": d.SchemaVersion})
	}
	p := &Project{
		ID:            d.ProjectID,
		SchemaVersion: SchemaV2,
		TickMs:        d.TickMs,
		Score: ScoreConfig{
			Bits: d.Score.Bits, Scale: d.Score.Scale, CountWeight: d.Score.CountWeight,
			PromoteThreshold: d.Score.PromoteThreshold, DemoteThreshold: d.Score.DemoteThreshold,
			Boundary: d.Score.Boundary,
		},
		Limits: Limits{
			MaxBatchObjects: d.Limits.MaxBatchObjects, MaxBatchBytes: d.Limits.MaxBatchBytes,
			WorkerBudget: d.Limits.WorkerBudget, PerLayerBuffer: d.Limits.PerLayerBuffer,
			CommitDelayMs: d.Limits.CommitDelayMs, MaxWorkers: d.Limits.MaxWorkers,
		},
	}
	for _, n := range d.Nodes {
		capacity, err := ratioToBytes(n.CapacityKiB, "nodes.capacity_kib")
		if err != nil {
			return nil, err
		}
		p.Nodes = append(p.Nodes, Node{
			ID: n.ID, Tier: n.Tier, Rack: n.Rack, CapacityBytes: capacity,
			AllowedObjects: append([]string(nil), n.Objects...), EvictionTick: n.EvictionTick,
		})
	}
	for _, o := range d.Objects {
		var watermark *int64
		if o.HeatWatermark != nil {
			v := *o.HeatWatermark
			watermark = &v
		}
		p.Objects = append(p.Objects, Object{
			ID: o.ID, SizeBytes: o.SizeBytes, InitialTier: o.Tier,
			AccessCounts: append([]int64(nil), o.AccessCounts...), LastAccessMs: o.LastAccessMs,
			TTLMs: o.TTLMs, Priority: o.Priority, Watermark: watermark,
		})
	}
	for _, e := range d.AccessEvents {
		p.AccessEvents = append(p.AccessEvents, AccessEvent{AtMs: e.AtMs, ObjectID: e.ObjectID, Count: e.Count})
	}
	for _, e := range d.OpEvents {
		p.OpEvents = append(p.OpEvents, OpEvent{AtMs: e.AtMs, ObjectID: e.ObjectID, Kind: e.Kind, RequestedTier: e.RequestedTier})
	}
	for _, e := range d.CapacityEvents {
		var capacity *int64
		if e.CapacityKiB != nil {
			v, err := ratioToBytes(*e.CapacityKiB, "capacity_events.capacity_kib")
			if err != nil {
				return nil, err
			}
			capacity = &v
		}
		p.CapacityEvents = append(p.CapacityEvents, CapacityEvent{AtMs: e.AtMs, NodeID: e.NodeID, CapacityBytes: capacity, Delete: e.Delete})
	}
	for _, m := range d.Migrations {
		p.Migrations = append(p.Migrations, MigrationCommand{
			AtMs: m.AtMs, ObjectID: m.ObjectID, FromTier: m.From, ToTier: m.To, TargetNodeID: m.TargetNodeID,
		})
	}
	return p, nil
}

// ---------------------------------------------------------------------------
// v3: racked/millisecond/nested tree

type v3Doc struct {
	SchemaVersion  string                `json:"schemaVersion"`
	ProjectID      string                `json:"projectId"`
	TickMs         int64                 `json:"tickMs"`
	Score          v3Score               `json:"score"`
	Limits         v3Limits              `json:"limits"`
	RackGroups     []v3RackGroup         `json:"rackGroups"`
	ObjectsByTier  map[string][]v3Object `json:"objectsByTier"`
	AccessEvents   []v3AccessEvent       `json:"accessEvents"`
	OpEvents       []v3OpEvent           `json:"opEvents"`
	CapacityEvents []v3CapacityEvent     `json:"capacityEvents"`
	Migrations     []v3Migration         `json:"migrations"`
}

type v3Score struct {
	Bits             int            `json:"bits"`
	Scale            int64          `json:"scale"`
	CountWeight      int64          `json:"countWeight"`
	PromoteThreshold int64          `json:"promoteThreshold"`
	DemoteThreshold  int64          `json:"demoteThreshold"`
	Boundary         BoundaryPolicy `json:"boundary"`
}

type v3Limits struct {
	MaxBatchObjects int   `json:"maxBatchObjects"`
	MaxBatchBytes   int64 `json:"maxBatchBytes"`
	WorkerBudget    int   `json:"workerBudget"`
	PerLayerBuffer  int   `json:"perLayerBuffer"`
	CommitDelayMs   int64 `json:"commitDelayMs"`
	MaxWorkers      int   `json:"maxWorkers"`
}

type v3RackGroup struct {
	Rack  string              `json:"rack"`
	Tiers map[string][]v3Node `json:"tiers"`
}

type v3Node struct {
	ID           string   `json:"id"`
	CapacityKiB  int64    `json:"capacityKiB"`
	Objects      []string `json:"objects"`
	EvictionTick int64    `json:"evictionTick"`
}

type v3Object struct {
	ID            string  `json:"id"`
	SizeBytes     int64   `json:"sizeBytes"`
	AccessCounts  []int64 `json:"accessCounts"`
	LastAccessMs  int64   `json:"lastAccessMs"`
	TTLMs         int64   `json:"ttlMs"`
	Priority      int     `json:"priority"`
	HeatWatermark *int64  `json:"heatWatermark"`
}

type v3AccessEvent struct {
	AtMs     int64  `json:"atMs"`
	ObjectID string `json:"objectId"`
	Count    int64  `json:"count"`
}

type v3OpEvent struct {
	AtMs          int64  `json:"atMs"`
	ObjectID      string `json:"objectId"`
	Kind          OpKind `json:"kind"`
	RequestedTier Tier   `json:"requestedTier"`
}

type v3CapacityEvent struct {
	AtMs        int64  `json:"atMs"`
	NodeID      string `json:"nodeId"`
	CapacityKiB *int64 `json:"capacityKiB"`
	Delete      bool   `json:"delete"`
}

type v3Migration struct {
	AtMs         int64  `json:"atMs"`
	ObjectID     string `json:"objectId"`
	From         Tier   `json:"fromTier"`
	To           Tier   `json:"toTier"`
	TargetNodeID string `json:"targetNodeId"`
}

func loadV3(data []byte) (*Project, error) {
	var d v3Doc
	if err := strictDecode(data, &d); err != nil {
		return nil, err
	}
	if d.SchemaVersion != "racked/v3" && d.SchemaVersion != "v3" {
		return nil, biz(CodeSchemaUnsupported, "unsupported v3 schema", map[string]any{"schema": d.SchemaVersion})
	}
	p := &Project{
		ID:            d.ProjectID,
		SchemaVersion: SchemaV3,
		TickMs:        d.TickMs,
		Score: ScoreConfig{
			Bits: d.Score.Bits, Scale: d.Score.Scale, CountWeight: d.Score.CountWeight,
			PromoteThreshold: d.Score.PromoteThreshold, DemoteThreshold: d.Score.DemoteThreshold,
			Boundary: d.Score.Boundary,
		},
		Limits: Limits{
			MaxBatchObjects: d.Limits.MaxBatchObjects, MaxBatchBytes: d.Limits.MaxBatchBytes,
			WorkerBudget: d.Limits.WorkerBudget, PerLayerBuffer: d.Limits.PerLayerBuffer,
			CommitDelayMs: d.Limits.CommitDelayMs, MaxWorkers: d.Limits.MaxWorkers,
		},
	}
	for _, group := range d.RackGroups {
		for tierName, nodes := range group.Tiers {
			tier := Tier(tierName)
			if !validateTier(tier) {
				return nil, biz(CodeSchemaInvalid, "unknown tier in rack group", map[string]any{"rack": group.Rack, "tier": tierName})
			}
			for _, n := range nodes {
				capacity, err := ratioToBytes(n.CapacityKiB, "rackGroups.tiers.capacityKiB")
				if err != nil {
					return nil, err
				}
				p.Nodes = append(p.Nodes, Node{
					ID: n.ID, Tier: tier, Rack: group.Rack, CapacityBytes: capacity,
					AllowedObjects: append([]string(nil), n.Objects...), EvictionTick: n.EvictionTick,
				})
			}
		}
	}
	for tierName, objects := range d.ObjectsByTier {
		tier := Tier(tierName)
		if !validateTier(tier) {
			return nil, biz(CodeSchemaInvalid, "unknown tier in object tree", map[string]any{"tier": tierName})
		}
		for _, o := range objects {
			var watermark *int64
			if o.HeatWatermark != nil {
				v := *o.HeatWatermark
				watermark = &v
			}
			p.Objects = append(p.Objects, Object{
				ID: o.ID, SizeBytes: o.SizeBytes, InitialTier: tier,
				AccessCounts: append([]int64(nil), o.AccessCounts...), LastAccessMs: o.LastAccessMs,
				TTLMs: o.TTLMs, Priority: o.Priority, Watermark: watermark,
			})
		}
	}
	for _, e := range d.AccessEvents {
		p.AccessEvents = append(p.AccessEvents, AccessEvent{AtMs: e.AtMs, ObjectID: e.ObjectID, Count: e.Count})
	}
	for _, e := range d.OpEvents {
		p.OpEvents = append(p.OpEvents, OpEvent{AtMs: e.AtMs, ObjectID: e.ObjectID, Kind: e.Kind, RequestedTier: e.RequestedTier})
	}
	for _, e := range d.CapacityEvents {
		var capacity *int64
		if e.CapacityKiB != nil {
			v, err := ratioToBytes(*e.CapacityKiB, "capacityEvents.capacityKiB")
			if err != nil {
				return nil, err
			}
			capacity = &v
		}
		p.CapacityEvents = append(p.CapacityEvents, CapacityEvent{AtMs: e.AtMs, NodeID: e.NodeID, CapacityBytes: capacity, Delete: e.Delete})
	}
	for _, m := range d.Migrations {
		p.Migrations = append(p.Migrations, MigrationCommand{
			AtMs: m.AtMs, ObjectID: m.ObjectID, FromTier: m.From, ToTier: m.To, TargetNodeID: m.TargetNodeID,
		})
	}
	return p, nil
}

func (p *Project) SemanticEqual(other *Project) bool {
	if p == nil || other == nil {
		return p == other
	}
	return p.Fingerprint() == other.Fingerprint()
}

func (p *Project) describeMismatch(other *Project) string {
	if p.SemanticEqual(other) {
		return ""
	}
	return fmt.Sprintf("fingerprint %s != %s", p.Fingerprint(), other.Fingerprint())
}
