package schema

import (
	"encoding/json"
	"strconv"

	"skillsched/internal/errx"
	"skillsched/internal/expr"
	"skillsched/internal/fixed"
	"skillsched/internal/model"
)

func loadV3(data []byte) (*LoadResult, error) {
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, errx.New(errx.ESchema, "invalid json").Wrap(err)
	}
	ver := model.V3
	rep := &Report{SourceVersion: ver}

	paramsRaw, ok := raw["Params"].(map[string]any)
	if !ok {
		return nil, schemaErr(ver, "Params")
	}
	pv, err := requireNumber(ver, paramsRaw, "Params.ParamRevision", "ParamRevision")
	if err != nil {
		return nil, err
	}
	tickSpan, err := requireNumber(ver, paramsRaw, "Params.TickSpanMs", "TickSpanMs")
	if err != nil {
		return nil, err
	}
	horizon, err := requireNumber(ver, paramsRaw, "Params.TickHorizon", "TickHorizon")
	if err != nil {
		return nil, err
	}
	window, err := requireNumber(ver, paramsRaw, "Params.WindowTicks", "WindowTicks")
	if err != nil {
		return nil, err
	}

	poolRaws, err := requireSlice(ver, raw, "ResourcePools", "ResourcePools")
	if err != nil {
		return nil, err
	}
	skillRaws, err := requireSlice(ver, raw, "Abilities", "Abilities")
	if err != nil {
		return nil, err
	}

	rules := &model.RuleSet{
		SourceVersion: ver,
		Params: model.Params{
			ParamVersion: int(pv), TickMillis: tickSpan,
			Horizon: int(horizon), WindowSize: int(window),
		},
	}

	for i, pr := range poolRaws {
		pm, ok := pr.(map[string]any)
		if !ok {
			return nil, schemaErr(ver, "ResourcePools["+strconv.Itoa(i)+"]")
		}
		path := "ResourcePools[" + strconv.Itoa(i) + "]"
		id, err := requireString(ver, pm, path+".PoolID", "PoolID")
		if err != nil {
			return nil, err
		}
		name, err := requireString(ver, pm, path+".DisplayName", "DisplayName")
		if err != nil {
			return nil, err
		}
		upper, err := requireNumber(ver, pm, path+".UpperBound", "UpperBound")
		if err != nil {
			return nil, err
		}
		if _, ok := pm["RegenExpr"]; !ok {
			return nil, schemaErr(ver, path+".RegenExpr")
		}
		var tree expr.Node
		tb, _ := json.Marshal(pm["RegenExpr"])
		if err := json.Unmarshal(tb, &tree); err != nil {
			return nil, schemaErr(ver, path+".RegenExpr")
		}
		// 尝试折叠为常量表；非线性树保留 nil 常量形态并记录（v3 允许非线性）。
		flat, ferr := expr.FlatFromTree(&tree)
		if ferr != nil {
			rep.Losses = append(rep.Losses, LossItem{
				Path: path + ".RegenExpr", Kind: "expression_tree",
				Disposition: "kept_tree_only", Detail: ferr.Error(),
			})
		}
		rules.Pools = append(rules.Pools, &model.Pool{
			ID: id, Name: name, Capacity: upper,
			Regen: flat, RegenTree: &tree, RegenScale: fixed.InternalScale,
		})
	}

	for i, sr := range skillRaws {
		sm, ok := sr.(map[string]any)
		if !ok {
			return nil, schemaErr(ver, "Abilities["+strconv.Itoa(i)+"]")
		}
		path := "Abilities[" + strconv.Itoa(i) + "]"
		id, err := requireString(ver, sm, path+".AbilityID", "AbilityID")
		if err != nil {
			return nil, err
		}
		name, err := requireString(ver, sm, path+".DisplayName", "DisplayName")
		if err != nil {
			return nil, err
		}
		poolID, err := requireString(ver, sm, path+".BoundPool", "BoundPool")
		if err != nil {
			return nil, err
		}
		cost, err := requireNumber(ver, sm, path+".CastCost", "CastCost")
		if err != nil {
			return nil, err
		}
		cd, err := requireNumber(ver, sm, path+".CooldownTicks", "CooldownTicks")
		if err != nil {
			return nil, err
		}
		cdr, err := requireNumber(ver, sm, path+".ReducePermyriad", "ReducePermyriad")
		if err != nil {
			return nil, err
		}
		capV, err := requireNumber(ver, sm, path+".ReduceCapPermyriad", "ReduceCapPermyriad")
		if err != nil {
			return nil, err
		}
		rank, err := requireNumber(ver, sm, path+".PriorityRank", "PriorityRank")
		if err != nil {
			return nil, err
		}
		if _, ok := sm["ReduceExpr"]; !ok {
			return nil, schemaErr(ver, path+".ReduceExpr")
		}
		var cdrTree expr.Node
		cb, _ := json.Marshal(sm["ReduceExpr"])
		if err := json.Unmarshal(cb, &cdrTree); err != nil {
			return nil, schemaErr(ver, path+".ReduceExpr")
		}
		group, _ := sm["LockGroup"].(string)
		rules.Skills = append(rules.Skills, &model.Skill{
			ID: id, Name: name, PoolID: poolID, Cost: cost, Cooldown: cd,
			CDR: cdr, CDRCap: capV, CDRTree: &cdrTree,
			SharedGroup: group, Priority: int(rank),
		})
	}

	if gr, ok := raw["CooldownLocks"].([]any); ok {
		for i, vr := range gr {
			gm, ok := vr.(map[string]any)
			if !ok {
				return nil, schemaErr(ver, "CooldownLocks["+strconv.Itoa(i)+"]")
			}
			path := "CooldownLocks[" + strconv.Itoa(i) + "]"
			id, err := requireString(ver, gm, path+".LockID", "LockID")
			if err != nil {
				return nil, err
			}
			name, err := requireString(ver, gm, path+".DisplayName", "DisplayName")
			if err != nil {
				return nil, err
			}
			lock, err := requireNumber(ver, gm, path+".LockTicks", "LockTicks")
			if err != nil {
				return nil, err
			}
			membersRaw, ok := gm["Members"].([]any)
			if !ok || len(membersRaw) == 0 {
				return nil, schemaErr(ver, path+".Members")
			}
			members := make([]string, 0, len(membersRaw))
			for _, m := range membersRaw {
				ms, ok := m.(string)
				if !ok {
					return nil, schemaErr(ver, path+".Members")
				}
				members = append(members, ms)
			}
			rules.Groups = append(rules.Groups, &model.SharedGroup{
				ID: id, Name: name, GroupCooldown: lock, Members: members,
			})
		}
	}

	return &LoadResult{Rules: rules, Report: rep}, nil
}
