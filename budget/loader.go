package budget

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
)

// LoadDocument detects one of the three dialects and maps it to Project.
func LoadDocument(data []byte) (*Project, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var root map[string]any
	if err := dec.Decode(&root); err != nil {
		return nil, &Error{Code: ECodeDecode, Message: "invalid JSON document", Cause: err.Error(), Path: "$"}
	}

	switch {
	case stringValue(root, "schema") == DocumentV1ID:
		return loadV1(root)
	case stringValue(root, "schemaVersion") == DocumentV2ID:
		return loadV2(root)
	case stringValue(root, "apiVersion") == DocumentV3ID:
		return loadV3(root)
	default:
		version := firstString(root, "schema", "schemaVersion", "apiVersion", "version")
		sv := SourceVersion("unknown:" + version)
		return nil, schemaError(sv, "$schema", "unsupported schema version %q", version)
	}
}

type loadContext struct {
	version SourceVersion
	traces  []FieldTrace
}

func (lc *loadContext) trace(path, sourceRaw, internalPath, internalValue, conversion string, rounding Rounding, residual int64) {
	lc.traces = append(lc.traces, FieldTrace{
		SourceVersion: lc.version, Path: path, SourceRaw: sourceRaw,
		InternalPath: internalPath, InternalValue: internalValue,
		Conversion: conversion, Rounding: rounding, Residual: residual,
	})
}

func loadV1(root map[string]any) (*Project, error) {
	lc := &loadContext{version: SourceV1}
	policy, err := expectScalePolicy(root, lc.version, "permille_1000", "tick", "flat_constant_table")
	if err != nil {
		return nil, err
	}
	scaleRounding, err := declaredRounding(policy, []string{"downscale_rounding", "fixed_point_rounding"}, RoundFloor, lc.version)
	if err != nil {
		return nil, err
	}
	id, err := reqString(root, "project_id", "project_id", lc.version)
	if err != nil {
		return nil, err
	}
	seed, err := reqInt(root, "seed", "seed", lc.version)
	if err != nil {
		return nil, err
	}
	tickMS, err := reqInt(root, "tick_duration_ms", "tick_duration_ms", lc.version)
	if err != nil {
		return nil, err
	}
	tickCount, err := reqInt(root, "horizon_ticks", "horizon_ticks", lc.version)
	if err != nil {
		return nil, err
	}
	resourceRaw, err := reqMap(root, "resource_pool", "resource_pool", lc.version)
	if err != nil {
		return nil, err
	}
	resourceID, err := reqString(resourceRaw, "resource_id", "resource_pool.resource_id", lc.version)
	if err != nil {
		return nil, err
	}
	maxPermille, err := reqInt(resourceRaw, "capacity_permille", "resource_pool.capacity_permille", lc.version)
	if err != nil {
		return nil, err
	}
	initialPermille, err := reqInt(resourceRaw, "initial_permille", "resource_pool.initial_permille", lc.version)
	if err != nil {
		return nil, err
	}
	regenKey, err := reqString(resourceRaw, "regen_table_key", "resource_pool.regen_table_key", lc.version)
	if err != nil {
		return nil, err
	}
	tablesRaw, err := reqMap(root, "constant_table", "constant_table", lc.version)
	if err != nil {
		return nil, err
	}
	regenPermille, err := reqTableInt(tablesRaw, regenKey, "constant_table."+regenKey, lc.version)
	if err != nil {
		return nil, err
	}
	regen, err := ConvertScaled(regenPermille, ScalePermille, ScaleInternal, RoundExact, "resource.regen")
	if err != nil {
		return nil, withSource(err, lc.version)
	}
	maxBP, err := ConvertScaled(maxPermille, ScalePermille, ScaleInternal, RoundExact, "resource.max")
	if err != nil {
		return nil, withSource(err, lc.version)
	}
	initialBP, err := ConvertScaled(initialPermille, ScalePermille, ScaleInternal, RoundExact, "resource.initial")
	if err != nil {
		return nil, withSource(err, lc.version)
	}
	lc.trace("resource_pool.capacity_permille", strconv.FormatInt(maxPermille, 10), "resource.max", strconv.FormatInt(maxBP.Value, 10), "permille*10", RoundExact, 0)
	lc.trace("resource_pool.initial_permille", strconv.FormatInt(initialPermille, 10), "resource.initial", strconv.FormatInt(initialBP.Value, 10), "permille*10", RoundExact, 0)
	lc.trace("constant_table."+regenKey, strconv.FormatInt(regenPermille, 10), "resource.regen_flat", strconv.FormatInt(regen.Value, 10), "permille*10", RoundExact, 0)

	skillsRaw, err := reqSlice(root, "skill_definitions", "skill_definitions", lc.version)
	if err != nil {
		return nil, err
	}
	skills := make([]Skill, 0, len(skillsRaw))
	for i, raw := range skillsRaw {
		path := fmt.Sprintf("skill_definitions[%d]", i)
		m, err := asMapRequired(raw, path, lc.version)
		if err != nil {
			return nil, err
		}
		skillID, err := reqString(m, "skill_id", path+".skill_id", lc.version)
		if err != nil {
			return nil, err
		}
		priority, err := reqInt(m, "arbitration_priority", path+".arbitration_priority", lc.version)
		if err != nil {
			return nil, err
		}
		costPermille, err := reqInt(m, "cost_permille", path+".cost_permille", lc.version)
		if err != nil {
			return nil, err
		}
		cooldownTicks, err := reqInt(m, "cooldown_ticks", path+".cooldown_ticks", lc.version)
		if err != nil {
			return nil, err
		}
		cdrKey, err := reqString(m, "cdr_table_key", path+".cdr_table_key", lc.version)
		if err != nil {
			return nil, err
		}
		cdrPermille, err := reqTableInt(tablesRaw, cdrKey, path+".cdr_table_key -> constant_table."+cdrKey, lc.version)
		if err != nil {
			return nil, err
		}
		cost, cerr := ConvertScaled(costPermille, ScalePermille, ScaleInternal, scaleRounding, "skills."+skillID+".cost")
		if cerr != nil {
			return nil, withSource(cerr, lc.version)
		}
		cdr, cerr := ConvertScaled(cdrPermille, ScalePermille, ScaleInternal, RoundExact, "skills."+skillID+".cdr")
		if cerr != nil {
			return nil, withSource(cerr, lc.version)
		}
		lc.trace(path+".cost_permille", strconv.FormatInt(costPermille, 10), "skills.cost", strconv.FormatInt(cost.Value, 10), "permille->myriad floor", RoundFloor, cost.Residual)
		lc.trace(path+".cooldown_ticks", strconv.FormatInt(cooldownTicks, 10), "skills.cooldown_ticks", strconv.FormatInt(cooldownTicks, 10), "tick_identity", "", 0)
		lc.trace("constant_table."+cdrKey, strconv.FormatInt(cdrPermille, 10), "skills.cdr_flat", strconv.FormatInt(cdr.Value, 10), "permille*10", RoundExact, 0)
		skills = append(skills, Skill{
			ID: skillID, Priority: int(priority), Cost: BP(cost.Value),
			CooldownTicks: cooldownTicks, CDRNode: cdrKey, CDRFlat: cdr.Value,
		})
	}

	groups, err := loadV1Groups(root, skills, lc)
	if err != nil {
		return nil, err
	}
	intents, err := loadIntentsV1(root, lc)
	if err != nil {
		return nil, err
	}

	p := &Project{
		ID: id, Seed: seed, TickMS: tickMS, TickCount: int(tickCount),
		SourceVersion: lc.version,
		Resource: ResourceRule{
			ID: resourceID, Max: BP(maxBP.Value), Initial: BP(initialBP.Value),
			RegenNode: regenKey, RegenFlat: BP(regen.Value),
		},
		Skills: skills, Groups: groups, Intents: intents, Traces: lc.traces,
	}
	return finalizeLoad(p)
}

func loadV1Groups(root map[string]any, skills []Skill, lc *loadContext) ([]CooldownGroup, error) {
	rawGroups, err := reqSlice(root, "cooldown_groups", "cooldown_groups", lc.version)
	if err != nil {
		return nil, err
	}
	groups := make([]CooldownGroup, 0, len(rawGroups))
	for i, raw := range rawGroups {
		path := fmt.Sprintf("cooldown_groups[%d]", i)
		m, err := asMapRequired(raw, path, lc.version)
		if err != nil {
			return nil, err
		}
		id, err := reqString(m, "group_id", path+".group_id", lc.version)
		if err != nil {
			return nil, err
		}
		cd, err := reqInt(m, "cooldown_ticks", path+".cooldown_ticks", lc.version)
		if err != nil {
			return nil, err
		}
		memberRaw, _ := m["member_skill_ids"].([]any)
		members := make([]string, 0, len(memberRaw))
		for j, member := range memberRaw {
			memberID, ok := member.(string)
			if !ok {
				return nil, schemaError(lc.version, fmt.Sprintf("%s.member_skill_ids[%d]", path, j), "member id must be string")
			}
			idx := findSkillIndex(skills, memberID)
			if idx < 0 {
				return nil, schemaError(lc.version, fmt.Sprintf("%s.member_skill_ids[%d]", path, j), "unknown skill %q", memberID)
			}
			skills[idx].GroupID = id
			members = append(members, memberID)
		}
		groups = append(groups, CooldownGroup{ID: id, CooldownTicks: cd, Members: members})
	}
	return groups, nil
}

func loadIntentsV1(root map[string]any, lc *loadContext) ([]Intent, error) {
	rawIntents, err := reqSlice(root, "cast_intent_queue", "cast_intent_queue", lc.version)
	if err != nil {
		return nil, err
	}
	intents := make([]Intent, 0, len(rawIntents))
	lastTick := -1
	lastSkill := ""
	for i, raw := range rawIntents {
		path := fmt.Sprintf("cast_intent_queue[%d]", i)
		m, err := asMapRequired(raw, path, lc.version)
		if err != nil {
			return nil, err
		}
		id, err := reqString(m, "intent_id", path+".intent_id", lc.version)
		if err != nil {
			return nil, err
		}
		tick64, err := reqInt(m, "at_tick", path+".at_tick", lc.version)
		if err != nil {
			return nil, err
		}
		skillID, err := reqString(m, "skill_id", path+".skill_id", lc.version)
		if err != nil {
			return nil, err
		}
		tick := int(tick64)
		if tick < 0 {
			return nil, schemaError(lc.version, path+".at_tick", "tick must be non-negative")
		}
		if tick < lastTick {
			t := tick
			return nil, &Error{
				Code: ECodeOrder, Message: "intent moments are out of order",
				Path: path + ".at_tick", Field: path + ".at_tick",
				SourceVersion: lc.version, Tick: &t, SkillID: skillID,
				Operands: []int64{int64(tick), int64(lastTick)},
				Basis:    fmt.Sprintf("previous_tick=%d;previous_skill=%s", lastTick, lastSkill),
			}
		}
		lastTick, lastSkill = tick, skillID
		intents = append(intents, Intent{IntentID: id, Tick: tick, SkillID: skillID, Ordinal: i})
	}
	return intents, nil
}

func loadV2(root map[string]any) (*Project, error) {
	lc := &loadContext{version: SourceV2}
	policy, err := expectScalePolicy(root, lc.version, "basis_points_10000", "millis", "flatConstantTable")
	if err != nil {
		return nil, err
	}
	cooldownRounding, err := declaredRounding(policy, []string{"cooldownRounding", "cooldown_rounding"}, RoundCeil, lc.version)
	if err != nil {
		return nil, err
	}
	intentRounding, err := declaredRounding(policy, []string{"intentRounding", "intent_rounding"}, RoundFloor, lc.version)
	if err != nil {
		return nil, err
	}
	id, err := reqString(root, "ruleProjectId", "ruleProjectId", lc.version)
	if err != nil {
		return nil, err
	}
	seed, err := reqInt(root, "seed", "seed", lc.version)
	if err != nil {
		return nil, err
	}
	tickMS, err := reqInt(root, "tickIntervalMillis", "tickIntervalMillis", lc.version)
	if err != nil {
		return nil, err
	}
	tickCount, err := reqInt(root, "simulationTicks", "simulationTicks", lc.version)
	if err != nil {
		return nil, err
	}
	resourceRaw, err := reqMap(root, "resourcePool", "resourcePool", lc.version)
	if err != nil {
		return nil, err
	}
	resourceID, err := reqString(resourceRaw, "resourceId", "resourcePool.resourceId", lc.version)
	if err != nil {
		return nil, err
	}
	maxBP, err := reqInt(resourceRaw, "capacityBasisPoints", "resourcePool.capacityBasisPoints", lc.version)
	if err != nil {
		return nil, err
	}
	initialBP, err := reqInt(resourceRaw, "initialBasisPoints", "resourcePool.initialBasisPoints", lc.version)
	if err != nil {
		return nil, err
	}
	regenKey, err := reqString(resourceRaw, "recoveryCoefficientKey", "resourcePool.recoveryCoefficientKey", lc.version)
	if err != nil {
		return nil, err
	}
	flatRaw, err := reqMap(root, "flatConstants", "flatConstants", lc.version)
	if err != nil {
		return nil, err
	}
	regen, err := reqTableInt(flatRaw, regenKey, "flatConstants."+regenKey, lc.version)
	if err != nil {
		return nil, err
	}

	skillsRaw, err := reqSlice(root, "skillDefinitions", "skillDefinitions", lc.version)
	if err != nil {
		return nil, err
	}
	skills := make([]Skill, 0, len(skillsRaw))
	for i, raw := range skillsRaw {
		path := fmt.Sprintf("skillDefinitions[%d]", i)
		m, err := asMapRequired(raw, path, lc.version)
		if err != nil {
			return nil, err
		}
		skillID, err := reqString(m, "definitionId", path+".definitionId", lc.version)
		if err != nil {
			return nil, err
		}
		priority, err := reqInt(m, "castPriority", path+".castPriority", lc.version)
		if err != nil {
			return nil, err
		}
		cost, err := reqInt(m, "resourceCostBasisPoints", path+".resourceCostBasisPoints", lc.version)
		if err != nil {
			return nil, err
		}
		cdMillis, err := reqInt(m, "cooldownMillis", path+".cooldownMillis", lc.version)
		if err != nil {
			return nil, err
		}
		cdrKey, err := reqString(m, "hasteCoefficientTableKey", path+".hasteCoefficientTableKey", lc.version)
		if err != nil {
			return nil, err
		}
		cdr, err := reqTableInt(flatRaw, cdrKey, path+".hasteCoefficientTableKey -> flatConstants."+cdrKey, lc.version)
		if err != nil {
			return nil, err
		}
		groupCode, _ := optionalString(m, "sharedCooldownGroupCode")
		cdTicks, residualMS, terr := MillisToTicks(cdMillis, tickMS, cooldownRounding, path+".cooldownMillis")
		if terr != nil {
			return nil, enrichSkillScale(withSource(terr, lc.version), root, "castIntents", "skillDefinitionId", "atMillis", skillID, tickMS)
		}
		lc.trace(path+".cooldownMillis", strconv.FormatInt(cdMillis, 10), "skills.cooldown_ticks", strconv.FormatInt(cdTicks, 10), "millis->tick ceil", RoundCeil, residualMS)
		skills = append(skills, Skill{
			ID: skillID, Priority: int(priority), Cost: BP(cost), CooldownTicks: cdTicks,
			CDRNode: cdrKey, CDRFlat: cdr, GroupID: groupCode,
		})
	}
	groups, err := loadV2Groups(root, tickMS, cooldownRounding, lc)
	if err != nil {
		return nil, err
	}
	intents, err := loadIntentsMillis(root, "castIntents", "intentId", "atMillis", "skillDefinitionId", tickMS, intentRounding, lc)
	if err != nil {
		return nil, err
	}
	p := &Project{
		ID: id, Seed: seed, TickMS: tickMS, TickCount: int(tickCount), SourceVersion: lc.version,
		Resource: ResourceRule{ID: resourceID, Max: BP(maxBP), Initial: BP(initialBP), RegenNode: regenKey, RegenFlat: BP(regen)},
		Skills:   skills, Groups: groups, Intents: intents, Traces: lc.traces,
	}
	return finalizeLoad(p)
}

func loadV2Groups(root map[string]any, tickMS int64, rounding Rounding, lc *loadContext) ([]CooldownGroup, error) {
	rawGroups, err := reqSlice(root, "sharedCooldownGroups", "sharedCooldownGroups", lc.version)
	if err != nil {
		return nil, err
	}
	groups := make([]CooldownGroup, 0, len(rawGroups))
	for i, raw := range rawGroups {
		path := fmt.Sprintf("sharedCooldownGroups[%d]", i)
		m, err := asMapRequired(raw, path, lc.version)
		if err != nil {
			return nil, err
		}
		id, err := reqString(m, "groupCode", path+".groupCode", lc.version)
		if err != nil {
			return nil, err
		}
		millis, err := reqInt(m, "cooldownMillis", path+".cooldownMillis", lc.version)
		if err != nil {
			return nil, err
		}
		ticks, residual, err := MillisToTicks(millis, tickMS, rounding, path+".cooldownMillis")
		if err != nil {
			return nil, withSource(err, lc.version)
		}
		lc.trace(path+".cooldownMillis", strconv.FormatInt(millis, 10), "groups.cooldown_ticks", strconv.FormatInt(ticks, 10), "millis->tick ceil", RoundCeil, residual)
		groups = append(groups, CooldownGroup{ID: id, CooldownTicks: ticks})
	}
	return groups, nil
}

func loadV3(root map[string]any) (*Project, error) {
	lc := &loadContext{version: SourceV3}
	policy, err := expectScalePolicy(root, lc.version, "myriad_10000", "millis", "expression_tree")
	if err != nil {
		return nil, err
	}
	cooldownRounding, err := declaredRounding(policy, []string{"cooldown-rounding", "cooldownRounding"}, RoundCeil, lc.version)
	if err != nil {
		return nil, err
	}
	intentRounding, err := declaredRounding(policy, []string{"intent-rounding", "intentRounding"}, RoundFloor, lc.version)
	if err != nil {
		return nil, err
	}
	projectRaw, err := reqMap(root, "project", "project", lc.version)
	if err != nil {
		return nil, err
	}
	id, err := reqString(projectRaw, "id", "project.id", lc.version)
	if err != nil {
		return nil, err
	}
	seed, err := reqInt(projectRaw, "seed", "project.seed", lc.version)
	if err != nil {
		return nil, err
	}
	tickMS, err := reqInt(projectRaw, "tick-duration-millis", "project.tick-duration-millis", lc.version)
	if err != nil {
		return nil, err
	}
	tickCount, err := reqInt(projectRaw, "tick-count", "project.tick-count", lc.version)
	if err != nil {
		return nil, err
	}
	resourceRaw, err := reqMap(root, "resource-pool", "resource-pool", lc.version)
	if err != nil {
		return nil, err
	}
	resourceID, err := reqString(resourceRaw, "resource-id", "resource-pool.resource-id", lc.version)
	if err != nil {
		return nil, err
	}
	maxMyriad, err := reqInt(resourceRaw, "capacity-myriad", "resource-pool.capacity-myriad", lc.version)
	if err != nil {
		return nil, err
	}
	initialMyriad, err := reqInt(resourceRaw, "initial-myriad", "resource-pool.initial-myriad", lc.version)
	if err != nil {
		return nil, err
	}
	regenNode, err := reqString(resourceRaw, "recovery-node", "resource-pool.recovery-node", lc.version)
	if err != nil {
		return nil, err
	}
	regenExprRaw, has := resourceRaw["recovery-expression"]
	if !has {
		return nil, schemaError(lc.version, "resource-pool.recovery-expression", "missing required field")
	}
	regenExpr, err := decodeExpr(regenExprRaw, "resource-pool.recovery-expression", lc.version)
	if err != nil {
		return nil, err
	}

	skillsRaw, err := reqSlice(root, "skills", "skills", lc.version)
	if err != nil {
		return nil, err
	}
	skills := make([]Skill, 0, len(skillsRaw))
	for i, raw := range skillsRaw {
		path := fmt.Sprintf("skills[%d]", i)
		m, err := asMapRequired(raw, path, lc.version)
		if err != nil {
			return nil, err
		}
		skillID, err := reqString(m, "skill-id", path+".skill-id", lc.version)
		if err != nil {
			return nil, err
		}
		priority, err := reqInt(m, "arbitration-priority", path+".arbitration-priority", lc.version)
		if err != nil {
			return nil, err
		}
		cost, err := reqInt(m, "cost-myriad", path+".cost-myriad", lc.version)
		if err != nil {
			return nil, err
		}
		cdMillis, err := reqInt(m, "cooldown-millis", path+".cooldown-millis", lc.version)
		if err != nil {
			return nil, err
		}
		cdrNode, err := reqString(m, "cooldown-reduction-node", path+".cooldown-reduction-node", lc.version)
		if err != nil {
			return nil, err
		}
		exprRaw, has := m["cooldown-reduction-expression"]
		if !has {
			return nil, schemaError(lc.version, path+".cooldown-reduction-expression", "missing required field")
		}
		cdrExpr, err := decodeExpr(exprRaw, path+".cooldown-reduction-expression", lc.version)
		if err != nil {
			return nil, err
		}
		groupID, _ := optionalString(m, "shared-cooldown-group")
		cdTicks, residual, terr := MillisToTicks(cdMillis, tickMS, cooldownRounding, path+".cooldown-millis")
		if terr != nil {
			return nil, enrichSkillScale(withSource(terr, lc.version), root, "player-intents", "skill-id", "at-millis", skillID, tickMS)
		}
		lc.trace(path+".cooldown-millis", strconv.FormatInt(cdMillis, 10), "skills.cooldown_ticks", strconv.FormatInt(cdTicks, 10), "millis->tick ceil", RoundCeil, residual)
		skills = append(skills, Skill{
			ID: skillID, Priority: int(priority), Cost: BP(cost), CooldownTicks: cdTicks,
			CDRNode: cdrNode, CDRExpr: cdrExpr, GroupID: groupID,
		})
	}
	groups, err := loadV3Groups(root, tickMS, cooldownRounding, lc)
	if err != nil {
		return nil, err
	}
	intents, err := loadIntentsMillis(root, "player-intents", "intent-id", "at-millis", "skill-id", tickMS, intentRounding, lc)
	if err != nil {
		return nil, err
	}
	p := &Project{
		ID: id, Seed: seed, TickMS: tickMS, TickCount: int(tickCount), SourceVersion: lc.version,
		Resource: ResourceRule{
			ID: resourceID, Max: BP(maxMyriad), Initial: BP(initialMyriad),
			RegenNode: regenNode, RegenExpr: regenExpr,
		},
		Skills: skills, Groups: groups, Intents: intents, Traces: lc.traces,
	}
	return finalizeLoad(p)
}

func loadV3Groups(root map[string]any, tickMS int64, rounding Rounding, lc *loadContext) ([]CooldownGroup, error) {
	rawGroups, err := reqSlice(root, "shared-cooldown-groups", "shared-cooldown-groups", lc.version)
	if err != nil {
		return nil, err
	}
	groups := make([]CooldownGroup, 0, len(rawGroups))
	for i, raw := range rawGroups {
		path := fmt.Sprintf("shared-cooldown-groups[%d]", i)
		m, err := asMapRequired(raw, path, lc.version)
		if err != nil {
			return nil, err
		}
		id, err := reqString(m, "group-id", path+".group-id", lc.version)
		if err != nil {
			return nil, err
		}
		millis, err := reqInt(m, "cooldown-millis", path+".cooldown-millis", lc.version)
		if err != nil {
			return nil, err
		}
		ticks, _, err := MillisToTicks(millis, tickMS, rounding, path+".cooldown-millis")
		if err != nil {
			return nil, withSource(err, lc.version)
		}
		groups = append(groups, CooldownGroup{ID: id, CooldownTicks: ticks})
	}
	return groups, nil
}

func loadIntentsMillis(root map[string]any, arrayKey, idKey, timeKey, skillKey string, tickMS int64, rounding Rounding, lc *loadContext) ([]Intent, error) {
	rawIntents, err := reqSlice(root, arrayKey, arrayKey, lc.version)
	if err != nil {
		return nil, err
	}
	intents := make([]Intent, 0, len(rawIntents))
	lastTick := -1
	lastSkill := ""
	for i, raw := range rawIntents {
		path := fmt.Sprintf("%s[%d]", arrayKey, i)
		m, err := asMapRequired(raw, path, lc.version)
		if err != nil {
			return nil, err
		}
		id, err := reqString(m, idKey, path+"."+idKey, lc.version)
		if err != nil {
			return nil, err
		}
		millis, err := reqInt(m, timeKey, path+"."+timeKey, lc.version)
		if err != nil {
			return nil, err
		}
		skillID, err := reqString(m, skillKey, path+"."+skillKey, lc.version)
		if err != nil {
			return nil, err
		}
		tick64, residual, err := MillisToTicks(millis, tickMS, rounding, path+"."+timeKey)
		if err != nil {
			return nil, withSource(err, lc.version)
		}
		if residual != 0 {
			lc.trace(path+"."+timeKey, strconv.FormatInt(millis, 10), "intents.tick", strconv.FormatInt(tick64, 10), "millis->tick floor", RoundFloor, residual)
		}
		tick := int(tick64)
		if tick < 0 {
			return nil, schemaError(lc.version, path+"."+timeKey, "tick must be non-negative")
		}
		if tick < lastTick {
			t := tick
			return nil, &Error{
				Code: ECodeOrder, Message: "intent moments are out of order after millis-to-tick conversion",
				Path: path + "." + timeKey, Field: path + "." + timeKey,
				SourceVersion: lc.version, Tick: &t, SkillID: skillID,
				Operands: []int64{int64(tick), int64(lastTick)},
				Basis:    fmt.Sprintf("previous_tick=%d;previous_skill=%s", lastTick, lastSkill),
			}
		}
		lastTick, lastSkill = tick, skillID
		intents = append(intents, Intent{IntentID: id, Tick: tick, SkillID: skillID, Ordinal: i})
	}
	return intents, nil
}

func finalizeLoad(p *Project) (*Project, error) {
	for i := range p.Skills {
		skill := &p.Skills[i]
		if skill.CDRExpr != nil {
			cdr, err := Eval(skill.CDRExpr, skill.CDRExpr.Vars, "skills."+skill.ID+".cdr_expression")
			if err != nil {
				return nil, withSource(err, p.SourceVersion)
			}
			skill.CDRFlat = cdr
		}
		if skill.CDRFlat < 0 || skill.CDRFlat > MaxCDRBasisPoints {
			return nil, withSource(&Error{
				Code: ECodeEval, Message: "CDR coefficient outside [0,10000]",
				Field: fmt.Sprintf("skills[%s].cdr", skill.ID), SkillID: skill.ID,
				Arguments: map[string]int64{"cdr_bp": skill.CDRFlat},
			}, p.SourceVersion)
		}
	}
	if p.Resource.RegenExpr != nil {
		regen, err := Eval(p.Resource.RegenExpr, p.Resource.RegenExpr.Vars, "resource.regen_expression")
		if err != nil {
			return nil, withSource(err, p.SourceVersion)
		}
		if err := ValidateRegenBoundary(p.Resource.RegenNode, BP(regen), map[string]int64{"regen": regen}); err != nil {
			return nil, withSource(err, p.SourceVersion)
		}
		p.Resource.RegenFlat = BP(regen)
	}
	if p.Resource.Initial < 0 || p.Resource.Max <= 0 || p.Resource.Initial > p.Resource.Max {
		return nil, withSource(&Error{
			Code: ECodeScale, Message: "resource initial/max scale contradiction",
			Field: "resource_pool", Operands: []int64{int64(p.Resource.Initial), int64(p.Resource.Max)},
		}, p.SourceVersion)
	}
	hash, err := p.SemanticHash()
	if err != nil {
		return nil, err
	}
	p.ContentHash = hash
	return p, nil
}

func expectScalePolicy(root map[string]any, version SourceVersion, fixedPoint, timeUnit, expressionForm string) (map[string]any, error) {
	var policy map[string]any
	policyPath := "scale_policy"
	policyKeys := []string{"scale_policy", "scalePolicy", "scale-policy"}
	switch version {
	case SourceV2:
		policyPath = "scalePolicy"
	case SourceV3:
		policyPath = "scale-policy"
	}
	for _, key := range policyKeys {
		if candidate, ok := root[key].(map[string]any); ok {
			policy = candidate
			break
		}
	}
	if policy == nil {
		return nil, schemaError(version, policyPath, "missing required scale policy")
	}
	checks := []struct {
		key, want, path string
	}{
		{firstMapKey(policy, "resource_fixed_point", "resourceFixedPoint", "resource-fixed-point"), fixedPoint, "resource_fixed_point"},
		{firstMapKey(policy, "time_unit", "timeUnit", "time-unit"), timeUnit, "time_unit"},
		{firstMapKey(policy, "expression_form", "expressionForm", "expression-form"), expressionForm, "expression_form"},
	}
	for _, check := range checks {
		got := stringValue(policy, check.key)
		if got == "" {
			return nil, schemaError(version, policyPath+"."+check.key, "missing required policy field")
		}
		if got != check.want {
			return nil, &Error{
				Code: ECodeScale, Message: "declared scale policy contradicts schema dialect",
				Path: policyPath + "." + check.key, Field: policyPath + "." + check.key,
				SourceVersion: version, Operands: []int64{},
				Expected: check.want, Actual: got,
			}
		}
	}
	return policy, nil
}

func declaredRounding(policy map[string]any, keys []string, fallback Rounding, version SourceVersion) (Rounding, error) {
	for _, key := range keys {
		value := stringValue(policy, key)
		if value == "" {
			continue
		}
		switch Rounding(value) {
		case RoundFloor, RoundCeil, RoundTrunc, RoundNearest, RoundExact:
			return Rounding(value), nil
		default:
			return fallback, schemaError(version, "scale_policy."+key, "unsupported rounding direction %q", value)
		}
	}
	return fallback, nil
}

func enrichSkillScale(err error, root map[string]any, arrayKey, skillKey, timeKey, skillID string, tickMS int64) error {
	be, ok := AsError(err)
	if !ok {
		return err
	}
	be.SkillID = skillID
	if tick := firstIntentTickMillis(root, arrayKey, skillKey, timeKey, skillID, tickMS); tick >= 0 {
		be.Tick = &tick
	}
	return be
}

func firstIntentTickMillis(root map[string]any, arrayKey, skillKey, timeKey, skillID string, tickMS int64) int {
	arr, ok := root[arrayKey].([]any)
	if !ok {
		return -1
	}
	for _, raw := range arr {
		m, ok := raw.(map[string]any)
		if !ok || stringValue(m, skillKey) != skillID {
			continue
		}
		n, err := asInt(m[timeKey], arrayKey+"."+timeKey, SourceCanonical)
		if err != nil || tickMS <= 0 || n < 0 {
			return -1
		}
		return int(n / tickMS)
	}
	return -1
}

func withSource(err error, version SourceVersion) error {
	if be, ok := AsError(err); ok {
		if be.SourceVersion == "" {
			be.SourceVersion = version
		}
		if be.Path == "" && be.Field != "" {
			be.Path = be.Field
		}
		return be
	}
	return err
}

func decodeExpr(raw any, path string, version SourceVersion) (*Expr, error) {
	data, err := json.Marshal(raw)
	if err != nil {
		return nil, schemaError(version, path, "cannot encode expression: %v", err)
	}
	var expr Expr
	if err := json.Unmarshal(data, &expr); err != nil {
		return nil, &Error{
			Code: ECodeSchema, Message: "invalid expression tree", Path: path, Field: path,
			SourceVersion: version, Cause: err.Error(),
		}
	}
	if expr.Kind == "" {
		return nil, schemaError(version, path+".kind", "expression kind is required")
	}
	return &expr, nil
}

func reqMap(m map[string]any, key, path string, v SourceVersion) (map[string]any, error) {
	raw, ok := m[key]
	if !ok || raw == nil {
		return nil, schemaError(v, path, "missing required field %q", key)
	}
	return asMapRequired(raw, path, v)
}

func asMapRequired(raw any, path string, v SourceVersion) (map[string]any, error) {
	m, ok := raw.(map[string]any)
	if !ok {
		return nil, schemaError(v, path, "expected object")
	}
	return m, nil
}

func reqSlice(m map[string]any, key, path string, v SourceVersion) ([]any, error) {
	raw, ok := m[key]
	if !ok || raw == nil {
		return nil, schemaError(v, path, "missing required field %q", key)
	}
	arr, ok := raw.([]any)
	if !ok {
		return nil, schemaError(v, path, "expected array")
	}
	return arr, nil
}

func reqString(m map[string]any, key, path string, v SourceVersion) (string, error) {
	raw, ok := m[key]
	if !ok || raw == nil {
		return "", schemaError(v, path, "missing required field %q", key)
	}
	s, ok := raw.(string)
	if !ok || s == "" {
		return "", schemaError(v, path, "expected non-empty string")
	}
	return s, nil
}

func optionalString(m map[string]any, key string) (string, bool) {
	s, ok := m[key].(string)
	return s, ok && s != ""
}

func reqInt(m map[string]any, key, path string, v SourceVersion) (int64, error) {
	raw, ok := m[key]
	if !ok || raw == nil {
		return 0, schemaError(v, path, "missing required field %q", key)
	}
	return asInt(raw, path, v)
}

func reqTableInt(table map[string]any, key, path string, v SourceVersion) (int64, error) {
	raw, ok := table[key]
	if !ok || raw == nil {
		return 0, schemaError(v, path, "missing flat table key %q", key)
	}
	return asInt(raw, path, v)
}

func asInt(raw any, path string, v SourceVersion) (int64, error) {
	switch value := raw.(type) {
	case json.Number:
		n, err := value.Int64()
		if err != nil {
			return 0, schemaError(v, path, "expected int64: %v", err)
		}
		return n, nil
	case int64:
		return value, nil
	case int:
		return int64(value), nil
	case float64:
		return int64(value), nil
	default:
		return 0, schemaError(v, path, "expected integer")
	}
}

func stringValue(m map[string]any, key string) string {
	s, _ := m[key].(string)
	return s
}

func firstString(m map[string]any, keys ...string) string {
	for _, key := range keys {
		if s := stringValue(m, key); s != "" {
			return s
		}
	}
	return ""
}

func firstMapKey(m map[string]any, keys ...string) string {
	for _, key := range keys {
		if _, ok := m[key]; ok {
			return key
		}
	}
	return keys[0]
}

func findSkillIndex(skills []Skill, id string) int {
	for i := range skills {
		if skills[i].ID == id {
			return i
		}
	}
	return -1
}
