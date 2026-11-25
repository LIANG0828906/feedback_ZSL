package pipeline

import (
	"encoding/json"

	"skeletonretarget/internal/model"
)

func CloneDocument(doc *model.Document) *model.Document {
	if doc == nil {
		return nil
	}
	raw, _ := json.Marshal(doc)
	var out model.Document
	_ = json.Unmarshal(raw, &out)
	return &out
}

func ApplyEdits(doc *model.Document, edits []Edit) *model.Document {
	out := CloneDocument(doc)
	for _, edit := range edits {
		switch edit.Kind {
		case EditConstraint:
			if edit.Constraint == nil {
				continue
			}
			for i := range out.Skeleton.Joints {
				if out.Skeleton.Joints[i].ID == edit.Joint {
					out.Skeleton.Joints[i].Constraint = *edit.Constraint
				}
			}
		case EditMapping:
			if edit.Mapping == nil {
				continue
			}
			for i := range out.Retarget.Mappings {
				target := edit.Target
				if target == "" {
					target = edit.Mapping.Target
				}
				if out.Retarget.Mappings[i].Target == target || out.Retarget.Mappings[i].ID == edit.Mapping.ID {
					out.Retarget.Mappings[i] = *edit.Mapping
				}
			}
		case EditTopology:
			for i := range out.Skeleton.Joints {
				if out.Skeleton.Joints[i].ID == edit.Joint {
					out.Skeleton.Joints[i].Parent = edit.Parent
				}
			}
		}
	}
	return out
}
