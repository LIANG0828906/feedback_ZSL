package macrokit

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

func LoadProjectFile(path string) (*Project, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, &Error{Code: ESchema, Path: path, Message: "read macro project", Cause: err}
	}
	p, err := LoadProjectBytes(data, path)
	if err != nil {
		return nil, err
	}
	p.SourcePath, _ = filepath.Abs(path)
	return p, nil
}

func LoadProjectBytes(data []byte, sourcePath string) (*Project, error) {
	var header struct {
		Format *int `json:"format"`
	}
	if err := json.Unmarshal(data, &header); err != nil {
		return nil, newSchemaError("$", "invalid JSON: %v", err)
	}
	if header.Format == nil {
		return nil, newSchemaError("/format", "required field is missing")
	}

	switch *header.Format {
	case FormatFlatFrameV1:
		return loadFormat1(data)
	case FormatNestedMSV2:
		return loadFormat2(data)
	case FormatTreeFrameV3:
		return loadFormat3(data)
	default:
		return nil, newSchemaError("/format", "unknown format number %d", *header.Format)
	}
}

func requiredString(value *string, path string) (string, error) {
	if value == nil || strings.TrimSpace(*value) == "" {
		return "", newSchemaError(path, "required field is missing or empty")
	}
	return *value, nil
}

func requiredInt(value *int, path string) (int, error) {
	if value == nil {
		return 0, newSchemaError(path, "required field is missing")
	}
	return *value, nil
}

func requiredInt64(value *int64, path string) (int64, error) {
	if value == nil {
		return 0, newSchemaError(path, "required field is missing")
	}
	return *value, nil
}

func defaultValue[T any](ptr *T, fallback T) T {
	if ptr == nil {
		return fallback
	}
	return *ptr
}

func normalizeOp(op string) ConditionKind {
	switch strings.ToLower(strings.TrimSpace(op)) {
	case "all", "and", "every":
		return CondAll
	case "any", "or", "some":
		return CondAny
	case "not", "none":
		return CondNot
	case "key", "keystate", "key_state":
		return CondKey
	case "flag", "bool", "variable":
		return CondFlag
	case "branch", "branchresult", "branch_result", "predecessor":
		return CondBranch
	case "const", "constant", "literal":
		return CondConstant
	default:
		return ConditionKind(strings.ToLower(op))
	}
}

func normalizeKeyState(state string) KeyState {
	switch strings.ToLower(strings.TrimSpace(state)) {
	case "pressed", "press", "edge", "down_edge":
		return KeyPressed
	case "down", "held", "hold":
		return KeyDown
	default:
		return KeyState(state)
	}
}

func defaultTimePolicy() TimePolicy {
	return TimePolicy{FrameToMS: RoundFloor, MSToFrame: RoundCeil}
}

func applyTimePolicyDTO(policy *TimePolicy, dto *dtoTimePolicy) {
	if dto == nil {
		return
	}
	if dto.FrameToMS != nil {
		policy.FrameToMS = RoundingDirection(*dto.FrameToMS)
	}
	if dto.FrameToMSCamel != nil {
		policy.FrameToMS = RoundingDirection(*dto.FrameToMSCamel)
	}
	if dto.MSToFrame != nil {
		policy.MSToFrame = RoundingDirection(*dto.MSToFrame)
	}
	if dto.MSToFrameCamel != nil {
		policy.MSToFrame = RoundingDirection(*dto.MSToFrameCamel)
	}
}

func validateProject(p *Project) error {
	if p.ID == "" {
		return newSchemaError("/project_id", "required field is missing")
	}
	if p.FrameRate <= 0 {
		return newSchemaError("/frame_rate", "frame_rate must be positive")
	}
	if p.TimePolicy.FrameToMS != RoundFloor && p.TimePolicy.FrameToMS != RoundCeil {
		return newSchemaError("/time_policy/frame_to_ms", "unsupported rounding direction %q", p.TimePolicy.FrameToMS)
	}
	if p.TimePolicy.MSToFrame != RoundFloor && p.TimePolicy.MSToFrame != RoundCeil {
		return newSchemaError("/time_policy/ms_to_frame", "unsupported rounding direction %q", p.TimePolicy.MSToFrame)
	}
	if len(p.Branches) == 0 {
		return newSchemaError("/branches", "at least one branch is required")
	}

	ids := make(map[string]bool, len(p.Branches))
	p.Index()
	for i, b := range p.Branches {
		base := fmt.Sprintf("/branches/%d", i)
		if b.ID == "" {
			return newSchemaError(base+"/id", "branch id is required")
		}
		if ids[b.ID] {
			return newSchemaError(base+"/id", "duplicate branch id %q", b.ID)
		}
		ids[b.ID] = true
		if b.MacroID == "" {
			return newSchemaError(base+"/macro_id", "macro id is required")
		}
		if b.DelayFrames < 0 {
			return newSchemaError(base+"/delay_frames", "delay must be non-negative")
		}
		if b.TriggerWindowFrames < 0 {
			return newSchemaError(base+"/trigger_window_frames", "window must be non-negative")
		}
		if b.Condition == nil {
			return newSchemaError(base+"/when", "condition is required")
		}
		if len(b.Steps) == 0 {
			return newSchemaError(base+"/steps", "at least one step is required")
		}
		for j, s := range b.Steps {
			stepPath := fmt.Sprintf("%s/steps/%d", base, j)
			if s.ID == "" {
				return newSchemaError(stepPath+"/id", "step id is required")
			}
			if s.Action == "" {
				return newSchemaError(stepPath+"/action", "step action is required")
			}
			if s.GapFrames < 0 || s.HoldFrames < 0 {
				return newSchemaError(stepPath, "step time values must be non-negative")
			}
		}
		if err := validateCondition(b.Condition, p.BranchByID, base+"/when"); err != nil {
			return err
		}
	}
	return validateBranchCycles(p)
}

func validateCondition(c *Condition, branches map[string]*Branch, path string) error {
	if c == nil {
		return newSchemaError(path, "condition is required")
	}
	switch c.Kind {
	case CondAll, CondAny:
		if len(c.Children) == 0 {
			return newSchemaError(path, "%s condition requires children", c.Kind)
		}
		for i, child := range c.Children {
			if err := validateCondition(child, branches, fmt.Sprintf("%s/children/%d", path, i)); err != nil {
				return err
			}
		}
	case CondNot:
		if len(c.Children) != 1 {
			return newSchemaError(path, "not condition requires exactly one child")
		}
		return validateCondition(c.Children[0], branches, path+"/child")
	case CondKey:
		if c.Key == "" {
			return newSchemaError(path+"/key", "key condition requires key")
		}
		if c.KeyState != "" && c.KeyState != KeyDown && c.KeyState != KeyPressed {
			return newSchemaError(path+"/state", "unsupported key state %q", c.KeyState)
		}
	case CondFlag:
		if c.Flag == "" {
			return newSchemaError(path+"/flag", "flag condition requires flag name")
		}
	case CondBranch:
		if c.BranchID == "" {
			return newSchemaError(path+"/branch_id", "branch condition requires branch id")
		}
		if branches != nil {
			if _, ok := branches[c.BranchID]; !ok {
				return newRefError(path+"/branch_id", c.BranchID, "branch reference is out of bounds")
			}
		}
	case CondConstant:
		if c.Value == nil {
			return newSchemaError(path+"/value", "constant condition requires value")
		}
	default:
		return newSchemaError(path+"/op", "unsupported condition op %q", c.Kind)
	}
	return nil
}

func validateBranchCycles(p *Project) error {
	const (
		unvisited = 0
		visiting  = 1
		done      = 2
	)
	state := map[string]int{}
	var visit func(id string, path string) error
	visit = func(id, path string) error {
		switch state[id] {
		case visiting:
			return newRefError(path, id, "cyclic predecessor branch reference")
		case done:
			return nil
		}
		state[id] = visiting
		b := p.BranchByID[id]
		var walk func(c *Condition) error
		walk = func(c *Condition) error {
			if c == nil {
				return nil
			}
			if c.Kind == CondBranch && c.BranchID != "" {
				if err := visit(c.BranchID, path+"/branch_id"); err != nil {
					return err
				}
			}
			for _, child := range c.Children {
				if err := walk(child); err != nil {
					return err
				}
			}
			return nil
		}
		if err := walk(b.Condition); err != nil {
			return err
		}
		state[id] = done
		return nil
	}
	for _, b := range p.SortedBranches() {
		if err := visit(b.ID, fmt.Sprintf("/branches/%s", b.ID)); err != nil {
			return err
		}
	}
	return nil
}

func cloneFlags(flags map[string]bool) map[string]bool {
	out := make(map[string]bool, len(flags))
	for k, v := range flags {
		out[k] = v
	}
	return out
}
