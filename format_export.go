package macrokit

import (
	"encoding/json"
	"fmt"
)

func ExportProject(p *Project, formatNo int) ([]byte, error) {
	var doc map[string]any
	var err error
	switch formatNo {
	case FormatFlatFrameV1:
		doc, err = exportFormat1(p)
	case FormatNestedMSV2:
		doc, err = exportFormat2(p)
	case FormatTreeFrameV3:
		doc, err = exportFormat3(p)
	default:
		return nil, newSchemaError("/format", "unknown export format %d", formatNo)
	}
	if err != nil {
		return nil, err
	}
	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(b, '\n'), nil
}

func ConvertProject(p *Project, targetFormat int) (*Project, []byte, error) {
	data, err := ExportProject(p, targetFormat)
	if err != nil {
		return nil, nil, err
	}
	converted, err := LoadProjectBytes(data, p.SourcePath)
	if err != nil {
		return nil, data, err
	}
	return converted, data, nil
}

func exportFormat1(p *Project) (map[string]any, error) {
	rows := make([]map[string]any, 0)
	branches := make([]map[string]any, 0, len(p.Branches))

	for _, b := range p.SortedBranches() {
		rootID := b.ID + "_when"
		flattenCondition1(b.Condition, rootID, &rows)

		bdoc := map[string]any{
			"id":       b.ID,
			"macro_id": b.MacroID,
			"name":     b.Name,
			"when_ref": rootID,
			"steps":    exportSteps1(b),
		}
		if b.HasPriority {
			bdoc["priority"] = b.Priority
		}
		if b.DelayFrames != 0 {
			bdoc["delay_frames"] = b.DelayFrames
		}
		if b.TriggerWindowFrames != 0 {
			bdoc["trigger_window_frames"] = b.TriggerWindowFrames
		}
		if len(b.CancelKeys) > 0 {
			bdoc["cancel_keys"] = b.CancelKeys
		}
		branches = append(branches, bdoc)
	}

	return map[string]any{
		"format":          FormatFlatFrameV1,
		"schema_version":  FormatVersionV1,
		"project_id":      p.ID,
		"name":            p.Name,
		"seed":            p.Seed,
		"frame_rate":      p.FrameRate,
		"time_policy":     map[string]string{"frame_to_ms": string(p.TimePolicy.FrameToMS), "ms_to_frame": string(p.TimePolicy.MSToFrame)},
		"flags":           p.Flags,
		"condition_table": rows,
		"branches":        branches,
	}, nil
}

func flattenCondition1(c *Condition, id string, rows *[]map[string]any) {
	row := map[string]any{"id": id, "op": string(c.Kind)}
	switch c.Kind {
	case CondAll, CondAny, CondNot:
		refs := make([]string, 0, len(c.Children))
		for i, child := range c.Children {
			childID := fmt.Sprintf("%s_%d", id, i)
			refs = append(refs, childID)
			flattenCondition1(child, childID, rows)
		}
		row["refs"] = refs
	case CondKey:
		row["key"] = c.Key
		if c.KeyState != "" {
			row["state"] = string(c.KeyState)
		}
	case CondFlag:
		row["flag"] = c.Flag
	case CondBranch:
		row["branch_id"] = c.BranchID
		if c.Outcome != "" {
			row["outcome"] = c.Outcome
		}
	case CondConstant:
		if c.Value != nil {
			row["value"] = *c.Value
		}
	}
	*rows = append([]map[string]any{row}, *rows...)
}

func exportSteps1(b *Branch) []map[string]any {
	out := make([]map[string]any, 0, len(b.Steps))
	for _, s := range b.Steps {
		doc := map[string]any{"id": s.ID, "action": s.Action}
		if s.Lane != "" {
			doc["lane"] = s.Lane
		}
		if s.GapFrames != 0 {
			doc["delay_frames"] = s.GapFrames
		}
		if s.HoldFrames != 0 {
			doc["hold_frames"] = s.HoldFrames
		}
		out = append(out, doc)
	}
	return out
}

func exportFormat2(p *Project) (map[string]any, error) {
	branches := make([]map[string]any, 0, len(p.Branches))
	for _, b := range p.SortedBranches() {
		delayMS, err := framesToMSForBranch(b.ID, b.DelayFrames, p.FrameRate, p.TimePolicy.FrameToMS, "/delayMs")
		if err != nil {
			return nil, err
		}
		windowMS, err := framesToMSForBranch(b.ID, b.TriggerWindowFrames, p.FrameRate, p.TimePolicy.FrameToMS, "/triggerWindowMs")
		if err != nil {
			return nil, err
		}
		steps := make([]map[string]any, 0, len(b.Steps))
		for _, s := range b.Steps {
			gapMS, err := framesToMSForBranch(b.ID, s.GapFrames, p.FrameRate, p.TimePolicy.FrameToMS, "/steps/delayMs")
			if err != nil {
				return nil, err
			}
			holdMS, err := framesToMSForBranch(b.ID, s.HoldFrames, p.FrameRate, p.TimePolicy.FrameToMS, "/steps/holdMs")
			if err != nil {
				return nil, err
			}
			doc := map[string]any{"stepId": s.ID, "actionName": s.Action}
			if s.Lane != "" {
				doc["laneName"] = s.Lane
			}
			if gapMS != 0 {
				doc["delayMs"] = gapMS
			}
			if holdMS != 0 {
				doc["holdMs"] = holdMS
			}
			steps = append(steps, doc)
		}
		bdoc := map[string]any{
			"branchId": b.ID,
			"macroId":  b.MacroID,
			"name":     b.Name,
			"when":     condition2(b.Condition),
			"steps":    steps,
		}
		if b.HasPriority {
			bdoc["priority"] = b.Priority
		}
		if delayMS != 0 {
			bdoc["delayMs"] = delayMS
		}
		if windowMS != 0 {
			bdoc["triggerWindowMs"] = windowMS
		}
		if len(b.CancelKeys) > 0 {
			bdoc["cancelKeys"] = b.CancelKeys
		}
		branches = append(branches, bdoc)
	}
	return map[string]any{
		"format":        FormatNestedMSV2,
		"schemaVersion": FormatVersionV2,
		"projectId":     p.ID,
		"name":          p.Name,
		"seed":          p.Seed,
		"frameRate":     p.FrameRate,
		"timePolicy":    map[string]string{"frameToMS": string(p.TimePolicy.FrameToMS), "msToFrame": string(p.TimePolicy.MSToFrame)},
		"flags":         p.Flags,
		"branches":      branches,
	}, nil
}

func condition2(c *Condition) map[string]any {
	doc := map[string]any{"op": string(c.Kind)}
	if len(c.Children) > 0 {
		children := make([]map[string]any, 0, len(c.Children))
		for _, child := range c.Children {
			children = append(children, condition2(child))
		}
		doc["children"] = children
	}
	if c.Key != "" {
		doc["keyCode"] = c.Key
	}
	if c.KeyState != "" {
		doc["state"] = string(c.KeyState)
	}
	if c.Flag != "" {
		doc["flagName"] = c.Flag
	}
	if c.BranchID != "" {
		doc["branchId"] = c.BranchID
	}
	if c.Outcome != "" {
		doc["expectOutcome"] = c.Outcome
	}
	if c.Value != nil {
		doc["value"] = *c.Value
	}
	return doc
}

func exportFormat3(p *Project) (map[string]any, error) {
	branches := make([]map[string]any, 0, len(p.Branches))
	for _, b := range p.SortedBranches() {
		steps := make([]map[string]any, 0, len(b.Steps))
		for _, s := range b.Steps {
			doc := map[string]any{"StepID": s.ID, "ActionName": s.Action}
			if s.Lane != "" {
				doc["LaneName"] = s.Lane
			}
			if s.GapFrames != 0 {
				doc["GapFrames"] = s.GapFrames
			}
			if s.HoldFrames != 0 {
				doc["HoldFrames"] = s.HoldFrames
			}
			steps = append(steps, doc)
		}
		bdoc := map[string]any{
			"BranchID":  b.ID,
			"MacroID":   b.MacroID,
			"Name":      b.Name,
			"Condition": condition3(b.Condition),
			"Steps":     steps,
		}
		if b.HasPriority {
			bdoc["Priority"] = b.Priority
		}
		if b.DelayFrames != 0 {
			bdoc["DelayFrames"] = b.DelayFrames
		}
		if b.TriggerWindowFrames != 0 {
			bdoc["WindowFrames"] = b.TriggerWindowFrames
		}
		if len(b.CancelKeys) > 0 {
			bdoc["CancelKeys"] = b.CancelKeys
		}
		branches = append(branches, bdoc)
	}
	return map[string]any{
		"format":        FormatTreeFrameV3,
		"SchemaVersion": FormatVersionV3,
		"ProjectID":     p.ID,
		"Name":          p.Name,
		"Seed":          p.Seed,
		"FrameRate":     p.FrameRate,
		"TimePolicy": map[string]string{
			"FrameToMS": string(p.TimePolicy.FrameToMS),
			"MSToFrame": string(p.TimePolicy.MSToFrame),
		},
		"Flags":    p.Flags,
		"Branches": branches,
	}, nil
}

func condition3(c *Condition) map[string]any {
	doc := map[string]any{"Type": titleOp(c.Kind)}
	if c.Ref != "" {
		doc["Ref"] = c.Ref
	}
	if len(c.Children) > 0 {
		nodes := make([]map[string]any, 0, len(c.Children))
		for _, child := range c.Children {
			nodes = append(nodes, condition3(child))
		}
		doc["Nodes"] = nodes
	}
	if c.Key != "" {
		doc["KeyCode"] = c.Key
	}
	if c.KeyState != "" {
		doc["KeyState"] = titleKeyState(c.KeyState)
	}
	if c.Flag != "" {
		doc["FlagName"] = c.Flag
	}
	if c.BranchID != "" {
		doc["BranchRef"] = c.BranchID
	}
	if c.Outcome != "" {
		doc["Outcome"] = c.Outcome
	}
	if c.Value != nil {
		doc["Constant"] = *c.Value
	}
	return doc
}

func titleOp(k ConditionKind) string {
	switch k {
	case CondAll:
		return "All"
	case CondAny:
		return "Any"
	case CondNot:
		return "Not"
	case CondKey:
		return "Key"
	case CondFlag:
		return "Flag"
	case CondBranch:
		return "Branch"
	case CondConstant:
		return "Constant"
	default:
		return string(k)
	}
}

func titleKeyState(s KeyState) string {
	if s == KeyPressed {
		return "Pressed"
	}
	return "Down"
}
