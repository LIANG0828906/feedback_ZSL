package diff

import (
	"fmt"
	"reflect"

	"replaybench/internal/eerr"
	"replaybench/internal/model"
)

func Snapshots(expected, actual []model.RoundSnapshot) error {
	if len(expected) != len(actual) {
		return eerr.Diff(0, "rounds.length", len(expected), len(actual))
	}
	for i := range expected {
		e := expected[i]
		a := actual[i]
		round := e.Round
		if a.Round != round {
			return eerr.Diff(i+1, "round", round, a.Round)
		}
		if e.ConsumedOffset != a.ConsumedOffset {
			return eerr.Diff(round, "consumed_offset", e.ConsumedOffset, a.ConsumedOffset)
		}
		if !reflect.DeepEqual(e.EffectiveSkills, a.EffectiveSkills) {
			return eerr.Diff(round, "effective_skills", e.EffectiveSkills, a.EffectiveSkills)
		}
		if err := compareStates(round, e.States, a.States); err != nil {
			return err
		}
		if !reflect.DeepEqual(e.GlobalCooldowns, a.GlobalCooldowns) {
			return eerr.Diff(round, "global_cooldowns", e.GlobalCooldowns, a.GlobalCooldowns)
		}
		if err := compareAdjudications(round, e.Adjudications, a.Adjudications); err != nil {
			return err
		}
	}
	return nil
}

func CommitSequences(expected, actual []int) error {
	if !reflect.DeepEqual(expected, actual) {
		return eerr.Diff(0, "window_commit_sequence", expected, actual)
	}
	return nil
}

func compareStates(round int, expected, actual []model.UnitSnapshot) error {
	if len(expected) != len(actual) {
		return eerr.Diff(round, "states.length", len(expected), len(actual))
	}
	actualByID := map[string]model.UnitSnapshot{}
	for _, s := range actual {
		actualByID[s.UnitID] = s
	}
	for _, es := range expected {
		as, ok := actualByID[es.UnitID]
		if !ok {
			return eerr.Diff(round, fmt.Sprintf("states.%s", es.UnitID), es.UnitID, "missing")
		}
		if es.HP != as.HP {
			return eerr.Diff(round, fmt.Sprintf("states.%s.hp", es.UnitID), es.HP, as.HP)
		}
		if es.Resource != as.Resource {
			return eerr.Diff(round, fmt.Sprintf("states.%s.resource", es.UnitID), es.Resource, as.Resource)
		}
		if es.Alive != as.Alive {
			return eerr.Diff(round, fmt.Sprintf("states.%s.alive", es.UnitID), es.Alive, as.Alive)
		}
		if !reflect.DeepEqual(es.Cooldowns, as.Cooldowns) {
			return eerr.Diff(round, fmt.Sprintf("states.%s.cooldowns", es.UnitID), es.Cooldowns, as.Cooldowns)
		}
		if !reflect.DeepEqual(es.GroupCooldowns, as.GroupCooldowns) {
			return eerr.Diff(round, fmt.Sprintf("states.%s.group_cooldowns", es.UnitID), es.GroupCooldowns, as.GroupCooldowns)
		}
	}
	return nil
}

func compareAdjudications(round int, expected, actual []model.Adjudication) error {
	if len(expected) != len(actual) {
		return eerr.Diff(round, "adjudications.length", len(expected), len(actual))
	}
	for i := range expected {
		path := fmt.Sprintf("adjudications[%d]", i)
		e := expected[i]
		a := actual[i]
		if e.UnitID != a.UnitID {
			return eerr.Diff(round, path+".unit_id", e.UnitID, a.UnitID)
		}
		if e.SkillID != a.SkillID {
			return eerr.Diff(round, path+".skill_id", e.SkillID, a.SkillID)
		}
		if e.ContentionType != a.ContentionType {
			return eerr.Diff(round, path+".contention_type", e.ContentionType, a.ContentionType)
		}
		if e.Result != a.Result {
			return eerr.Diff(round, path+".result", e.Result, a.Result)
		}
		if !reflect.DeepEqual(e.ContendingSkills, a.ContendingSkills) {
			return eerr.Diff(round, path+".contending_skills", e.ContendingSkills, a.ContendingSkills)
		}
	}
	return nil
}
