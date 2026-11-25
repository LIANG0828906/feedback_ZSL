package engine

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"replaybench/internal/eerr"
	"replaybench/internal/fixed"
	"replaybench/internal/model"
)

type RuntimeState struct {
	Units         map[string]*UnitRuntime
	GlobalGroupCD map[string]int64
}

type UnitRuntime struct {
	Unit           model.Unit
	Alive          bool
	HP             fixed.P
	Resource       fixed.P
	Cooldowns      map[string]int64
	GroupCooldowns map[string]int64
}

type Result struct {
	Snapshots  []model.RoundSnapshot
	Verdict    string
	WinnerTeam string
}

type intent struct {
	seq      int
	skill    model.Skill
	unitID   string
	weight   int64
	queuePos int
	key      string
}

func NewState(r *model.Replay) *RuntimeState {
	st := &RuntimeState{Units: map[string]*UnitRuntime{}, GlobalGroupCD: map[string]int64{}}
	for _, u := range r.Units {
		cds := map[string]int64{}
		groups := map[string]int64{}
		if u.CooldownSkillID != "" && u.CooldownRemain > 0 {
			cds[u.CooldownSkillID] = u.CooldownRemain
			if sk, ok := r.SkillByID(u.CooldownSkillID); ok && sk.CooldownGroup != "" && sk.CooldownScope == "unit" {
				groups[sk.CooldownGroup] = u.CooldownRemain
			}
		}
		st.Units[u.ID] = &UnitRuntime{
			Unit: u, Alive: u.HP > 0, HP: u.HP, Resource: u.Resource,
			Cooldowns: cds, GroupCooldowns: groups,
		}
	}
	return st
}

func StateFromSnapshot(snap model.RoundSnapshot) *RuntimeState {
	st := &RuntimeState{Units: map[string]*UnitRuntime{}, GlobalGroupCD: map[string]int64{}}
	for _, s := range snap.States {
		cds := map[string]int64{}
		groups := map[string]int64{}
		for k, v := range s.Cooldowns {
			if v > 0 {
				cds[k] = v
			}
		}
		for k, v := range s.GroupCooldowns {
			if v > 0 {
				groups[k] = v
			}
		}
		st.Units[s.UnitID] = &UnitRuntime{
			Unit: model.Unit{
				ID: s.UnitID, Team: s.Team, MaxHP: s.MaxHP, MaxResource: s.MaxResource,
			},
			Alive: s.Alive, HP: s.HP, Resource: s.Resource,
			Cooldowns: cds, GroupCooldowns: groups,
		}
	}
	for k, v := range snap.GlobalCooldowns {
		if v > 0 {
			st.GlobalGroupCD[k] = v
		}
	}
	return st
}

func Run(ctx context.Context, r *model.Replay) (*Result, error) {
	st := NewState(r)
	snaps := make([]model.RoundSnapshot, 0, r.Rounds)
	for round := 1; round <= r.Rounds; round++ {
		if err := ctx.Err(); err != nil {
			return nil, eerr.New(eerr.ECanceled, "$", "canceled before round %d: %v", round, err)
		}
		snap, err := Step(ctx, r, st, round)
		if err != nil {
			return nil, err
		}
		snaps = append(snaps, snap)
	}
	verdict, winner := Verdict(snaps[len(snaps)-1])
	return &Result{Snapshots: snaps, Verdict: verdict, WinnerTeam: winner}, nil
}

func Step(ctx context.Context, r *model.Replay, st *RuntimeState, round int) (model.RoundSnapshot, error) {
	if round < 1 || round > len(r.Directives) {
		return model.RoundSnapshot{}, eerr.Schema("$.rounds", "no directive for internal round %d", round)
	}
	directive := r.Directives[round-1]
	rules, err := RulesForRound(r, round)
	if err != nil {
		return model.RoundSnapshot{}, err
	}
	queuePos := map[string]int{}
	for i, id := range directive.ActorQueue {
		queuePos[id] = i
	}
	if rules.Queue != nil {
		queuePos = map[string]int{}
		for i, id := range rules.Queue {
			queuePos[id] = i
		}
	}

	intents := make([]intent, 0, len(directive.CastSequence))
	for seq, skillID := range directive.CastSequence {
		sk, ok := r.SkillByID(skillID)
		if !ok {
			return model.RoundSnapshot{}, eerr.Schema(fmt.Sprintf("$.directives[%d].cast_sequence[%d]", round-1, seq), "unknown skill %q", skillID)
		}
		unit := st.Units[sk.OwnerID]
		if unit == nil {
			return model.RoundSnapshot{}, eerr.Schema("$.units", "runtime unit %q missing", sk.OwnerID)
		}
		weight := rules.Weights[sk.OwnerID]
		pos, ok := queuePos[sk.OwnerID]
		if !ok {
			pos = 1_000_000 + seq
		}
		it := intent{
			seq: seq, skill: sk, unitID: sk.OwnerID, weight: weight, queuePos: pos,
			key: fmt.Sprintf("w=%d,q=%d,p=%d,u=%s,s=%s,i=%d", weight, pos, sk.Priority, sk.OwnerID, sk.ID, seq),
		}
		intents = append(intents, it)
	}
	ordered, err := arbitrate(intents, r.Rules.Precedence)
	if err != nil {
		return model.RoundSnapshot{}, err
	}

	adjudications := make([]model.Adjudication, 0, len(ordered))
	effective := []string{}
	for _, idx := range ordered {
		if err := ctx.Err(); err != nil {
			return model.RoundSnapshot{}, eerr.New(eerr.ECanceled, "$", "canceled during round %d: %v", round, err)
		}
		it := intents[idx]
		unit := st.Units[it.unitID]
		if it.skill.ConditionFlag != "" && !rules.Flags[it.skill.ConditionFlag] {
			adjudications = append(adjudications, decision(round, it, "condition_flag", "skipped", "conditional flag is false", nil))
			continue
		}
		if !unit.Alive {
			adjudications = append(adjudications, decision(round, it, "unit_state", "rejected", "caster is dead", nil))
			continue
		}
		if remain := unit.Cooldowns[it.skill.ID]; remain > 0 {
			adjudications = append(adjudications, decision(round, it, "cooldown", "rejected", fmt.Sprintf("skill cooldown remains %d", remain), nil))
			continue
		}
		if it.skill.CooldownGroup != "" {
			remain := groupCooldown(st, unit, it.skill)
			if remain > 0 {
				set := groupContenders(intents, it.skill.CooldownGroup)
				adjudications = append(adjudications, decision(round, it, "cooldown_group", "rejected", fmt.Sprintf("shared cooldown remains %d", remain), set))
				continue
			}
		}
		if unit.Resource < it.skill.ResourceCost {
			set := resourceContenders(intents, it.unitID)
			adjudications = append(adjudications, decision(round, it, "resource", "rejected",
				fmt.Sprintf("resource %s < cost %s", fixed.String(unit.Resource), fixed.String(it.skill.ResourceCost)), set))
			continue
		}

		newResource, err := fixed.AddChecked(unit.Resource, -it.skill.ResourceCost, "$.resource")
		if err != nil {
			return model.RoundSnapshot{}, err
		}
		unit.Resource = fixed.Clamp(newResource, 0, unit.Unit.MaxResource)
		if it.skill.Damage > 0 {
			target := st.Units[it.skill.TargetID]
			if target == nil {
				return model.RoundSnapshot{}, eerr.Schema("$.skills.target_id", "target %q missing", it.skill.TargetID)
			}
			nextHP, err := fixed.AddChecked(target.HP, -it.skill.Damage, "$.hp")
			if err != nil {
				return model.RoundSnapshot{}, err
			}
			target.HP = fixed.Clamp(nextHP, 0, target.Unit.MaxHP)
			target.Alive = target.HP > 0
		}
		if it.skill.Heal > 0 {
			target := st.Units[it.skill.TargetID]
			if target == nil {
				target = unit
			}
			next, err := fixed.AddChecked(target.HP, it.skill.Heal, "$.hp")
			if err != nil {
				return model.RoundSnapshot{}, err
			}
			target.HP = fixed.Clamp(next, 0, target.Unit.MaxHP)
			target.Alive = target.HP > 0
		}
		if it.skill.GrantResource > 0 {
			next, err := fixed.AddChecked(unit.Resource, it.skill.GrantResource, "$.resource")
			if err != nil {
				return model.RoundSnapshot{}, err
			}
			unit.Resource = fixed.Clamp(next, 0, unit.Unit.MaxResource)
		}
		cd := it.skill.Cooldown - rules.CooldownReduction
		if cd < 0 {
			cd = 0
		}
		if cd > 0 {
			unit.Cooldowns[it.skill.ID] = cd
			if it.skill.CooldownGroup != "" && it.skill.CooldownScope == "unit" {
				unit.GroupCooldowns[it.skill.CooldownGroup] = cd
			}
			if it.skill.CooldownGroup != "" && it.skill.CooldownScope == "global" {
				st.GlobalGroupCD[it.skill.CooldownGroup] = cd
			}
		}
		cType := "none"
		var contenders []string
		if it.skill.CooldownGroup != "" && len(groupContenders(intents, it.skill.CooldownGroup)) > 1 {
			cType = "cooldown_group"
			contenders = groupContenders(intents, it.skill.CooldownGroup)
		} else if len(resourceContenders(intents, it.unitID)) > 1 {
			cType = "resource"
			contenders = resourceContenders(intents, it.unitID)
		}
		adjudications = append(adjudications, decision(round, it, cType, "cast", "priority order admitted", contenders))
		effective = append(effective, it.skill.ID)
	}

	for _, uid := range sortedRuntimeUnitIDs(st) {
		u := st.Units[uid]
		if !u.Alive {
			continue
		}
		next, err := fixed.AddChecked(u.Resource, rules.Regen, "$.resource_regen")
		if err != nil {
			return model.RoundSnapshot{}, err
		}
		u.Resource = fixed.Clamp(next, 0, u.Unit.MaxResource)
	}
	tickCooldowns(st)

	snap := st.Snapshot(r, round, directive.Offset+1)
	snap.Adjudications = adjudications
	snap.EffectiveSkills = effective
	snap.StateDigest = model.SnapshotDigest(snap)
	return snap, nil
}

type RoundRules struct {
	Weights           map[string]int64
	CooldownReduction int64
	Regen             fixed.P
	Flags             map[string]bool
	Queue             []string
}

func RulesForRound(r *model.Replay, round int) (RoundRules, error) {
	rr := RoundRules{
		Weights: map[string]int64{}, CooldownReduction: r.Rules.CooldownReduction,
		Regen: r.Rules.ResourceRegen, Flags: map[string]bool{},
	}
	for k, v := range r.Rules.Weights {
		rr.Weights[k] = v
	}
	for k, v := range r.Rules.Flags {
		rr.Flags[k] = v
	}
	for _, e := range r.Edits {
		if e.Round > round {
			continue
		}
		switch e.Type {
		case "weight":
			rr.Weights[e.UnitID] = e.Value
		case "cooldown_reduction":
			rr.CooldownReduction = e.Value
		case "resource_regen":
			regen, err := fixed.FromWhole(e.Value, "$.edits.resource_regen")
			if err != nil {
				return rr, err
			}
			rr.Regen = regen
		case "flag":
			rr.Flags[e.Flag] = e.BoolValue
		case "reorder":
			rr.Queue = append([]string(nil), e.Order...)
		}
	}
	return rr, nil
}

func (st *RuntimeState) Snapshot(r *model.Replay, round, consumedOffset int) model.RoundSnapshot {
	snap := model.RoundSnapshot{
		ReplayID: r.ID, Round: round, ConsumedOffset: consumedOffset,
		ContentHash: r.ContentHash, SourceFormat: r.Format, SourceFormatVer: r.SourceFormatVersion,
		ParamsVersion: model.ParamsVersion, AlgorithmVersion: model.AlgorithmVersion,
		States: []model.UnitSnapshot{}, GlobalCooldowns: map[string]int64{},
	}
	for _, id := range sortedRuntimeUnitIDs(st) {
		u := st.Units[id]
		cds := map[string]int64{}
		groups := map[string]int64{}
		for k, v := range u.Cooldowns {
			if v > 0 {
				cds[k] = v
			}
		}
		for k, v := range u.GroupCooldowns {
			if v > 0 {
				groups[k] = v
			}
		}
		snap.States = append(snap.States, model.UnitSnapshot{
			UnitID: id, Team: u.Unit.Team, Alive: u.Alive, HP: u.HP, MaxHP: u.Unit.MaxHP,
			Resource: u.Resource, MaxResource: u.Unit.MaxResource,
			Cooldowns: cds, GroupCooldowns: groups,
		})
	}
	for k, v := range st.GlobalGroupCD {
		if v > 0 {
			snap.GlobalCooldowns[k] = v
		}
	}
	return snap
}

func Verdict(snap model.RoundSnapshot) (string, string) {
	teams := map[string]bool{}
	teamHP := map[string]fixed.P{}
	for _, s := range snap.States {
		if s.Alive {
			teams[s.Team] = true
			teamHP[s.Team] += s.HP
		}
	}
	if len(teams) == 1 {
		for team := range teams {
			return "win", team
		}
	}
	keys := make([]string, 0, len(teamHP))
	for k := range teamHP {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if len(keys) >= 2 && teamHP[keys[0]] != teamHP[keys[1]] {
		if teamHP[keys[0]] > teamHP[keys[1]] {
			return "decision", keys[0]
		}
		return "decision", keys[1]
	}
	return "draw", ""
}

func arbitrate(intents []intent, precedence [][2]string) ([]int, error) {
	if len(precedence) == 0 {
		idx := make([]int, len(intents))
		for i := range idx {
			idx[i] = i
		}
		sort.SliceStable(idx, func(a, b int) bool { return lessIntent(intents[idx[a]], intents[idx[b]]) })
		return idx, nil
	}
	skillNodes := map[string][]int{}
	for i, it := range intents {
		skillNodes[it.skill.ID] = append(skillNodes[it.skill.ID], i)
	}
	indegree := make([]int, len(intents))
	edges := map[int][]int{}
	for _, edge := range precedence {
		froms := skillNodes[edge[0]]
		tos := skillNodes[edge[1]]
		if len(froms) == 0 || len(tos) == 0 {
			continue
		}
		for _, from := range froms {
			for _, to := range tos {
				edges[from] = append(edges[from], to)
				indegree[to]++
			}
		}
	}
	available := []int{}
	for i, d := range indegree {
		if d == 0 {
			available = append(available, i)
		}
	}
	out := make([]int, 0, len(intents))
	for len(available) > 0 {
		sort.SliceStable(available, func(a, b int) bool {
			return lessIntent(intents[available[a]], intents[available[b]])
		})
		pick := available[0]
		available = available[1:]
		out = append(out, pick)
		for _, to := range edges[pick] {
			indegree[to]--
			if indegree[to] == 0 {
				available = append(available, to)
			}
		}
	}
	if len(out) != len(intents) {
		set := []string{}
		seen := map[string]bool{}
		for i, d := range indegree {
			if d > 0 && !seen[intents[i].skill.ID] {
				seen[intents[i].skill.ID] = true
				set = append(set, intents[i].skill.ID)
			}
		}
		sort.Strings(set)
		return nil, eerr.Conflict("$.rules.precedence", set, "unresolvable precedence cycle among %s", strings.Join(set, ","))
	}
	return out, nil
}

func lessIntent(a, b intent) bool {
	if a.weight != b.weight {
		return a.weight > b.weight
	}
	if a.queuePos != b.queuePos {
		return a.queuePos < b.queuePos
	}
	if a.skill.Priority != b.skill.Priority {
		return a.skill.Priority > b.skill.Priority
	}
	if a.unitID != b.unitID {
		return a.unitID < b.unitID
	}
	if a.skill.ID != b.skill.ID {
		return a.skill.ID < b.skill.ID
	}
	return a.seq < b.seq
}

func decision(round int, it intent, cType, result, reason string, set []string) model.Adjudication {
	if set != nil {
		set = append([]string(nil), set...)
		sort.Strings(set)
	}
	return model.Adjudication{
		Round: round, UnitID: it.unitID, SkillID: it.skill.ID,
		ContentionType: cType, Result: result, Reason: reason,
		ContendingSkills: set, PriorityKey: it.key,
	}
}

func resourceContenders(intents []intent, unitID string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, it := range intents {
		if it.unitID == unitID && it.skill.ResourceCost > 0 && !seen[it.skill.ID] {
			seen[it.skill.ID] = true
			out = append(out, it.skill.ID)
		}
	}
	sort.Strings(out)
	return out
}

func groupContenders(intents []intent, group string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, it := range intents {
		if it.skill.CooldownGroup == group && !seen[it.skill.ID] {
			seen[it.skill.ID] = true
			out = append(out, it.skill.ID)
		}
	}
	sort.Strings(out)
	return out
}

func groupCooldown(st *RuntimeState, unit *UnitRuntime, sk model.Skill) int64 {
	if sk.CooldownScope == "global" {
		return st.GlobalGroupCD[sk.CooldownGroup]
	}
	return unit.GroupCooldowns[sk.CooldownGroup]
}

func tickCooldowns(st *RuntimeState) {
	for _, uid := range sortedRuntimeUnitIDs(st) {
		u := st.Units[uid]
		skillKeys := make([]string, 0, len(u.Cooldowns))
		for k := range u.Cooldowns {
			skillKeys = append(skillKeys, k)
		}
		sort.Strings(skillKeys)
		for _, k := range skillKeys {
			if u.Cooldowns[k] > 0 {
				u.Cooldowns[k]--
				if u.Cooldowns[k] == 0 {
					delete(u.Cooldowns, k)
				}
			}
		}
		groupKeys := make([]string, 0, len(u.GroupCooldowns))
		for k := range u.GroupCooldowns {
			groupKeys = append(groupKeys, k)
		}
		sort.Strings(groupKeys)
		for _, k := range groupKeys {
			if u.GroupCooldowns[k] > 0 {
				u.GroupCooldowns[k]--
				if u.GroupCooldowns[k] == 0 {
					delete(u.GroupCooldowns, k)
				}
			}
		}
	}
	globalKeys := make([]string, 0, len(st.GlobalGroupCD))
	for k := range st.GlobalGroupCD {
		globalKeys = append(globalKeys, k)
	}
	sort.Strings(globalKeys)
	for _, k := range globalKeys {
		if st.GlobalGroupCD[k] > 0 {
			st.GlobalGroupCD[k]--
			if st.GlobalGroupCD[k] == 0 {
				delete(st.GlobalGroupCD, k)
			}
		}
	}
}

func sortedRuntimeUnitIDs(st *RuntimeState) []string {
	ids := make([]string, 0, len(st.Units))
	for id := range st.Units {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}
