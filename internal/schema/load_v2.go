package schema

import (
	"encoding/json"
	"strconv"

	"skillsched/internal/errx"
	"skillsched/internal/expr"
	"skillsched/internal/fixed"
	"skillsched/internal/model"
)

func loadV2(data []byte) (*LoadResult, error) {
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, errx.New(errx.ESchema, "invalid json").Wrap(err)
	}
	ver := model.V2
	rep := &Report{SourceVersion: ver}

	paramsRaw, ok := raw["params"].(map[string]any)
	if !ok {
		return nil, schemaErr(ver, "params")
	}
	pv, err := requireNumber(ver, paramsRaw, "params.param_version", "param_version")
	if err != nil {
		return nil, err
	}
	tickMs, err := requireNumber(ver, paramsRaw, "params.tick_ms", "tick_ms")
	if err != nil {
		return nil, err
	}
	horizonMs, err := requireNumber(ver, paramsRaw, "params.horizon_ms", "horizon_ms")
	if err != nil {
		return nil, err
	}
	windowMs, err := requireNumber(ver, paramsRaw, "params.window_ms", "window_ms")
	if err != nil {
		return nil, err
	}
	horizon, err := msToTicks(rep, "params.horizon_ms", horizonMs)
	if err != nil {
		return nil, err
	}
	window, err := msToTicks(rep, "params.window_ms", windowMs)
	if err != nil {
		return nil, err
	}

	poolRaws, err := requireSlice(ver, raw, "pools", "pools")
	if err != nil {
		return nil, err
	}
	skillRaws, err := requireSlice(ver, raw, "skills", "skills")
	if err != nil {
		return nil, err
	}

	rules := &model.RuleSet{
		SourceVersion: ver,
		Params: model.Params{
			ParamVersion: int(pv), TickMillis: tickMs,
			Horizon: int(horizon), WindowSize: int(window),
		},
	}

	for i, pr := range poolRaws {
		pm, ok := pr.(map[string]any)
		if !ok {
			return nil, schemaErr(ver, "pools["+strconv.Itoa(i)+"]")
		}
		path := "pools[" + strconv.Itoa(i) + "]"
		id, err := requireString(ver, pm, path+".pool_id", "pool_id")
		if err != nil {
			return nil, err
		}
		name, err := requireString(ver, pm, path+".pool_name", "pool_name")
		if err != nil {
			return nil, err
		}
		capV, err := requireNumber(ver, pm, path+".max_amount", "max_amount")
		if err != nil {
			return nil, err
		}
		if _, ok := pm["regen_table"]; !ok {
			return nil, schemaErr(ver, path+".regen_table")
		}
		var flat expr.Flat
		rb, _ := json.Marshal(pm["regen_table"])
		if err := json.Unmarshal(rb, &flat); err != nil {
			return nil, schemaErr(ver, path+".regen_table")
		}
		rules.Pools = append(rules.Pools, &model.Pool{
			ID: id, Name: name, Capacity: capV,
			Regen: &flat, RegenTree: expr.TreeFromFlat("regen."+id, &flat),
			RegenScale: fixed.InternalScale,
		})
	}

	for i, sr := range skillRaws {
		sm, ok := sr.(map[string]any)
		if !ok {
			return nil, schemaErr(ver, "skills["+strconv.Itoa(i)+"]")
		}
		path := "skills[" + strconv.Itoa(i) + "]"
		id, err := requireString(ver, sm, path+".skill_id", "skill_id")
		if err != nil {
			return nil, err
		}
		name, err := requireString(ver, sm, path+".skill_name", "skill_name")
		if err != nil {
			return nil, err
		}
		poolID, err := requireString(ver, sm, path+".use_pool", "use_pool")
		if err != nil {
			return nil, err
		}
		cost, err := requireNumber(ver, sm, path+".resource_cost", "resource_cost")
		if err != nil {
			return nil, err
		}
		cdMs, err := requireNumber(ver, sm, path+".cooldown_ms", "cooldown_ms")
		if err != nil {
			return nil, err
		}
		cdrPerm, err := requireNumber(ver, sm, path+".cooldown_reduce_permille", "cooldown_reduce_permille")
		if err != nil {
			return nil, err
		}
		weight, err := requireNumber(ver, sm, path+".order_weight", "order_weight")
		if err != nil {
			return nil, err
		}
		cd, err := msToTicks(rep, path+".cooldown_ms", cdMs)
		if err != nil {
			return nil, err
		}
		cdr, err := permilleToInternal(rep, path+".cooldown_reduce_permille", cdrPerm)
		if err != nil {
			return nil, err
		}
		// 共享组字段在 v2 中存在但可空；冷却缩减上界仍缺失，按默认补齐。
		group, _ := sm["shared_group_id"].(string)
		rep.DefaultFills = append(rep.DefaultFills, DefaultFill{
			Path: path + ".cdr_cap", OldValue: "<absent>",
			DefaultValue: strconv.FormatInt(DefaultCDRCapPermyriad, 10),
		})
		rules.Skills = append(rules.Skills, &model.Skill{
			ID: id, Name: name, PoolID: poolID, Cost: cost, Cooldown: cd,
			CDR: cdr, CDRCap: DefaultCDRCapPermyriad,
			CDRTree:     &expr.Node{ID: "cdr." + id, Op: expr.OpNum, Value: cdr},
			SharedGroup: group, Priority: int(weight),
		})
	}

	if gr, ok := raw["shared_groups"].([]any); ok {
		for i, vr := range gr {
			gm, ok := vr.(map[string]any)
			if !ok {
				return nil, schemaErr(ver, "shared_groups["+strconv.Itoa(i)+"]")
			}
			path := "shared_groups[" + strconv.Itoa(i) + "]"
			id, err := requireString(ver, gm, path+".group_id", "group_id")
			if err != nil {
				return nil, err
			}
			name, err := requireString(ver, gm, path+".group_name", "group_name")
			if err != nil {
				return nil, err
			}
			lockMs, err := requireNumber(ver, gm, path+".lock_ms", "lock_ms")
			if err != nil {
				return nil, err
			}
			membersRaw, ok := gm["member_skill_ids"].([]any)
			if !ok || len(membersRaw) == 0 {
				return nil, schemaErr(ver, path+".member_skill_ids")
			}
			lock, err := msToTicks(rep, path+".lock_ms", lockMs)
			if err != nil {
				return nil, err
			}
			members := make([]string, 0, len(membersRaw))
			for _, m := range membersRaw {
				ms, ok := m.(string)
				if !ok {
					return nil, schemaErr(ver, path+".member_skill_ids")
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
