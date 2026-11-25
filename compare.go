package skillcore

import (
	"fmt"
	"reflect"
)

type Mismatch struct {
	Tick     int64  `json:"tick"`
	SkillID  string `json:"skillId,omitempty"`
	Field    string `json:"field"`
	Expected string `json:"expected"`
	Actual   string `json:"actual"`
}

func (m *Mismatch) Error() string {
	if m == nil {
		return ""
	}
	return fmt.Sprintf("first mismatch at tick=%d skill=%q field=%s: expected=%s actual=%s",
		m.Tick, m.SkillID, m.Field, m.Expected, m.Actual)
}

// CompareSnapshots returns the first deterministic difference in tick, skill,
// pool, decision and field order. It is used by all three execution paths.
func CompareSnapshots(expected, actual []TickSnapshot) *Mismatch {
	limit := len(expected)
	if len(actual) < limit {
		limit = len(actual)
	}
	for i := 0; i < limit; i++ {
		left, right := expected[i], actual[i]
		if left.Tick != right.Tick {
			return &Mismatch{
				Tick:     left.Tick,
				Field:    "tick",
				Expected: fmt.Sprint(left.Tick),
				Actual:   fmt.Sprint(right.Tick),
			}
		}
		if mismatch := compareTick(left, right); mismatch != nil {
			return mismatch
		}
	}
	if len(expected) != len(actual) {
		tick := int64(0)
		if limit > 0 {
			tick = expected[limit-1].Tick + 1
		}
		return &Mismatch{
			Tick:     tick,
			Field:    "snapshot.count",
			Expected: fmt.Sprint(len(expected)),
			Actual:   fmt.Sprint(len(actual)),
		}
	}
	return nil
}

func compareTick(expected, actual TickSnapshot) *Mismatch {
	expectedPools := make(map[string]PoolSnapshot, len(expected.Pools))
	for _, pool := range expected.Pools {
		expectedPools[pool.ID] = pool
	}
	actualPools := make(map[string]PoolSnapshot, len(actual.Pools))
	for _, pool := range actual.Pools {
		actualPools[pool.ID] = pool
	}
	for _, left := range expected.Pools {
		right, ok := actualPools[left.ID]
		if !ok {
			return &Mismatch{Tick: expected.Tick, Field: "pools." + left.ID, Expected: "present", Actual: "missing"}
		}
		if left != right {
			return &Mismatch{
				Tick:     expected.Tick,
				Field:    "pools." + left.ID + ".amount",
				Expected: fmt.Sprintf("amount=%d,max=%d", left.Amount, left.Max),
				Actual:   fmt.Sprintf("amount=%d,max=%d", right.Amount, right.Max),
			}
		}
	}
	if len(expected.Pools) != len(actual.Pools) {
		return &Mismatch{Tick: expected.Tick, Field: "pools.count", Expected: fmt.Sprint(len(expected.Pools)), Actual: fmt.Sprint(len(actual.Pools))}
	}
	expectedSkills := make(map[string]SkillSnapshot, len(expected.Skills))
	for _, skill := range expected.Skills {
		expectedSkills[skill.ID] = skill
	}
	actualSkills := make(map[string]SkillSnapshot, len(actual.Skills))
	for _, skill := range actual.Skills {
		actualSkills[skill.ID] = skill
	}
	for _, left := range expected.Skills {
		right, ok := actualSkills[left.ID]
		if !ok {
			return &Mismatch{Tick: expected.Tick, SkillID: left.ID, Field: "skills." + left.ID, Expected: "present", Actual: "missing"}
		}
		if left.Enabled != right.Enabled {
			return boolMismatch(expected.Tick, left.ID, "enabled", left.Enabled, right.Enabled)
		}
		if left.TimersReady != right.TimersReady {
			return boolMismatch(expected.Tick, left.ID, "timersReady", left.TimersReady, right.TimersReady)
		}
		if left.Affordable != right.Affordable {
			return boolMismatch(expected.Tick, left.ID, "affordable", left.Affordable, right.Affordable)
		}
		if left.Available != right.Available {
			return boolMismatch(expected.Tick, left.ID, "available", left.Available, right.Available)
		}
		if left.CooldownTicks != right.CooldownTicks {
			return intMismatch(expected.Tick, left.ID, "cooldownTicks", left.CooldownTicks, right.CooldownTicks)
		}
		if left.CastRemainingTicks != right.CastRemainingTicks {
			return intMismatch(expected.Tick, left.ID, "castRemainingTicks", left.CastRemainingTicks, right.CastRemainingTicks)
		}
	}
	if len(expected.Skills) != len(actual.Skills) {
		return &Mismatch{Tick: expected.Tick, Field: "skills.count", Expected: fmt.Sprint(len(expected.Skills)), Actual: fmt.Sprint(len(actual.Skills))}
	}
	if len(expected.Decisions) != len(actual.Decisions) {
		return &Mismatch{Tick: expected.Tick, Field: "decisions.count", Expected: fmt.Sprint(len(expected.Decisions)), Actual: fmt.Sprint(len(actual.Decisions))}
	}
	for i := range expected.Decisions {
		left, right := expected.Decisions[i], actual.Decisions[i]
		if reflect.DeepEqual(left, right) {
			continue
		}
		if left.Tick != right.Tick {
			return intMismatch(expected.Tick, left.SkillID, fmt.Sprintf("decisions[%d].tick", i), left.Tick, right.Tick)
		}
		if left.Order != right.Order {
			return intMismatch(expected.Tick, left.SkillID, fmt.Sprintf("decisions[%d].order", i), int64(left.Order), int64(right.Order))
		}
		if left.SkillID != right.SkillID {
			return &Mismatch{Tick: expected.Tick, SkillID: left.SkillID, Field: fmt.Sprintf("decisions[%d].skillId", i), Expected: left.SkillID, Actual: right.SkillID}
		}
		if left.TargetID != right.TargetID {
			return &Mismatch{Tick: expected.Tick, SkillID: left.SkillID, Field: fmt.Sprintf("decisions[%d].targetId", i), Expected: left.TargetID, Actual: right.TargetID}
		}
		if left.Priority != right.Priority {
			return intMismatch(expected.Tick, left.SkillID, fmt.Sprintf("decisions[%d].priority", i), int64(left.Priority), int64(right.Priority))
		}
		if left.ConflictType != right.ConflictType {
			return &Mismatch{Tick: expected.Tick, SkillID: left.SkillID, Field: fmt.Sprintf("decisions[%d].conflictType", i), Expected: left.ConflictType, Actual: right.ConflictType}
		}
		if left.Result != right.Result {
			return &Mismatch{Tick: expected.Tick, SkillID: left.SkillID, Field: fmt.Sprintf("decisions[%d].result", i), Expected: left.Result, Actual: right.Result}
		}
		if left.Reason != right.Reason {
			return &Mismatch{Tick: expected.Tick, SkillID: left.SkillID, Field: fmt.Sprintf("decisions[%d].reason", i), Expected: left.Reason, Actual: right.Reason}
		}
		if left.PoolAfter != right.PoolAfter {
			return intMismatch(expected.Tick, left.SkillID, fmt.Sprintf("decisions[%d].poolAfter", i), left.PoolAfter, right.PoolAfter)
		}
		if left.CooldownAfter != right.CooldownAfter {
			return intMismatch(expected.Tick, left.SkillID, fmt.Sprintf("decisions[%d].cooldownAfter", i), left.CooldownAfter, right.CooldownAfter)
		}
	}
	return nil
}

func boolMismatch(tick int64, skillID, field string, expected, actual bool) *Mismatch {
	return &Mismatch{Tick: tick, SkillID: skillID, Field: field, Expected: fmt.Sprint(expected), Actual: fmt.Sprint(actual)}
}

func intMismatch(tick int64, skillID, field string, expected, actual int64) *Mismatch {
	return &Mismatch{Tick: tick, SkillID: skillID, Field: field, Expected: fmt.Sprint(expected), Actual: fmt.Sprint(actual)}
}
