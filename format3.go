package macrokit

import (
	"encoding/json"
	"fmt"
)

type dtoFormat3 struct {
	Format        *int            `json:"format"`
	SchemaVersion *string         `json:"SchemaVersion"`
	ProjectID     *string         `json:"ProjectID"`
	Name          string          `json:"Name"`
	FrameRate     *int            `json:"FrameRate"`
	TimePolicy    *dtoTimePolicy3 `json:"TimePolicy"`
	Flags         map[string]bool `json:"Flags"`
	Branches      *[]dtoBranch3   `json:"Branches"`
}

type dtoTimePolicy3 struct {
	FrameToMS string `json:"FrameToMS"`
	MSToFrame string `json:"MSToFrame"`
}

type dtoBranch3 struct {
	BranchID            *string     `json:"BranchID"`
	MacroID             *string     `json:"MacroID"`
	Name                string      `json:"Name"`
	Priority            *int        `json:"Priority"`
	DelayFrames         *int64      `json:"DelayFrames"`
	TriggerWindowFrames *int64      `json:"WindowFrames"`
	Condition           *dtoCond3   `json:"Condition"`
	CancelKeys          []string    `json:"CancelKeys"`
	Steps               *[]dtoStep3 `json:"Steps"`
}

type dtoCond3 struct {
	Type      string      `json:"Type"`
	Nodes     []*dtoCond3 `json:"Nodes"`
	Ref       string      `json:"Ref"`
	KeyCode   string      `json:"KeyCode"`
	KeyState  string      `json:"KeyState"`
	FlagName  string      `json:"FlagName"`
	BranchRef string      `json:"BranchRef"`
	Outcome   string      `json:"Outcome"`
	Constant  *bool       `json:"Constant"`
}

type dtoStep3 struct {
	StepID     *string `json:"StepID"`
	ActionName *string `json:"ActionName"`
	LaneName   string  `json:"LaneName"`
	GapFrames  *int64  `json:"GapFrames"`
	HoldFrames *int64  `json:"HoldFrames"`
}

func loadFormat3(data []byte) (*Project, error) {
	var dto dtoFormat3
	if err := json.Unmarshal(data, &dto); err != nil {
		return nil, newSchemaError("$", "invalid format-3 JSON: %v", err)
	}
	version, err := requiredString(dto.SchemaVersion, "/SchemaVersion")
	if err != nil {
		return nil, err
	}
	projectID, err := requiredString(dto.ProjectID, "/ProjectID")
	if err != nil {
		return nil, err
	}
	rate, err := requiredInt(dto.FrameRate, "/FrameRate")
	if err != nil {
		return nil, err
	}
	if dto.Branches == nil {
		return nil, newSchemaError("/Branches", "required field is missing")
	}

	policy := defaultTimePolicy()
	if dto.TimePolicy != nil {
		if dto.TimePolicy.FrameToMS != "" {
			policy.FrameToMS = RoundingDirection(dto.TimePolicy.FrameToMS)
		}
		if dto.TimePolicy.MSToFrame != "" {
			policy.MSToFrame = RoundingDirection(dto.TimePolicy.MSToFrame)
		}
	}

	p := &Project{
		FormatNo:      FormatTreeFrameV3,
		FormatName:    "pascal_tree_frame",
		FormatVersion: version,
		ID:            projectID,
		Name:          dto.Name,
		FrameRate:     rate,
		TimePolicy:    policy,
		Flags:         cloneFlags(dto.Flags),
	}

	for i, bdto := range *dto.Branches {
		bp := fmt.Sprintf("/Branches/%d", i)
		id, err := requiredString(bdto.BranchID, bp+"/BranchID")
		if err != nil {
			return nil, err
		}
		macroID, err := requiredString(bdto.MacroID, bp+"/MacroID")
		if err != nil {
			return nil, err
		}
		if bdto.Condition == nil {
			return nil, newSchemaError(bp+"/Condition", "required field is missing")
		}
		window := defaultValue(bdto.TriggerWindowFrames, 1)
		if window == 0 {
			window = 1
		}
		b := &Branch{
			ID:                  id,
			MacroID:             macroID,
			Name:                bdto.Name,
			HasPriority:         bdto.Priority != nil,
			Priority:            defaultValue(bdto.Priority, 0),
			DelayFrames:         defaultValue(bdto.DelayFrames, 0),
			TriggerWindowFrames: window,
			Condition:           buildCond3(bdto.Condition),
			CancelKeys:          append([]string(nil), bdto.CancelKeys...),
		}
		if bdto.Steps == nil {
			return nil, newSchemaError(bp+"/Steps", "required field is missing")
		}
		if len(*bdto.Steps) == 0 {
			return nil, newSchemaError(bp+"/Steps", "at least one step is required")
		}
		for j, sdto := range *bdto.Steps {
			sp := fmt.Sprintf("%s/Steps/%d", bp, j)
			sid, err := requiredString(sdto.StepID, sp+"/StepID")
			if err != nil {
				return nil, err
			}
			action, err := requiredString(sdto.ActionName, sp+"/ActionName")
			if err != nil {
				return nil, err
			}
			b.Steps = append(b.Steps, &Step{
				ID:         sid,
				Action:     action,
				Lane:       sdto.LaneName,
				GapFrames:  defaultValue(sdto.GapFrames, 0),
				HoldFrames: defaultValue(sdto.HoldFrames, 0),
			})
		}
		p.Branches = append(p.Branches, b)
	}

	if err := validateProject(p); err != nil {
		return nil, err
	}
	return p, nil
}

func buildCond3(dto *dtoCond3) *Condition {
	if dto == nil {
		return nil
	}
	c := &Condition{
		Kind:     normalizeOp(dto.Type),
		Ref:      dto.Ref,
		Key:      dto.KeyCode,
		KeyState: normalizeKeyState(dto.KeyState),
		Flag:     dto.FlagName,
		BranchID: dto.BranchRef,
		Outcome:  dto.Outcome,
		Value:    dto.Constant,
	}
	for _, child := range dto.Nodes {
		c.Children = append(c.Children, buildCond3(child))
	}
	return c
}
