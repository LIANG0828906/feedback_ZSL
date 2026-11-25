package sample

import (
	"fmt"
	"sort"

	"battlekernel/internal/model"
)

// BuildReferenceTruth is the generator-owned reference construction. It is
// deliberately separate from internal/engine so sample truth is not produced
// by the code under verification.
func BuildReferenceTruth(p *model.Project) (*model.Truth, error) {
	states := map[string]model.UnitState{}
	for _, state := range p.InitialStates {
		state.Alive = true
		states[state.UnitID] = state
	}

	truth := &model.Truth{ProjectID: p.ID, Seed: p.Seed, EnemyHP: p.EnemyHP}
	damage := 0

	for _, roundSpec := range p.Rounds {
		roundTruth := model.RoundTruth{Round: roundSpec.Round, TimestampMs: roundSpec.TimestampMs}
		type candidate struct {
			entry model.QueueEntry
		}
		var candidates []candidate
		frozen := map[string]bool{}

		for _, action := range roundSpec.Actions {
			unit, ok := p.Unit(action.UnitID)
			if !ok {
				return nil, fmt.Errorf("reference: missing unit %s", action.UnitID)
			}
			skill, ok := p.Skill(action.SkillID)
			if !ok {
				return nil, fmt.Errorf("reference: missing skill %s", action.SkillID)
			}
			state := states[action.UnitID]
			if skill.Conditional && (action.Flag == nil || !*action.Flag) {
				roundTruth.Unqueued = append(roundTruth.Unqueued, model.UnqueuedEntry{
					Round: roundSpec.Round, UnitID: action.UnitID, SkillID: action.SkillID,
					Reason: "flag_false", Detail: "normalized condition flag is false",
				})
				frozen[action.UnitID] = true
				continue
			}
			if state.CooldownRemaining > 0 {
				roundTruth.Unqueued = append(roundTruth.Unqueued, model.UnqueuedEntry{
					Round: roundSpec.Round, UnitID: action.UnitID, SkillID: action.SkillID,
					Reason: "cooldown_wait", Detail: fmt.Sprintf("cooldown_remaining=%d", state.CooldownRemaining),
				})
				continue
			}
			weight := referenceWeight(p, unit.ID, roundSpec.Round)
			tie := referenceTie(p, unit.ID, roundSpec.Round)
			candidates = append(candidates, candidate{entry: model.QueueEntry{
				Round: roundSpec.Round, UnitID: unit.ID, SkillID: skill.ID,
				Weight: weight, TieOrdinal: tie,
				Basis: fmt.Sprintf("weight=%d:desc;tie_ordinal=%d:asc;unit_id=%s:asc;skill_id=%s:asc", weight, tie, unit.ID, skill.ID),
			}})
		}

		sort.SliceStable(candidates, func(i, j int) bool {
			a, b := candidates[i].entry, candidates[j].entry
			if a.Weight != b.Weight {
				return a.Weight > b.Weight
			}
			if a.TieOrdinal != b.TieOrdinal {
				return a.TieOrdinal < b.TieOrdinal
			}
			if a.UnitID != b.UnitID {
				return a.UnitID < b.UnitID
			}
			return a.SkillID < b.SkillID
		})
		for i, candidate := range candidates {
			candidate.entry.Position = i + 1
			roundTruth.Order = append(roundTruth.Order, candidate.entry)
		}
		sort.Slice(roundTruth.Unqueued, func(i, j int) bool {
			if roundTruth.Unqueued[i].UnitID != roundTruth.Unqueued[j].UnitID {
				return roundTruth.Unqueued[i].UnitID < roundTruth.Unqueued[j].UnitID
			}
			return roundTruth.Unqueued[i].SkillID < roundTruth.Unqueued[j].SkillID
		})

		cast := map[string]bool{}
		castPosition := 0
		for _, queueEntry := range roundTruth.Order {
			skill, _ := p.Skill(queueEntry.SkillID)
			state := states[queueEntry.UnitID]
			record := model.ActionRecord{
				QueuePosition: queueEntry.Position, Round: roundSpec.Round,
				UnitID: queueEntry.UnitID, SkillID: queueEntry.SkillID,
				Weight: queueEntry.Weight, TieOrdinal: queueEntry.TieOrdinal,
				ResourceBefore: state.Resource, CooldownBefore: state.CooldownRemaining,
				Power: skill.Power,
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
				record.Damage = skill.Power
				record.ResourceAfter = state.Resource
				record.CooldownAfter = state.CooldownRemaining
				cast[queueEntry.UnitID] = true
				castPosition++
				record.Position = castPosition
				roundTruth.Casts = append(roundTruth.Casts, record)
				roundTruth.TotalDamage += skill.Power
				damage += skill.Power
			}
			states[queueEntry.UnitID] = state
		}

		for _, unit := range p.Units {
			state := states[unit.ID]
			state.Resource += unit.Regen
			if state.Resource > unit.MaxResource {
				state.Resource = unit.MaxResource
			}
			if frozen[unit.ID] {
				// Freeze cooldown.
			} else if cast[unit.ID] {
				// Keep full cooldown at commit boundary.
			} else if state.CooldownRemaining > 0 {
				state.CooldownRemaining--
			}
			states[unit.ID] = state
		}

		roundTruth.States = make([]model.UnitState, 0, len(p.Units))
		for _, unit := range p.Units {
			state := states[unit.ID]
			roundTruth.States = append(roundTruth.States, state)
		}
		sort.Slice(roundTruth.States, func(i, j int) bool { return roundTruth.States[i].UnitID < roundTruth.States[j].UnitID })
		for i := range roundTruth.Casts {
			state := states[roundTruth.Casts[i].UnitID]
			roundTruth.Casts[i].ResourceAfter = state.Resource
			roundTruth.Casts[i].CooldownAfter = state.CooldownRemaining
		}
		truth.Rounds = append(truth.Rounds, roundTruth)
	}

	if damage >= p.EnemyHP {
		truth.FinalWinner = "attacker"
	} else {
		truth.FinalWinner = "defender"
	}
	return truth, nil
}

func referenceWeight(p *model.Project, unitID string, round int64) int {
	unit, _ := p.Unit(unitID)
	value := unit.BaseWeight
	for _, edit := range p.WeightEdits {
		if edit.UnitID == unitID && round >= edit.FromRound {
			value = edit.Weight
		}
	}
	return value
}

func referenceTie(p *model.Project, unitID string, round int64) int {
	unit, _ := p.Unit(unitID)
	value := unit.TieOrdinal
	activeOffset := -1
	for _, command := range p.Reorders {
		if command.UnitID == unitID && round > command.AfterRound && command.Offset > activeOffset {
			value = command.NewTieOrdinal
			activeOffset = command.Offset
		}
	}
	return value
}
