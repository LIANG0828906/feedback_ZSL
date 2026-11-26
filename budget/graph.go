package budget

import (
	"fmt"
	"sort"
	"strconv"
)

type Node struct {
	ID          string   `json:"id"`
	Kind        string   `json:"kind"`
	ContentHash string   `json:"content_hash"`
	Deps        []string `json:"deps,omitempty"`
	Tick        int      `json:"tick,omitempty"`
}

type Graph struct {
	Nodes map[string]*Node `json:"nodes"`
}

type Invalidation struct {
	NodeID        string      `json:"node_id"`
	Reason        string      `json:"reason"`
	AffectedNodes []string    `json:"affected_nodes"`
	Ticks         []int       `json:"ticks"`
	Ranges        []TickRange `json:"ranges"`
}

type InvalidationPlan struct {
	ChangedNodes  []string       `json:"changed_nodes"`
	Entries       []Invalidation `json:"entries"`
	InvalidTicks  []int          `json:"invalid_ticks"`
	ReusableTicks []int          `json:"reusable_ticks"`
}

func BuildDependencyGraph(project *Project, report *RunReport) (*Graph, error) {
	g := &Graph{Nodes: map[string]*Node{}}
	add := func(node *Node) {
		sort.Strings(node.Deps)
		g.Nodes[node.ID] = node
	}

	resourceID := project.Resource.ID
	add(&Node{
		ID: "resource:" + resourceID + ":max", Kind: "resource_max",
		ContentHash: hashStrings("resource_max", strconv.FormatInt(int64(project.Resource.Max), 10)),
	})
	add(&Node{
		ID: "resource:" + resourceID + ":initial", Kind: "resource_initial",
		ContentHash: hashStrings("resource_initial", strconv.FormatInt(int64(project.Resource.Initial), 10)),
	})
	add(&Node{
		ID: project.Resource.RegenNode, Kind: "regen_expression",
		ContentHash: hashStrings("regen", strconv.FormatInt(int64(project.Resource.RegenFlat), 10)),
	})

	for _, skill := range project.Skills {
		add(&Node{
			ID: "skill:" + skill.ID + ":priority", Kind: "skill_priority",
			ContentHash: hashStrings("priority", strconv.Itoa(skill.Priority)),
		})
		add(&Node{
			ID: "skill:" + skill.ID + ":cost", Kind: "skill_cost",
			ContentHash: hashStrings("cost", strconv.FormatInt(int64(skill.Cost), 10)),
		})
		cdrNode := skill.CDRNode
		if cdrNode == "" {
			cdrNode = "cdr:" + skill.ID
		}
		add(&Node{
			ID: cdrNode, Kind: "cdr_expression",
			ContentHash: hashStrings("cdr", strconv.FormatInt(skill.CDRFlat, 10)),
		})
		add(&Node{
			ID: "skill:" + skill.ID + ":cooldown", Kind: "skill_cooldown",
			ContentHash: hashStrings("cooldown", strconv.FormatInt(skill.CooldownTicks, 10)),
			Deps:        []string{cdrNode},
		})
	}

	for _, group := range project.Groups {
		members := append([]string(nil), group.Members...)
		for _, skill := range project.Skills {
			if skill.GroupID == group.ID {
				members = append(members, skill.ID)
			}
		}
		members = uniqueSorted(members)
		add(&Node{
			ID: "group:" + group.ID, Kind: "shared_cooldown_group",
			ContentHash: hashStrings("group", strconv.FormatInt(group.CooldownTicks, 10), fmt.Sprint(members)),
		})
	}

	if report != nil {
		for _, state := range report.Ticks {
			intentIDs := []string{}
			for _, intent := range project.Intents {
				if intent.Tick == state.Tick {
					intentIDs = append(intentIDs, intent.IntentID+":"+intent.SkillID)
				}
			}
			content := hashStrings(append([]string{"tick", strconv.Itoa(state.Tick)}, intentIDs...)...)
			deps := append([]string(nil), state.Dependencies...)
			add(&Node{
				ID: fmt.Sprintf("tick:%d", state.Tick), Kind: "tick_result",
				ContentHash: content, Deps: deps, Tick: state.Tick,
			})
		}
	}

	if err := g.validateDeps(); err != nil {
		return nil, err
	}
	return g, nil
}

func (g *Graph) validateDeps() error {
	for id, node := range g.Nodes {
		for _, dep := range node.Deps {
			if _, ok := g.Nodes[dep]; !ok {
				return &Error{Code: ECodeStage, Message: "dependency graph references missing node", Node: id, Field: dep}
			}
		}
	}
	return nil
}

// ComputeKeys computes content-addressed keys in topological order. Each key
// includes scoped rule content hash, node ID, parameter version and all direct
// upstream cache keys.
func (g *Graph) ComputeKeys(paramVersion string) (map[string]string, error) {
	keys := map[string]string{}
	visiting := map[string]bool{}
	visited := map[string]bool{}

	var visit func(id string) error
	visit = func(id string) error {
		if visited[id] {
			return nil
		}
		if visiting[id] {
			return &Error{Code: ECodeStage, Message: "dependency cycle detected", Node: id}
		}
		node, ok := g.Nodes[id]
		if !ok {
			return &Error{Code: ECodeStage, Message: "missing graph node", Node: id}
		}
		visiting[id] = true
		for _, dep := range node.Deps {
			if err := visit(dep); err != nil {
				return err
			}
		}
		visiting[id] = false
		visited[id] = true

		upstream := make([]string, 0, len(node.Deps))
		for _, dep := range node.Deps {
			upstream = append(upstream, dep+"="+keys[dep])
		}
		sort.Strings(upstream)
		parts := append([]string{"cache-key", paramVersion, node.ID, node.ContentHash}, upstream...)
		keys[id] = hashStrings(parts...)
		return nil
	}

	ids := make([]string, 0, len(g.Nodes))
	for id := range g.Nodes {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if err := visit(id); err != nil {
			return nil, err
		}
	}
	return keys, nil
}

func (g *Graph) reverseAdjacency() map[string][]string {
	reverse := map[string][]string{}
	for id, node := range g.Nodes {
		for _, dep := range node.Deps {
			reverse[dep] = append(reverse[dep], id)
		}
	}
	for id := range reverse {
		reverse[id] = uniqueSorted(reverse[id])
	}
	return reverse
}

// PlanInvalidation propagates changed nodes through reverse dependencies.
// Tick-chain edges make every tick after the first actually changed settlement
// depend on the new upstream state; earlier ticks remain reusable.
func PlanInvalidation(project *Project, oldGraph *Graph, changedNodes []string) (*InvalidationPlan, error) {
	reverse := oldGraph.reverseAdjacency()
	plan := &InvalidationPlan{ChangedNodes: uniqueSorted(changedNodes)}
	invalidSet := map[int]bool{}

	tickCount := project.TickCount
	allTicks := make([]int, 0, tickCount)
	for i := 0; i < tickCount; i++ {
		allTicks = append(allTicks, i)
	}

	for _, changed := range plan.ChangedNodes {
		if _, ok := oldGraph.Nodes[changed]; !ok {
			return nil, &Error{Code: ECodeSchema, Message: "invalidation node is absent from old graph", Node: changed}
		}
		affected := map[string]bool{changed: true}
		queue := []string{changed}
		for len(queue) > 0 {
			id := queue[0]
			queue = queue[1:]
			for _, dependent := range reverse[id] {
				if !affected[dependent] {
					affected[dependent] = true
					queue = append(queue, dependent)
				}
			}
		}
		affectedIDs := make([]string, 0, len(affected))
		ticks := []int{}
		for id := range affected {
			affectedIDs = append(affectedIDs, id)
			if node, ok := oldGraph.Nodes[id]; ok && node.Kind == "tick_result" {
				ticks = append(ticks, node.Tick)
				invalidSet[node.Tick] = true
			}
		}
		sort.Strings(affectedIDs)
		sort.Ints(ticks)
		plan.Entries = append(plan.Entries, Invalidation{
			NodeID: changed, Reason: "rule content hash changed",
			AffectedNodes: affectedIDs, Ticks: ticks, Ranges: TickRanges(ticks),
		})
	}

	for tick := range invalidSet {
		plan.InvalidTicks = append(plan.InvalidTicks, tick)
	}
	sort.Ints(plan.InvalidTicks)
	invalid := map[int]bool{}
	for _, tick := range plan.InvalidTicks {
		invalid[tick] = true
	}
	for _, tick := range allTicks {
		if !invalid[tick] {
			plan.ReusableTicks = append(plan.ReusableTicks, tick)
		}
	}
	return plan, nil
}

func TickRanges(ticks []int) []TickRange {
	if len(ticks) == 0 {
		return nil
	}
	sorted := append([]int(nil), ticks...)
	sort.Ints(sorted)
	ranges := []TickRange{}
	start, prev := sorted[0], sorted[0]
	for _, tick := range sorted[1:] {
		if tick == prev+1 {
			prev = tick
			continue
		}
		ranges = append(ranges, TickRange{First: start, Last: prev})
		start, prev = tick, tick
	}
	ranges = append(ranges, TickRange{First: start, Last: prev})
	return ranges
}

func FirstInvalidTick(plan *InvalidationPlan) int {
	if len(plan.InvalidTicks) == 0 {
		return -1
	}
	return plan.InvalidTicks[0]
}
