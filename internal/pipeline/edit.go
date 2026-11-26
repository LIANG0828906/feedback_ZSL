package pipeline

import (
	"encoding/json"
	"fmt"

	"wardrobekernel/internal/model"
)

// CloneProject deep-copies a project so incremental edits never alias samples.
func CloneProject(project *model.Project) (*model.Project, error) {
	data, err := json.Marshal(project)
	if err != nil {
		return nil, err
	}
	var out model.Project
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ClonePlan deep-copies a plan for mismatch diagnostics and negative tests.
func ClonePlan(plan *model.Plan) (*model.Plan, error) {
	data, err := json.Marshal(plan)
	if err != nil {
		return nil, err
	}
	var out model.Plan
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// EditMaterialVersion changes one declared material slot version.
func EditMaterialVersion(project *model.Project, partID, slotID, newVersion string) error {
	for i := range project.Parts {
		part := &project.Parts[i]
		if part.ID != partID {
			continue
		}
		for j := range part.MaterialSlots {
			slot := &part.MaterialSlots[j]
			if slotID == "" || slot.SlotID == slotID {
				slot.MaterialVersion = newVersion
				return nil
			}
		}
		return fmt.Errorf("part %s slot %s not found", partID, slotID)
	}
	return fmt.Errorf("part %s not found", partID)
}

// EditGeometryScale changes the declared vertex scale/budget footprint while
// leaving submesh intervals valid; a positive delta models more vertices.
func EditGeometryScale(project *model.Project, partID string, vertexDelta int) error {
	for i := range project.Parts {
		part := &project.Parts[i]
		if part.ID != partID {
			continue
		}
		if part.Vertices+vertexDelta <= 0 {
			return fmt.Errorf("vertex delta %d makes part %s non-positive", vertexDelta, partID)
		}
		part.Vertices += vertexDelta
		return nil
	}
	return fmt.Errorf("part %s not found", partID)
}
