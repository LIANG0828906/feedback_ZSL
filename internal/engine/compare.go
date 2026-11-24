package engine

import (
	"fmt"

	"replaykernel/internal/model"
	"replaykernel/internal/store"
)

// DiffPoint 定位首个不一致的回合、单位、字段与双方取值。
type DiffPoint struct {
	TurnNo int
	UnitID string
	Field  string
	Expect string
	Got    string
}

// CompareStores 比对两条路径的持久化产物：快照、裁决、窗口提交序列。
// 全程只读 SQLite，不引用任何内存主链路状态；不一致返回 E_DIFF。
func CompareStores(matchID string, a, b *store.Store) error {
	snapsA, err := a.LoadSnapshots(matchID)
	if err != nil {
		return err
	}
	snapsB, err := b.LoadSnapshots(matchID)
	if err != nil {
		return err
	}
	if dp, ok := diffSnapshots(snapsA, snapsB); !ok {
		return model.NewError(model.EDiff,
			"snapshot mismatch at turn=%d unit=%s field=%s: expect=%s got=%s",
			dp.TurnNo, dp.UnitID, dp.Field, dp.Expect, dp.Got).
			WithMatch(matchID).WithTurn(dp.TurnNo).WithField(dp.Field, 0, 0)
	}

	rulingsA, err := a.LoadRulings(matchID)
	if err != nil {
		return err
	}
	rulingsB, err := b.LoadRulings(matchID)
	if err != nil {
		return err
	}
	if dp, ok := diffRulings(rulingsA, rulingsB); !ok {
		return model.NewError(model.EDiff,
			"ruling mismatch at turn=%d unit=%s field=%s: expect=%s got=%s",
			dp.TurnNo, dp.UnitID, dp.Field, dp.Expect, dp.Got).
			WithMatch(matchID).WithTurn(dp.TurnNo)
	}

	winA, err := a.LoadWindows(matchID)
	if err != nil {
		return err
	}
	winB, err := b.LoadWindows(matchID)
	if err != nil {
		return err
	}
	if dp, ok := diffWindows(winA, winB); !ok {
		return model.NewError(model.EDiff,
			"window commit sequence mismatch at window field=%s: expect=%s got=%s",
			dp.Field, dp.Expect, dp.Got).WithMatch(matchID)
	}
	return nil
}

func diffSnapshots(a, b []model.TurnState) (DiffPoint, bool) {
	if len(a) != len(b) {
		return DiffPoint{Field: "turn_count", Expect: fmt.Sprint(len(a)), Got: fmt.Sprint(len(b))}, false
	}
	for i := range a {
		if a[i].TurnNo != b[i].TurnNo {
			return DiffPoint{TurnNo: a[i].TurnNo, Field: "turn_no",
				Expect: fmt.Sprint(a[i].TurnNo), Got: fmt.Sprint(b[i].TurnNo)}, false
		}
		if a[i].Winner != b[i].Winner {
			return DiffPoint{TurnNo: a[i].TurnNo, Field: "winner",
				Expect: fmt.Sprint(a[i].Winner), Got: fmt.Sprint(b[i].Winner)}, false
		}
		if len(a[i].Units) != len(b[i].Units) {
			return DiffPoint{TurnNo: a[i].TurnNo, Field: "unit_count",
				Expect: fmt.Sprint(len(a[i].Units)), Got: fmt.Sprint(len(b[i].Units))}, false
		}
		for j := range a[i].Units {
			ua, ub := a[i].Units[j], b[i].Units[j]
			if ua.UnitID != ub.UnitID {
				return DiffPoint{TurnNo: a[i].TurnNo, Field: "unit_id",
					Expect: ua.UnitID, Got: ub.UnitID}, false
			}
			if dp, ok := diffUnit(a[i].TurnNo, ua, ub); !ok {
				return dp, false
			}
		}
	}
	return DiffPoint{}, true
}

func diffUnit(turn int, a, b model.UnitState) (DiffPoint, bool) {
	base := DiffPoint{TurnNo: turn, UnitID: a.UnitID}
	if a.HP != b.HP {
		base.Field = "hp_milli"
		base.Expect, base.Got = fmt.Sprint(a.HP), fmt.Sprint(b.HP)
		return base, false
	}
	if a.MP != b.MP {
		base.Field = "mp_milli"
		base.Expect, base.Got = fmt.Sprint(a.MP), fmt.Sprint(b.MP)
		return base, false
	}
	if a.Alive != b.Alive {
		base.Field = "alive"
		base.Expect, base.Got = fmt.Sprint(a.Alive), fmt.Sprint(b.Alive)
		return base, false
	}
	if len(a.Cooldowns) != len(b.Cooldowns) {
		base.Field = "cooldown_count"
		base.Expect, base.Got = fmt.Sprint(len(a.Cooldowns)), fmt.Sprint(len(b.Cooldowns))
		return base, false
	}
	for skill, ca := range a.Cooldowns {
		cb, ok := b.Cooldowns[skill]
		if !ok || ca != cb {
			base.Field = "cooldown." + skill
			base.Expect, base.Got = fmt.Sprint(ca), fmt.Sprint(cb)
			return base, false
		}
	}
	return DiffPoint{}, true
}

func diffRulings(a, b []model.Ruling) (DiffPoint, bool) {
	if len(a) != len(b) {
		return DiffPoint{Field: "ruling_count", Expect: fmt.Sprint(len(a)), Got: fmt.Sprint(len(b))}, false
	}
	for i := range a {
		ra, rb := a[i], b[i]
		if ra.TurnNo != rb.TurnNo {
			return DiffPoint{TurnNo: ra.TurnNo, Field: "ruling.turn_no",
				Expect: fmt.Sprint(ra.TurnNo), Got: fmt.Sprint(rb.TurnNo)}, false
		}
		if ra.UnitID != rb.UnitID {
			return DiffPoint{TurnNo: ra.TurnNo, UnitID: ra.UnitID, Field: "ruling.unit_id",
				Expect: ra.UnitID, Got: rb.UnitID}, false
		}
		if ra.SkillID != rb.SkillID {
			return DiffPoint{TurnNo: ra.TurnNo, UnitID: ra.UnitID, Field: "ruling.skill_id",
				Expect: ra.SkillID, Got: rb.SkillID}, false
		}
		if ra.Reason != rb.Reason {
			return DiffPoint{TurnNo: ra.TurnNo, UnitID: ra.UnitID, Field: "ruling.reason",
				Expect: string(ra.Reason), Got: string(rb.Reason)}, false
		}
		if ra.Allowed != rb.Allowed {
			return DiffPoint{TurnNo: ra.TurnNo, UnitID: ra.UnitID, Field: "ruling.allowed",
				Expect: fmt.Sprint(ra.Allowed), Got: fmt.Sprint(rb.Allowed)}, false
		}
		if len(ra.Violations) != len(rb.Violations) {
			return DiffPoint{TurnNo: ra.TurnNo, UnitID: ra.UnitID, Field: "ruling.violations_len",
				Expect: fmt.Sprint(len(ra.Violations)), Got: fmt.Sprint(len(rb.Violations))}, false
		}
		for v := range ra.Violations {
			if ra.Violations[v] != rb.Violations[v] {
				return DiffPoint{TurnNo: ra.TurnNo, UnitID: ra.UnitID, Field: "ruling.violations",
					Expect: string(ra.Violations[v]), Got: string(rb.Violations[v])}, false
			}
		}
	}
	return DiffPoint{}, true
}

func diffWindows(a, b []store.WindowRow) (DiffPoint, bool) {
	if len(a) != len(b) {
		return DiffPoint{Field: "window_count", Expect: fmt.Sprint(len(a)), Got: fmt.Sprint(len(b))}, false
	}
	for i := range a {
		if a[i].WindowNo != b[i].WindowNo {
			return DiffPoint{Field: "window_no", Expect: fmt.Sprint(a[i].WindowNo), Got: fmt.Sprint(b[i].WindowNo)}, false
		}
		if a[i].Status != b[i].Status {
			return DiffPoint{Field: "status", Expect: a[i].Status, Got: b[i].Status}, false
		}
		if a[i].ContentHash != b[i].ContentHash {
			return DiffPoint{Field: "content_hash", Expect: a[i].ContentHash, Got: b[i].ContentHash}, false
		}
		if a[i].ChainHash != b[i].ChainHash {
			return DiffPoint{Field: "chain_hash", Expect: a[i].ChainHash, Got: b[i].ChainHash}, false
		}
		if a[i].ActionOffset != b[i].ActionOffset {
			return DiffPoint{Field: "action_offset", Expect: fmt.Sprint(a[i].ActionOffset), Got: fmt.Sprint(b[i].ActionOffset)}, false
		}
	}
	return DiffPoint{}, true
}
