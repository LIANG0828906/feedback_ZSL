package gen

import (
	"sort"

	"skillsched/internal/expr"
	"skillsched/internal/model"
)

// 与引擎状态机字面量保持一致（gen 不反向依赖 engine 以避免 import cycle）。
const (
	engineReady     = "ready"
	engineCommitted = "committed"
	engineFailed    = "failed"
)

// refState 是参考模拟的私有运行态，刻意与 engine 包隔离，
// 以保证真值是“解析构造”而非引擎自证。
type refState struct {
	poolRemain map[string]int64
	coolRemain map[string]int64
	groupLock  map[string]int64
}

func referenceSimulate(name string, seed int64, rules *model.RuleSet, intents []model.Intent) *model.Truth {
	st := &refState{
		poolRemain: map[string]int64{},
		coolRemain: map[string]int64{},
		groupLock:  map[string]int64{},
	}
	for _, p := range rules.Pools {
		st.poolRemain[p.ID] = p.Capacity
	}

	truth := &model.Truth{Scenario: name, Seed: seed, Horizon: rules.Params.Horizon}
	skillByID := rules.SkillByID()
	groupByID := rules.GroupByID()

	// 按 tick 预聚合意图，避免依赖 map 遍历。
	intentsAt := map[int][]model.Intent{}
	for _, it := range intents {
		intentsAt[it.Tick] = append(intentsAt[it.Tick], it)
	}

	for tick := 0; tick < rules.Params.Horizon; tick++ {
		tt := model.TickTruth{Tick: tick, Pools: map[string]int64{}}

		// 1) 冷却与共享组锁推进。
		for sid := range st.coolRemain {
			if st.coolRemain[sid] > 0 {
				st.coolRemain[sid]--
			}
		}
		for gid := range st.groupLock {
			if st.groupLock[gid] > 0 {
				st.groupLock[gid]--
			}
		}

		// 2) 资源回复：用扁平常量表求值（树形态等价性由引擎侧交叉验证）。
		for _, p := range rules.Pools {
			env := expr.Env{
				"base_regen":    10,
				"pool_pressure": st.poolRemain[p.ID],
			}
			delta, _ := p.Regen.Eval(env)
			nv := st.poolRemain[p.ID] + delta
			if nv < 0 {
				nv = 0
			}
			if nv > p.Capacity {
				nv = p.Capacity
			}
			st.poolRemain[p.ID] = nv
		}

		// 3) 意图裁决：稳定排序（priority, skillID, seq），不依赖 map/线程。
		batch := append([]model.Intent{}, intentsAt[tick]...)
		sort.SliceStable(batch, func(i, j int) bool {
			si, sj := skillByID[batch[i].SkillID], skillByID[batch[j].SkillID]
			if si.Priority != sj.Priority {
				return si.Priority < sj.Priority
			}
			if si.ID != sj.ID {
				return si.ID < sj.ID
			}
			return batch[i].Seq < batch[j].Seq
		})

		suppressed := map[string]bool{}
		casted := map[string]bool{}
		for _, it := range batch {
			s := skillByID[it.SkillID]
			if suppressed[s.ID] || casted[s.ID] {
				continue
			}
			// 前置校验：冷却、共享组锁、资源。
			if st.coolRemain[s.ID] > 0 {
				tt.Arbitrations = append(tt.Arbitrations, model.Arbitration{
					Tick: tick, SkillID: s.ID, Contention: "cooldown",
					Winner: "", Suppressed: s.ID, Basis: "cooldown_remaining>0",
				})
				suppressed[s.ID] = true
				continue
			}
			if s.SharedGroup != "" && st.groupLock[s.SharedGroup] > 0 {
				winner := groupWinner(rules, st, s.SharedGroup, casted)
				tt.Arbitrations = append(tt.Arbitrations, model.Arbitration{
					Tick: tick, SkillID: s.ID, Contention: "shared_group",
					Winner: winner, Suppressed: s.ID,
					Basis: "group_lock_active;lower_priority_loses",
				})
				suppressed[s.ID] = true
				continue
			}
			if st.poolRemain[s.PoolID] < s.Cost {
				// 同池资源争用：按优先级裁决，资源不足者被抑制。
				winner := poolWinner(rules, s.PoolID, casted)
				tt.Arbitrations = append(tt.Arbitrations, model.Arbitration{
					Tick: tick, SkillID: s.ID, Contention: "resource",
					Winner: winner, Suppressed: s.ID,
					Basis: "insufficient_pool;higher_priority_first",
				})
				suppressed[s.ID] = true
				continue
			}
			// 施放成功。
			st.poolRemain[s.PoolID] -= s.Cost
			effCD := effectiveCooldown(s)
			st.coolRemain[s.ID] = effCD
			if s.SharedGroup != "" {
				g := groupByID[s.SharedGroup]
				st.groupLock[s.SharedGroup] = g.GroupCooldown
				// 同组其余成员本 tick 被抑制。
				for _, mid := range g.Members {
					if mid != s.ID && !casted[mid] {
						suppressed[mid] = true
						tt.Arbitrations = append(tt.Arbitrations, model.Arbitration{
							Tick: tick, SkillID: mid, Contention: "shared_group",
							Winner: s.ID, Suppressed: mid,
							Basis: "same_group_cast_locks_members",
						})
					}
				}
			}
			casted[s.ID] = true
		}

		// 4) 汇总逐技能状态。
		for _, s := range rules.Skills {
			avail := st.coolRemain[s.ID] == 0 &&
				(s.SharedGroup == "" || st.groupLock[s.SharedGroup] == 0) &&
				st.poolRemain[s.PoolID] >= s.Cost && !suppressed[s.ID]
			state := engineReady // 本 tick 未参与结算：保持待结算态
			reason := ""
			if casted[s.ID] {
				state = engineCommitted
			} else if suppressed[s.ID] {
				state = engineFailed
				reason = "suppressed_or_precheck_failed"
			}
			tt.Skills = append(tt.Skills, model.SkillTick{
				SkillID: s.ID, Available: avail, CooldownRem: st.coolRemain[s.ID],
				PoolID: s.PoolID, PoolRemain: st.poolRemain[s.PoolID],
				State: state, Reason: reason,
			})
		}
		for pid, v := range st.poolRemain {
			tt.Pools[pid] = v
		}
		truth.Ticks = append(truth.Ticks, tt)
	}
	return truth
}

// effectiveCooldown 应用冷却缩减，限制不为负且不超过声明上界。
func effectiveCooldown(s *model.Skill) int64 {
	cdr := s.CDR
	if cdr > s.CDRCap {
		cdr = s.CDRCap
	}
	// 缩减量 = cooldown * cdr / 10000，向下取整，保证剩余冷却非负。
	reduce := s.Cooldown * cdr / 10000
	eff := s.Cooldown - reduce
	if eff < 0 {
		eff = 0
	}
	return eff
}

func groupWinner(rules *model.RuleSet, st *refState, gid string, casted map[string]bool) string {
	for _, s := range rules.Skills {
		if s.SharedGroup == gid && casted[s.ID] {
			return s.ID
		}
	}
	return gid
}

func poolWinner(rules *model.RuleSet, poolID string, casted map[string]bool) string {
	for _, s := range rules.Skills {
		if s.PoolID == poolID && casted[s.ID] {
			return s.ID
		}
	}
	return poolID
}
