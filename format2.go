package macrokit

import (
	"encoding/json"
	"fmt"
)

type dtoFormat2 struct {
	Format        *int            `json:"format"`
	SchemaVersion *string         `json:"schemaVersion"`
	ProjectID     *string         `json:"projectId"`
	Name          string          `json:"name"`
	FrameRate     *int            `json:"frameRate"`
	TimePolicy    *dtoTimePolicy  `json:"timePolicy"`
	Flags         map[string]bool `json:"flags"`
	Branches      *[]dtoBranch2   `json:"branches"`
}

type dtoBranch2 struct {
	BranchID        *string     `json:"branchId"`
	MacroID         *string     `json:"macroId"`
	Name            string      `json:"name"`
	Priority        *int        `json:"priority"`
	DelayMS         *int64      `json:"delayMs"`
	TriggerWindowMS *int64      `json:"triggerWindowMs"`
	When            *dtoCond2   `json:"when"`
	CancelKeys      []string    `json:"cancelKeys"`
	Steps           *[]dtoStep2 `json:"steps"`
}

type dtoCond2 struct {
	Op            string      `json:"op"`
	Children      []*dtoCond2 `json:"children"`
	KeyCode       string      `json:"keyCode"`
	State         string      `json:"state"`
	FlagName      string      `json:"flagName"`
	BranchID      string      `json:"branchId"`
	ExpectOutcome string      `json:"expectOutcome"`
	Value         *bool       `json:"value"`
}

type dtoStep2 struct {
	StepID  *string `json:"stepId"`
	Action  *string `json:"actionName"`
	Lane    string  `json:"laneName"`
	DelayMS *int64  `json:"delayMs"`
	GapMS   *int64  `json:"gapMs"`
	HoldMS  *int64  `json:"holdMs"`
}

func loadFormat2(data []byte) (*Project, error) {
	var dto dtoFormat2
	if err := json.Unmarshal(data, &dto); err != nil {
		return nil, newSchemaError("$", "invalid format-2 JSON: %v", err)
	}
	version, err := requiredString(dto.SchemaVersion, "/schemaVersion")
	if err != nil {
		return nil, err
	}
	projectID, err := requiredString(dto.ProjectID, "/projectId")
	if err != nil {
		return nil, err
	}
	rate, err := requiredInt(dto.FrameRate, "/frameRate")
	if err != nil {
		return nil, err
	}
	if dto.Branches == nil {
		return nil, newSchemaError("/branches", "required field is missing")
	}

	policy := defaultTimePolicy()
	if dto.TimePolicy != nil {
		applyTimePolicyDTO(&policy, dto.TimePolicy)
	}

	p := &Project{
		FormatNo:      FormatNestedMSV2,
		FormatName:    "nested_ms",
		FormatVersion: version,
		ID:            projectID,
		Name:          dto.Name,
		FrameRate:     rate,
		TimePolicy:    policy,
		Flags:         cloneFlags(dto.Flags),
	}

	for i, bdto := range *dto.Branches {
		bp := fmt.Sprintf("/branches/%d", i)
		id, err := requiredString(bdto.BranchID, bp+"/branchId")
		if err != nil {
			return nil, err
		}
		macroID, err := requiredString(bdto.MacroID, bp+"/macroId")
		if err != nil {
			return nil, err
		}
		if bdto.When == nil {
			return nil, newSchemaError(bp+"/when", "required field is missing")
		}
		cond := buildCond2(bdto.When)

		delayMS := defaultValue(bdto.DelayMS, 0)
		delayFrames, err := msToFramesForBranch(id, delayMS, rate, policy.MSToFrame, bp+"/delayMs")
		if err != nil {
			return nil, err
		}
		windowMS := defaultValue(bdto.TriggerWindowMS, 0)
		windowFrames, err := msToFramesForBranch(id, windowMS, rate, policy.MSToFrame, bp+"/triggerWindowMs")
		if err != nil {
			return nil, err
		}
		if windowMS != 0 && windowFrames == 0 {
			windowFrames = 1
		}
		if windowFrames == 0 {
			windowFrames = 1
		}

		b := &Branch{
			ID:                  id,
			MacroID:             macroID,
			Name:                bdto.Name,
			HasPriority:         bdto.Priority != nil,
			Priority:            defaultValue(bdto.Priority, 0),
			DelayFrames:         delayFrames,
			TriggerWindowFrames: windowFrames,
			Condition:           cond,
			CancelKeys:          append([]string(nil), bdto.CancelKeys...),
		}
		if bdto.Steps == nil {
			return nil, newSchemaError(bp+"/steps", "required field is missing")
		}
		if len(*bdto.Steps) == 0 {
			return nil, newSchemaError(bp+"/steps", "at least one step is required")
		}
		for j, sdto := range *bdto.Steps {
			sp := fmt.Sprintf("%s/steps/%d", bp, j)
			sid, err := requiredString(sdto.StepID, sp+"/stepId")
			if err != nil {
				return nil, err
			}
			action, err := requiredString(sdto.Action, sp+"/actionName")
			if err != nil {
				return nil, err
			}
			gapMS := int64(0)
			if sdto.GapMS != nil {
				gapMS = *sdto.GapMS
			} else if sdto.DelayMS != nil {
				gapMS = *sdto.DelayMS
			}
			gapFrames, err := msToFramesForBranch(id, gapMS, rate, policy.MSToFrame, sp+"/delayMs")
			if err != nil {
				return nil, err
			}
			holdMS := defaultValue(sdto.HoldMS, 0)
			holdFrames, err := msToFramesForBranch(id, holdMS, rate, policy.MSToFrame, sp+"/holdMs")
			if err != nil {
				return nil, err
			}
			b.Steps = append(b.Steps, &Step{
				ID:         sid,
				Action:     action,
				Lane:       sdto.Lane,
				GapFrames:  gapFrames,
				HoldFrames: holdFrames,
			})
		}
		p.Branches = append(p.Branches, b)
	}

	if err := validateProject(p); err != nil {
		return nil, err
	}
	return p, nil
}

func buildCond2(dto *dtoCond2) *Condition {
	if dto == nil {
		return nil
	}
	c := &Condition{
		Kind:     normalizeOp(dto.Op),
		Key:      dto.KeyCode,
		KeyState: normalizeKeyState(dto.State),
		Flag:     dto.FlagName,
		BranchID: dto.BranchID,
		Outcome:  dto.ExpectOutcome,
		Value:    dto.Value,
	}
	for _, child := range dto.Children {
		c.Children = append(c.Children, buildCond2(child))
	}
	return c
}
