package engine

import "replaykernel/internal/model"

// unitRuntime 是单位在内存中的结算状态。
type unitRuntime struct {
	spec      model.UnitSpec
	hp        int64
	mp        int64
	alive     bool
	cooldowns map[string]int
}

// matchRuntime 是一局回放的全量内存结算状态。
type matchRuntime struct {
	replay       *model.Replay
	units        map[string]*unitRuntime
	unitOrder    []string
	skills       map[string]model.Skill
	snapshots    []model.TurnState
	rulings      []model.Ruling
	actionOffset int
	rngDraws     int
	lastRngSeed  int64
	finished     bool
	winner       int
}

func newMatchRuntime(r *model.Replay) *matchRuntime {
	m := &matchRuntime{
		replay:      r,
		units:       make(map[string]*unitRuntime, len(r.Units)),
		skills:      make(map[string]model.Skill, len(r.Skills)),
		winner:      -1,
		lastRngSeed: 0,
	}
	for _, s := range r.Skills {
		m.skills[s.ID] = s
	}
	for _, u := range r.Units {
		cd := make(map[string]int, len(u.Skills))
		for _, sid := range u.Skills {
			cd[sid] = 0
		}
		m.units[u.ID] = &unitRuntime{
			spec:      u,
			hp:        u.StartHP,
			mp:        u.StartMP,
			alive:     true,
			cooldowns: cd,
		}
		m.unitOrder = append(m.unitOrder, u.ID)
	}
	return m
}

// snapshot 固化当前全部单位状态，单位顺序按回放声明顺序，保证字节级确定。
func (m *matchRuntime) snapshot(turnNo int) model.TurnState {
	out := make([]model.UnitState, 0, len(m.unitOrder))
	for _, id := range m.unitOrder {
		u := m.units[id]
		cd := make(map[string]int, len(u.cooldowns))
		for k, v := range u.cooldowns {
			cd[k] = v
		}
		out = append(out, model.UnitState{
			UnitID:    id,
			HP:        u.hp,
			MP:        u.mp,
			Alive:     u.alive,
			Cooldowns: cd,
		})
	}
	return model.TurnState{TurnNo: turnNo, Winner: m.winner, Units: out}
}

// deepCopy 用于并行场景下的状态隔离（本内核并行只在对局间进行，
// 单局内严格串行；该方法服务于测试与窗口重试隔离）。
func (m *matchRuntime) deepCopy() *matchRuntime {
	cp := &matchRuntime{
		replay:       m.replay,
		units:        make(map[string]*unitRuntime, len(m.units)),
		unitOrder:    append([]string(nil), m.unitOrder...),
		skills:       m.skills,
		actionOffset: m.actionOffset,
		rngDraws:     m.rngDraws,
		lastRngSeed:  m.lastRngSeed,
		finished:     m.finished,
		winner:       m.winner,
	}
	for id, u := range m.units {
		cd := make(map[string]int, len(u.cooldowns))
		for k, v := range u.cooldowns {
			cd[k] = v
		}
		cp.units[id] = &unitRuntime{
			spec:      u.spec,
			hp:        u.hp,
			mp:        u.mp,
			alive:     u.alive,
			cooldowns: cd,
		}
	}
	cp.snapshots = append([]model.TurnState(nil), m.snapshots...)
	cp.rulings = append([]model.Ruling(nil), m.rulings...)
	return cp
}
