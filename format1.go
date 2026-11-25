package macrokit

import (
	"encoding/json"
	"fmt"
)

type dtoTimePolicy struct {
	FrameToMS      *string `json:"frame_to_ms"`
	MSToFrame      *string `json:"ms_to_frame"`
	FrameToMSCamel *string `json:"frameToMS"`
	MSToFrameCamel *string `json:"msToFrame"`
}

type dtoFormat1 struct {
	Format         *int            `json:"format"`
	SchemaVersion  *string         `json:"schema_version"`
	ProjectID      *string         `json:"project_id"`
	Name           string          `json:"name"`
	FrameRate      *int            `json:"frame_rate"`
	TimePolicy     *dtoTimePolicy  `json:"time_policy"`
	Flags          map[string]bool `json:"flags"`
	ConditionTable []dtoFlatRow1   `json:"condition_table"`
	Branches       []dtoBranch1    `json:"branches"`
}

type dtoFlatRow1 struct {
	ID       *string  `json:"id"`
	Op       *string  `json:"op"`
	Refs     []string `json:"refs"`
	Key      string   `json:"key"`
	State    string   `json:"state"`
	Flag     string   `json:"flag"`
	BranchID string   `json:"branch_id"`
	Outcome  string   `json:"outcome"`
	Value    *bool    `json:"value"`
}

type dtoBranch1 struct {
	ID                  *string     `json:"id"`
	MacroID             *string     `json:"macro_id"`
	Name                string      `json:"name"`
	WhenRef             *string     `json:"when_ref"`
	Priority            *int        `json:"priority"`
	DelayFrames         *int64      `json:"delay_frames"`
	TriggerWindowFrames *int64      `json:"trigger_window_frames"`
	CancelKeys          []string    `json:"cancel_keys"`
	Steps               *[]dtoStep1 `json:"steps"`
}

type dtoStep1 struct {
	ID          *string `json:"id"`
	Action      *string `json:"action"`
	Lane        string  `json:"lane"`
	DelayFrames *int64  `json:"delay_frames"`
	GapFrames   *int64  `json:"gap_frames"`
	HoldFrames  *int64  `json:"hold_frames"`
}

func loadFormat1(data []byte) (*Project, error) {
	var dto dtoFormat1
	if err := json.Unmarshal(data, &dto); err != nil {
		return nil, newSchemaError("$", "invalid format-1 JSON: %v", err)
	}

	version, err := requiredString(dto.SchemaVersion, "/schema_version")
	if err != nil {
		return nil, err
	}
	projectID, err := requiredString(dto.ProjectID, "/project_id")
	if err != nil {
		return nil, err
	}
	rate, err := requiredInt(dto.FrameRate, "/frame_rate")
	if err != nil {
		return nil, err
	}

	rows := make(map[string]dtoFlatRow1, len(dto.ConditionTable))
	rowIndex := make(map[string]int, len(dto.ConditionTable))
	for i, row := range dto.ConditionTable {
		id, err := requiredString(row.ID, fmt.Sprintf("/condition_table/%d/id", i))
		if err != nil {
			return nil, err
		}
		if _, exists := rows[id]; exists {
			return nil, newSchemaError(fmt.Sprintf("/condition_table/%d/id", i), "duplicate condition row %q", id)
		}
		if row.Op == nil || *row.Op == "" {
			return nil, newSchemaError(fmt.Sprintf("/condition_table/%d/op", i), "required field is missing")
		}
		rows[id] = row
		rowIndex[id] = i
	}

	policy := defaultTimePolicy()
	if dto.TimePolicy != nil {
		applyTimePolicyDTO(&policy, dto.TimePolicy)
	}

	p := &Project{
		FormatNo:      FormatFlatFrameV1,
		FormatName:    "flat_const_frame",
		FormatVersion: version,
		ID:            projectID,
		Name:          dto.Name,
		FrameRate:     rate,
		TimePolicy:    policy,
		Flags:         cloneFlags(dto.Flags),
	}

	for i, bdto := range dto.Branches {
		bp := fmt.Sprintf("/branches/%d", i)
		id, err := requiredString(bdto.ID, bp+"/id")
		if err != nil {
			return nil, err
		}
		macroID, err := requiredString(bdto.MacroID, bp+"/macro_id")
		if err != nil {
			return nil, err
		}
		whenRef, err := requiredString(bdto.WhenRef, bp+"/when_ref")
		if err != nil {
			return nil, err
		}
		if _, ok := rows[whenRef]; !ok {
			return nil, newRefError(bp+"/when_ref", whenRef, "condition reference is out of bounds")
		}

		cond, err := resolveFlatRef(whenRef, rows, rowIndex, map[string]bool{})
		if err != nil {
			return nil, err
		}

		b := &Branch{
			ID:                  id,
			MacroID:             macroID,
			Name:                bdto.Name,
			HasPriority:         bdto.Priority != nil,
			Priority:            defaultValue(bdto.Priority, 0),
			DelayFrames:         defaultValue(bdto.DelayFrames, 0),
			TriggerWindowFrames: defaultValue(bdto.TriggerWindowFrames, 1),
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
			sid, err := requiredString(sdto.ID, sp+"/id")
			if err != nil {
				return nil, err
			}
			action, err := requiredString(sdto.Action, sp+"/action")
			if err != nil {
				return nil, err
			}
			gap := int64(0)
			if sdto.GapFrames != nil {
				gap = *sdto.GapFrames
			} else if sdto.DelayFrames != nil {
				gap = *sdto.DelayFrames
			}
			b.Steps = append(b.Steps, &Step{
				ID:         sid,
				Action:     action,
				Lane:       sdto.Lane,
				GapFrames:  gap,
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

func resolveFlatRef(id string, rows map[string]dtoFlatRow1, indexes map[string]int, visiting map[string]bool) (*Condition, error) {
	if visiting[id] {
		idx := indexes[id]
		return nil, newRefError(fmt.Sprintf("/condition_table/%d/refs", idx), id, "cyclic condition reference")
	}
	row, ok := rows[id]
	if !ok {
		idx := indexes[id]
		return nil, newRefError(fmt.Sprintf("/condition_table/%d/refs", idx), id, "condition reference is out of bounds")
	}
	visiting[id] = true
	defer delete(visiting, id)

	idx := indexes[id]
	path := fmt.Sprintf("/condition_table/%d", idx)
	kind := normalizeOp(*row.Op)
	cond := &Condition{
		Kind:     kind,
		Ref:      id,
		Key:      row.Key,
		KeyState: normalizeKeyState(row.State),
		Flag:     row.Flag,
		BranchID: row.BranchID,
		Outcome:  row.Outcome,
		Value:    row.Value,
	}

	switch kind {
	case CondAll, CondAny:
		if len(row.Refs) == 0 {
			return nil, newSchemaError(path+"/refs", "%s row requires refs", kind)
		}
		for j, ref := range row.Refs {
			child, err := resolveFlatRef(ref, rows, indexes, visiting)
			if err != nil {
				if domainErr, ok := AsError(err); ok && domainErr.Path == "" {
					domainErr.Path = fmt.Sprintf("%s/refs/%d", path, j)
				}
				return nil, err
			}
			cond.Children = append(cond.Children, child)
		}
	case CondNot:
		if len(row.Refs) != 1 {
			return nil, newSchemaError(path+"/refs", "not row requires exactly one ref")
		}
		child, err := resolveFlatRef(row.Refs[0], rows, indexes, visiting)
		if err != nil {
			return nil, err
		}
		cond.Children = []*Condition{child}
	case CondKey, CondFlag, CondBranch, CondConstant:
		// Field validation happens in validateCondition.
	default:
		return nil, newSchemaError(path+"/op", "unsupported op %q", *row.Op)
	}
	return cond, nil
}
