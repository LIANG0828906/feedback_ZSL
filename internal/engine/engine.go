package engine

import (
	"fmt"
	"sort"

	"battlekernel/internal/canonical"
	"battlekernel/internal/errs"
	"battlekernel/internal/loader"
	"battlekernel/internal/model"
)

type Candidate struct {
	Entry model.QueueEntry
}

func InitialStateMap(p *model.Project) map[string]model.UnitState {
	states := make(map[string]model.UnitState, len(p.InitialStates))
	for _, state := range p.InitialStates {
		state.Alive = true
		states[state.UnitID] = state
	}
	for _, unit := range p.Units {
		if _, ok := states[unit.ID]; !ok {
			states[unit.ID] = model.UnitState{UnitID: unit.ID, Resource: unit.InitialResource, CooldownRemaining: unit.InitialCooldown, Alive: true}
		}
	}
	return states
}

func CloneStateMap(in map[string]model.UnitState) map[string]model.UnitState {
	out := make(map[string]model.UnitState, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func AdjudicateQueue(p *model.Project, spec model.RoundSpec, states map[string]model.UnitState) ([]model.QueueEntry, []model.UnqueuedEntry, error) {
	var queued []model.QueueEntry
	var unqueued []model.UnqueuedEntry

	for _, action := range spec.Actions {
		unit, ok := p.Unit(action.UnitID)
		if !ok {
			return nil, nil, errs.New(errs.EOrder, "actions[].unit_id", "unknown unit %q", action.UnitID).WithRound(spec.Round)
		}
		skill, ok := p.Skill(action.SkillID)
		if !ok {
			return nil, nil, errs.New(errs.EOrder, "actions[].skill_id", "unknown skill %q", action.SkillID).WithRound(spec.Round).WithUnit(action.UnitID)
		}
		state, ok := states[action.UnitID]
		if !ok {
			return nil, nil, errs.New(errs.EState, "states", "missing state").WithRound(spec.Round).WithUnit(action.UnitID).WithSkill(action.SkillID)
		}
		if err := ValidateStateBounds(spec.Round, action.UnitID, action.SkillID, state); err != nil {
			return nil, nil, err
		}
		if skill.Conditional {
			if action.Flag == nil {
				return nil, nil, errs.New(errs.EFlag, "actions[].flag", "conditional flag is missing").WithRound(spec.Round).WithUnit(action.UnitID).WithSkill(action.SkillID)
			}
			if !*action.Flag {
				unqueued = append(unqueued, model.UnqueuedEntry{Round: spec.Round, UnitID: action.UnitID, SkillID: action.SkillID, Reason: "flag_false", Detail: "normalized condition flag is false"})
				continue
			}
		}
		if state.CooldownRemaining > 0 {
			unqueued = append(unqueued, model.UnqueuedEntry{Round: spec.Round, UnitID: action.UnitID, SkillID: action.SkillID, Reason: "cooldown_wait", Detail: fmt.Sprintf("cooldown_remaining=%d", state.CooldownRemaining)})
			continue
		}
		weight := ActiveWeight(p, action.UnitID, spec.Round)
		tie := ActiveTieOrdinal(p, action.UnitID, spec.Round)
		queued = append(queued, model.QueueEntry{
			Round: spec.Round, UnitID: action.UnitID, SkillID: action.SkillID,
			Weight: weight, TieOrdinal: tie,
			Basis: fmt.Sprintf("weight=%d:desc;tie_ordinal=%d:asc;unit_id=%s:asc;skill_id=%s:asc", weight, tie, action.UnitID, action.SkillID),
		})
		_ = unit
	}

	sort.SliceStable(queued, func(i, j int) bool {
		if queued[i].Weight != queued[j].Weight {
			return queued[i].Weight > queued[j].Weight
		}
		if queued[i].TieOrdinal != queued[j].TieOrdinal {
			return queued[i].TieOrdinal < queued[j].TieOrdinal
		}
		if queued[i].UnitID != queued[j].UnitID {
			return queued[i].UnitID < queued[j].UnitID
		}
		return queued[i].SkillID < queued[j].SkillID
	})
	for i := range queued {
		queued[i].Position = i + 1
	}
	sort.Slice(unqueued, func(i, j int) bool {
		if unqueued[i].UnitID != unqueued[j].UnitID {
			return unqueued[i].UnitID < unqueued[j].UnitID
		}
		return unqueued[i].SkillID < unqueued[j].SkillID
	})
	return queued, unqueued, nil
}

func ValidateStateBounds(round int64, unitID, skillID string, state model.UnitState) error {
	if state.CooldownRemaining < 0 {
		return (&errs.Error{Code: errs.ECool, Field: "state.cooldown_remaining", Round: round, UnitID: unitID, SkillID: skillID, Values: []string{fmt.Sprint(state.CooldownRemaining), "0"}, Message: "cooldown is negative"})
	}
	if state.CooldownRemaining > loader.MaxCooldownRemaining {
		return (&errs.Error{Code: errs.ECool, Field: "state.cooldown_remaining", Round: round, UnitID: unitID, SkillID: skillID, Values: []string{fmt.Sprint(state.CooldownRemaining), fmt.Sprint(loader.MaxCooldownRemaining)}, Message: "cooldown exceeds protocol bound"})
	}
	if state.Resource < 0 {
		return (&errs.Error{Code: errs.EState, Field: "state.resource", Round: round, UnitID: unitID, SkillID: skillID, Values: []string{fmt.Sprint(state.Resource), "0"}, Message: "resource is negative"})
	}
	return nil
}

func SettleRound(p *model.Project, spec model.RoundSpec, incoming map[string]model.UnitState) (model.RoundResult, map[string]model.UnitState, error) {
	states := CloneStateMap(incoming)
	queue, unqueued, err := AdjudicateQueue(p, spec, states)
	if err != nil {
		return model.RoundResult{}, nil, err
	}

	result := model.RoundResult{
		Round: spec.Round, TimestampMs: spec.TimestampMs, Queue: queue, Unqueued: unqueued,
		Status: "committed",
	}
	inputHash, err := canonical.JSONHash(spec)
	if err != nil {
		return model.RoundResult{}, nil, err
	}
	result.InputHash = inputHash

	frozen := map[string]bool{}
	cast := map[string]bool{}
	recordIndex := map[string]int{}

	for _, entry := range queue {
		skill, _ := p.Skill(entry.SkillID)
		state := states[entry.UnitID]
		record := model.ActionRecord{
			Position: len(result.Settlements) + 1, QueuePosition: entry.Position, Round: spec.Round,
			UnitID: entry.UnitID, SkillID: entry.SkillID, Weight: entry.Weight, TieOrdinal: entry.TieOrdinal,
			ResourceBefore: state.Resource, CooldownBefore: state.CooldownRemaining, Power: skill.Power,
		}
		if state.Resource < skill.Cost {
			record.Decision = "skipped_resource"
			record.Reason = fmt.Sprintf("resource %d < cost %d; no resource consumed", state.Resource, skill.Cost)
			record.ResourceAfter = state.Resource
			record.CooldownAfter = state.CooldownRemaining
		} else {
			state.Resource -= skill.Cost
			state.CooldownRemaining = skill.Cooldown
			record.Decision = "cast"
			record.ResourceAfter = state.Resource
			record.CooldownAfter = state.CooldownRemaining
			record.Damage = skill.Power
			result.TotalDamage += skill.Power
			cast[entry.UnitID] = true
		}
		states[entry.UnitID] = state
		recordIndex[entry.UnitID] = len(result.Settlements)
		result.Settlements = append(result.Settlements, record)
		if record.Decision == "cast" {
			actual := record
			actual.Position = len(result.ActionOrder) + 1
			result.ActionOrder = append(result.ActionOrder, actual)
		}
	}

	for _, item := range unqueued {
		if item.Reason == "flag_false" {
			frozen[item.UnitID] = true
		}
	}

	for _, unit := range p.Units {
		state := states[unit.ID]
		state.Resource += unit.Regen
		if state.Resource > unit.MaxResource {
			state.Resource = unit.MaxResource
		}
		if frozen[unit.ID] {
			// Explicitly do not advance cooldown when the condition is not met.
		} else if cast[unit.ID] {
			// Cooldown was set to the full cast cooldown and remains at that
			// value through this round's commit boundary.
		} else if state.CooldownRemaining > 0 {
			state.CooldownRemaining--
		}
		if err := ValidateStateBounds(spec.Round, unit.ID, "", state); err != nil {
			return model.RoundResult{}, nil, err
		}
		states[unit.ID] = state
		if idx, ok := recordIndex[unit.ID]; ok {
			result.Settlements[idx].ResourceAfter = state.Resource
			result.Settlements[idx].CooldownAfter = state.CooldownRemaining
		}
	}

	result.States = sortedStatesFromMap(states)
	result.ContentHash, err = HashRoundResult(result)
	if err != nil {
		return model.RoundResult{}, nil, err
	}
	return result, states, nil
}

func sortedStatesFromMap(states map[string]model.UnitState) []model.UnitState {
	out := make([]model.UnitState, 0, len(states))
	for _, state := range states {
		out = append(out, state)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UnitID < out[j].UnitID })
	return out
}

func HashRoundResult(result model.RoundResult) (string, error) {
	result.ContentHash = ""
	return canonical.JSONHash(result)
}

func SimulateAll(p *model.Project) ([]model.RoundResult, []WindowSummary, error) {
	states := InitialStateMap(p)
	results := make([]model.RoundResult, 0, len(p.Rounds))
	for _, spec := range p.Rounds {
		result, next, err := SettleRound(p, spec, states)
		if err != nil {
			return results, nil, err
		}
		states = next
		results = append(results, result)
	}
	windows := BuildWindowSummaries(p, results, 2)
	return results, windows, nil
}

type WindowSummary struct {
	WindowIndex int
	StartRound  int64
	EndRound    int64
	Rounds      []model.RoundResult
	Hash        string
	Offsets     []int
}

func BuildWindowSummaries(p *model.Project, results []model.RoundResult, size int) []WindowSummary {
	if size < 1 {
		size = 1
	}
	var windows []WindowSummary
	for start := 0; start < len(results); start += size {
		end := start + size
		if end > len(results) {
			end = len(results)
		}
		rounds := results[start:end]
		h, _ := canonical.JSONHash(rounds)
		startRound := rounds[0].Round
		endRound := rounds[len(rounds)-1].Round
		windows = append(windows, WindowSummary{
			WindowIndex: len(windows) + 1, StartRound: startRound, EndRound: endRound,
			Rounds: rounds, Hash: h, Offsets: ConsumedReorderOffsets(p, endRound),
		})
	}
	return windows
}
