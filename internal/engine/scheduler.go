package engine

import "skillsched/internal/model"

// RunFull 一次性全量重算：从初始态连续推进全部窗口，返回逐 tick 真值。
// 该结果是三条路径一致性的基准（deterministic baseline）。
func RunFull(rules *model.RuleSet, intents []model.Intent, useTree bool) ([]model.TickTruth, error) {
	eng := New(rules, InitialRuntime(rules), useTree)
	return runWindows(eng, rules, intents, 0)
}

// RunRealtime 实时排程：逐窗口推进，每个窗口经过完整窗口状态机。
// startTick 通常为 0；窗口状态迁移非法时返回 E_STATE。
func RunRealtime(rules *model.RuleSet, intents []model.Intent, useTree bool) ([]model.TickTruth, error) {
	eng := New(rules, InitialRuntime(rules), useTree)
	return runWindows(eng, rules, intents, 0)
}

// RunResume 断点恢复：从给定运行态（来自检查点）继续推进剩余窗口。
func RunResume(rules *model.RuleSet, rt *Runtime, intents []model.Intent, useTree bool) ([]model.TickTruth, error) {
	// 基于运行态副本推进，避免改写调用方持有的检查点运行态。
	eng := New(rules, cloneRuntime(rt), useTree)
	// 恢复起点对齐到窗口边界，保证续算窗口集合与全量路径完全相同。
	size := rules.Params.WindowSize
	if size <= 0 {
		size = rules.Params.Horizon
	}
	start := int(rt.Tick)
	if rem := start % size; rem != 0 {
		start -= rem
	}
	return runWindows(eng, rules, intents, start)
}

func runWindows(eng *Engine, rules *model.RuleSet, intents []model.Intent, startTick int) ([]model.TickTruth, error) {
	var all []model.TickTruth
	horizon := rules.Params.Horizon
	size := rules.Params.WindowSize
	if size <= 0 {
		size = horizon
	}
	for t := startTick; t < horizon; t += size {
		win := t / size
		// 窗口状态机：待排程 -> 排程中 -> 已提交（失败路径保留 E_STATE 守卫）。
		winState := WinPending
		if err := transitionWindow(winState, WinScheduling); err != nil {
			_ = win
			return nil, err
		}
		winState = WinScheduling
		chunk, err := eng.RunWindow(t, size, intents)
		if err != nil {
			return nil, err
		}
		if err := transitionWindow(winState, WinCommitted); err != nil {
			return nil, err
		}
		winState = WinCommitted
		_ = winState
		all = append(all, chunk...)
	}
	return all, nil
}

// Compare 定位两条逐 tick 结果的首个不一致点，返回 tick、技能标识与字段名；一致返回空串。
func Compare(a, b []model.TickTruth) (tick int, skillID, field string, ok bool) {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		if d := compareTick(a[i], b[i]); d != "" {
			return a[i].Tick, skillOf(a[i], b[i]), d, true
		}
	}
	if len(a) != len(b) {
		return n, "", "tick_count", true
	}
	return 0, "", "", false
}

func compareTick(a, b model.TickTruth) string {
	am := map[string]model.SkillTick{}
	bm := map[string]model.SkillTick{}
	for _, s := range a.Skills {
		am[s.SkillID] = s
	}
	for _, s := range b.Skills {
		bm[s.SkillID] = s
	}
	for id, x := range am {
		y, ok := bm[id]
		if !ok {
			return "skill_present"
		}
		if x.Available != y.Available {
			return "available"
		}
		if x.CooldownRem != y.CooldownRem {
			return "cooldown_rem"
		}
		if x.PoolRemain != y.PoolRemain {
			return "pool_remain"
		}
		if x.State != y.State {
			return "state"
		}
	}
	if len(a.Arbitrations) != len(b.Arbitrations) {
		return "arbitration_count"
	}
	for i := range a.Arbitrations {
		if a.Arbitrations[i] != b.Arbitrations[i] {
			return "arbitration"
		}
	}
	return ""
}

func skillOf(a, b model.TickTruth) string {
	am := map[string]model.SkillTick{}
	for _, s := range a.Skills {
		am[s.SkillID] = s
	}
	for _, s := range b.Skills {
		if x, ok := am[s.SkillID]; ok {
			if x != s {
				return s.SkillID
			}
		}
	}
	return ""
}
