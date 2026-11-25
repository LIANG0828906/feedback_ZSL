package graph

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	"replaybench/internal/engine"
	"replaybench/internal/model"
)

type Node struct {
	ID        string   `json:"id"`
	Kind      string   `json:"kind"`
	Round     int      `json:"round,omitempty"`
	InputHash string   `json:"input_hash"`
	DependsOn []string `json:"depends_on"`
	CacheKey  string   `json:"cache_key"`
}

type Graph struct {
	ReplayID      string
	ParamsVersion string
	Nodes         map[string]*Node
	NodeOrder     []string
	RoundNodeIDs  map[int]string
}

type Interval struct {
	From int `json:"from"`
	To   int `json:"to"`
}

type Report struct {
	TriggerNode    string     `json:"trigger_node"`
	ChangedNodeIDs []string   `json:"changed_node_ids"`
	InvalidRounds  []int      `json:"invalid_rounds"`
	ReusedRounds   []int      `json:"reused_rounds"`
	Intervals      []Interval `json:"intervals"`
}

type Cache interface {
	GetNode(cacheKey string) ([]byte, bool, error)
	PutNode(nodeID string, round int, oldKey, newKey string, payload []byte) error
}

type incrementalResult struct {
	snapshot model.RoundSnapshot
	err      error
}

func Build(r *model.Replay) *Graph {
	g := &Graph{
		ReplayID: r.ID, ParamsVersion: model.ParamsVersion,
		Nodes: map[string]*Node{}, RoundNodeIDs: map[int]string{},
	}
	add := func(n *Node) {
		n.CacheKey = cacheKey(n.InputHash, n.ID, g.ParamsVersion, n.DependsOn, g.Nodes)
		g.Nodes[n.ID] = n
		g.NodeOrder = append(g.NodeOrder, n.ID)
	}

	add(&Node{ID: "input:replay", Kind: "input", InputHash: r.ContentHash})
	add(&Node{ID: "rule:weights", Kind: "action_weights", InputHash: hash(r.Rules.Weights), DependsOn: []string{"input:replay"}})
	add(&Node{ID: "rule:cooldown_reduction", Kind: "cooldown_reduction", InputHash: hash(r.Rules.CooldownReduction), DependsOn: []string{"input:replay"}})
	add(&Node{ID: "rule:flags", Kind: "conditional_flags", InputHash: hash(r.Rules.Flags), DependsOn: []string{"input:replay"}})
	add(&Node{ID: "rule:formation", Kind: "formation_reorder", InputHash: hash(r.Rules.Formation), DependsOn: []string{"input:replay"}})

	editIDs := []string{}
	for i, e := range r.Edits {
		id := editNodeID(e, i)
		deps := []string{"input:replay"}
		switch e.Type {
		case "weight":
			deps = append(deps, "rule:weights")
		case "cooldown_reduction":
			deps = append(deps, "rule:cooldown_reduction")
		case "flag":
			deps = append(deps, "rule:flags")
		case "reorder":
			deps = append(deps, "rule:formation")
		}
		add(&Node{ID: id, Kind: "edit_" + e.Type, Round: e.Round, InputHash: hash(e), DependsOn: deps})
		editIDs = append(editIDs, id)
	}

	for round := 1; round <= r.Rounds; round++ {
		contentID := fmt.Sprintf("content:round:%d", round)
		var directive model.Directive
		if round-1 < len(r.Directives) {
			directive = r.Directives[round-1]
		}
		relevant := struct {
			Directive model.Directive `json:"directive"`
			Skills    []model.Skill   `json:"skills"`
			Units     []model.Unit    `json:"units"`
			Time      model.TimeBasis `json:"time"`
		}{directive, r.Skills, r.Units, r.Time}
		add(&Node{ID: contentID, Kind: "round_content", Round: round, InputHash: hash(relevant), DependsOn: []string{"input:replay"}})

		snapID := fmt.Sprintf("snapshot:round:%d", round)
		deps := []string{contentID, "rule:weights", "rule:cooldown_reduction", "rule:flags", "rule:formation"}
		if round > 1 {
			deps = append(deps, fmt.Sprintf("snapshot:round:%d", round-1))
		}
		for i, e := range r.Edits {
			if e.Round <= round {
				deps = append(deps, editNodeID(e, i))
			}
		}
		sort.Strings(deps)
		deps = unique(deps)
		add(&Node{ID: snapID, Kind: "round_snapshot", Round: round, InputHash: hash(round), DependsOn: deps})
		g.RoundNodeIDs[round] = snapID
	}
	return g
}

func PlanInvalidation(old, next *Graph) Report {
	rep := Report{InvalidRounds: []int{}, ReusedRounds: []int{}, ChangedNodeIDs: []string{}}
	if old == nil {
		for round := 1; round <= next.roundCount(); round++ {
			rep.InvalidRounds = append(rep.InvalidRounds, round)
		}
		rep.Intervals = compress(rep.InvalidRounds)
		return rep
	}
	for id, nn := range next.Nodes {
		on, ok := old.Nodes[id]
		if !ok || on.CacheKey != nn.CacheKey {
			rep.ChangedNodeIDs = append(rep.ChangedNodeIDs, id)
		}
	}
	sort.Strings(rep.ChangedNodeIDs)
	for _, id := range rep.ChangedNodeIDs {
		n := next.Nodes[id]
		if n != nil && n.Kind != "round_snapshot" && n.Kind != "round_content" {
			rep.TriggerNode = id
			break
		}
	}
	for round := 1; round <= next.roundCount(); round++ {
		id := next.RoundNodeIDs[round]
		nn := next.Nodes[id]
		on := old.Nodes[id]
		if on == nil || on.CacheKey != nn.CacheKey {
			rep.InvalidRounds = append(rep.InvalidRounds, round)
		} else {
			rep.ReusedRounds = append(rep.ReusedRounds, round)
		}
	}
	rep.Intervals = compress(rep.InvalidRounds)
	return rep
}

func CacheMissReport(g *Graph, c Cache) (Report, error) {
	rep := Report{InvalidRounds: []int{}, ReusedRounds: []int{}}
	for round := 1; round <= g.roundCount(); round++ {
		key := g.Nodes[g.RoundNodeIDs[round]].CacheKey
		_, ok, err := c.GetNode(key)
		if err != nil {
			return rep, err
		}
		if ok {
			rep.ReusedRounds = append(rep.ReusedRounds, round)
		} else {
			rep.InvalidRounds = append(rep.InvalidRounds, round)
		}
	}
	rep.Intervals = compress(rep.InvalidRounds)
	return rep, nil
}

type RunReport struct {
	Snapshots       []model.RoundSnapshot `json:"-"`
	Invalidation    Report                `json:"invalidation"`
	CacheMissRounds []int                 `json:"cache_miss_rounds"`
	CacheHitRounds  []int                 `json:"cache_hit_rounds"`
}

func RunIncremental(ctx context.Context, r *model.Replay, cache Cache) (*RunReport, error) {
	g := Build(r)
	plan, err := CacheMissReport(g, cache)
	if err != nil {
		return nil, err
	}
	out := make([]model.RoundSnapshot, 0, r.Rounds)
	var st *engine.RuntimeState
	hitRounds := []int{}
	missRounds := []int{}
	for round := 1; round <= r.Rounds; round++ {
		nodeID := g.RoundNodeIDs[round]
		key := g.Nodes[nodeID].CacheKey
		payload, hit, err := cache.GetNode(key)
		if err != nil {
			return nil, err
		}
		var snap model.RoundSnapshot
		if hit {
			if err := json.Unmarshal(payload, &snap); err != nil {
				hit = false
			}
		}
		if hit {
			hitRounds = append(hitRounds, round)
			st = engine.StateFromSnapshot(snap)
			out = append(out, snap)
			continue
		}
		if round == 1 || st == nil {
			st = engine.NewState(r)
		}
		snap, err = engine.Step(ctx, r, st, round)
		if err != nil {
			return nil, err
		}
		b, err := json.Marshal(snap)
		if err != nil {
			return nil, err
		}
		if err := cache.PutNode(nodeID, round, "", key, b); err != nil {
			return nil, err
		}
		missRounds = append(missRounds, round)
		out = append(out, snap)
	}
	plan.ReusedRounds = append([]int(nil), hitRounds...)
	return &RunReport{Snapshots: out, Invalidation: plan, CacheMissRounds: missRounds, CacheHitRounds: hitRounds}, nil
}

func editNodeID(e model.Edit, index int) string {
	switch e.Type {
	case "weight":
		return fmt.Sprintf("edit:weight:%s@%d", e.UnitID, e.Round)
	case "flag":
		return fmt.Sprintf("edit:flag:%s@%d", e.Flag, e.Round)
	case "reorder":
		return fmt.Sprintf("edit:reorder@%d", e.Round)
	case "cooldown_reduction":
		return fmt.Sprintf("edit:cooldown_reduction@%d", e.Round)
	default:
		return fmt.Sprintf("edit:%s@%d#%d", e.Type, e.Round, index)
	}
}

func cacheKey(inputHash, id, paramsVersion string, deps []string, nodes map[string]*Node) string {
	depKeys := make([]string, 0, len(deps))
	for _, d := range deps {
		if n, ok := nodes[d]; ok {
			depKeys = append(depKeys, n.CacheKey)
		} else {
			depKeys = append(depKeys, "missing:"+d)
		}
	}
	sort.Strings(depKeys)
	material := struct {
		InputHash     string   `json:"input_hash"`
		NodeID        string   `json:"node_id"`
		ParamsVersion string   `json:"params_version"`
		DepKeys       []string `json:"dep_keys"`
	}{inputHash, id, paramsVersion, depKeys}
	return model.SHA256Hex(model.CanonicalJSON(material))
}

func hash(v any) string {
	return model.SHA256Hex(model.CanonicalJSON(v))
}

func unique(in []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, s := range in {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	sort.Strings(out)
	return out
}

func compress(rounds []int) []Interval {
	if len(rounds) == 0 {
		return []Interval{}
	}
	out := []Interval{}
	start, prev := rounds[0], rounds[0]
	for _, r := range rounds[1:] {
		if r == prev+1 {
			prev = r
			continue
		}
		out = append(out, Interval{start, prev})
		start, prev = r, r
	}
	out = append(out, Interval{start, prev})
	return out
}

func (g *Graph) roundCount() int {
	max := 0
	for r := range g.RoundNodeIDs {
		if r > max {
			max = r
		}
	}
	return max
}
