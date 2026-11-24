package interop

import "saveinterop/internal/core"

func (m *Middleware) evaluateCondition(branch core.BranchSpec, root map[string]any, id string, stack []string) (bool, error) {
	for i, current := range stack {
		if current == id {
			chain := append(append([]string(nil), stack[i:]...), id)
			return false, &core.Error{
				Code: core.CodeCycle, Op: "evaluate-condition",
				Message: "cyclic condition reference", Branch: branch.ID,
				Conditional: id, Chain: chain, Window: -1,
			}
		}
	}
	rule, ok := branch.Condition(id)
	if !ok {
		return false, &core.Error{
			Code: core.CodeCondition, Op: "evaluate-condition",
			Message: "condition rule is not declared", Branch: branch.ID,
			Conditional: id, FieldPath: "conditions." + id,
			Measured: "<missing>", Window: -1,
		}
	}
	stack = append(stack, id)
	switch rule.Op {
	case "flag":
		return m.evaluateFlag(branch, root, rule)
	case "and":
		if len(rule.Refs) == 0 {
			return false, conditionRuleError(branch, id, "and condition has no references")
		}
		for _, ref := range rule.Refs {
			value, err := m.evaluateCondition(branch, root, ref, stack)
			if err != nil {
				return false, err
			}
			if !value {
				return false, nil
			}
		}
		return true, nil
	case "or":
		if len(rule.Refs) == 0 {
			return false, conditionRuleError(branch, id, "or condition has no references")
		}
		for _, ref := range rule.Refs {
			value, err := m.evaluateCondition(branch, root, ref, stack)
			if err != nil {
				return false, err
			}
			if value {
				return true, nil
			}
		}
		return false, nil
	case "not":
		if len(rule.Refs) != 1 {
			return false, conditionRuleError(branch, id, "not condition requires exactly one reference")
		}
		value, err := m.evaluateCondition(branch, root, rule.Refs[0], stack)
		if err != nil {
			return false, err
		}
		return !value, nil
	default:
		return false, conditionRuleError(branch, id, "unsupported condition operation "+rule.Op)
	}
}

func (m *Middleware) evaluateFlag(branch core.BranchSpec, root map[string]any, rule core.ConditionRule) (bool, error) {
	flag, ok := branch.FlagBySemantic(rule.Flag)
	if !ok {
		return false, &core.Error{
			Code: core.CodeCondition, Op: "evaluate-flag",
			Message: "condition references undeclared semantic flag",
			Branch:  branch.ID, Conditional: rule.ID, Semantic: rule.Flag,
			FieldPath: "flags." + rule.Flag, Measured: "<missing>", Window: -1,
		}
	}
	raw, ok := core.Lookup(root, flag.Path)
	if !ok {
		return false, &core.Error{
			Code: core.CodeCondition, Op: "evaluate-flag",
			Message: "condition flag is missing",
			Branch:  branch.ID, Conditional: rule.ID, Semantic: rule.Flag,
			FieldPath: flag.Path, Measured: "<missing>", Window: -1,
		}
	}
	value, ok := core.DecodeBool(raw)
	if !ok {
		return false, &core.Error{
			Code: core.CodeCondition, Op: "evaluate-flag",
			Message: "condition flag is not boolean",
			Branch:  branch.ID, Conditional: rule.ID, Semantic: rule.Flag,
			FieldPath: flag.Path, Measured: raw, Window: -1,
		}
	}
	return value != flag.Invert, nil
}

func conditionRuleError(branch core.BranchSpec, id, message string) error {
	return &core.Error{
		Code: core.CodeCondition, Op: "evaluate-condition",
		Message: message, Branch: branch.ID, Conditional: id,
		FieldPath: "conditions." + id, Measured: "<invalid>", Window: -1,
	}
}

func (m *Middleware) conditionHoldsForModel(branch core.BranchSpec, model core.SemanticModel, id string) (bool, error) {
	root := make(map[string]any)
	for _, flag := range branch.Flags {
		if value, ok := model.Flags[flag.Semantic]; ok {
			core.Set(root, flag.Path, value != flag.Invert)
		}
	}
	return m.evaluateCondition(branch, root, id, nil)
}

// conditionHoldsForModelLenient is used during conflict detection. A source
// model may not carry every flag a different branch requires; that is a
// resolvable cross-branch conflict, not malformed input. Cycles are still hard
// errors because no deterministic decision can be made from a cyclic graph.
func (m *Middleware) conditionHoldsForModelLenient(branch core.BranchSpec, model core.SemanticModel, id string) (bool, error) {
	root := make(map[string]any)
	for _, flag := range branch.Flags {
		value, ok := model.Flags[flag.Semantic]
		if !ok {
			value = false
		}
		core.Set(root, flag.Path, value != flag.Invert)
	}
	return m.evaluateCondition(branch, root, id, nil)
}

func (m *Middleware) collectFlagValues(branch core.BranchSpec, root map[string]any, model *core.SemanticModel) {
	if model.Flags == nil {
		model.Flags = make(map[string]bool)
	}
	for _, flag := range branch.Flags {
		raw, ok := core.Lookup(root, flag.Path)
		if !ok {
			continue
		}
		value, ok := core.DecodeBool(raw)
		if !ok {
			continue
		}
		model.Flags[flag.Semantic] = value != flag.Invert
	}
}

func (m *Middleware) logEvaluation(branch core.BranchSpec, root map[string]any, evaluations *[]Evaluation) {
	for _, condition := range branch.Conditions {
		value, err := m.evaluateCondition(branch, root, condition.ID, nil)
		if err != nil {
			continue
		}
		*evaluations = append(*evaluations, Evaluation{ConditionID: condition.ID, Value: value})
	}
}
