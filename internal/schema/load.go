// Package schema loads and validates deterministic wardrobe documents.
package schema

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"wardrobekernel/internal/model"
	"wardrobekernel/internal/xerr"
)

// ParseProject parses and validates project JSON bytes.
func ParseProject(data []byte) (*model.Project, error) {
	var project model.Project
	if err := json.Unmarshal(data, &project); err != nil {
		return nil, &xerr.Error{
			Code:    xerr.ESchema,
			Message: "invalid project JSON",
			Cause:   err,
			Violations: []xerr.Violation{{
				Code:      xerr.ESchema,
				FieldPath: "$",
				Message:   err.Error(),
			}},
		}
	}
	if err := ValidateProject(&project); err != nil {
		return nil, err
	}
	return &project, nil
}

// LoadProject reads, parses and validates a project file.
func LoadProject(path string) (*model.Project, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, xerr.Wrap(xerr.EIO, "read project: "+path, err)
	}
	project, err := ParseProject(data)
	if err != nil {
		if coded, ok := xerr.AsError(err); ok {
			coded.Message = path + ": " + coded.Message
			return nil, coded
		}
		return nil, err
	}
	return project, nil
}

// LoadTruth reads and minimally validates a truth file.
func LoadTruth(path string) (*model.Truth, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, xerr.Wrap(xerr.EIO, "read truth: "+path, err)
	}
	var truth model.Truth
	if err := json.Unmarshal(data, &truth); err != nil {
		return nil, &xerr.Error{Code: xerr.ESchema, Message: "invalid truth JSON", Cause: err}
	}
	if len(truth.Batches) == 0 {
		return nil, &xerr.Error{Code: xerr.ESchema, Message: "truth.batches must not be empty"}
	}
	return &truth, nil
}

// LoadBundleDir loads conventional project.json and truth.json.
func LoadBundleDir(dir string) (*model.Project, *model.Truth, error) {
	project, err := LoadProject(filepath.Join(dir, "project.json"))
	if err != nil {
		return nil, nil, err
	}
	truth, err := LoadTruth(filepath.Join(dir, "truth.json"))
	if err != nil {
		return nil, nil, err
	}
	return project, truth, nil
}

// ValidateProject performs all structural checks required by acceptance v1.
func ValidateProject(p *model.Project) error {
	violations := make([]xerr.Violation, 0)
	add := func(partID, path, format string, args ...any) {
		violations = append(violations, xerr.Violation{
			Code:      xerr.ESchema,
			PartID:    partID,
			FieldPath: path,
			Message:   fmt.Sprintf(format, args...),
		})
	}

	if p.SchemaVersion == "" {
		p.SchemaVersion = model.SchemaVersion
	}
	if p.AlgorithmVersion == "" {
		p.AlgorithmVersion = model.AlgorithmVersion
	}
	if p.ConstraintVersion == "" {
		p.ConstraintVersion = model.ConstraintVersion
	}
	if p.Character.ID == "" {
		add("", "character.id", "character id is required")
	}
	if len(p.AttachPoints) == 0 {
		add("", "attach_points", "at least one attach point is required")
	}
	if len(p.Parts) == 0 {
		add("", "parts", "at least one part is required")
	}

	pointIDs := map[string]struct{}{}
	for i, point := range p.AttachPoints {
		path := fmt.Sprintf("attach_points[%d].id", i)
		if point.ID == "" {
			add("", path, "attach point id is required")
			continue
		}
		if _, exists := pointIDs[point.ID]; exists {
			add("", path, "duplicate attach point id %q", point.ID)
		}
		pointIDs[point.ID] = struct{}{}
	}

	partIDs := map[string]struct{}{}
	for i := range p.Parts {
		part := &p.Parts[i]
		base := fmt.Sprintf("parts[%d]", i)
		if part.ID == "" {
			add("", base+".id", "part id is required")
		} else if _, exists := partIDs[part.ID]; exists {
			add(part.ID, base+".id", "duplicate part id %q", part.ID)
		}
		partIDs[part.ID] = struct{}{}

		if part.AttachPointID == "" {
			add(part.ID, base+".attach_point_id", "attach_point_id is required")
		} else if _, ok := pointIDs[part.AttachPointID]; !ok {
			add(part.ID, base+".attach_point_id", "attach point %q does not exist", part.AttachPointID)
		}
		if part.Vertices <= 0 {
			add(part.ID, base+".vertices", "vertices must be a positive integer, got %d", part.Vertices)
		}
		if part.Triangles < 0 {
			add(part.ID, base+".triangles", "triangles must be non-negative, got %d", part.Triangles)
		}
		if part.ArbitrationPriority < 0 {
			add(part.ID, base+".arbitration_priority", "priority must be non-negative, got %d", part.ArbitrationPriority)
		}
		if len(part.MaterialSlots) == 0 {
			add(part.ID, base+".material_slots", "at least one material slot declaration is required")
		}
		if len(part.Submeshes) == 0 {
			add(part.ID, base+".submeshes", "at least one submesh declaration is required")
		}
		switch part.PressureAction {
		case "", model.PressureSplit, model.PressureDefer:
		default:
			add(part.ID, base+".pressure_action", "pressure_action must be split or defer, got %q", part.PressureAction)
		}
		if part.PressureAction == "" {
			part.PressureAction = model.PressureSplit
		}

		slotIDs := map[string]struct{}{}
		for j, slot := range part.MaterialSlots {
			slotPath := fmt.Sprintf("%s.material_slots[%d]", base, j)
			if slot.SlotID == "" {
				add(part.ID, slotPath+".slot_id", "material slot id is required")
			} else if _, dup := slotIDs[slot.SlotID]; dup {
				add(part.ID, slotPath+".slot_id", "duplicate material slot %q in part", slot.SlotID)
			}
			slotIDs[slot.SlotID] = struct{}{}
			if slot.MaterialVersion == "" {
				add(part.ID, slotPath+".material_version", "material_version is required for cache propagation")
			}
		}

		vertexIntervals := make([]intervalCheck, 0, len(part.Submeshes))
		indexIntervals := make([]intervalCheck, 0, len(part.Submeshes))
		for j, sm := range part.Submeshes {
			smPath := fmt.Sprintf("%s.submeshes[%d]", base, j)
			if sm.ID == "" {
				add(part.ID, smPath+".id", "submesh id is required")
			}
			if sm.MaterialSlotID == "" {
				add(part.ID, smPath+".material_slot_id", "material_slot_id is required")
			} else if len(slotIDs) > 0 {
				if _, ok := slotIDs[sm.MaterialSlotID]; !ok {
					add(part.ID, smPath+".material_slot_id", "material slot %q is not declared", sm.MaterialSlotID)
				}
			}

			if sm.VertexStart < 0 || sm.VertexCount <= 0 {
				add(part.ID, smPath+".vertex_start", "vertex interval must start >= 0 and have positive count")
			} else if sm.VertexStart+sm.VertexCount > part.Vertices {
				add(part.ID, smPath+".vertex_start", "vertex interval [%d,%d) exceeds part vertices %d",
					sm.VertexStart, sm.VertexStart+sm.VertexCount, part.Vertices)
			} else {
				vertexIntervals = append(vertexIntervals, intervalCheck{
					Start: sm.VertexStart, End: sm.VertexStart + sm.VertexCount,
					Label: sm.ID, Path: smPath + ".vertex_start",
				})
			}

			indexLimit := part.IndexCount()
			if sm.IndexStart < 0 || sm.IndexCount <= 0 {
				add(part.ID, smPath+".index_start", "index interval must start >= 0 and have positive count")
			} else if sm.IndexStart+sm.IndexCount > indexLimit {
				add(part.ID, smPath+".index_start", "index interval [%d,%d) exceeds triangle index buffer %d",
					sm.IndexStart, sm.IndexStart+sm.IndexCount, indexLimit)
			} else {
				indexIntervals = append(indexIntervals, intervalCheck{
					Start: sm.IndexStart, End: sm.IndexStart + sm.IndexCount,
					Label: sm.ID, Path: smPath + ".index_start",
				})
			}
		}
		for _, overlap := range findOverlaps(vertexIntervals) {
			add(part.ID, overlap.Path, "vertex intervals overlap: %s", overlap.Message)
		}
		for _, overlap := range findOverlaps(indexIntervals) {
			add(part.ID, overlap.Path, "index intervals overlap: %s", overlap.Message)
		}
	}

	for i, rule := range p.MutexRules {
		rulePath := fmt.Sprintf("mutex_rules[%d]", i)
		if rule.ID == "" {
			add("", rulePath+".id", "mutex rule id is required")
		}
		if len(rule.PartIDs) < 2 {
			add("", rulePath+".part_ids", "mutex rule must reference at least two parts")
		}
		for _, id := range rule.PartIDs {
			if _, ok := partIDs[id]; !ok {
				add("", rulePath+".part_ids", "mutex rule %q references unknown part %q", rule.ID, id)
			}
		}
	}

	c := p.Constraints
	if c.MaxMaterialSlots <= 0 {
		add("", "batch_constraints.max_material_slots", "must be a positive integer, got %d", c.MaxMaterialSlots)
	}
	if c.MaxVertexBudget <= 0 {
		add("", "batch_constraints.max_vertex_budget", "must be a positive integer, got %d", c.MaxVertexBudget)
	}
	if c.MaxIndexBudget <= 0 {
		add("", "batch_constraints.max_index_budget", "must be a positive integer, got %d", c.MaxIndexBudget)
	}
	if c.Arbitration.TiePolicy == "" {
		p.Constraints.Arbitration.TiePolicy = model.TieLexicographicID
	}
	switch c.Arbitration.TiePolicy {
	case model.TieLexicographicID, model.TieStrict:
	default:
		add("", "batch_constraints.arbitration.tie_policy", "unsupported tie policy %q", c.Arbitration.TiePolicy)
	}
	if c.Scheduler.MaxParallelism == 0 {
		p.Constraints.Scheduler.MaxParallelism = 2
	}
	if c.Scheduler.CandidateBufferPerWorker == 0 {
		p.Constraints.Scheduler.CandidateBufferPerWorker = 2
	}
	if c.Scheduler.PendingQueueBudget == 0 {
		p.Constraints.Scheduler.PendingQueueBudget = len(p.Parts)
	}
	if c.Scheduler.MaxParallelism <= 0 {
		add("", "batch_constraints.scheduler.max_parallelism", "must be positive")
	}
	if c.Scheduler.CandidateBufferPerWorker <= 0 {
		add("", "batch_constraints.scheduler.candidate_buffer_per_worker", "must be positive")
	}
	if c.Scheduler.PendingQueueBudget <= 0 {
		add("", "batch_constraints.scheduler.pending_queue_budget", "must be positive")
	}

	for i, id := range p.CandidateAssembly {
		if _, ok := partIDs[id]; !ok {
			add("", fmt.Sprintf("candidate_assembly[%d]", i), "unknown candidate part %q", id)
		}
	}

	if len(violations) > 0 {
		return &xerr.Error{
			Code:       xerr.ESchema,
			Message:    fmt.Sprintf("schema validation failed with %d violation(s)", len(violations)),
			Violations: violations,
		}
	}
	return nil
}

type intervalCheck struct {
	Start, End int
	Label      string
	Path       string
}

type overlapResult struct {
	Path    string
	Message string
}

func findOverlaps(intervals []intervalCheck) []overlapResult {
	sorted := append([]intervalCheck(nil), intervals...)
	sort.SliceStable(sorted, func(i, j int) bool {
		if sorted[i].Start != sorted[j].Start {
			return sorted[i].Start < sorted[j].Start
		}
		return sorted[i].End < sorted[j].End
	})
	var out []overlapResult
	for i := 1; i < len(sorted); i++ {
		prev := sorted[i-1]
		cur := sorted[i]
		if cur.Start < prev.End {
			out = append(out, overlapResult{
				Path:    cur.Path,
				Message: fmt.Sprintf("%s [%d,%d) overlaps %s [%d,%d)", prev.Label, prev.Start, prev.End, cur.Label, cur.Start, cur.End),
			})
		}
	}
	return out
}
