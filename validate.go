package balance

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
)

func LoadProject(path string) (*Project, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, &AppError{Code: CodeIO, Object: path, Detail: err.Error()}
	}
	project, err := DecodeProject(data)
	if err != nil {
		return nil, err
	}
	if err := ValidateProject(project); err != nil {
		return nil, err
	}
	return project, nil
}

func DecodeProject(data []byte) (*Project, error) {
	if err := validateProjectPresence(data); err != nil {
		return nil, err
	}
	var project Project
	decoder := json.NewDecoder(bytes.NewReader(data))
	if err := decoder.Decode(&project); err != nil {
		var typeErr *json.UnmarshalTypeError
		if errors.As(err, &typeErr) && typeErr.Value == "number" {
			return nil, &AppError{Code: CodeRange, Object: typeErr.Field, Detail: err.Error()}
		}
		return nil, &AppError{Code: CodeSchema, Object: "project", Detail: err.Error()}
	}
	return &project, nil
}

func validateProjectPresence(data []byte) error {
	var root map[string]json.RawMessage
	if err := json.Unmarshal(data, &root); err != nil {
		return &AppError{Code: CodeSchema, Object: "project", Detail: err.Error()}
	}
	if err := requireKeys(root, "project",
		"schemaVersion", "seed", "algorithmVersion", "budgetVersion",
		"classes", "skills", "formulas", "builds", "scenarios", "plans",
		"thresholds", "budget"); err != nil {
		return err
	}
	if err := requireObjectArray(root["classes"], "classes", "id", "name"); err != nil {
		return err
	}
	if err := requireObjectArray(root["skills"], "skills",
		"id", "classId", "formulaId", "baseDamage", "growthCoefficient",
		"cooldown", "resourceCost", "hitVarianceMinBps", "hitVarianceMaxBps"); err != nil {
		return err
	}
	if err := validateFormulaPresence(root["formulas"]); err != nil {
		return err
	}
	if err := requireObjectArray(root["builds"], "builds",
		"id", "classId", "maxHp", "maxResource", "resourceRegen", "attributes", "rotation"); err != nil {
		return err
	}
	if err := requireObjectArray(root["scenarios"], "scenarios",
		"id", "kind", "leftBuilds", "rightBuilds", "maxRounds", "reviveRounds"); err != nil {
		return err
	}
	if err := requireObjectArray(root["plans"], "plans",
		"id", "priority", "targetScenarios", "adjustments"); err != nil {
		return err
	}
	if err := requireObjectArray(root["thresholds"], "thresholds",
		"id", "metric", "min", "max"); err != nil {
		return err
	}
	var thresholds []json.RawMessage
	if err := json.Unmarshal(root["thresholds"], &thresholds); err == nil {
		for index, raw := range thresholds {
			var object map[string]json.RawMessage
			if err := json.Unmarshal(raw, &object); err != nil {
				return &AppError{Code: CodeSchema, Object: fmt.Sprintf("thresholds[%d]", index), Detail: err.Error()}
			}
			if err := requireKeys(object, fmt.Sprintf("thresholds[%d]", index), "id", "metric", "min", "max"); err != nil {
				return err
			}
		}
	}
	var plans []json.RawMessage
	if err := json.Unmarshal(root["plans"], &plans); err == nil {
		for index, raw := range plans {
			var object map[string]json.RawMessage
			if err := json.Unmarshal(raw, &object); err != nil {
				return &AppError{Code: CodeSchema, Object: fmt.Sprintf("plans[%d]", index), Detail: err.Error()}
			}
			if err := requireKeys(object, fmt.Sprintf("plans[%d]", index),
				"id", "priority", "targetScenarios", "adjustments"); err != nil {
				return err
			}
			if err := requireObjectArray(object["adjustments"], fmt.Sprintf("plans[%d].adjustments", index),
				"skillId", "baseDamageDelta", "growthCoefficientDelta",
				"resourceCostDelta", "cooldownDelta"); err != nil {
				return err
			}
		}
	}
	var budget map[string]json.RawMessage
	if err := json.Unmarshal(root["budget"], &budget); err != nil {
		return &AppError{Code: CodeSchema, Object: "budget", Detail: err.Error()}
	}
	if err := requireKeys(budget, "budget",
		"slots", "maxMatchesPerRound", "memoryBuffer", "maxRounds",
		"targetMatchesPerCombo", "maxTotalMatches", "priorityRule"); err != nil {
		return err
	}
	return nil
}

func requireObjectArray(raw json.RawMessage, path string, keys ...string) error {
	if len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return &AppError{Code: CodeSchema, Object: path, Field: path, Detail: "required array is missing"}
	}
	var values []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &values); err != nil {
		return &AppError{Code: CodeSchema, Object: path, Detail: err.Error()}
	}
	for index, value := range values {
		if err := requireKeys(value, fmt.Sprintf("%s[%d]", path, index), keys...); err != nil {
			return err
		}
	}
	return nil
}

func validateFormulaPresence(raw json.RawMessage) error {
	if err := requireObjectArray(raw, "formulas", "id", "op"); err != nil {
		return err
	}
	var values []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &values); err != nil {
		return &AppError{Code: CodeSchema, Object: "formulas", Detail: err.Error()}
	}
	for index, value := range values {
		var op string
		_ = json.Unmarshal(value["op"], &op)
		switch op {
		case "const":
			if err := requireKeys(value, fmt.Sprintf("formulas[%d]", index), "value"); err != nil {
				return err
			}
		case "attr", "formula":
			if err := requireKeys(value, fmt.Sprintf("formulas[%d]", index), "ref"); err != nil {
				return err
			}
		case "skill_base", "skill_growth":
		case "add", "sub", "mul", "div", "min", "max":
			if err := requireKeys(value, fmt.Sprintf("formulas[%d]", index), "inputs"); err != nil {
				return err
			}
		default:
			return &AppError{
				Code:   CodeSchema,
				Object: fmt.Sprintf("formulas[%d]", index),
				Field:  "op",
				Detail: fmt.Sprintf("unknown operator %q", op),
			}
		}
	}
	return nil
}

func requireKeys(object map[string]json.RawMessage, path string, keys ...string) error {
	for _, key := range keys {
		raw, ok := object[key]
		if !ok || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return &AppError{
				Code:   CodeSchema,
				Object: path + "." + key,
				Field:  key,
				Detail: "required field is missing",
			}
		}
	}
	return nil
}

func ValidateProject(project *Project) error {
	if project == nil {
		return &AppError{Code: CodeSchema, Object: "project", Detail: "project is nil"}
	}
	if project.SchemaVersion != SchemaVersion {
		return &AppError{Code: CodeSchema, Object: "project.schemaVersion", Field: "schemaVersion", Detail: "unsupported schema version"}
	}
	if project.AlgorithmVersion == "" || project.BudgetVersion == "" {
		return &AppError{Code: CodeSchema, Object: "project", Field: "version", Detail: "algorithm and budget versions are required"}
	}

	classIDs := make(map[string]struct{}, len(project.Classes))
	for _, class := range project.Classes {
		if class.ID == "" || class.Name == "" {
			return newError(CodeSchema, class.ID, "class id and name are required")
		}
		if _, exists := classIDs[class.ID]; exists {
			return newError(CodeSchema, class.ID, "duplicate class id")
		}
		classIDs[class.ID] = struct{}{}
	}

	formulas := make(map[string]Formula, len(project.Formulas))
	for _, formula := range project.Formulas {
		if formula.ID == "" {
			return newError(CodeSchema, "formulas", "formula id is required")
		}
		if _, exists := formulas[formula.ID]; exists {
			return newError(CodeSchema, formula.ID, "duplicate formula id")
		}
		formulas[formula.ID] = formula
	}
	for _, formula := range project.Formulas {
		switch formula.Op {
		case "const", "skill_base", "skill_growth":
		case "attr", "formula":
			if formula.Ref == "" {
				return newError(CodeSchema, formula.ID, "formula ref is required")
			}
		case "add", "sub", "mul", "div", "min", "max":
			if len(formula.Inputs) < 2 {
				return newError(CodeSchema, formula.ID, "formula operator requires at least two inputs")
			}
			if (formula.Op == "mul" || formula.Op == "div") && len(formula.Inputs) != 2 {
				return newError(CodeSchema, formula.ID, "formula operator requires exactly two inputs")
			}
		default:
			return newError(CodeSchema, formula.ID, "unknown formula operator %q", formula.Op)
		}
		if !inFixedRange(formula.Value) {
			return newError(CodeRange, formula.ID, "formula constant is outside fixed-point range")
		}
		for _, input := range formula.Inputs {
			if _, exists := formulas[input]; !exists {
				return newError(CodeSchema, formula.ID, "formula input %q does not exist", input)
			}
		}
		if formula.Op == "formula" {
			if _, exists := formulas[formula.Ref]; !exists {
				return newError(CodeSchema, formula.ID, "formula ref %q does not exist", formula.Ref)
			}
		}
	}
	if err := detectFormulaCycle(formulas); err != nil {
		return err
	}

	skills := make(map[string]Skill, len(project.Skills))
	for _, skill := range project.Skills {
		if skill.ID == "" {
			return newError(CodeSchema, "skills", "skill id is required")
		}
		if _, exists := skills[skill.ID]; exists {
			return newError(CodeSchema, skill.ID, "duplicate skill id")
		}
		if _, exists := classIDs[skill.ClassID]; !exists {
			return newError(CodeSchema, skill.ID, "skill class %q does not exist", skill.ClassID)
		}
		if _, exists := formulas[skill.FormulaID]; !exists {
			return newError(CodeSchema, skill.ID, "skill formula %q does not exist", skill.FormulaID)
		}
		if err := validateSkillValues(skill); err != nil {
			return err
		}
		skills[skill.ID] = skill
	}

	builds := make(map[string]Build, len(project.Builds))
	for _, build := range project.Builds {
		if build.ID == "" {
			return newError(CodeSchema, "builds", "build id is required")
		}
		if _, exists := builds[build.ID]; exists {
			return newError(CodeSchema, build.ID, "duplicate build id")
		}
		if _, exists := classIDs[build.ClassID]; !exists {
			return newError(CodeSchema, build.ID, "build class %q does not exist", build.ClassID)
		}
		if build.MaxHP <= 0 || build.MaxResource < 0 || build.ResourceRegen < 0 {
			return newError(CodeRange, build.ID, "build hp/resource values are out of range")
		}
		if !inFixedRange(build.MaxHP) || !inFixedRange(build.MaxResource) || !inFixedRange(build.ResourceRegen) {
			return newError(CodeRange, build.ID, "build fixed-point value is out of range")
		}
		if len(build.Rotation) == 0 {
			return newError(CodeSchema, build.ID, "build rotation is empty")
		}
		seenRotation := make(map[string]struct{}, len(build.Rotation))
		for _, skillID := range build.Rotation {
			if _, exists := skills[skillID]; !exists {
				return newError(CodeSchema, build.ID, "rotation skill %q does not exist", skillID)
			}
			if _, exists := seenRotation[skillID]; exists {
				return newError(CodeSchema, build.ID, "duplicate rotation skill %q", skillID)
			}
			seenRotation[skillID] = struct{}{}
		}
		for key, value := range build.Attributes {
			if key == "" || !inFixedRange(value) {
				return newError(CodeRange, build.ID, "attribute %q is out of range", key)
			}
		}
		builds[build.ID] = build
	}

	scenarios := make(map[string]Scenario, len(project.Scenarios))
	buildUses := make(map[string]int, len(project.Builds))
	for _, scenario := range project.Scenarios {
		if scenario.ID == "" {
			return newError(CodeSchema, "scenarios", "scenario id is required")
		}
		if _, exists := scenarios[scenario.ID]; exists {
			return newError(CodeSchema, scenario.ID, "duplicate scenario id")
		}
		if scenario.Kind != ScenarioDuel && scenario.Kind != ScenarioSquad {
			return newError(CodeSchema, scenario.ID, "unknown scenario kind %q", scenario.Kind)
		}
		if scenario.MaxRounds <= 0 || scenario.MaxRounds > 1_000_000 ||
			scenario.ReviveRounds < 0 || scenario.ReviveRounds > 1_000_000 {
			return newError(CodeRange, scenario.ID, "scenario rounds are out of range")
		}
		if scenario.Kind == ScenarioDuel && (len(scenario.LeftBuilds) != 1 || len(scenario.RightBuilds) != 1) {
			return newError(CodeSchema, scenario.ID, "duel requires exactly one build per side")
		}
		if len(scenario.LeftBuilds) == 0 || len(scenario.RightBuilds) == 0 {
			return newError(CodeSchema, scenario.ID, "both sides require at least one build")
		}
		for _, buildID := range append(append([]string{}, scenario.LeftBuilds...), scenario.RightBuilds...) {
			if _, exists := builds[buildID]; !exists {
				return newError(CodeSchema, scenario.ID, "scenario build %q does not exist", buildID)
			}
			buildUses[buildID]++
		}
		scenarios[scenario.ID] = scenario
	}
	for _, build := range project.Builds {
		useCount := buildUses[build.ID]
		if useCount != 1 {
			return &AppError{
				Code:   CodeSchema,
				Object: build.ID,
				Field:  "scenario.buildMapping",
				Detail: fmt.Sprintf("build must be used by exactly one scenario side, got %d", useCount),
			}
		}
	}

	plans := make(map[string]Plan, len(project.Plans))
	for _, plan := range project.Plans {
		if plan.ID == "" {
			return newError(CodeSchema, "plans", "plan id is required")
		}
		if _, exists := plans[plan.ID]; exists {
			return newError(CodeSchema, plan.ID, "duplicate plan id")
		}
		if len(plan.TargetScenarios) == 0 {
			return newError(CodeSchema, plan.ID, "plan target scenarios are empty")
		}
		seenScenarios := make(map[string]struct{}, len(plan.TargetScenarios))
		for _, scenarioID := range plan.TargetScenarios {
			if _, exists := scenarios[scenarioID]; !exists {
				return newError(CodeSchema, plan.ID, "target scenario %q does not exist", scenarioID)
			}
			if _, exists := seenScenarios[scenarioID]; exists {
				return newError(CodeSchema, plan.ID, "duplicate target scenario %q", scenarioID)
			}
			seenScenarios[scenarioID] = struct{}{}
		}
		seenAdjustments := make(map[string]struct{}, len(plan.Adjustments))
		for _, adjustment := range plan.Adjustments {
			skill, exists := skills[adjustment.SkillID]
			if !exists {
				return newError(CodeSchema, plan.ID, "adjustment skill %q does not exist", adjustment.SkillID)
			}
			if _, exists := seenAdjustments[adjustment.SkillID]; exists {
				return newError(CodeSchema, plan.ID, "duplicate adjustment for skill %q", adjustment.SkillID)
			}
			seenAdjustments[adjustment.SkillID] = struct{}{}
			if _, err := applyAdjustment(plan.ID, skill, adjustment); err != nil {
				return err
			}
		}
		plans[plan.ID] = plan
	}

	thresholdMetrics := map[string]bool{
		"win_rate_ppm":      true,
		"avg_damage":        true,
		"max_ci_width_ppm":  true,
		"completed_matches": true,
	}
	for _, threshold := range project.Thresholds {
		if threshold.ID == "" || threshold.Metric == "" {
			return newError(CodeSchema, "thresholds", "threshold id and metric are required")
		}
		if !thresholdMetrics[threshold.Metric] {
			return newError(CodeSchema, threshold.ID, "unknown threshold metric %q", threshold.Metric)
		}
		if threshold.Max < threshold.Min {
			return newError(CodeRange, threshold.ID, "threshold max is smaller than min")
		}
		if threshold.PlanID != "" {
			if _, exists := plans[threshold.PlanID]; !exists {
				return newError(CodeSchema, threshold.ID, "threshold plan %q does not exist", threshold.PlanID)
			}
		}
	}

	if err := validateBudget(project.Budget); err != nil {
		return err
	}

	nodes := formulas
	for _, build := range project.Builds {
		for _, skill := range project.Skills {
			if _, err := evalFormula(skill.FormulaID, formulaEvalContext{
				nodes: nodes,
				skill: skill,
				attrs: build.Attributes,
			}); err != nil {
				if appErr := AsAppError(err); appErr != nil {
					appErr.Object = skill.ID
				}
				return err
			}
		}
	}

	_ = builds
	return nil
}

func validateSkillValues(skill Skill) error {
	if skill.BaseDamage < 0 || skill.BaseDamage > MaxFixed {
		return newError(CodeRange, skill.ID, "base damage is outside fixed-point range")
	}
	if skill.GrowthCoefficient < 0 || skill.GrowthCoefficient > 10*FixedScale {
		return newError(CodeRange, skill.ID, "growth coefficient is outside fixed-point range")
	}
	if skill.Cooldown < 0 || skill.Cooldown > 10000 {
		return newError(CodeRange, skill.ID, "cooldown is out of range")
	}
	if skill.ResourceCost < 0 || skill.ResourceCost > MaxFixed {
		return newError(CodeRange, skill.ID, "resource cost is outside fixed-point range")
	}
	if skill.HitVarianceMinBPS < -maxPercentBPS || skill.HitVarianceMinBPS > maxPercentBPS ||
		skill.HitVarianceMaxBPS < -maxPercentBPS || skill.HitVarianceMaxBPS > maxPercentBPS {
		return newError(CodeRange, skill.ID, "hit variance is out of range")
	}
	if skill.HitVarianceMinBPS > skill.HitVarianceMaxBPS {
		return newError(CodeRange, skill.ID, "hit variance min exceeds max")
	}
	return nil
}

func validateBudget(budget Budget) error {
	values := map[string]int{
		"slots":                 budget.Slots,
		"maxMatchesPerRound":    budget.MaxMatchesPerRound,
		"memoryBuffer":          budget.MemoryBuffer,
		"maxRounds":             budget.MaxRounds,
		"targetMatchesPerCombo": budget.TargetMatchesPerCombo,
		"maxTotalMatches":       budget.MaxTotalMatches,
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if values[key] < 0 || values[key] > 1_000_000_000 {
			return newError(CodeRange, "budget."+key, "budget value must be non-negative and bounded")
		}
	}
	if budget.PriorityRule != PriorityFirst && budget.PriorityRule != PriorityEqual {
		return &AppError{
			Code:   CodeSchema,
			Object: "budget.priorityRule",
			Field:  "priorityRule",
			Detail: "unknown priority rule",
		}
	}
	return nil
}

func detectFormulaCycle(nodes map[string]Formula) error {
	ids := make([]string, 0, len(nodes))
	for id := range nodes {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	state := make(map[string]int, len(nodes))
	stack := make([]string, 0, len(nodes))

	var visit func(string) error
	visit = func(id string) error {
		switch state[id] {
		case 1:
			start := 0
			for index, value := range stack {
				if value == id {
					start = index
					break
				}
			}
			cycle := append(append([]string{}, stack[start:]...), id)
			return &AppError{
				Code:   CodeCycle,
				Object: id,
				Field:  "formula.dependency",
				Detail: "formula dependency cycle: " + strings.Join(cycle, " -> "),
			}
		case 2:
			return nil
		}
		state[id] = 1
		stack = append(stack, id)
		node := nodes[id]
		edges := append([]string{}, node.Inputs...)
		if node.Op == "formula" {
			edges = append(edges, node.Ref)
		}
		for _, edge := range edges {
			if _, exists := nodes[edge]; !exists {
				continue
			}
			if err := visit(edge); err != nil {
				return err
			}
		}
		stack = stack[:len(stack)-1]
		state[id] = 2
		return nil
	}

	for _, id := range ids {
		if err := visit(id); err != nil {
			return err
		}
	}
	return nil
}
