package orchestrator

import (
	"fmt"
	"math"
	"sort"
	"strings"
)

type EventKind string

const (
	EventProbe   EventKind = "probe"
	EventCommit  EventKind = "commit"
	EventRecover EventKind = "recover"
	EventRequest EventKind = "request"
)

type TimeUnit string

const (
	UnitTick TimeUnit = "tick"
	UnitMS   TimeUnit = "ms"
)

type CapacityUnit string

const (
	UnitByte CapacityUnit = "byte"
	UnitKiB  CapacityUnit = "kib"
)

// Ratio is an exact non-negative rational number.
type Ratio struct {
	Num uint64 `json:"num"`
	Den uint64 `json:"den"`
}

// Timeout keeps both the normalized tick duration and its wire provenance.
// Set distinguishes an omitted optional field from a zero timeout.
type Timeout struct {
	Set     bool     `json:"set"`
	Ticks   Ratio    `json:"ticks"`
	Wire    uint64   `json:"wire"`
	Unit    TimeUnit `json:"unit"`
	Default bool     `json:"default"`
}

type Workload struct {
	ID          string `json:"id"`
	NodeID      string `json:"node_id"`
	BudgetBytes int64  `json:"budget_bytes"`
	UsedBytes   int64  `json:"used_bytes"`
}

type Node struct {
	ID        string     `json:"id"`
	Rack      string     `json:"rack"`
	Zone      int64      `json:"zone"`
	Workloads []Workload `json:"workloads"`
	Timeout   Timeout    `json:"timeout"`
}

// Edge means From depends on To. Isolation propagates from To to From.
type Edge struct {
	From string `json:"from"`
	To   string `json:"to"`
}

type Event struct {
	Tick        int64     `json:"tick"`
	Seq         int       `json:"seq"`
	Kind        EventKind `json:"kind"`
	NodeID      string    `json:"node_id,omitempty"`
	DomainID    *int64    `json:"domain_id,omitempty"`
	WorkloadID  string    `json:"workload_id,omitempty"`
	AmountBytes int64     `json:"amount_bytes,omitempty"`
}

type Project struct {
	SchemaVersion         string  `json:"schema_version"`
	ProjectID             string  `json:"project_id"`
	SnapshotID            string  `json:"snapshot_id"`
	Revision              string  `json:"revision"`
	TickMillis            Ratio   `json:"tick_millis"`
	TimeoutBits           uint8   `json:"timeout_bits"`
	DefaultTimeout        Timeout `json:"default_timeout"`
	Nodes                 []Node  `json:"nodes"`
	Edges                 []Edge  `json:"edges"`
	Events                []Event `json:"events"`
	WorkerBudget          int     `json:"worker_budget"`
	PerDomainPendingLimit int     `json:"per_domain_pending_limit"`
}

func (p *Project) Node(id string) (Node, bool) {
	i := sort.Search(len(p.Nodes), func(i int) bool { return p.Nodes[i].ID >= id })
	if i < len(p.Nodes) && p.Nodes[i].ID == id {
		return p.Nodes[i], true
	}
	return Node{}, false
}

func (p *Project) Workload(id string) (Workload, bool) {
	for _, node := range p.Nodes {
		for _, workload := range node.Workloads {
			if workload.ID == id {
				return workload, true
			}
		}
	}
	return Workload{}, false
}

func (p *Project) NodeIndex() map[string]Node {
	result := make(map[string]Node, len(p.Nodes))
	for _, node := range p.Nodes {
		result[node.ID] = node
	}
	return result
}

// Clone returns a deep copy so an incremental solver can branch safely.
func (p *Project) Clone() *Project {
	if p == nil {
		return nil
	}
	clone := *p
	clone.Nodes = make([]Node, len(p.Nodes))
	for i, node := range p.Nodes {
		clone.Nodes[i] = node
		clone.Nodes[i].Workloads = append([]Workload(nil), node.Workloads...)
	}
	clone.Edges = append([]Edge(nil), p.Edges...)
	clone.Events = make([]Event, len(p.Events))
	for i, event := range p.Events {
		clone.Events[i] = event
		if event.DomainID != nil {
			domain := *event.DomainID
			clone.Events[i].DomainID = &domain
		}
	}
	return &clone
}

// Normalize sorts all collections into the canonical traversal order and
// validates cross references. It does not silently drop invalid records.
func (p *Project) Normalize() error {
	if p == nil {
		return newError(CodeInvalidSchema, "$", "nil project")
	}
	if p.SchemaVersion == "" {
		return newError(CodeInvalidSchema, "$.schema", "schema version is required")
	}
	if p.ProjectID == "" {
		return newError(CodeInvalidSchema, "$.project_id", "project id is required")
	}
	if p.TickMillis.Num == 0 || p.TickMillis.Den == 0 {
		return newError(CodeInvalidSchema, "$.tick_millis", "tick duration must be positive")
	}
	if p.TimeoutBits == 0 {
		p.TimeoutBits = 64
	}
	if p.TimeoutBits != 32 && p.TimeoutBits != 64 {
		return newError(CodeInvalidSchema, "$.timeout_bits", "declared timeout width must be 32 or 64")
	}
	if p.WorkerBudget <= 0 {
		p.WorkerBudget = 1
	}
	if p.PerDomainPendingLimit <= 0 {
		p.PerDomainPendingLimit = 64
	}
	fallbackTimeout, err := defaultTimeout(p)
	if err != nil {
		return err
	}

	sort.Slice(p.Nodes, func(i, j int) bool { return p.Nodes[i].ID < p.Nodes[j].ID })
	seenNodes := make(map[string]struct{}, len(p.Nodes))
	seenWorkloads := make(map[string]string)
	for i := range p.Nodes {
		node := &p.Nodes[i]
		if node.ID == "" {
			return newError(CodeInvalidSchema, fmt.Sprintf("$.nodes[%d].id", i), "node id is required")
		}
		if _, ok := seenNodes[node.ID]; ok {
			return newError(CodeDuplicateNode, "$.nodes", "duplicate node %q", node.ID)
		}
		seenNodes[node.ID] = struct{}{}
		node.Workloads = append([]Workload(nil), node.Workloads...)
		sort.Slice(node.Workloads, func(j, k int) bool { return node.Workloads[j].ID < node.Workloads[k].ID })
		for j := range node.Workloads {
			workload := &node.Workloads[j]
			if workload.ID == "" {
				return newError(CodeInvalidSchema, fmt.Sprintf("$.nodes[%s].workloads", node.ID), "workload id is required")
			}
			if owner, ok := seenWorkloads[workload.ID]; ok {
				return newError(CodeDuplicateWorkload, "$.nodes", "workload %q is owned by both %s and %s", workload.ID, owner, node.ID)
			}
			if workload.BudgetBytes < 0 || workload.UsedBytes < 0 || workload.UsedBytes > workload.BudgetBytes {
				return newError(CodeInvalidSchema, fmt.Sprintf("$.nodes[%s].workloads[%s]", node.ID, workload.ID), "budget must be non-negative and used must not exceed budget")
			}
			workload.NodeID = node.ID
			seenWorkloads[workload.ID] = node.ID
		}
		if !node.Timeout.Set {
			node.Timeout = fallbackTimeout
		}
		if err := node.Timeout.Ticks.check(p.TimeoutBits, fmt.Sprintf("$.nodes[%s].timeout", node.ID)); err != nil {
			return err
		}
	}

	sort.Slice(p.Edges, func(i, j int) bool {
		if p.Edges[i].From != p.Edges[j].From {
			return p.Edges[i].From < p.Edges[j].From
		}
		return p.Edges[i].To < p.Edges[j].To
	})
	seenEdges := make(map[Edge]struct{}, len(p.Edges))
	for i, edge := range p.Edges {
		if _, ok := seenNodes[edge.From]; !ok {
			return newError(CodeUnknownNode, fmt.Sprintf("$.edges[%d].from", i), "unknown node %q", edge.From)
		}
		if _, ok := seenNodes[edge.To]; !ok {
			return newError(CodeUnknownNode, fmt.Sprintf("$.edges[%d].to", i), "unknown node %q", edge.To)
		}
		if _, ok := seenEdges[edge]; ok {
			return newError(CodeInvalidSchema, fmt.Sprintf("$.edges[%d]", i), "duplicate dependency %s -> %s", edge.From, edge.To)
		}
		seenEdges[edge] = struct{}{}
	}

	sort.SliceStable(p.Events, func(i, j int) bool {
		a, b := p.Events[i], p.Events[j]
		if a.Tick != b.Tick {
			return a.Tick < b.Tick
		}
		if a.Seq != b.Seq {
			return a.Seq < b.Seq
		}
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		if a.NodeID != b.NodeID {
			return a.NodeID < b.NodeID
		}
		return a.WorkloadID < b.WorkloadID
	})
	lastTick, lastSeq := int64(math.MinInt64), math.MinInt
	for i, event := range p.Events {
		path := fmt.Sprintf("$.events[%d]", i)
		if event.Tick < 0 {
			return newError(CodeInvalidSchema, path+".tick", "logical tick must be non-negative")
		}
		if event.Tick == lastTick && event.Seq == lastSeq {
			return newError(CodeInvalidEvent, path+".seq", "event sequence must be unique within tick %d", event.Tick)
		}
		lastTick, lastSeq = event.Tick, event.Seq
		switch event.Kind {
		case EventProbe, EventRecover:
			if _, ok := seenNodes[event.NodeID]; !ok {
				return newError(CodeUnknownNode, path+".node_id", "unknown node %q", event.NodeID)
			}
		case EventCommit:
			if event.DomainID == nil {
				if _, ok := seenNodes[event.NodeID]; !ok {
					return newError(CodeUnknownNode, path+".node_id", "commit requires a known node or domain")
				}
			}
		case EventRequest:
			workload, ok := lookupWorkload(p.Nodes, event.WorkloadID)
			if !ok {
				return newError(CodeUnknownWorkload, path+".workload_id", "unknown workload %q", event.WorkloadID)
			}
			event.NodeID = workload.NodeID
			p.Events[i].NodeID = workload.NodeID
			if event.AmountBytes <= 0 {
				return newError(CodeInvalidEvent, path+".amount_bytes", "request amount must be positive")
			}
		default:
			return newError(CodeInvalidEvent, path+".kind", "unknown event kind %q", event.Kind)
		}
	}
	return nil
}

func lookupWorkload(nodes []Node, id string) (Workload, bool) {
	for _, node := range nodes {
		for _, workload := range node.Workloads {
			if workload.ID == id {
				return workload, true
			}
		}
	}
	return Workload{}, false
}

func (r *Ratio) normalize() error {
	if r.Num == 0 || r.Den == 0 {
		return newError(CodeInvalidSchema, "$.ratio", "ratio terms must be positive (num=%d den=%d)", r.Num, r.Den)
	}
	g := gcd(r.Num, r.Den)
	r.Num /= g
	r.Den /= g
	return nil
}

func (r Ratio) check(bits uint8, path string) error {
	if r.Den == 0 {
		return newError(CodeInvalidSchema, path, "timeout denominator must be positive")
	}
	var limit uint64
	if bits == 32 {
		limit = math.MaxUint32
	} else {
		limit = math.MaxUint64
	}
	if r.Num > limit || r.Den > limit {
		return newError(CodeTimeoutOverflow, path, "normalized timeout exceeds declared %d-bit width", bits)
	}
	return nil
}

func gcd(a, b uint64) uint64 {
	for b != 0 {
		a, b = b, a%b
	}
	return a
}

func (r Ratio) String() string {
	if r.Den == 1 {
		return fmt.Sprintf("%d", r.Num)
	}
	return fmt.Sprintf("%d/%d", r.Num, r.Den)
}

// DeadlineTicks returns the first integer tick at which elapsed >= timeout.
// It is the exact ceiling of Num/Den without overflowing Num+Den.
func (r Ratio) DeadlineTicks() (uint64, error) {
	if r.Den == 0 {
		return 0, newError(CodeInvalidSchema, "$.timeout", "timeout denominator must be positive")
	}
	if r.Num == 0 {
		return 0, nil
	}
	ticks := r.Num / r.Den
	if r.Num%r.Den != 0 {
		if ticks == math.MaxUint64 {
			return 0, newError(CodeTimeoutOverflow, "$.timeout", "deadline exceeds uint64")
		}
		ticks++
	}
	return ticks, nil
}

// TimedOutAt reports the exact boundary decision. It avoids multiplication
// entirely, so a large elapsed tick count cannot overflow or accumulate error.
func (r Ratio) TimedOutAt(elapsed uint64) bool {
	if r.Den == 0 {
		return false
	}
	if r.Num == 0 {
		return true
	}
	whole, remainder := r.Num/r.Den, r.Num%r.Den
	if elapsed < whole {
		return false
	}
	if elapsed > whole {
		return true
	}
	return remainder == 0
}

func checkedMul(a, b uint64) (uint64, bool) {
	if a == 0 || b == 0 {
		return 0, true
	}
	if a > math.MaxUint64/b {
		return 0, false
	}
	return a * b, true
}

func checkedAdd(a, b uint64) (uint64, bool) {
	if a > math.MaxUint64-b {
		return 0, false
	}
	return a + b, true
}

func formatIDs(ids []string) string {
	return strings.Join(ids, ",")
}
