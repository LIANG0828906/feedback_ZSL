package engine

import (
	"replaykernel/internal/model"
	"replaykernel/internal/prng"
)

// CritPerMille 是攻击技能暴击的固定千分概率（15%）。
const CritPerMille = 150

// CritNumerator 是暴击伤害倍率 1.5 的定点分子（除以 Milli）。
const CritNumerator int64 = 1500

// resolveTurn 按行动顺序结算一个完整回合，返回本回合新增裁决。
// 全部数值走定点整型与溢出检查；随机只来自 turnSeed 派生的子随机源。
func (m *matchRuntime) resolveTurn(turn model.TurnRecord) ([]model.Ruling, error) {
	intentByUnit := make(map[string]model.Intent, len(turn.Intents))
	for _, in := range turn.Intents {
		intentByUnit[in.UnitID] = in
	}

	var rulings []model.Ruling
	seed := prng.TurnSeed(m.replay.Seed, turn.TurnNo)
	rng := prng.New(seed)
	m.lastRngSeed = int64(seed)

	// 回合开始：冷却统一推进 1 拍。
	for _, id := range m.unitOrder {
		u := m.units[id]
		for sid := range u.cooldowns {
			if u.cooldowns[sid] > 0 {
				u.cooldowns[sid]--
			}
		}
	}

	// 按回放行动顺序逐单位处理；顺序是输入数据的一部分，不依赖 map 遍历。
	for _, unitID := range turn.Order {
		intent, hasIntent := intentByUnit[unitID]
		actor := m.units[unitID]
		if !hasIntent || !actor.alive {
			continue
		}
		m.actionOffset++
		skill := m.skills[intent.SkillID]

		ruling, allowed, err := m.adjudicate(turn.TurnNo, intent, actor, skill)
		if err != nil {
			return nil, err
		}
		rulings = append(rulings, ruling)
		if !allowed {
			continue
		}

		if err := m.applyCast(actor, intent, skill, rng); err != nil {
			return nil, model.NewError(model.EStage, "cast resolution failed: %v", err).
				WithMatch(m.replay.MatchID).WithTurn(turn.TurnNo)
		}
	}

	// 回合结束：存活单位资源回复；阵亡单位资源归零冻结。
	for _, id := range m.unitOrder {
		u := m.units[id]
		if !u.alive {
			u.mp = 0
			continue
		}
		mp, err := model.AddChecked(u.mp, u.spec.MPRegen)
		if err != nil {
			return nil, model.NewError(model.EStage, "mp regen overflow: %v", err).
				WithMatch(m.replay.MatchID).WithTurn(turn.TurnNo)
		}
		u.mp = model.Clamp(mp, 0, u.spec.MaxMP)
	}

	m.rngDraws += rng.Draws()
	m.resolveWinner()

	snap := m.snapshot(turn.TurnNo)
	m.snapshots = append(m.snapshots, snap)
	return rulings, nil
}

// adjudicate 执行前置校验与固定优先级冲突裁决。
// 多条违规同时存在时逐条收集，按 RulingPriority 决定首要原因，与遍历顺序无关。
func (m *matchRuntime) adjudicate(turnNo int, intent model.Intent, actor *unitRuntime, skill model.Skill) (model.Ruling, bool, error) {
	ruling := model.Ruling{
		TurnNo:  turnNo,
		UnitID:  intent.UnitID,
		SkillID: intent.SkillID,
	}

	// E_CONFLICT 注入：无任何违规却被裁决失败。
	if intent.InjectConflict {
		if len(m.collectViolations(intent, actor, skill)) != 0 {
			return ruling, false, model.NewError(model.EConflict,
				"phantom rejection injected but real violations exist").
				WithMatch(m.replay.MatchID).WithTurn(turnNo)
		}
		ruling.Violations = []model.Violation{}
		ruling.Reason = model.ViolationPhantomRejection
		ruling.Allowed = false
		ke := model.NewError(model.EConflict,
			"cast rejected despite no violation (phantom rejection): unit=%s skill=%s",
			intent.UnitID, intent.SkillID).
			WithMatch(m.replay.MatchID).WithTurn(turnNo)
		ke.Ruling = &ruling
		return ruling, false, ke
	}

	violations := m.collectViolations(intent, actor, skill)
	if len(violations) == 0 {
		ruling.Violations = []model.Violation{}
		ruling.Allowed = true
		return ruling, true, nil
	}
	ruling.Violations = violations
	ruling.Reason = violations[0] // collectViolations 已按固定优先级排序
	ruling.Allowed = false
	return ruling, false, nil
}

// collectViolations 按固定优先级收集全部违规，保证可解释且不依赖遍历顺序。
func (m *matchRuntime) collectViolations(intent model.Intent, actor *unitRuntime, skill model.Skill) []model.Violation {
	found := map[model.Violation]bool{}

	if actor.mp < skill.Cost {
		found[model.ViolationInsufficientResource] = true
	}
	if actor.cooldowns[skill.ID] > 0 {
		found[model.ViolationCooldownActive] = true
	}
	if target, ok := m.units[intent.TargetID]; ok && !target.alive {
		found[model.ViolationTargetDead] = true
	}

	out := make([]model.Violation, 0, len(found))
	for _, v := range model.RulingPriority {
		if found[v] {
			out = append(out, v)
		}
	}
	return out
}

// applyCast 执行资源消耗、伤害/治疗结算与冷却登记。
func (m *matchRuntime) applyCast(actor *unitRuntime, intent model.Intent, skill model.Skill, rng *prng.Source) error {
	mp, err := model.AddChecked(actor.mp, -skill.Cost)
	if err != nil {
		return err
	}
	actor.mp = model.Clamp(mp, 0, actor.spec.MaxMP)
	actor.cooldowns[skill.ID] = skill.Cooldown

	target := m.units[intent.TargetID]
	switch skill.Kind {
	case model.SkillAttack:
		dmg := skill.Power
		if rng.Chance(CritPerMille) {
			dmg, err = model.ScaleMilli(dmg, CritNumerator)
			if err != nil {
				return err
			}
		}
		hp, err := model.AddChecked(target.hp, -dmg)
		if err != nil {
			return err
		}
		target.hp = model.Clamp(hp, 0, target.spec.MaxHP)
		if target.hp == 0 {
			target.alive = false
		}
	case model.SkillHeal:
		hp, err := model.AddChecked(target.hp, skill.Power)
		if err != nil {
			return err
		}
		target.hp = model.Clamp(hp, 0, target.spec.MaxHP)
		target.alive = true
	default:
		return model.NewError(model.ESchema, "unknown skill kind: %s", skill.Kind)
	}
	return nil
}

// resolveWinner 在回合结束判定存活队伍：0 存活=平局，1 存活=该队胜，否则继续。
func (m *matchRuntime) resolveWinner() {
	aliveTeams := map[int]bool{}
	for _, id := range m.unitOrder {
		if m.units[id].alive {
			aliveTeams[m.units[id].spec.Team] = true
		}
	}
	if len(aliveTeams) == 0 {
		m.winner = 0
		m.finished = true
		return
	}
	if len(aliveTeams) == 1 {
		for team := range aliveTeams {
			m.winner = team
		}
		m.finished = true
		return
	}
	m.winner = -1
}
