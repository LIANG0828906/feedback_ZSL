package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
)

type SchemaVersion string

const (
	SchemaV1 SchemaVersion = "v1"
	SchemaV2 SchemaVersion = "v2"
	SchemaV3 SchemaVersion = "v3"
)

func ParseSchemaVersion(value string) (SchemaVersion, error) {
	switch value {
	case "v1", "V1", "cache-warm.v1", "1":
		return SchemaV1, nil
	case "v2", "V2", "cache-warm.v2", "2":
		return SchemaV2, nil
	case "v3", "V3", "cache-warm.v3", "3":
		return SchemaV3, nil
	default:
		return "", NewBizError(CodeUnsupportedSchema, "parse_schema", "unsupported schema version", map[string]any{"schema": value})
	}
}

type Node struct {
	ID                 string `json:"id"`
	Group              string `json:"group"`
	JoinTick           int64  `json:"join_tick"`
	RetireTick         int64  `json:"retire_tick,omitempty"`
	ClaimIntervalTicks int64  `json:"claim_interval_ticks"`
	LeaseTicks         int64  `json:"lease_ticks"`
	MaxConcurrent      int    `json:"max_concurrent"`
	BufferLimit        int    `json:"buffer_limit"`
}

func (n Node) ActiveAt(tick int64) bool {
	if tick < n.JoinTick {
		return false
	}
	return n.RetireTick == 0 || tick < n.RetireTick
}

func (n Node) RetiredAtOrBefore(tick int64) bool {
	return n.RetireTick > 0 && n.RetireTick <= tick
}

type Shard struct {
	ID              string `json:"id"`
	NodeID          string `json:"node_id"`
	Group           string `json:"group"`
	Digest          uint64 `json:"digest"`
	SourceVersion   int64  `json:"source_version"`
	PreviousVersion int64  `json:"previous_version"`
	BandwidthMilli  int64  `json:"bandwidth_milli"`
	PreheatTicks    int64  `json:"preheat_ticks"`
}

type Project struct {
	ID          string        `json:"id"`
	Schema      SchemaVersion `json:"schema"`
	Seed        int64         `json:"seed"`
	TickMS      int64         `json:"tick_ms"`
	Watermark   int64         `json:"watermark"`
	BudgetMilli int64         `json:"budget_milli"`
	BudgetBasis int           `json:"budget_basis"`
	Nodes       []Node        `json:"nodes"`
	Shards      []Shard       `json:"shards"`
}

func (p Project) Clone() Project {
	clone := p
	clone.Nodes = append([]Node(nil), p.Nodes...)
	clone.Shards = append([]Shard(nil), p.Shards...)
	return clone
}

func (p *Project) Normalize() {
	sort.Slice(p.Nodes, func(i, j int) bool { return p.Nodes[i].ID < p.Nodes[j].ID })
	sort.Slice(p.Shards, func(i, j int) bool { return p.Shards[i].ID < p.Shards[j].ID })
	if p.Watermark == 0 {
		p.Watermark = 1000
	}
}

type CanonicalNode struct {
	ID                 string `json:"id"`
	Group              string `json:"group"`
	JoinTick           int64  `json:"join_tick"`
	RetireTick         int64  `json:"retire_tick,omitempty"`
	ClaimIntervalTicks int64  `json:"claim_interval_ticks"`
	LeaseTicks         int64  `json:"lease_ticks"`
	MaxConcurrent      int    `json:"max_concurrent"`
	BufferLimit        int    `json:"buffer_limit"`
}

type CanonicalShard struct {
	ID              string `json:"id"`
	NodeID          string `json:"node_id"`
	Group           string `json:"group"`
	Digest          uint64 `json:"digest"`
	SourceVersion   int64  `json:"source_version"`
	PreviousVersion int64  `json:"previous_version"`
	BandwidthMilli  int64  `json:"bandwidth_milli"`
	PreheatTicks    int64  `json:"preheat_ticks"`
}

type Canonical struct {
	ProjectID   string           `json:"project_id"`
	Seed        int64            `json:"seed"`
	TickMS      int64            `json:"tick_ms"`
	Watermark   int64            `json:"watermark"`
	BudgetMilli int64            `json:"budget_milli"`
	BudgetBasis int              `json:"budget_basis"`
	Nodes       []CanonicalNode  `json:"nodes"`
	Shards      []CanonicalShard `json:"shards"`
}

func (p Project) Canonical() Canonical {
	cp := Canonical{
		ProjectID:   p.ID,
		Seed:        p.Seed,
		TickMS:      p.TickMS,
		Watermark:   p.Watermark,
		BudgetMilli: p.BudgetMilli,
		BudgetBasis: p.BudgetBasis,
	}
	for _, node := range p.Nodes {
		cp.Nodes = append(cp.Nodes, CanonicalNode{
			ID:                 node.ID,
			Group:              node.Group,
			JoinTick:           node.JoinTick,
			RetireTick:         node.RetireTick,
			ClaimIntervalTicks: node.ClaimIntervalTicks,
			LeaseTicks:         node.LeaseTicks,
			MaxConcurrent:      node.MaxConcurrent,
			BufferLimit:        node.BufferLimit,
		})
	}
	for _, shard := range p.Shards {
		cp.Shards = append(cp.Shards, CanonicalShard{
			ID:              shard.ID,
			NodeID:          shard.NodeID,
			Group:           shard.Group,
			Digest:          shard.Digest,
			SourceVersion:   shard.SourceVersion,
			PreviousVersion: shard.PreviousVersion,
			BandwidthMilli:  shard.BandwidthMilli,
			PreheatTicks:    shard.PreheatTicks,
		})
	}
	sort.Slice(cp.Nodes, func(i, j int) bool { return cp.Nodes[i].ID < cp.Nodes[j].ID })
	sort.Slice(cp.Shards, func(i, j int) bool { return cp.Shards[i].ID < cp.Shards[j].ID })
	return cp
}

func (p Project) SemanticJSON() []byte {
	value, _ := json.Marshal(p.Canonical())
	return value
}

func (p Project) SemanticHash() string {
	sum := sha256.Sum256(p.SemanticJSON())
	return hex.EncodeToString(sum[:])
}

func (p Project) Node(id string) (Node, bool) {
	for _, node := range p.Nodes {
		if node.ID == id {
			return node, true
		}
	}
	return Node{}, false
}

func (p Project) Shard(id string) (Shard, bool) {
	for _, shard := range p.Shards {
		if shard.ID == id {
			return shard, true
		}
	}
	return Shard{}, false
}

func (p Project) Validate() error {
	if p.ID == "" {
		return NewBizError(CodeBadRequest, "validate_project", "project id is required", nil)
	}
	if p.TickMS <= 0 {
		return NewBizError(CodeBadRequest, "validate_project", "tick_ms must be positive", map[string]any{"tick_ms": p.TickMS})
	}
	if p.BudgetBasis != 1000 && p.BudgetBasis != 10000 {
		return NewBizError(CodeBadBandwidthScale, "validate_project", "bandwidth basis must be 1000 or 10000", map[string]any{"basis": p.BudgetBasis})
	}
	if p.BudgetMilli <= 0 {
		return NewBizError(CodeBadBandwidthScale, "validate_project", "bandwidth budget must be positive", map[string]any{"budget_milli": p.BudgetMilli})
	}
	nodes := map[string]Node{}
	for _, node := range p.Nodes {
		if node.ID == "" {
			return NewBizError(CodeBadRequest, "validate_project", "node id is required", nil)
		}
		if _, exists := nodes[node.ID]; exists {
			return NewBizError(CodeBadRequest, "validate_project", "duplicate node", map[string]any{"node": node.ID})
		}
		if node.LeaseTicks <= 0 {
			return NewBizError(CodeZeroLease, "validate_project", "lease duration must be positive", map[string]any{"node": node.ID, "lease_ticks": node.LeaseTicks})
		}
		if node.ClaimIntervalTicks <= 0 {
			return NewBizError(CodeBadRequest, "validate_project", "claim interval must be positive", map[string]any{"node": node.ID})
		}
		if node.MaxConcurrent <= 0 {
			return NewBizError(CodeBadRequest, "validate_project", "max_concurrent must be positive", map[string]any{"node": node.ID})
		}
		if node.BufferLimit <= 0 {
			return NewBizError(CodeBufferBudgetInvalid, "validate_project", "buffer_limit must be positive", map[string]any{"node": node.ID})
		}
		nodes[node.ID] = node
	}
	seen := map[string]bool{}
	for _, shard := range p.Shards {
		if shard.ID == "" {
			return NewBizError(CodeBadRequest, "validate_project", "shard id is required", nil)
		}
		if seen[shard.ID] {
			return NewBizError(CodeDuplicateShard, "validate_project", "duplicate shard id", map[string]any{"shard": shard.ID})
		}
		seen[shard.ID] = true
		node, ok := nodes[shard.NodeID]
		if !ok {
			return NewBizError(CodeRetiredNode, "validate_project", "shard points to an unknown node", map[string]any{"shard": shard.ID, "node": shard.NodeID})
		}
		if node.RetiredAtOrBefore(node.JoinTick) {
			return NewBizError(CodeRetiredNode, "validate_project", "shard points to a node retired before joining", map[string]any{"shard": shard.ID, "node": shard.NodeID})
		}
		if shard.BandwidthMilli <= 0 {
			return NewBizError(CodeBadBandwidthScale, "validate_project", "shard bandwidth must be positive", map[string]any{"shard": shard.ID})
		}
		if shard.PreheatTicks <= 0 {
			return NewBizError(CodeBadRequest, "validate_project", "preheat ticks must be positive", map[string]any{"shard": shard.ID})
		}
		if shard.SourceVersion <= shard.PreviousVersion {
			return NewBizError(CodeBadRequest, "validate_project", "source version must advance previous version", map[string]any{"shard": shard.ID})
		}
	}
	return nil
}

func MustEqualProjects(left, right Project) error {
	if left.SemanticHash() == right.SemanticHash() {
		return nil
	}
	return NewBizError(CodeSemanticHashMismatch, "compare_projects", "normalized projects differ",
		map[string]any{"left": left.SemanticHash(), "right": right.SemanticHash(), "left_schema": left.Schema, "right_schema": right.Schema})
}

func (p Project) String() string {
	return fmt.Sprintf("%s/%s(%d shards, %d nodes)", p.ID, p.Schema, len(p.Shards), len(p.Nodes))
}
