package engine

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"

	"quotakernel/model"
)

var ErrVersionMismatch = errors.New("state version mismatch")

// State is the mutable committed ledger at a logical tick boundary.
type State struct {
	Tick         model.Tick                    `json:"tick"`
	Tenants      map[string]*model.TenantState `json:"tenants"`
	Nodes        map[string]*model.NodeState   `json:"nodes"`
	Queue        []model.QueueItem             `json:"queue"`
	OpenReserves []model.ReserveRecord         `json:"openReserves"`
}

func NewState(project *model.Project) *State {
	state := &State{
		Tenants: make(map[string]*model.TenantState, len(project.Tenants)),
		Nodes:   make(map[string]*model.NodeState, len(project.Nodes)),
	}
	for _, tenant := range project.Tenants {
		t := tenant
		state.Tenants[t.ID] = &model.TenantState{Tenant: t}
	}
	for _, node := range project.Nodes {
		n := node
		state.Nodes[n.ID] = &model.NodeState{Node: n}
	}
	return state
}

func (s *State) Clone() *State {
	data, _ := json.Marshal(s)
	var out State
	_ = json.Unmarshal(data, &out)
	if out.Tenants == nil {
		out.Tenants = map[string]*model.TenantState{}
	}
	if out.Nodes == nil {
		out.Nodes = map[string]*model.NodeState{}
	}
	return &out
}

func (s *State) Hash() string {
	data, err := json.Marshal(s.canonical())
	if err != nil {
		panic(err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func (s *State) canonical() any {
	tenantIDs := make([]string, 0, len(s.Tenants))
	for id := range s.Tenants {
		tenantIDs = append(tenantIDs, id)
	}
	sort.Strings(tenantIDs)
	nodeIDs := make([]string, 0, len(s.Nodes))
	for id := range s.Nodes {
		nodeIDs = append(nodeIDs, id)
	}
	sort.Strings(nodeIDs)
	queue := append([]model.QueueItem(nil), s.Queue...)
	sort.Slice(queue, func(i, j int) bool {
		if queue[i].EnqueuedTick != queue[j].EnqueuedTick {
			return queue[i].EnqueuedTick < queue[j].EnqueuedTick
		}
		if queue[i].Priority != queue[j].Priority {
			return queue[i].Priority > queue[j].Priority
		}
		return queue[i].ID < queue[j].ID
	})
	return struct {
		Tick     model.Tick            `json:"tick"`
		Tenants  []*model.TenantState  `json:"tenants"`
		Nodes    []*model.NodeState    `json:"nodes"`
		Queue    []model.QueueItem     `json:"queue"`
		Reserves []model.ReserveRecord `json:"openReserves"`
	}{
		Tick: s.Tick, Tenants: orderedTenants(s, tenantIDs),
		Nodes: orderedNodes(s, nodeIDs), Queue: queue, Reserves: s.OpenReserves,
	}
}

func orderedTenants(s *State, ids []string) []*model.TenantState {
	out := make([]*model.TenantState, 0, len(ids))
	for _, id := range ids {
		out = append(out, s.Tenants[id])
	}
	return out
}

func orderedNodes(s *State, ids []string) []*model.NodeState {
	out := make([]*model.NodeState, 0, len(ids))
	for _, id := range ids {
		out = append(out, s.Nodes[id])
	}
	return out
}

func (s *State) Snapshot(projectFingerprint string) model.Snapshot {
	tenants := make([]model.TenantState, 0, len(s.Tenants))
	for _, t := range s.Tenants {
		tenants = append(tenants, *t)
	}
	nodes := make([]model.NodeState, 0, len(s.Nodes))
	for _, n := range s.Nodes {
		nodes = append(nodes, *n)
	}
	sort.Slice(tenants, func(i, j int) bool { return tenants[i].ID < tenants[j].ID })
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].ID < nodes[j].ID })
	return model.Snapshot{
		FormatVersion:      model.StateFormatVersion,
		CommittedTick:      s.Tick,
		ProjectFingerprint: projectFingerprint,
		Tenants:            tenants,
		Nodes:              nodes,
		Queue:              append([]model.QueueItem(nil), s.Queue...),
		OpenReserves:       append([]model.ReserveRecord(nil), s.OpenReserves...),
		StateHash:          s.Hash(),
	}
}

func StateFromSnapshot(project *model.Project, snap model.Snapshot) (*State, error) {
	if snap.FormatVersion != model.StateFormatVersion {
		return nil, fmt.Errorf("%w: state format %d, want %d", ErrVersionMismatch, snap.FormatVersion, model.StateFormatVersion)
	}
	state := NewState(project)
	state.Tick = snap.CommittedTick
	state.Queue = append([]model.QueueItem(nil), snap.Queue...)
	state.OpenReserves = append([]model.ReserveRecord(nil), snap.OpenReserves...)
	for _, ts := range snap.Tenants {
		ts := ts
		state.Tenants[ts.ID] = &ts
	}
	for _, ns := range snap.Nodes {
		ns := ns
		state.Nodes[ns.ID] = &ns
	}
	if got := state.Hash(); snap.StateHash != "" && got != snap.StateHash {
		return nil, fmt.Errorf("snapshot hash mismatch: got %s want %s", got, snap.StateHash)
	}
	return state, nil
}

// Fingerprint returns a stable hash of the normalized project declaration.
func Fingerprint(project *model.Project) string {
	data, err := json.Marshal(project)
	if err != nil {
		panic(err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:16])
}
