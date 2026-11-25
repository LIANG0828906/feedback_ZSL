package skillcore

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
)

type DependencyNode struct {
	ID   string `json:"id"`
	Kind string `json:"kind"`
	Ref  string `json:"ref"`
}

type DependencyEdge struct {
	From string `json:"from"`
	To   string `json:"to"`
}

type DependencyGraph struct {
	totalTicks int64
	nodes      map[string]DependencyNode
	out        map[string]map[string]struct{}
	in         map[string]map[string]struct{}
}

func BuildDependencyGraph(project *Project, intents []Intent, totalTicks int64) (*DependencyGraph, error) {
	if project == nil {
		return nil, domainErr(CodeInput, "", "nil project")
	}
	if totalTicks < 0 {
		return nil, domainErr(CodeInput, "totalTicks", "must be non-negative")
	}
	clone := project.Clone()
	if err := clone.Normalize(); err != nil {
		return nil, err
	}
	graph := &DependencyGraph{
		totalTicks: totalTicks,
		nodes:      make(map[string]DependencyNode),
		out:        make(map[string]map[string]struct{}),
		in:         make(map[string]map[string]struct{}),
	}
	addNode := func(id, kind, ref string) {
		graph.nodes[id] = DependencyNode{ID: id, Kind: kind, Ref: ref}
	}
	addEdge := func(from, to string) {
		if _, ok := graph.nodes[from]; !ok {
			return
		}
		if _, ok := graph.nodes[to]; !ok {
			return
		}
		if graph.out[from] == nil {
			graph.out[from] = make(map[string]struct{})
		}
		if graph.in[to] == nil {
			graph.in[to] = make(map[string]struct{})
		}
		graph.out[from][to] = struct{}{}
		graph.in[to][from] = struct{}{}
	}

	for i := range clone.Pools {
		pool := &clone.Pools[i]
		addNode("pool:"+pool.ID+":regen", "resource_regen", pool.ID)
		addNode("pool:"+pool.ID+":budget", "resource_budget", pool.ID)
		addEdge("pool:"+pool.ID+":regen", "pool:"+pool.ID+":budget")
	}
	for i := range clone.Skills {
		skill := &clone.Skills[i]
		addNode("skill:"+skill.ID+":cdr", "cooldown_reduction", skill.ID)
		addNode("skill:"+skill.ID+":cooldown", "cooldown", skill.ID)
		addNode("skill:"+skill.ID+":group", "shared_group_binding", skill.ID)
		addEdge("skill:"+skill.ID+":cdr", "skill:"+skill.ID+":cooldown")
		if skill.SharedGroup != "" {
			groupNode := "group:" + skill.SharedGroup
			addNode(groupNode, "shared_group", skill.SharedGroup)
			addEdge("skill:"+skill.ID+":group", groupNode)
		}
	}

	for tick := int64(0); tick < totalTicks; tick++ {
		for i := range clone.Pools {
			poolID := clone.Pools[i].ID
			poolNode := fmt.Sprintf("tick:%d:pool:%s", tick, poolID)
			addNode(poolNode, "tick_pool", poolID)
			addEdge("pool:"+poolID+":regen", poolNode)
			addEdge(poolNode, fmt.Sprintf("tick:%d:snapshot", tick))
			if tick+1 < totalTicks {
				next := fmt.Sprintf("tick:%d:pool:%s", tick+1, poolID)
				addEdge(poolNode, next)
			}
		}
		for i := range clone.Skills {
			skill := &clone.Skills[i]
			skillNode := fmt.Sprintf("tick:%d:skill:%s", tick, skill.ID)
			addNode(skillNode, "tick_skill", skill.ID)
			addEdge("skill:"+skill.ID+":cooldown", skillNode)
			addEdge("pool:"+skill.PoolID+":budget", skillNode)
			if skill.SharedGroup != "" {
				addEdge("group:"+skill.SharedGroup, skillNode)
			}
			addEdge(fmt.Sprintf("tick:%d:pool:%s", tick, skill.PoolID), skillNode)
			addEdge(skillNode, fmt.Sprintf("tick:%d:snapshot", tick))
			if tick+1 < totalTicks {
				next := fmt.Sprintf("tick:%d:skill:%s", tick+1, skill.ID)
				addEdge(skillNode, next)
			}
		}
		addNode(fmt.Sprintf("tick:%d:snapshot", tick), "tick_snapshot", fmt.Sprint(tick))
	}

	byTick := make(map[int64][]Intent)
	for _, intent := range intents {
		if intent.Tick < 0 || intent.Tick >= totalTicks {
			continue
		}
		if clone.SkillByID(intent.SkillID) == nil {
			return nil, &Error{
				Code:    CodeInput,
				Path:    "intent.skillId",
				Message: fmt.Sprintf("unknown skill %q", intent.SkillID),
				Tick:    intent.Tick,
				HasTick: true,
				SkillID: intent.SkillID,
			}
		}
		byTick[intent.Tick] = append(byTick[intent.Tick], intent)
	}
	for tick := int64(0); tick < totalTicks; tick++ {
		intentsAtTick := byTick[tick]
		sortIntents(clone, intentsAtTick)
		for index, intent := range intentsAtTick {
			skill := clone.SkillByID(intent.SkillID)
			decisionNode := fmt.Sprintf("tick:%d:decision:%d:%s", tick, index, skill.ID)
			addNode(decisionNode, "decision", skill.ID)
			addEdge("skill:"+skill.ID+":cooldown", decisionNode)
			addEdge("pool:"+skill.PoolID+":budget", decisionNode)
			addEdge(fmt.Sprintf("tick:%d:skill:%s", tick, skill.ID), decisionNode)
			if skill.SharedGroup != "" {
				addEdge("group:"+skill.SharedGroup, decisionNode)
			}
			addEdge(decisionNode, fmt.Sprintf("tick:%d:snapshot", tick))
			if tick+1 < totalTicks {
				addEdge(decisionNode, fmt.Sprintf("tick:%d:pool:%s", tick+1, skill.PoolID))
				if skill.SharedGroup != "" {
					for _, member := range clone.SkillsInGroup(skill.SharedGroup) {
						addEdge(decisionNode, fmt.Sprintf("tick:%d:skill:%s", tick+1, member.ID))
					}
				}
			}
		}
	}
	return graph, nil
}

func (g *DependencyGraph) Nodes() []DependencyNode {
	out := make([]DependencyNode, 0, len(g.nodes))
	for _, node := range g.nodes {
		out = append(out, node)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func (g *DependencyGraph) Edges() []DependencyEdge {
	out := make([]DependencyEdge, 0)
	for from, targets := range g.out {
		for to := range targets {
			out = append(out, DependencyEdge{From: from, To: to})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].From != out[j].From {
			return out[i].From < out[j].From
		}
		return out[i].To < out[j].To
	})
	return out
}

func (g *DependencyGraph) Dependents(nodeID string) []string {
	out := make([]string, 0, len(g.out[nodeID]))
	for id := range g.out[nodeID] {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

func (g *DependencyGraph) Dependencies(nodeID string) []string {
	out := make([]string, 0, len(g.in[nodeID]))
	for id := range g.in[nodeID] {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

type EditKind string

const (
	EditCooldown    EditKind = "skill_cooldown"
	EditCDR         EditKind = "skill_cdr"
	EditRegen       EditKind = "resource_regen"
	EditSharedGroup EditKind = "shared_group"
)

type Edit struct {
	AtTick  int64    `json:"atTick"`
	Kind    EditKind `json:"kind"`
	SkillID string   `json:"skillId,omitempty"`
	PoolID  string   `json:"poolId,omitempty"`
	Group   string   `json:"group,omitempty"`
	Field   string   `json:"field,omitempty"`
	Reason  string   `json:"reason,omitempty"`
}

type Invalidation struct {
	TickStart int64  `json:"tickStart"`
	TickEnd   int64  `json:"tickEnd"`
	SkillID   string `json:"skillId,omitempty"`
	Field     string `json:"field"`
	Reason    string `json:"reason"`
}

type InvalidationPlan struct {
	Edits           []Edit         `json:"edits"`
	Invalidations   []Invalidation `json:"invalidations"`
	AffectedWindows []int          `json:"affectedWindows"`
	ReusableWindows []int          `json:"reusableWindows"`
}

func (g *DependencyGraph) PlanInvalidation(windowSize int64, edits []Edit) (*InvalidationPlan, error) {
	if windowSize <= 0 {
		return nil, domainErr(CodeInput, "windowSize", "must be positive")
	}
	plan := &InvalidationPlan{Edits: append([]Edit(nil), edits...)}
	affected := make(map[int]struct{})
	ranges := make(map[string]*Invalidation)
	for _, edit := range edits {
		if edit.AtTick < 0 || edit.AtTick >= g.totalTicks {
			continue
		}
		root, err := g.editRoot(edit)
		if err != nil {
			return nil, err
		}
		descendants := g.descendants(root)
		reason := edit.Reason
		if reason == "" {
			reason = string(edit.Kind)
		}
		field := edit.Field
		if field == "" {
			field = defaultEditField(edit)
		}
		for _, nodeID := range descendants {
			node := g.nodes[nodeID]
			tick, skillID, nodeField, ok := parseTickNode(node)
			if !ok || tick < edit.AtTick {
				continue
			}
			if nodeField != "" {
				fieldForNode := nodeField
				if field != "" && node.Kind != "tick_pool" {
					fieldForNode = field
				}
				key := fmt.Sprintf("%s|%s|%s", skillID, fieldForNode, reason)
				if existing, ok := ranges[key]; ok {
					if tick < existing.TickStart {
						existing.TickStart = tick
					}
					if tick > existing.TickEnd {
						existing.TickEnd = tick
					}
				} else {
					ranges[key] = &Invalidation{
						TickStart: tick,
						TickEnd:   tick,
						SkillID:   skillID,
						Field:     fieldForNode,
						Reason:    reason,
					}
				}
			}
			affected[int(tick/windowSize)] = struct{}{}
		}
	}
	for _, item := range ranges {
		plan.Invalidations = append(plan.Invalidations, *item)
	}
	sort.Slice(plan.Invalidations, func(i, j int) bool {
		left, right := plan.Invalidations[i], plan.Invalidations[j]
		if left.TickStart != right.TickStart {
			return left.TickStart < right.TickStart
		}
		if left.SkillID != right.SkillID {
			return left.SkillID < right.SkillID
		}
		if left.Field != right.Field {
			return left.Field < right.Field
		}
		return left.Reason < right.Reason
	})
	windowCount := int((g.totalTicks + windowSize - 1) / windowSize)
	for index := 0; index < windowCount; index++ {
		if _, ok := affected[index]; ok {
			plan.AffectedWindows = append(plan.AffectedWindows, index)
		} else {
			plan.ReusableWindows = append(plan.ReusableWindows, index)
		}
	}
	return plan, nil
}

func (g *DependencyGraph) editRoot(edit Edit) (string, error) {
	switch edit.Kind {
	case EditCooldown:
		if edit.SkillID == "" {
			return "", domainErr(CodeInput, "edit.skillId", "skill id is required")
		}
		node := "skill:" + edit.SkillID + ":cooldown"
		if _, ok := g.nodes[node]; !ok {
			return "", domainErr(CodeInput, "edit.skillId", fmt.Sprintf("unknown skill %q", edit.SkillID))
		}
		return node, nil
	case EditCDR:
		if edit.SkillID == "" {
			return "", domainErr(CodeInput, "edit.skillId", "skill id is required")
		}
		node := "skill:" + edit.SkillID + ":cdr"
		if _, ok := g.nodes[node]; !ok {
			return "", domainErr(CodeInput, "edit.skillId", fmt.Sprintf("unknown skill %q", edit.SkillID))
		}
		return node, nil
	case EditRegen:
		if edit.PoolID == "" {
			return "", domainErr(CodeInput, "edit.poolId", "pool id is required")
		}
		node := "pool:" + edit.PoolID + ":regen"
		if _, ok := g.nodes[node]; !ok {
			return "", domainErr(CodeInput, "edit.poolId", fmt.Sprintf("unknown pool %q", edit.PoolID))
		}
		return node, nil
	case EditSharedGroup:
		if edit.SkillID == "" {
			return "", domainErr(CodeInput, "edit.skillId", "skill id is required")
		}
		node := "skill:" + edit.SkillID + ":group"
		if _, ok := g.nodes[node]; !ok {
			return "", domainErr(CodeInput, "edit.skillId", fmt.Sprintf("unknown skill %q", edit.SkillID))
		}
		return node, nil
	default:
		return "", domainErr(CodeInput, "edit.kind", fmt.Sprintf("unknown edit kind %q", edit.Kind))
	}
}

func defaultEditField(edit Edit) string {
	switch edit.Kind {
	case EditCooldown:
		return "skills." + edit.SkillID + ".cooldown"
	case EditCDR:
		return "skills." + edit.SkillID + ".cdr"
	case EditRegen:
		return "pools." + edit.PoolID + ".regen"
	case EditSharedGroup:
		return "skills." + edit.SkillID + ".group"
	default:
		return "rules"
	}
}

func (g *DependencyGraph) descendants(root string) []string {
	seen := map[string]struct{}{root: {}}
	queue := []string{root}
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		for next := range g.out[current] {
			if _, ok := seen[next]; ok {
				continue
			}
			seen[next] = struct{}{}
			queue = append(queue, next)
		}
	}
	out := make([]string, 0, len(seen))
	for id := range seen {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

func parseTickNode(node DependencyNode) (tick int64, skillID, field string, ok bool) {
	switch node.Kind {
	case "tick_skill":
		var parsed int64
		if _, err := fmt.Sscanf(node.ID, "tick:%d:skill:", &parsed); err != nil {
			return 0, "", "", false
		}
		return parsed, node.Ref, "skills." + node.Ref, true
	case "decision":
		var parsed int64
		if _, err := fmt.Sscanf(node.ID, "tick:%d:decision:", &parsed); err != nil {
			return 0, "", "", false
		}
		return parsed, node.Ref, "decisions." + node.Ref, true
	case "tick_pool":
		var parsed int64
		if _, err := fmt.Sscanf(node.ID, "tick:%d:pool:", &parsed); err != nil {
			return 0, "", "", false
		}
		return parsed, "", "pools." + node.Ref, true
	default:
		return 0, "", "", false
	}
}

// WindowDependencyKey gives the checkpoint cache a stable key for one window.
// Callers may replace it with a narrower key derived from InvalidationPlan when
// applying an edit sequence.
func WindowDependencyKey(project *Project, start, end int64, intents []Intent) string {
	type keyDocument struct {
		RuleHash string   `json:"ruleHash"`
		Start    int64    `json:"start"`
		End      int64    `json:"end"`
		Intents  []Intent `json:"intents"`
	}
	filtered := make([]Intent, 0)
	for _, intent := range intents {
		if intent.Tick >= start && intent.Tick < end {
			filtered = append(filtered, intent)
		}
	}
	sort.Slice(filtered, func(i, j int) bool {
		if filtered[i].Tick != filtered[j].Tick {
			return filtered[i].Tick < filtered[j].Tick
		}
		if filtered[i].SkillID != filtered[j].SkillID {
			return filtered[i].SkillID < filtered[j].SkillID
		}
		if filtered[i].Sequence != filtered[j].Sequence {
			return filtered[i].Sequence < filtered[j].Sequence
		}
		return filtered[i].TargetID < filtered[j].TargetID
	})
	data, _ := json.Marshal(keyDocument{RuleHash: project.ContentHash(), Start: start, End: end, Intents: filtered})
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// EditAwareWindowKeys builds cache keys for a rerun after an edit sequence.
// Windows before every affected tick retain previousKeys and are eligible for
// persistent cache reuse; affected windows receive keys derived from the new
// rule content.
func EditAwareWindowKeys(
	project *Project,
	intents []Intent,
	totalTicks, windowSize int64,
	edits []Edit,
	previousKeys []string,
) ([]string, *InvalidationPlan, error) {
	if windowSize <= 0 {
		return nil, nil, domainErr(CodeInput, "windowSize", "must be positive")
	}
	windowCount := int((totalTicks + windowSize - 1) / windowSize)
	if len(previousKeys) > 0 && len(previousKeys) != windowCount {
		return nil, nil, domainErr(CodeInput, "previousKeys",
			fmt.Sprintf("expected %d keys, got %d", windowCount, len(previousKeys)))
	}
	graph, err := BuildDependencyGraph(project, intents, totalTicks)
	if err != nil {
		return nil, nil, err
	}
	plan, err := graph.PlanInvalidation(windowSize, edits)
	if err != nil {
		return nil, nil, err
	}
	affected := make(map[int]struct{}, len(plan.AffectedWindows))
	for _, index := range plan.AffectedWindows {
		affected[index] = struct{}{}
	}
	keys := make([]string, windowCount)
	for index := 0; index < windowCount; index++ {
		start := int64(index) * windowSize
		end := start + windowSize
		if end > totalTicks {
			end = totalTicks
		}
		_, isAffected := affected[index]
		if !isAffected && len(previousKeys) == windowCount {
			keys[index] = previousKeys[index]
			continue
		}
		keys[index] = WindowDependencyKey(project, start, end, intents)
	}
	return keys, plan, nil
}
