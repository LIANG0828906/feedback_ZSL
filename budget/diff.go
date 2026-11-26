package budget

import (
	"fmt"
	"reflect"
)

// CompareReports compares every deterministic tick field, arbitration detail
// and window commit boundary. It returns E_DIFF with the first tick/field.
func CompareReports(want, got *RunReport) error {
	return compareReports(want, got, true)
}

// CompareTickReports compares tick states and arbitration but intentionally
// ignores window commit sequences. The parsed truth has no persistence layer.
func CompareTickReports(want, got *RunReport) error {
	return compareReports(want, got, false)
}

func compareReports(want, got *RunReport, compareCommitSequence bool) error {
	if want == nil || got == nil {
		return &Error{Code: ECodeDiff, Message: "nil report in comparison"}
	}
	if want.ContentHash != got.ContentHash {
		return &Error{
			Code: ECodeDiff, Message: "content hash differs", Field: "content_hash",
			Expected: want.ContentHash, Actual: got.ContentHash,
		}
	}
	if want.AlgorithmVersion != got.AlgorithmVersion {
		return &Error{
			Code: ECodeDiff, Message: "algorithm version differs", Field: "algorithm_version",
			Expected: want.AlgorithmVersion, Actual: got.AlgorithmVersion,
		}
	}
	if len(want.Ticks) != len(got.Ticks) {
		return diffError(-1, "tick_count", len(want.Ticks), len(got.Ticks))
	}
	for i := range want.Ticks {
		w := want.Ticks[i]
		g := got.Ticks[i]
		tick := w.Tick
		if w.Tick != g.Tick {
			return diffError(i, "tick", w.Tick, g.Tick)
		}
		if w.ResourceCurrent != g.ResourceCurrent {
			return diffError(tick, "resource_current", w.ResourceCurrent, g.ResourceCurrent)
		}
		if w.RegenApplied != g.RegenApplied {
			return diffError(tick, "regen_applied", w.RegenApplied, g.RegenApplied)
		}
		if w.Spent != g.Spent {
			return diffError(tick, "spent", w.Spent, g.Spent)
		}
		if w.ConsumedIntentOffset != g.ConsumedIntentOffset {
			return diffError(tick, "consumed_intent_offset", w.ConsumedIntentOffset, g.ConsumedIntentOffset)
		}
		if err := compareSkillStates(tick, w.Skills, g.Skills); err != nil {
			return err
		}
		if err := compareGroupStates(tick, w.Groups, g.Groups); err != nil {
			return err
		}
		if len(w.Casts) != len(g.Casts) {
			return diffError(tick, "casts.length", len(w.Casts), len(g.Casts))
		}
		for j := range w.Casts {
			if !reflect.DeepEqual(w.Casts[j], g.Casts[j]) {
				return diffError(tick, fmt.Sprintf("casts[%d]", j), w.Casts[j], g.Casts[j])
			}
		}
		if err := compareArbitration(tick, want.Arbitration, got.Arbitration); err != nil {
			return err
		}
	}
	if compareCommitSequence {
		return compareCommits(want.Commits, got.Commits)
	}
	return nil
}

func compareSkillStates(tick int, want, got []SkillState) error {
	if len(want) != len(got) {
		return diffError(tick, "skills.length", len(want), len(got))
	}
	wantMap := map[string]SkillState{}
	gotMap := map[string]SkillState{}
	for _, state := range want {
		wantMap[state.SkillID] = state
	}
	for _, state := range got {
		gotMap[state.SkillID] = state
	}
	for id, w := range wantMap {
		g, ok := gotMap[id]
		if !ok {
			return diffError(tick, fmt.Sprintf("skills.%s", id), "present", "missing")
		}
		if w.Available != g.Available {
			return diffError(tick, fmt.Sprintf("skills.%s.available", id), w.Available, g.Available)
		}
		if w.CastableBefore != g.CastableBefore {
			return diffError(tick, fmt.Sprintf("skills.%s.castable_before_settlement", id), w.CastableBefore, g.CastableBefore)
		}
		if w.CooldownRemaining != g.CooldownRemaining {
			return diffError(tick, fmt.Sprintf("skills.%s.cooldown_remaining", id), w.CooldownRemaining, g.CooldownRemaining)
		}
	}
	return nil
}

func compareGroupStates(tick int, want, got []GroupState) error {
	if len(want) != len(got) {
		return diffError(tick, "groups.length", len(want), len(got))
	}
	wantMap := map[string]GroupState{}
	for _, state := range want {
		wantMap[state.GroupID] = state
	}
	for _, g := range got {
		w, ok := wantMap[g.GroupID]
		if !ok {
			return diffError(tick, fmt.Sprintf("groups.%s", g.GroupID), "present", "missing")
		}
		if w.CooldownRemaining != g.CooldownRemaining {
			return diffError(tick, fmt.Sprintf("groups.%s.cooldown_remaining", g.GroupID), w.CooldownRemaining, g.CooldownRemaining)
		}
	}
	return nil
}

func compareArbitration(tick int, want, got [][]ArbitrationRecord) error {
	var wGroup, gGroup []ArbitrationRecord
	for _, group := range want {
		if len(group) > 0 && group[0].Tick == tick {
			wGroup = group
			break
		}
	}
	for _, group := range got {
		if len(group) > 0 && group[0].Tick == tick {
			gGroup = group
			break
		}
	}
	if len(wGroup) != len(gGroup) {
		return diffError(tick, "arbitration.length", len(wGroup), len(gGroup))
	}
	for i := range wGroup {
		w, g := wGroup[i], gGroup[i]
		base := fmt.Sprintf("arbitration[%d]", i)
		if w.SkillID != g.SkillID {
			return diffError(tick, base+".skill_id", w.SkillID, g.SkillID)
		}
		if w.Contention != g.Contention {
			return diffError(tick, base+".contention", w.Contention, g.Contention)
		}
		if w.Decision != g.Decision {
			return diffError(tick, base+".decision", w.Decision, g.Decision)
		}
		if w.Reason != g.Reason {
			return diffError(tick, base+".reason", w.Reason, g.Reason)
		}
		if w.Priority != g.Priority || w.Ordinal != g.Ordinal {
			return diffError(tick, base+".order_key", []int{w.Priority, w.Ordinal}, []int{g.Priority, g.Ordinal})
		}
	}
	return nil
}

func compareCommits(want, got []WindowCommit) error {
	if len(want) != len(got) {
		return diffError(-1, "commits.length", len(want), len(got))
	}
	for i := range want {
		w, g := want[i], got[i]
		field := fmt.Sprintf("commits[%d]", i)
		if w.WindowIndex != g.WindowIndex {
			return diffError(-1, field+".window_index", w.WindowIndex, g.WindowIndex)
		}
		if w.FirstTick != g.FirstTick || w.LastTick != g.LastTick {
			return diffError(w.FirstTick, field+".tick_range", []int{w.FirstTick, w.LastTick}, []int{g.FirstTick, g.LastTick})
		}
		if w.IntentOffset != g.IntentOffset {
			return diffError(w.LastTick, field+".intent_offset", w.IntentOffset, g.IntentOffset)
		}
	}
	return nil
}

func diffError(tick int, field string, expected, actual any) *Error {
	err := &Error{
		Code: ECodeDiff, Message: "reports differ at first located field",
		Field: field, Path: field, Expected: expected, Actual: actual,
	}
	if tick >= 0 {
		err.Tick = &tick
	}
	return err
}
