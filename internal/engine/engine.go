// Package engine 实现逐 tick 技能排程核心、同 tick 确定性争用裁决，
// 以及实时/恢复/全量三条路径共用的无浮点确定性计算。
package engine

import (
	"sort"

	"skillsched/internal/errx"
	"skillsched/internal/expr"
	"skillsched/internal/model"
)

// Runtime 是排程可变运行态（可序列化为检查点）。
type Runtime struct {
	Tick         int64             `json:"tick"`
	PoolRemain   map[string]int64  `json:"pool_remain"`
	CoolRemain   map[string]int64  `json:"cool_remain"`
	GroupLock    map[string]int64  `json:"group_lock"`
	SkillStates  map[string]string `json:"skill_states"`
	IntentOffset int               `json:"intent_offset"`
}

// Engine 绑定只读规则工程与运行态。
type Engine struct {
	rules   *model.RuleSet
	rt      *Runtime
	useTree bool // true 时用表达式树求值，false 用扁平常量表（结果必须一致）
}

// New 构造引擎；runtime 为 nil 时从初始状态开始。
func New(rules *model.RuleSet, rt *Runtime, useTree bool) *Engine {
	if rt == nil {
		rt = InitialRuntime(rules)
	}
	return &Engine{rules: rules, rt: rt, useTree: useTree}
}

// InitialRuntime 构造初始运行态。
func InitialRuntime(rules *model.RuleSet) *Runtime {
	rt := &Runtime{
		PoolRemain:  map[string]int64{},
		CoolRemain:  map[string]int64{},
		GroupLock:   map[string]int64{},
		SkillStates: map[string]string{},
	}
	for _, p := range rules.Pools {
		rt.PoolRemain[p.ID] = p.Capacity
	}
	for _, s := range rules.Skills {
		rt.CoolRemain[s.ID] = 0
		rt.SkillStates[s.ID] = SkillPending
	}
	for _, g := range rules.Groups {
		rt.GroupLock[g.ID] = 0
	}
	return rt
}

func (e *Engine) Runtime() *Runtime { return e.rt }

// EffectiveCooldown 应用冷却缩减，限制不为负且不超过声明上界。
func EffectiveCooldown(s *model.Skill) int64 {
	cdr := s.CDR
	if cdr > s.CDRCap {
		cdr = s.CDRCap
	}
	reduce := s.Cooldown * cdr / 10000 // 整型向下取整
	eff := s.Cooldown - reduce
	if eff < 0 {
		return 0
	}
	return eff
}

func (e *Engine) evalRegen(p *model.Pool) (int64, error) {
	env := expr.Env{"base_regen": 10, "pool_pressure": e.rt.PoolRemain[p.ID]}
	if e.useTree && p.RegenTree != nil {
		return p.RegenTree.Eval(env)
	}
	return p.Regen.Eval(env)
}

// RunWindow 推进 [startTick, startTick+size) 共 size 个 tick，返回该窗口逐 tick 真值。
// 出现无法裁决冲突时返回 E_CONFLICT，且运行态回滚到窗口开始（保留该 tick 原状态）。
func (e *Engine) RunWindow(startTick, size int, intents []model.Intent) ([]model.TickTruth, error) {
	snapshot := e.snapshot()
	out := make([]model.TickTruth, 0, size)
	intentsAt := map[int][]model.Intent{}
	for _, it := range intents {
		if it.Tick >= startTick && it.Tick < startTick+size {
			intentsAt[it.Tick] = append(intentsAt[it.Tick], it)
		}
	}

	for tick := startTick; tick < startTick+size; tick++ {
		tt, err := e.step(tick, intentsAt[tick])
		if err != nil {
			e.restore(snapshot)
			return nil, err
		}
		out = append(out, tt)
	}
	return out, nil
}

func (e *Engine) snapshot() *Runtime {
	cp := &Runtime{
		Tick: e.rt.Tick, IntentOffset: e.rt.IntentOffset,
		PoolRemain:  map[string]int64{},
		CoolRemain:  map[string]int64{},
		GroupLock:   map[string]int64{},
		SkillStates: map[string]string{},
	}
	for k, v := range e.rt.PoolRemain {
		cp.PoolRemain[k] = v
	}
	for k, v := range e.rt.CoolRemain {
		cp.CoolRemain[k] = v
	}
	for k, v := range e.rt.GroupLock {
		cp.GroupLock[k] = v
	}
	for k, v := range e.rt.SkillStates {
		cp.SkillStates[k] = v
	}
	return cp
}

func (e *Engine) restore(cp *Runtime) {
	e.rt = cp
}

func (e *Engine) step(tick int, batch []model.Intent) (model.TickTruth, error) {
	rules := e.rules
	skillByID := rules.SkillByID()
	groupByID := rules.GroupByID()
	tt := model.TickTruth{Tick: tick, Pools: map[string]int64{}}

	// 技能进入待结算。
	for _, s := range rules.Skills {
		switch e.rt.SkillStates[s.ID] {
		case SkillPending:
			if err := transitionSkill(e.rt.SkillStates[s.ID], SkillReady); err != nil {
				return tt, err
			}
			e.rt.SkillStates[s.ID] = SkillReady
		case SkillCommitted, SkillReady:
			// 上一 tick 已提交或本就待结算：本 tick 重新进入待结算（合法自循环语义）。
			e.rt.SkillStates[s.ID] = SkillReady
		case SkillFailed:
			if err := transitionSkill(SkillFailed, SkillReady); err != nil {
				return tt, err
			}
			e.rt.SkillStates[s.ID] = SkillReady
		}
	}

	// 1) 冷却/组锁推进。
	for sid := range e.rt.CoolRemain {
		if e.rt.CoolRemain[sid] > 0 {
			e.rt.CoolRemain[sid]--
		}
	}
	for gid := range e.rt.GroupLock {
		if e.rt.GroupLock[gid] > 0 {
			e.rt.GroupLock[gid]--
		}
	}

	// 2) 资源回复（无浮点）。
	for _, p := range rules.Pools {
		delta, err := e.evalRegen(p)
		if err != nil {
			return tt, err
		}
		nv := e.rt.PoolRemain[p.ID] + delta
		if nv < 0 {
			nv = 0
		}
		if nv > p.Capacity {
			nv = p.Capacity
		}
		e.rt.PoolRemain[p.ID] = nv
	}

	// 3) 稳定排序意图，杜绝 map 遍历/线程相关。
	ordered := append([]model.Intent{}, batch...)
	if dup := findDuplicate(ordered); dup != "" {
		return tt, errx.New(errx.EConflict, "duplicate unresolved intent for skill in same tick").
			With("tick", itoa(tick), "skill_id", dup)
	}
	sort.SliceStable(ordered, func(i, j int) bool {
		si, sj := skillByID[ordered[i].SkillID], skillByID[ordered[j].SkillID]
		if si.Priority != sj.Priority {
			return si.Priority < sj.Priority
		}
		if si.ID != sj.ID {
			return si.ID < sj.ID
		}
		return ordered[i].Seq < ordered[j].Seq
	})

	suppressed := map[string]bool{}
	casted := map[string]bool{}
	for _, it := range ordered {
		s := skillByID[it.SkillID]
		if suppressed[s.ID] || casted[s.ID] {
			continue
		}
		e.rt.SkillStates[s.ID] = SkillSettling
		reason := e.tryCast(tick, s, casted, suppressed, &tt, groupByID)
		if reason == "" {
			if err := transitionSkill(SkillSettling, SkillCommitted); err != nil {
				return tt, err
			}
			e.rt.SkillStates[s.ID] = SkillCommitted
			casted[s.ID] = true
		} else {
			if err := transitionSkill(SkillSettling, SkillFailed); err != nil {
				return tt, err
			}
			e.rt.SkillStates[s.ID] = SkillFailed
			suppressed[s.ID] = true
		}
	}

	// 4) 汇总。
	for _, s := range rules.Skills {
		// 被同组预抑制而未进入结算循环的技能，按失败口径记录终态快照。
		skillFailed := suppressed[s.ID]
		avail := e.rt.CoolRemain[s.ID] == 0 &&
			(s.SharedGroup == "" || e.rt.GroupLock[s.SharedGroup] == 0) &&
			e.rt.PoolRemain[s.PoolID] >= s.Cost && !suppressed[s.ID]
		state := e.rt.SkillStates[s.ID]
		if skillFailed {
			state = SkillFailed
		}
		tt.Skills = append(tt.Skills, model.SkillTick{
			SkillID: s.ID, Available: avail, CooldownRem: e.rt.CoolRemain[s.ID],
			PoolID: s.PoolID, PoolRemain: e.rt.PoolRemain[s.PoolID], State: state,
		})
	}
	for pid, v := range e.rt.PoolRemain {
		tt.Pools[pid] = v
	}
	e.rt.Tick = int64(tick + 1)
	return tt, nil
}

// tryCast 尝试施放，返回空串表示成功，否则返回失败原因并记录裁决。
func (e *Engine) tryCast(tick int, s *model.Skill, casted, suppressed map[string]bool,
	tt *model.TickTruth, groupByID map[string]*model.SharedGroup) string {
	if e.rt.CoolRemain[s.ID] > 0 {
		tt.Arbitrations = append(tt.Arbitrations, model.Arbitration{
			Tick: tick, SkillID: s.ID, Contention: "cooldown",
			Suppressed: s.ID, Basis: "cooldown_remaining>0",
		})
		return "cooldown"
	}
	if s.SharedGroup != "" && e.rt.GroupLock[s.SharedGroup] > 0 {
		tt.Arbitrations = append(tt.Arbitrations, model.Arbitration{
			Tick: tick, SkillID: s.ID, Contention: "shared_group",
			Winner: firstCasted(e.rules, s.SharedGroup, casted), Suppressed: s.ID,
			Basis: "group_lock_active;lower_priority_loses",
		})
		return "group_locked"
	}
	if e.rt.PoolRemain[s.PoolID] < s.Cost {
		tt.Arbitrations = append(tt.Arbitrations, model.Arbitration{
			Tick: tick, SkillID: s.ID, Contention: "resource",
			Winner: firstCastedInPool(e.rules, s.PoolID, casted), Suppressed: s.ID,
			Basis: "insufficient_pool;higher_priority_first",
		})
		return "insufficient_resource"
	}
	// 成功施放：扣资源、设冷却、加共享组锁并抑制同组其余成员。
	e.rt.PoolRemain[s.PoolID] -= s.Cost
	e.rt.CoolRemain[s.ID] = EffectiveCooldown(s)
	if s.SharedGroup != "" {
		g := groupByID[s.SharedGroup]
		e.rt.GroupLock[s.SharedGroup] = g.GroupCooldown
		for _, mid := range g.Members {
			if mid != s.ID && !casted[mid] {
				suppressed[mid] = true
				// 被同组施放抑制的成员本 tick 置失败，下 tick 回到待结算可重试。
				e.rt.SkillStates[mid] = SkillFailed
				tt.Arbitrations = append(tt.Arbitrations, model.Arbitration{
					Tick: tick, SkillID: mid, Contention: "shared_group",
					Winner: s.ID, Suppressed: mid, Basis: "same_group_cast_locks_members",
				})
			}
		}
	}
	return ""
}

func firstCasted(rules *model.RuleSet, gid string, casted map[string]bool) string {
	for _, s := range rules.Skills {
		if s.SharedGroup == gid && casted[s.ID] {
			return s.ID
		}
	}
	return gid
}

func firstCastedInPool(rules *model.RuleSet, poolID string, casted map[string]bool) string {
	for _, s := range rules.Skills {
		if s.PoolID == poolID && casted[s.ID] {
			return s.ID
		}
	}
	return poolID
}

func findDuplicate(batch []model.Intent) string {
	seen := map[string]int{}
	for _, it := range batch {
		seen[it.SkillID]++
		if seen[it.SkillID] > 1 {
			return it.SkillID
		}
	}
	return ""
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}
