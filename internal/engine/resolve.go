package engine

import (
	"fmt"

	"battlekernel/internal/model"
)

func BuildDependencies(p *model.Project) []model.Dependency {
	deps := []model.Dependency{
		{Key: "algorithm:order", Kind: "algorithm", Source: "weight+tie_ordinal+unit_id", AffectedKeys: []string{"queue:*", "settlement:*"}, Description: "Queue order is deterministic from weight descending, tie ordinal ascending, then stable IDs."},
		{Key: "rule:resource", Kind: "rule", Source: "skill.cost+unit.resource+unit.regen", AffectedKeys: []string{"state:resource:*", "settlement:skip"}, Description: "Insufficient resource skips a skill without consuming resource."},
		{Key: "rule:cooldown", Kind: "rule", Source: "skill.cooldown+state.cooldown_remaining", AffectedKeys: []string{"state:cooldown:*", "queue:cooldown_wait"}, Description: "Casts set cooldown; waiting cooldowns decrement; false condition flags freeze cooldown."},
	}
	for _, u := range p.Units {
		deps = append(deps, model.Dependency{
			Key: "weight:" + u.ID, Kind: "action_weight", Source: "units[id=" + u.ID + "].base_weight or weight_edit",
			AffectedKeys: []string{fmt.Sprintf("queue:round>=*:unit=%s", u.ID), "rank_neighbors:*"},
			Description:  "Action weight changes the unit's candidate position and can swap rank neighbors.",
		})
		deps = append(deps, model.Dependency{
			Key: "cooldown:" + u.ID, Kind: "skill_cooldown", Source: "state." + u.ID + ".cooldown_remaining",
			AffectedKeys: []string{fmt.Sprintf("queue:future:unit=%s", u.ID), fmt.Sprintf("state:future:unit=%s", u.ID)},
			Description:  "Cooldown state propagates to subsequent rounds.",
		})
		deps = append(deps, model.Dependency{
			Key: "flag:" + u.ID, Kind: "condition_flag", Source: "round.actions[unit=" + u.ID + "].flag",
			AffectedKeys: []string{fmt.Sprintf("queue:unit=%s", u.ID), fmt.Sprintf("cooldown:unit=%s", u.ID)},
			Description:  "A false condition flag removes the conditional skill and freezes its cooldown for that round.",
		})
	}
	for _, r := range p.Reorders {
		deps = append(deps, model.Dependency{
			Key: "reorder:" + r.ID, Kind: "formation_reorder", Source: fmt.Sprintf("reorders[offset=%d]:unit=%s:after_round=%d", r.Offset, r.UnitID, r.AfterRound),
			AffectedKeys: []string{fmt.Sprintf("tie_ordinal:unit=%s:round>%d", r.UnitID, r.AfterRound), "rank_neighbors:same_weight"},
			Description:  "Reorder changes deterministic tie ordinal only for the target unit in rounds strictly after AfterRound.",
		})
	}
	return deps
}

func ActiveWeight(p *model.Project, unitID string, round int64) int {
	weight, _ := p.Unit(unitID)
	value := weight.BaseWeight
	for _, edit := range p.WeightEdits {
		if edit.UnitID == unitID && round >= edit.FromRound {
			value = edit.Weight
		}
	}
	return value
}

func ActiveTieOrdinal(p *model.Project, unitID string, round int64) int {
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

func ConsumedReorderOffsets(p *model.Project, throughRound int64) []int {
	var offsets []int
	for _, command := range p.Reorders {
		if throughRound > command.AfterRound {
			offsets = append(offsets, command.Offset)
		}
	}
	return offsets
}
