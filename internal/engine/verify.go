package engine

import (
	"fmt"

	"battlekernel/internal/errs"
	"battlekernel/internal/model"
)

func VerifyTruth(p *model.Project, results []model.RoundResult) error {
	if p.Truth == nil {
		return errs.New(errs.ESchema, "truth", "project has no attached truth")
	}
	truth := p.Truth
	if len(results) != len(truth.Rounds) {
		return errs.New(errs.EOrder, "rounds", "result round count %d != truth %d", len(results), len(truth.Rounds))
	}
	totalDamage := 0
	for i := range results {
		result := results[i]
		expected := truth.Rounds[i]
		if err := compareQueue(result, expected); err != nil {
			return err
		}
		if err := compareCasts(result, expected); err != nil {
			return err
		}
		if err := compareUnqueued(result, expected); err != nil {
			return err
		}
		if err := compareStates(p, result, expected); err != nil {
			return err
		}
		totalDamage += result.TotalDamage
	}
	winner := "defender"
	if totalDamage >= p.EnemyHP {
		winner = "attacker"
	}
	if winner != truth.FinalWinner {
		return &errs.Error{Code: errs.EState, Field: "truth.final_winner", Message: fmt.Sprintf("winner %s != truth %s", winner, truth.FinalWinner), Values: []string{winner, truth.FinalWinner}}
	}
	return nil
}

func compareQueue(got model.RoundResult, want model.RoundTruth) error {
	if len(got.Queue) != len(want.Order) {
		unit := firstQueueUnit(got.Queue)
		return errs.New(errs.EOrder, "rounds.queue", "round %d queue length %d != truth %d", got.Round, len(got.Queue), len(want.Order)).WithRound(got.Round).WithUnit(unit)
	}
	for j := range got.Queue {
		g := got.Queue[j]
		w := want.Order[j]
		if g.Position != w.Position || g.UnitID != w.UnitID || g.SkillID != w.SkillID {
			return errs.New(errs.EOrder, fmt.Sprintf("rounds[%d].queue[%d]", got.Round, j), "got %s/%s position %d, want %s/%s position %d", g.UnitID, g.SkillID, g.Position, w.UnitID, w.SkillID, w.Position).WithRound(got.Round).WithUnit(g.UnitID).WithSkill(g.SkillID)
		}
		if g.Weight != w.Weight || g.TieOrdinal != w.TieOrdinal {
			return errs.New(errs.EOrder, fmt.Sprintf("rounds[%d].queue[%d].weight", got.Round, j), "got weight=%d tie=%d, want weight=%d tie=%d", g.Weight, g.TieOrdinal, w.Weight, w.TieOrdinal).WithRound(got.Round).WithUnit(g.UnitID).WithSkill(g.SkillID)
		}
	}
	return nil
}

func compareCasts(got model.RoundResult, want model.RoundTruth) error {
	if len(got.ActionOrder) != len(want.Casts) {
		unit := ""
		if len(got.ActionOrder) > 0 {
			unit = got.ActionOrder[0].UnitID
		}
		return errs.New(errs.EOrder, "rounds.action_order", "round %d cast count %d != truth %d", got.Round, len(got.ActionOrder), len(want.Casts)).WithRound(got.Round).WithUnit(unit)
	}
	for j := range got.ActionOrder {
		g := got.ActionOrder[j]
		w := want.Casts[j]
		if g.Position != w.Position || g.UnitID != w.UnitID || g.SkillID != w.SkillID {
			return errs.New(errs.EOrder, fmt.Sprintf("rounds[%d].action_order[%d]", got.Round, j), "position %d got %s/%s, want %s/%s", j+1, g.UnitID, g.SkillID, w.UnitID, w.SkillID).WithRound(got.Round).WithUnit(g.UnitID).WithSkill(g.SkillID)
		}
	}
	return nil
}

func compareUnqueued(got model.RoundResult, want model.RoundTruth) error {
	gotFlags := map[string]string{}
	wantFlags := map[string]string{}
	for _, item := range got.Unqueued {
		gotFlags[item.UnitID+"/"+item.SkillID] = item.Reason
	}
	for _, item := range want.Unqueued {
		wantFlags[item.UnitID+"/"+item.SkillID] = item.Reason
	}
	for key, reason := range wantFlags {
		if gotFlags[key] != reason {
			unit, skill := splitKey(key)
			return errs.New(errs.EOrder, "rounds.unqueued", "round %d %s reason %q != truth %q", got.Round, key, gotFlags[key], reason).WithRound(got.Round).WithUnit(unit).WithSkill(skill)
		}
	}
	for key, reason := range gotFlags {
		if _, ok := wantFlags[key]; !ok {
			unit, skill := splitKey(key)
			return errs.New(errs.EOrder, "rounds.unqueued", "round %d unexpected unqueued %s reason %q", got.Round, key, reason).WithRound(got.Round).WithUnit(unit).WithSkill(skill)
		}
	}
	return nil
}

func compareStates(p *model.Project, got model.RoundResult, want model.RoundTruth) error {
	wantStates := model.StateMap(want.States)
	for _, g := range got.States {
		w, ok := wantStates[g.UnitID]
		if !ok {
			skill, _ := p.SkillForUnit(g.UnitID)
			return errs.New(errs.EState, "states", "truth missing state").WithRound(got.Round).WithUnit(g.UnitID).WithSkill(skill.ID)
		}
		skill, _ := p.SkillForUnit(g.UnitID)
		if g.CooldownRemaining != w.CooldownRemaining {
			return &errs.Error{Code: errs.EState, Field: "states.cooldown_remaining", Round: got.Round, UnitID: g.UnitID, SkillID: skill.ID, Message: "cooldown does not match truth", Values: []string{fmt.Sprint(g.CooldownRemaining), fmt.Sprint(w.CooldownRemaining)}}
		}
		if g.Resource != w.Resource {
			return &errs.Error{Code: errs.EState, Field: "states.resource", Round: got.Round, UnitID: g.UnitID, SkillID: skill.ID, Message: "resource does not match truth", Values: []string{fmt.Sprint(g.Resource), fmt.Sprint(w.Resource)}}
		}
	}
	return nil
}

func firstQueueUnit(queue []model.QueueEntry) string {
	if len(queue) == 0 {
		return ""
	}
	return queue[0].UnitID
}

func splitKey(key string) (string, string) {
	for i := 0; i < len(key); i++ {
		if key[i] == '/' {
			return key[:i], key[i+1:]
		}
	}
	return key, ""
}
