package budget

import "encoding/json"

const (
	DocumentV1ID = "combat-budget/v1"
	DocumentV2ID = "2.0"
	DocumentV3ID = "combat-budget/v3"
)

func RenderDocument(project *Project, version SourceVersion) ([]byte, error) {
	var doc any
	var err error
	switch version {
	case SourceV1:
		doc, err = renderV1(project)
	case SourceV2:
		doc, err = renderV2(project)
	case SourceV3:
		doc, err = renderV3(project)
	default:
		return nil, newError(ECodeSchema, "unknown render version %q", version)
	}
	if err != nil {
		return nil, err
	}
	return json.MarshalIndent(doc, "", "  ")
}

func renderV1(p *Project) (any, error) {
	constants := map[string]int64{}
	resourcePermille, err := toPermille(int64(p.Resource.RegenFlat), "resource_pool.regen_permille_per_tick")
	if err != nil {
		return nil, err
	}
	constants[p.Resource.RegenNode] = resourcePermille

	skills := make([]map[string]any, 0, len(p.Skills))
	for _, skill := range p.Skills {
		cost, err := toPermille(int64(skill.Cost), "skills.cost_permille")
		if err != nil {
			return nil, err
		}
		cdr, err := toPermille(skill.CDRFlat, "skills.cdr_permille")
		if err != nil {
			return nil, err
		}
		constants[skill.CDRNode] = cdr
		// Deliberately no shared_cooldown_group field on each skill. V1 uses
		// cooldown_groups[].members as the ownership direction.
		skills = append(skills, map[string]any{
			"skill_id":             skill.ID,
			"arbitration_priority": skill.Priority,
			"cost_permille":        cost,
			"cooldown_ticks":       skill.CooldownTicks,
			"cdr_table_key":        skill.CDRNode,
		})
	}

	groups := make([]map[string]any, 0, len(p.Groups))
	for _, group := range p.Groups {
		groups = append(groups, map[string]any{
			"group_id":         group.ID,
			"cooldown_ticks":   group.CooldownTicks,
			"member_skill_ids": group.Members,
		})
	}

	intents := make([]map[string]any, 0, len(p.Intents))
	for _, intent := range p.Intents {
		intents = append(intents, map[string]any{
			"intent_id": intent.IntentID,
			"at_tick":   intent.Tick,
			"skill_id":  intent.SkillID,
		})
	}

	return map[string]any{
		"schema":           DocumentV1ID,
		"project_id":       p.ID,
		"seed":             p.Seed,
		"tick_duration_ms": p.TickMS,
		"horizon_ticks":    p.TickCount,
		"scale_policy": map[string]any{
			"resource_fixed_point": "permille_1000",
			"time_unit":            "tick",
			"expression_form":      "flat_constant_table",
			"downscale_rounding":   string(RoundFloor),
		},
		"resource_pool": map[string]any{
			"resource_id":       p.Resource.ID,
			"capacity_permille": mustPermille(int64(p.Resource.Max)),
			"initial_permille":  mustPermille(int64(p.Resource.Initial)),
			"regen_table_key":   p.Resource.RegenNode,
		},
		"constant_table":    constants,
		"skill_definitions": skills,
		"cooldown_groups":   groups,
		"cast_intent_queue": intents,
	}, nil
}

func renderV2(p *Project) (any, error) {
	flat := map[string]int64{
		p.Resource.RegenNode: int64(p.Resource.RegenFlat),
	}
	skills := make([]map[string]any, 0, len(p.Skills))
	for _, skill := range p.Skills {
		flat[skill.CDRNode] = skill.CDRFlat
		skills = append(skills, map[string]any{
			"definitionId":             skill.ID,
			"castPriority":             skill.Priority,
			"resourceCostBasisPoints":  int64(skill.Cost),
			"cooldownMillis":           skill.CooldownTicks * p.TickMS,
			"hasteCoefficientTableKey": skill.CDRNode,
			"sharedCooldownGroupCode":  skill.GroupID,
		})
	}
	groups := make([]map[string]any, 0, len(p.Groups))
	for _, group := range p.Groups {
		groups = append(groups, map[string]any{
			"groupCode":      group.ID,
			"cooldownMillis": group.CooldownTicks * p.TickMS,
		})
	}
	intents := make([]map[string]any, 0, len(p.Intents))
	for _, intent := range p.Intents {
		intents = append(intents, map[string]any{
			"intentId":          intent.IntentID,
			"atMillis":          int64(intent.Tick) * p.TickMS,
			"skillDefinitionId": intent.SkillID,
		})
	}
	return map[string]any{
		"schemaVersion":      DocumentV2ID,
		"ruleProjectId":      p.ID,
		"seed":               p.Seed,
		"tickIntervalMillis": p.TickMS,
		"simulationTicks":    p.TickCount,
		"scalePolicy": map[string]any{
			"resourceFixedPoint": "basis_points_10000",
			"timeUnit":           "millis",
			"expressionForm":     "flatConstantTable",
			"cooldownRounding":   string(RoundCeil),
			"intentRounding":     string(RoundFloor),
		},
		"resourcePool": map[string]any{
			"resourceId":             p.Resource.ID,
			"capacityBasisPoints":    int64(p.Resource.Max),
			"initialBasisPoints":     int64(p.Resource.Initial),
			"recoveryCoefficientKey": p.Resource.RegenNode,
		},
		"flatConstants":        flat,
		"skillDefinitions":     skills,
		"sharedCooldownGroups": groups,
		"castIntents":          intents,
	}, nil
}

func renderV3(p *Project) (any, error) {
	skills := make([]map[string]any, 0, len(p.Skills))
	for _, skill := range p.Skills {
		skills = append(skills, map[string]any{
			"skill-id":                      skill.ID,
			"arbitration-priority":          skill.Priority,
			"cost-myriad":                   int64(skill.Cost),
			"cooldown-millis":               skill.CooldownTicks * p.TickMS,
			"cooldown-reduction-node":       skill.CDRNode,
			"cooldown-reduction-expression": skill.CDRExpr,
			"shared-cooldown-group":         skill.GroupID,
		})
	}
	groups := make([]map[string]any, 0, len(p.Groups))
	for _, group := range p.Groups {
		groups = append(groups, map[string]any{
			"group-id":        group.ID,
			"cooldown-millis": group.CooldownTicks * p.TickMS,
		})
	}
	intents := make([]map[string]any, 0, len(p.Intents))
	for _, intent := range p.Intents {
		intents = append(intents, map[string]any{
			"intent-id": intent.IntentID,
			"at-millis": int64(intent.Tick) * p.TickMS,
			"skill-id":  intent.SkillID,
		})
	}
	return map[string]any{
		"apiVersion": DocumentV3ID,
		"project": map[string]any{
			"id":                   p.ID,
			"seed":                 p.Seed,
			"tick-duration-millis": p.TickMS,
			"tick-count":           p.TickCount,
		},
		"scale-policy": map[string]any{
			"resource-fixed-point": "myriad_10000",
			"time-unit":            "millis",
			"expression-form":      "expression_tree",
			"cooldown-rounding":    string(RoundCeil),
			"intent-rounding":      string(RoundFloor),
		},
		"resource-pool": map[string]any{
			"resource-id":         p.Resource.ID,
			"capacity-myriad":     int64(p.Resource.Max),
			"initial-myriad":      int64(p.Resource.Initial),
			"recovery-node":       p.Resource.RegenNode,
			"recovery-expression": p.Resource.RegenExpr,
		},
		"skills":                 skills,
		"shared-cooldown-groups": groups,
		"player-intents":         intents,
	}, nil
}

func toPermille(bp int64, field string) (int64, error) {
	v, err := ConvertScaled(bp, ScalePermyriad, ScalePermille, RoundExact, field)
	if err != nil {
		return 0, err
	}
	return v.Value, nil
}

func mustPermille(bp int64) int64 {
	v, err := toPermille(bp, "render.permille")
	if err != nil {
		panic(err)
	}
	return v
}
