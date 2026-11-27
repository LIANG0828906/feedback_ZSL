package schema

import (
	"encoding/json"
	"strconv"

	"skillsched/internal/errx"
	"skillsched/internal/expr"
	"skillsched/internal/fixed"
	"skillsched/internal/model"
)

// Export 把内部规则工程导出为目标 schema 版本的 JSON 字节流，并附降级报告。
// 降级无法表达的表达式树/共享组/上界会写入 Report.Losses（E_LOSSY 清单）。
func Export(rules *model.RuleSet, target model.Version) ([]byte, *Report, error) {
	rep := &Report{SourceVersion: rules.SourceVersion, TargetVersion: target}
	switch target {
	case model.V1:
		return exportV1(rules, rep)
	case model.V2:
		return exportV2(rules, rep)
	case model.V3:
		return exportV3(rules, rep)
	default:
		return nil, nil, errx.New(errx.ESchema, "unknown export target").
			With("target_version", string(target))
	}
}

func ticksToMs(rep *Report, path string, ticks int64) (int64, error) {
	ms, err := fixed.TicksToMs(&rep.Truncations, path, ticks)
	if err != nil {
		return 0, err
	}
	rep.Conversions = append(rep.Conversions, FieldConv{
		Path: path, FromVersion: "internal",
		FromValue: strconv.FormatInt(ticks, 10) + "tick",
		ToValue:   strconv.FormatInt(ms, 10) + "ms", Note: "tick->ms exact",
	})
	return ms, nil
}

func internalToPermille(rep *Report, path string, v int64) (int64, error) {
	out, err := fixed.FromInternal(&rep.Truncations, path, v, fixed.Permille, fixed.RoundDown)
	if err != nil {
		return 0, err
	}
	rep.Conversions = append(rep.Conversions, FieldConv{
		Path: path, FromVersion: "internal",
		FromValue: strconv.FormatInt(v, 10) + "/10000",
		ToValue:   strconv.FormatInt(out, 10) + "/1000", Note: "permyriad->permille round down",
	})
	return out, nil
}

func flattenOrLoss(rep *Report, path string, tree *expr.Node) *expr.Flat {
	if tree == nil {
		return &expr.Flat{}
	}
	flat, err := expr.FlatFromTree(tree)
	if err != nil {
		// 无法折叠为线性表：降级为仅常数项，保留节点信息于损失清单。
		val, eerr := tree.Eval(expr.Env{})
		if eerr != nil {
			val = 0
		}
		rep.Losses = append(rep.Losses, LossItem{
			Path: path, Kind: "expression_tree",
			Disposition: "collapse_to_constant", Detail: err.Error(),
		})
		return &expr.Flat{Const: val}
	}
	return flat
}

func exportV1(rules *model.RuleSet, rep *Report) ([]byte, *Report, error) {
	if len(rules.Groups) > 0 {
		rep.Losses = append(rep.Losses, LossItem{
			Path: "shared_groups", Kind: "shared_group",
			Disposition: "dropped", Detail: "v1 has no shared group field",
		})
	}
	out := wireV1{SchemaVersion: 1}
	horizonMs, err := ticksToMs(rep, "params.horizonMs", int64(rules.Params.Horizon))
	if err != nil {
		return nil, nil, err
	}
	windowMs, err := ticksToMs(rep, "params.windowMs", int64(rules.Params.WindowSize))
	if err != nil {
		return nil, nil, err
	}
	out.Params = wireV1Params{
		ParamVersion: rules.Params.ParamVersion, TickMs: rules.Params.TickMillis,
		HorizonMs: int(horizonMs), WindowMs: int(windowMs),
	}
	for _, p := range rules.Pools {
		flat := flattenOrLoss(rep, "pools."+p.ID+".regenTable", p.RegenTree)
		out.Pools = append(out.Pools, wireV1Pool{
			PoolId: p.ID, PoolName: p.Name, MaxAmount: p.Capacity, RegenTable: flat,
		})
	}
	for _, s := range rules.Skills {
		cdMs, err := ticksToMs(rep, "skills."+s.ID+".cooldownMs", s.Cooldown)
		if err != nil {
			return nil, nil, err
		}
		cdrPerm, err := internalToPermille(rep, "skills."+s.ID+".cooldownReducePermille", s.CDR)
		if err != nil {
			return nil, nil, err
		}
		rep.Losses = append(rep.Losses, LossItem{
			Path: "skills." + s.ID + ".cdr_cap", Kind: "cdr_cap",
			Disposition: "dropped", Detail: "v1 has no cdr cap field",
		})
		out.Skills = append(out.Skills, wireV1Skill{
			SkillId: s.ID, SkillName: s.Name, UsePool: s.PoolID,
			ResourceCost: s.Cost, CooldownMs: cdMs,
			CooldownReducePermille: cdrPerm, OrderWeight: s.Priority,
		})
	}
	b, err := json.Marshal(out)
	return b, rep, err
}

func exportV2(rules *model.RuleSet, rep *Report) ([]byte, *Report, error) {
	out := wireV2{SchemaVersion: 2}
	horizonMs, err := ticksToMs(rep, "params.horizon_ms", int64(rules.Params.Horizon))
	if err != nil {
		return nil, nil, err
	}
	windowMs, err := ticksToMs(rep, "params.window_ms", int64(rules.Params.WindowSize))
	if err != nil {
		return nil, nil, err
	}
	out.Params = wireV2Params{
		ParamVersion: rules.Params.ParamVersion, TickMs: rules.Params.TickMillis,
		HorizonMs: int(horizonMs), WindowMs: int(windowMs),
	}
	for _, p := range rules.Pools {
		flat := flattenOrLoss(rep, "pools."+p.ID+".regen_table", p.RegenTree)
		out.Pools = append(out.Pools, wireV2Pool{
			PoolID: p.ID, PoolName: p.Name, MaxAmount: p.Capacity, RegenTable: flat,
		})
	}
	for _, s := range rules.Skills {
		cdMs, err := ticksToMs(rep, "skills."+s.ID+".cooldown_ms", s.Cooldown)
		if err != nil {
			return nil, nil, err
		}
		cdrPerm, err := internalToPermille(rep, "skills."+s.ID+".cooldown_reduce_permille", s.CDR)
		if err != nil {
			return nil, nil, err
		}
		rep.Losses = append(rep.Losses, LossItem{
			Path: "skills." + s.ID + ".cdr_cap", Kind: "cdr_cap",
			Disposition: "dropped", Detail: "v2 has no cdr cap field",
		})
		out.Skills = append(out.Skills, wireV2Skill{
			SkillID: s.ID, SkillName: s.Name, UsePool: s.PoolID,
			ResourceCost: s.Cost, CooldownMs: cdMs,
			CooldownReducePermille: cdrPerm,
			SharedGroupID:          s.SharedGroup, OrderWeight: s.Priority,
		})
	}
	for _, g := range rules.Groups {
		lockMs, err := ticksToMs(rep, "shared_groups."+g.ID+".lock_ms", g.GroupCooldown)
		if err != nil {
			return nil, nil, err
		}
		out.SharedGroups = append(out.SharedGroups, wireV2Group{
			GroupID: g.ID, GroupName: g.Name, LockMs: lockMs, MemberSkillIDs: g.Members,
		})
	}
	b, err := json.Marshal(out)
	return b, rep, err
}

func exportV3(rules *model.RuleSet, rep *Report) ([]byte, *Report, error) {
	out := wireV3{SchemaVer: 3}
	out.Params = wireV3Params{
		ParamRevision: rules.Params.ParamVersion, TickSpanMs: rules.Params.TickMillis,
		TickHorizon: rules.Params.Horizon, WindowTicks: rules.Params.WindowSize,
	}
	for _, p := range rules.Pools {
		out.ResourcePools = append(out.ResourcePools, wireV3Pool{
			PoolID: p.ID, DisplayName: p.Name, UpperBound: p.Capacity, RegenExpr: p.RegenTree,
		})
	}
	for _, s := range rules.Skills {
		out.Abilities = append(out.Abilities, wireV3Skill{
			AbilityID: s.ID, DisplayName: s.Name, BoundPool: s.PoolID,
			CastCost: s.Cost, CooldownTicks: s.Cooldown,
			ReducePermyriad: s.CDR, ReduceCapPermyriad: s.CDRCap,
			ReduceExpr: s.CDRTree, LockGroup: s.SharedGroup, PriorityRank: s.Priority,
		})
	}
	for _, g := range rules.Groups {
		out.CooldownLocks = append(out.CooldownLocks, wireV3Group{
			LockID: g.ID, DisplayName: g.Name, LockTicks: g.GroupCooldown, Members: g.Members,
		})
	}
	b, err := json.Marshal(out)
	return b, rep, err
}
