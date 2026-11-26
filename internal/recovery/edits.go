package recovery

import (
	"encoding/json"
	"fmt"

	"enginelab.local/internal/model"
)

const (
	EditTruncateSegment   = "truncate_segment"
	EditOverwriteRecord   = "overwrite_record"
	EditAddCheckpoint     = "add_checkpoint"
	EditRewriteDependency = "rewrite_dependency"
)

type Edit struct {
	ID              string   `json:"id"`
	Type            string   `json:"type"`
	Description     string   `json:"description,omitempty"`
	SegmentID       string   `json:"segment_id,omitempty"`
	RecordID        string   `json:"record_id,omitempty"`
	ToRecordCount   int      `json:"to_record_count,omitempty"`
	NewValue        string   `json:"new_value,omitempty"`
	NewVersion      int64    `json:"new_version,omitempty"`
	CheckpointID    string   `json:"checkpoint_id,omitempty"`
	NewPredecessors []string `json:"new_predecessors,omitempty"`
}

func CloneProject(p *model.Project) (*model.Project, error) {
	data, err := json.Marshal(p)
	if err != nil {
		return nil, model.WrapError(model.EStateConflict, "$.clone", err)
	}
	var out model.Project
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, model.WrapError(model.EStateConflict, "$.clone", err)
	}
	return &out, nil
}

func ApplyEdit(p *model.Project, edit Edit) (*model.Checkpoint, error) {
	switch edit.Type {
	case EditTruncateSegment:
		return nil, applyTruncate(p, edit)
	case EditOverwriteRecord:
		return nil, applyOverwrite(p, edit)
	case EditAddCheckpoint:
		return applyAddCheckpoint(p, edit)
	case EditRewriteDependency:
		return nil, applyRewrite(p, edit)
	default:
		return nil, model.NewError(model.EStateConflict, "$.edit.type", "unknown edit type %q", edit.Type)
	}
}

func applyTruncate(p *model.Project, edit Edit) error {
	seg := p.Segment(edit.SegmentID)
	if seg == nil {
		return model.NewError(model.EFieldRequired, edit.SegmentID, "segment not found")
	}
	count := edit.ToRecordCount
	if count < 0 || count > len(seg.Records) {
		return model.NewError(model.EFieldRequired, edit.SegmentID+".to_record_count",
			"count %d outside [0,%d]", count, len(seg.Records))
	}
	oldCount := len(seg.Records)
	seg.Records = seg.Records[:count]
	seg.CompleteRecords = count
	seg.RawRecordCount = oldCount
	seg.Integrity = model.IntegrityDegraded
	seg.Diagnostics = append(seg.Diagnostics, model.Diagnostic{
		Code:     model.ESegmentTruncated,
		Severity: "warning",
		Locator:  fmt.Sprintf("%s.record[%d:%d]", seg.ID, count, oldCount),
		Message:  "segment logically truncated; tail records are no longer replayed",
		Details:  map[string]any{"old_count": oldCount, "new_count": count},
	})
	if count > 0 {
		seg.LastLSN = seg.Records[count-1].LSN
	} else {
		seg.LastLSN = seg.BaseLSN
	}
	removeCheckpointsForMissingRecords(p, seg)
	return nil
}

func removeCheckpointsForMissingRecords(p *model.Project, seg *model.Segment) {
	alive := map[string]bool{}
	for _, rec := range seg.Records {
		alive[rec.ID] = true
	}
	kept := p.Checkpoints[:0]
	for _, cp := range p.Checkpoints {
		if cp.SegmentID == seg.ID && cp.RecordID != "" && !alive[cp.RecordID] {
			continue
		}
		kept = append(kept, cp)
	}
	p.Checkpoints = kept
	if len(p.Checkpoints) > 0 {
		p.LatestCheckpoint = p.SortedCheckpoints()[len(p.SortedCheckpoints())-1]
	} else {
		p.LatestCheckpoint = nil
	}
}

func applyOverwrite(p *model.Project, edit Edit) error {
	for _, seg := range p.Segments {
		for _, rec := range seg.Records {
			if rec.ID == edit.RecordID {
				if edit.NewValue != "" {
					rec.Value = edit.NewValue
				}
				if edit.NewVersion != 0 {
					rec.Version = edit.NewVersion
				}
				seg.Diagnostics = append(seg.Diagnostics, model.Diagnostic{
					Code:     model.EOk,
					Severity: "info",
					Locator:  edit.RecordID,
					Message:  "record payload overwritten in place",
					Details:  map[string]any{"edit_id": edit.ID},
				})
				return nil
			}
		}
	}
	return model.NewError(model.EFieldRequired, edit.RecordID, "record not found for overwrite")
}

func applyAddCheckpoint(p *model.Project, edit Edit) (*model.Checkpoint, error) {
	seg := p.Segment(edit.SegmentID)
	if seg == nil {
		return nil, model.NewError(model.EFieldRequired, edit.SegmentID, "checkpoint segment not found")
	}
	cpID := edit.CheckpointID
	if cpID == "" {
		cpID = fmt.Sprintf("cp-%d", len(p.Checkpoints)+1)
	}
	for _, cp := range p.Checkpoints {
		if cp.ID == cpID {
			return nil, model.NewError(model.EDupRecord, "$.checkpoints", "checkpoint %s already exists", cpID)
		}
	}
	if len(seg.Records) == 0 {
		return nil, model.NewError(model.EStateConflict, edit.SegmentID, "cannot checkpoint an empty segment")
	}
	anchor := seg.Records[len(seg.Records)-1]
	cp := &model.Checkpoint{
		ID:        cpID,
		RecordID:  anchor.ID,
		LSN:       anchor.LSN,
		SegmentID: seg.ID,
		AtMS:      anchor.TimeMS,
		Sequence:  len(p.Checkpoints) + 1,
	}
	p.Checkpoints = append(p.Checkpoints, cp)
	p.LatestCheckpoint = p.SortedCheckpoints()[len(p.SortedCheckpoints())-1]
	return cp, nil
}

func applyRewrite(p *model.Project, edit Edit) error {
	seg := p.Segment(edit.SegmentID)
	if seg == nil {
		return model.NewError(model.EFieldRequired, edit.SegmentID, "dependency segment not found")
	}
	seen := map[string]bool{}
	for i, pred := range edit.NewPredecessors {
		if pred == "" {
			return model.NewError(model.EFieldRequired, fmt.Sprintf("%s.new_predecessors[%d]", seg.ID, i), "empty predecessor")
		}
		if seen[pred] {
			return model.NewError(model.EDupRef, fmt.Sprintf("%s.new_predecessors[%d]", seg.ID, i), "duplicate predecessor %q", pred)
		}
		seen[pred] = true
	}
	seg.PredecessorIDs = append([]string(nil), edit.NewPredecessors...)
	return nil
}

func FullSegmentRange(seg *model.Segment) model.ReplayRange {
	usable := usableRecordCount(seg)
	rng := model.ReplayRange{SegmentID: seg.ID, Replay: true, Reason: "cache_local_full"}
	if usable > 0 {
		first := seg.Records[0]
		last := seg.Records[usable-1]
		rng.Start = pointFromRecord(first)
		rng.End = pointFromRecord(last)
		rng.StartLSN = first.LSN
		rng.EndLSN = last.LSN
		rng.StartIndex = 0
		rng.EndIndex = usable - 1
		rng.RecordCount = usable
	} else {
		rng.StartIndex = 0
		rng.EndIndex = -1
	}
	return rng
}
