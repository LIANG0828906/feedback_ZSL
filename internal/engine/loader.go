package engine

import (
	"bytes"
	"encoding/json"
	"fmt"

	"replaykernel/internal/model"
)

// LoadReplay 从字节加载并完整校验一局回放。
// 损坏 JSON -> E_DECODE；字段缺失/非法 -> E_SCHEMA；跳号/倒置 -> E_ORDER。
func LoadReplay(data []byte) (*model.Replay, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var r model.Replay
	if err := dec.Decode(&r); err != nil {
		return nil, model.NewError(model.EDecode, "replay payload corrupted: %v", err)
	}
	if err := ValidateReplay(&r); err != nil {
		return nil, err
	}
	return &r, nil
}

// ValidateReplay 执行全部结构与引用完整性校验。
func ValidateReplay(r *model.Replay) error {
	if r.MatchID == "" {
		return schemaErr("match_id", "must not be empty")
	}
	if err := validateSkills(r); err != nil {
		return err
	}
	if err := validateUnits(r); err != nil {
		return err
	}
	if err := validateTurns(r); err != nil {
		return err
	}
	return nil
}

func validateSkills(r *model.Replay) error {
	ids := make(map[string]bool, len(r.Skills))
	for i, s := range r.Skills {
		p := fmt.Sprintf("skills[%d]", i)
		if s.ID == "" {
			return schemaErr(p+".id", "must not be empty")
		}
		if ids[s.ID] {
			return schemaErr(p+".id", "duplicate skill id %q", s.ID)
		}
		ids[s.ID] = true
		if s.Kind != model.SkillAttack && s.Kind != model.SkillHeal {
			return schemaErr(p+".kind", "must be attack or heal, got %q", s.Kind)
		}
		if s.Power <= 0 {
			return schemaErr(p+".power_milli", "must be positive")
		}
		if s.Cost < 0 {
			return schemaErr(p+".cost_milli", "must be non-negative")
		}
		if s.Cooldown < 0 {
			return schemaErr(p+".cooldown", "must be non-negative")
		}
	}
	return nil
}

func validateUnits(r *model.Replay) error {
	ids := make(map[string]bool, len(r.Units))
	skillSet := make(map[string]bool, len(r.Skills))
	for _, s := range r.Skills {
		skillSet[s.ID] = true
	}
	for i, u := range r.Units {
		p := fmt.Sprintf("units[%d]", i)
		if u.ID == "" {
			return schemaErr(p+".id", "must not be empty")
		}
		if ids[u.ID] {
			return schemaErr(p+".id", "duplicate unit id %q", u.ID)
		}
		ids[u.ID] = true
		if u.MaxHP <= 0 || u.MaxMP <= 0 {
			return schemaErr(p, "max_hp_milli/max_mp_milli must be positive")
		}
		if u.StartHP <= 0 || u.StartHP > u.MaxHP {
			return schemaErr(p+".start_hp_milli", "must be within (0,max_hp_milli]")
		}
		if u.StartMP < 0 || u.StartMP > u.MaxMP {
			return schemaErr(p+".start_mp_milli", "must be within [0,max_mp_milli]")
		}
		if u.MPRegen < 0 {
			return schemaErr(p+".mp_regen_milli", "must be non-negative")
		}
		if len(u.Skills) == 0 {
			return schemaErr(p+".skills", "at least one skill required")
		}
		for j, sid := range u.Skills {
			if !skillSet[sid] {
				return schemaErr(fmt.Sprintf("%s.skills[%d]", p, j), "unknown skill %q", sid)
			}
		}
	}
	return nil
}

func validateTurns(r *model.Replay) error {
	if len(r.Turns) == 0 {
		return schemaErr("turns", "at least one turn required")
	}
	unitSet := make(map[string]bool, len(r.Units))
	skillSet := make(map[string]bool, len(r.Skills))
	for _, u := range r.Units {
		unitSet[u.ID] = true
	}
	for _, s := range r.Skills {
		skillSet[s.ID] = true
	}

	for ti, t := range r.Turns {
		p := fmt.Sprintf("turns[%d]", ti)
		expected := ti + 1
		if t.TurnNo != expected {
			return model.NewError(model.EOrder,
				"turn sequence broken at %s: expected turn_no=%d got %d",
				p, expected, t.TurnNo).
				WithMatch(r.MatchID).
				WithField(p+".turn_no", int64(expected), int64(t.TurnNo))
		}

		// 行动顺序必须是参战单位的合法排列：长度一致、无重复、无未知单位。
		if len(t.Order) != len(r.Units) {
			return schemaErr(p+".order",
				"order length %d != unit count %d", len(t.Order), len(r.Units))
		}
		seen := make(map[string]bool, len(t.Order))
		for oi, uid := range t.Order {
			op := fmt.Sprintf("%s.order[%d]", p, oi)
			if !unitSet[uid] {
				return schemaErr(op, "unknown unit %q", uid)
			}
			if seen[uid] {
				return schemaErr(op, "duplicate unit %q in action order", uid)
			}
			seen[uid] = true
		}

		intentUnits := make(map[string]bool, len(t.Intents))
		for ii, in := range t.Intents {
			ip := fmt.Sprintf("%s.intents[%d]", p, ii)
			if in.UnitID == "" {
				return schemaErr(ip+".unit_id", "must not be empty")
			}
			if !unitSet[in.UnitID] {
				return schemaErr(ip+".unit_id", "unknown unit %q", in.UnitID)
			}
			if intentUnits[in.UnitID] {
				return schemaErr(ip+".unit_id", "duplicate intent for unit %q", in.UnitID)
			}
			intentUnits[in.UnitID] = true
			if in.SkillID == "" {
				return schemaErr(ip+".skill_id", "must not be empty")
			}
			if !skillSet[in.SkillID] {
				return schemaErr(ip+".skill_id", "unknown skill %q", in.SkillID)
			}
			if in.TargetID != "" && !unitSet[in.TargetID] {
				return schemaErr(ip+".target_id", "unknown target unit %q", in.TargetID)
			}
		}
	}
	return nil
}

func schemaErr(field, format string, args ...any) error {
	return model.NewError(model.ESchema, "%s: %s", field, fmt.Sprintf(format, args...)).
		WithField(field, 0, 0)
}
