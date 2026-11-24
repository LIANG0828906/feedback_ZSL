package balance

import "fmt"

type formulaEvalContext struct {
	nodes map[string]Formula
	skill Skill
	attrs map[string]Fixed
}

func evalFormula(root string, ctx formulaEvalContext) (Fixed, error) {
	memo := make(map[string]Fixed, len(ctx.nodes))
	visiting := make(map[string]bool, len(ctx.nodes))

	var eval func(string) (Fixed, error)
	eval = func(id string) (Fixed, error) {
		if value, ok := memo[id]; ok {
			return value, nil
		}
		node, ok := ctx.nodes[id]
		if !ok {
			return 0, newError(CodeSchema, id, "formula reference is missing")
		}
		if visiting[id] {
			return 0, &AppError{
				Code:   CodeCycle,
				Object: id,
				Field:  "formula.dependency",
				Detail: "cycle detected while evaluating formula",
			}
		}
		visiting[id] = true
		defer delete(visiting, id)

		input := func(index int) (Fixed, error) {
			if index < 0 || index >= len(node.Inputs) {
				return 0, newError(CodeSchema, id, "formula input %d is missing", index)
			}
			return eval(node.Inputs[index])
		}

		var (
			out Fixed
			err error
		)
		switch node.Op {
		case "const":
			out = node.Value
		case "skill_base":
			out = ctx.skill.BaseDamage
		case "skill_growth":
			out = ctx.skill.GrowthCoefficient
		case "attr":
			value, ok := ctx.attrs[node.Ref]
			if !ok {
				return 0, newError(CodeSchema, id, "attribute %q is missing from build", node.Ref)
			}
			out = value
		case "add", "sub", "mul", "div", "min", "max":
			if len(node.Inputs) < 2 {
				return 0, newError(CodeSchema, id, "operator %s requires at least two inputs", node.Op)
			}
			out, err = input(0)
			if err != nil {
				return 0, err
			}
			for index := 1; index < len(node.Inputs); index++ {
				rhs, inputErr := input(index)
				if inputErr != nil {
					return 0, inputErr
				}
				switch node.Op {
				case "add":
					var ok bool
					out, ok = addFixed(out, rhs)
					if !ok {
						return 0, newError(CodeRange, id, "formula addition overflows")
					}
				case "sub":
					var ok bool
					out, ok = subFixed(out, rhs)
					if !ok {
						return 0, newError(CodeRange, id, "formula subtraction overflows")
					}
				case "mul":
					var ok bool
					out, ok = MulDiv(out, rhs, int64(FixedScale))
					if !ok {
						return 0, newError(CodeRange, id, "formula multiplication overflows")
					}
				case "div":
					if rhs == 0 {
						return 0, newError(CodeRange, id, "division by zero")
					}
					var ok bool
					out, ok = MulDiv(out, FixedScale, int64(rhs))
					if !ok {
						return 0, newError(CodeRange, id, "formula division overflows")
					}
				case "min":
					if rhs < out {
						out = rhs
					}
				case "max":
					if rhs > out {
						out = rhs
					}
				}
			}
			if node.Op == "mul" || node.Op == "div" {
				if len(node.Inputs) != 2 {
					return 0, newError(CodeSchema, id, "%s requires exactly two inputs", node.Op)
				}
			}
		case "formula":
			out, err = eval(node.Ref)
			if err != nil {
				return 0, err
			}
		default:
			return 0, newError(CodeSchema, id, "unknown formula operator %q", node.Op)
		}
		if !inFixedRange(out) {
			return 0, newError(CodeRange, id, "formula result %d is outside fixed-point range", out)
		}
		memo[id] = out
		return out, nil
	}

	return eval(root)
}

type skillModel struct {
	SkillID           string
	BaseDamage        Fixed
	GrowthCoefficient Fixed
	Cooldown          int
	ResourceCost      Fixed
	HitMinBPS         int
	HitMaxBPS         int
	MinDamage         Fixed
	MaxDamage         Fixed
}

type preparedPlan struct {
	Plan        Plan
	Skills      map[string]skillModel
	BuildSkills map[string]map[string]skillModel
}

func preparePlan(project *Project, plan Plan) (*preparedPlan, error) {
	nodes := make(map[string]Formula, len(project.Formulas))
	for _, formula := range project.Formulas {
		nodes[formula.ID] = formula
	}
	skills := make(map[string]Skill, len(project.Skills))
	for _, skill := range project.Skills {
		skills[skill.ID] = skill
	}
	adjustments := make(map[string]SkillAdjustment, len(plan.Adjustments))
	for _, adjustment := range plan.Adjustments {
		adjustments[adjustment.SkillID] = adjustment
	}

	effective := make(map[string]Skill, len(project.Skills))
	for skillID, skill := range skills {
		if adjustment, ok := adjustments[skillID]; ok {
			var err error
			skill, err = applyAdjustment(plan.ID, skill, adjustment)
			if err != nil {
				return nil, err
			}
		}
		if err := validateSkillValues(skill); err != nil {
			return nil, err
		}
		effective[skillID] = skill
	}

	prepared := &preparedPlan{
		Plan:        plan,
		Skills:      make(map[string]skillModel, len(effective)),
		BuildSkills: make(map[string]map[string]skillModel),
	}
	for skillID, skill := range effective {
		prepared.Skills[skillID] = skillModel{
			SkillID:           skillID,
			BaseDamage:        skill.BaseDamage,
			GrowthCoefficient: skill.GrowthCoefficient,
			Cooldown:          skill.Cooldown,
			ResourceCost:      skill.ResourceCost,
			HitMinBPS:         skill.HitVarianceMinBPS,
			HitMaxBPS:         skill.HitVarianceMaxBPS,
		}
	}

	for _, build := range project.Builds {
		buildModels := make(map[string]skillModel, len(build.Rotation))
		for _, skillID := range build.Rotation {
			skill, ok := effective[skillID]
			if !ok {
				return nil, &AppError{
					Code:   CodeSchema,
					Object: build.ID,
					PlanID: plan.ID,
					Field:  "build.rotation",
					Detail: fmt.Sprintf("unknown skill %q", skillID),
				}
			}
			base, err := evalFormula(skill.FormulaID, formulaEvalContext{
				nodes: nodes,
				skill: skill,
				attrs: build.Attributes,
			})
			if err != nil {
				var appErr *AppError
				if AsAppError(err) != nil {
					appErr = AsAppError(err)
					appErr.PlanID = plan.ID
				}
				return nil, err
			}
			if base < 0 {
				return nil, &AppError{
					Code:   CodeRange,
					Object: skillID,
					PlanID: plan.ID,
					Field:  "skill.baseDamage",
					Detail: "evaluated base damage is negative",
				}
			}
			minDamage, ok := damageWithVariance(base, skill.HitVarianceMinBPS)
			if !ok {
				return nil, newError(CodeRange, skillID, "minimum hit variance overflows")
			}
			maxDamage, ok := damageWithVariance(base, skill.HitVarianceMaxBPS)
			if !ok {
				return nil, newError(CodeRange, skillID, "maximum hit variance overflows")
			}
			model := prepared.Skills[skillID]
			model.BaseDamage = base
			model.MinDamage = minDamage
			model.MaxDamage = maxDamage
			prepared.Skills[skillID] = model
			buildModels[skillID] = model
		}
		prepared.BuildSkills[build.ID] = buildModels
	}
	return prepared, nil
}

func applyAdjustment(planID string, skill Skill, adjustment SkillAdjustment) (Skill, error) {
	base, ok := addFixed(skill.BaseDamage, adjustment.BaseDamageDelta)
	if !ok {
		return skill, newError(CodeRange, skill.ID, "base damage adjustment overflows")
	}
	growth, ok := addFixed(skill.GrowthCoefficient, adjustment.GrowthCoefficientDelta)
	if !ok {
		return skill, newError(CodeRange, skill.ID, "growth adjustment overflows")
	}
	cost, ok := addFixed(skill.ResourceCost, adjustment.ResourceCostDelta)
	if !ok {
		return skill, newError(CodeRange, skill.ID, "resource cost adjustment overflows")
	}
	cooldown := skill.Cooldown + adjustment.CooldownDelta
	skill.BaseDamage = base
	skill.GrowthCoefficient = growth
	skill.ResourceCost = cost
	skill.Cooldown = cooldown
	if err := validateSkillValues(skill); err != nil {
		if appErr := AsAppError(err); appErr != nil {
			appErr.PlanID = planID
		}
		return skill, err
	}
	return skill, nil
}
