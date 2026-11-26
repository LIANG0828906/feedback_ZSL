package strokelab

import (
	"fmt"
	"sort"
)

type Edit struct {
	StrokeID string `json:"stroke_id"`
	Start    *Point `json:"start,omitempty"`
	End      *Point `json:"end,omitempty"`
	Hardness *int   `json:"hardness_permille,omitempty"`
	Weight   *int   `json:"weight_pp10000,omitempty"`
	Stage    *int   `json:"stage,omitempty"`
	Seq      *int   `json:"seq,omitempty"`
}

func ApplyEdit(p *Project, edit Edit) (*Project, error) {
	q := p.Clone()
	found := -1
	for i := range q.Strokes {
		if q.Strokes[i].ID == edit.StrokeID {
			found = i
			break
		}
	}
	if found < 0 {
		return nil, coded(CodeVersion, "edit references unknown stroke %q", edit.StrokeID)
	}
	s := &q.Strokes[found]
	if edit.Start != nil {
		s.Start = *edit.Start
	}
	if edit.End != nil {
		s.End = *edit.End
	}
	if edit.Hardness != nil {
		s.Hardness = *edit.Hardness
	}
	if edit.Weight != nil {
		s.Weight = *edit.Weight
	}
	if edit.Stage != nil {
		s.Stage = *edit.Stage
	}
	if edit.Seq != nil {
		s.Seq = *edit.Seq
	}
	if err := q.Validate(); err != nil {
		return nil, err
	}
	if _, err := OrderStrokes(q.Strokes); err != nil {
		return nil, err
	}
	return q, nil
}

type InvalidationEntry struct {
	Kind      string `json:"kind"`
	StrokeID  string `json:"stroke_id"`
	StrokeSeq int    `json:"stroke_seq"`
	BlockID   int    `json:"block_id"`
	Reason    string `json:"reason"`
	Field     string `json:"field,omitempty"`
}

type InvalidationPlan struct {
	StrokeID      string              `json:"stroke_id"`
	ChangedFields []string            `json:"changed_fields"`
	Affected      []int               `json:"affected_blocks"`
	Entries       []InvalidationEntry `json:"entries"`
	Downstream    []string            `json:"downstream"`
	Dependencies  *DependencyGraph    `json:"dependencies,omitempty"`
}

func (p *InvalidationPlan) Contains(blockID int) bool {
	for _, id := range p.Affected {
		if id == blockID {
			return true
		}
	}
	return false
}

// PlanInvalidation identifies only blocks whose local inputs or local ordering
// changed. A stage/seq edit invalidates a block only when the canonical order
// of that block's participants actually flips.
func PlanInvalidation(oldProject, newProject *Project, edit Edit) (*InvalidationPlan, error) {
	oldOrdered, err := OrderStrokes(oldProject.Strokes)
	if err != nil {
		return nil, err
	}
	newOrdered, err := OrderStrokes(newProject.Strokes)
	if err != nil {
		return nil, err
	}
	var oldStroke, newStroke *Stroke
	for i := range oldProject.Strokes {
		if oldProject.Strokes[i].ID == edit.StrokeID {
			oldStroke = &oldProject.Strokes[i]
			break
		}
	}
	for i := range newProject.Strokes {
		if newProject.Strokes[i].ID == edit.StrokeID {
			newStroke = &newProject.Strokes[i]
			break
		}
	}
	if oldStroke == nil || newStroke == nil {
		return nil, coded(CodeVersion, "edit stroke %q not found", edit.StrokeID)
	}
	blocks := BuildBlocks(oldProject.Width, oldProject.Height, oldProject.BlockSize)
	plan := &InvalidationPlan{
		StrokeID:     edit.StrokeID,
		Dependencies: BuildDependencyGraph(newProject, newOrdered, blocks),
	}
	var changed []string
	if oldStroke.Start != newStroke.Start {
		changed = append(changed, "start")
	}
	if oldStroke.End != newStroke.End {
		changed = append(changed, "end")
	}
	if oldStroke.Hardness != newStroke.Hardness {
		changed = append(changed, "hardness")
	}
	if oldStroke.Weight != newStroke.Weight {
		changed = append(changed, "weight")
	}
	if oldStroke.Stage != newStroke.Stage {
		changed = append(changed, "stage")
	}
	if oldStroke.Seq != newStroke.Seq {
		changed = append(changed, "seq")
	}
	plan.ChangedFields = changed
	if len(changed) == 0 {
		return plan, nil
	}
	affected := make(map[int]string)
	geometryChanged := oldStroke.Start != newStroke.Start || oldStroke.End != newStroke.End ||
		oldStroke.Hardness != newStroke.Hardness || oldStroke.Weight != newStroke.Weight
	if geometryChanged {
		reason := "stroke_geometry_or_brush"
		for _, id := range blocksForRect(blocks, oldStroke.Bounds()) {
			affected[id] = reason
		}
		for _, id := range blocksForRect(blocks, newStroke.Bounds()) {
			affected[id] = reason
		}
	}
	if oldStroke.Stage != newStroke.Stage || oldStroke.Seq != newStroke.Seq {
		for _, b := range blocks {
			oldParts := participantsForBlock(oldOrdered, b.Rect)
			newParts := participantsForBlock(newOrdered, b.Rect)
			if LocalOrderVersion(oldParts) != LocalOrderVersion(newParts) {
				affected[b.ID] = "composition_order_flip"
			}
		}
	}
	// Cache-key comparison is a final safety net for edits that changed a
	// participating stroke's content but evaded the footprint shortcut.
	oldBlockMap := blockByID(blocks)
	for _, b := range blocks {
		oldKey := ComputeBlockCacheKey(oldProject, b.Rect, participantsForBlock(oldOrdered, b.Rect))
		newKey := ComputeBlockCacheKey(newProject, b.Rect, participantsForBlock(newOrdered, b.Rect))
		if oldKey != newKey {
			if _, ok := affected[b.ID]; !ok {
				affected[b.ID] = "cache_key_changed"
			}
		}
	}
	plan.Affected = sortedBlockIDs(affected)
	strokeSeq := 0
	for _, os := range newOrdered {
		if os.Stroke.ID == edit.StrokeID {
			strokeSeq = os.Position
			break
		}
	}
	for _, id := range plan.Affected {
		reason := affected[id]
		plan.Entries = append(plan.Entries, InvalidationEntry{
			Kind: "block", StrokeID: edit.StrokeID, StrokeSeq: strokeSeq,
			BlockID: id, Reason: reason, Field: joinFields(changed),
		})
	}
	sort.SliceStable(plan.Entries, func(i, j int) bool {
		if plan.Entries[i].BlockID != plan.Entries[j].BlockID {
			return plan.Entries[i].BlockID < plan.Entries[j].BlockID
		}
		return plan.Entries[i].Reason < plan.Entries[j].Reason
	})
	if len(plan.Affected) > 0 {
		plan.Downstream = []string{"topology:verify", "attribution:recompute", "diagnostic:refresh"}
		for _, id := range plan.Affected {
			_ = oldBlockMap[id]
		}
	}
	return plan, nil
}

func joinFields(fields []string) string {
	if len(fields) == 0 {
		return ""
	}
	out := fields[0]
	for _, f := range fields[1:] {
		out += "+" + f
	}
	return out
}

func SummarizeInvalidation(plan *InvalidationPlan) string {
	if plan == nil || len(plan.Entries) == 0 {
		return "no invalidated blocks"
	}
	return fmt.Sprintf("%d block(s) invalidated by %s (%s)",
		len(plan.Affected), plan.StrokeID, joinFields(plan.ChangedFields))
}
