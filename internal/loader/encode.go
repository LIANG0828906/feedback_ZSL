package loader

import (
	"encoding/json"

	"enginelab.local/internal/model"
)

func Encode(p *model.Project, version model.SchemaVersion) ([]byte, error) {
	switch version {
	case model.SchemaV1:
		return EncodeV1(p)
	case model.SchemaV2:
		return EncodeV2(p)
	case model.SchemaV3:
		return EncodeV3(p)
	default:
		return nil, model.NewError(model.ESchemaUnsupported, "$.schema", "unsupported schema %q", version)
	}
}

func jsonMarshal(v any) ([]byte, error) {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return nil, model.WrapError(model.EFieldRequired, "$", err)
	}
	return data, nil
}

func EncodeV1(p *model.Project) ([]byte, error) {
	doc := v1Document{
		Schema:         model.SchemaV1,
		EngineeringID:  p.ID,
		Seed:           p.Seed,
		TickIntervalMS: p.TickIntervalMS,
		TimeBasis:      "tick",
		SizeBasis:      "byte",
		SequenceBasis:  "absolute_lsn",
	}
	cpByRecord := map[string]*model.Checkpoint{}
	for _, cp := range p.Checkpoints {
		if cp.RecordID != "" {
			cpByRecord[cp.RecordID] = cp
		}
	}
	for _, seg := range p.SortedSegments() {
		out := v1Segment{
			ID:             seg.ID,
			Index:          seg.Index,
			FirstLSN:       seg.BaseLSN,
			SizeBytes:      seg.SizeBytes,
			CreatedTick:    seg.CreatedTick,
			PredecessorIDs: append([]string(nil), seg.PredecessorIDs...),
		}
		for _, r := range seg.Records {
			rr := v1Record{
				RID:     r.ID,
				LSN:     r.LSN,
				TxID:    r.TransactionID,
				Type:    string(r.Op),
				Key:     r.Key,
				Value:   r.Value,
				Version: r.Version,
				Tick:    r.Tick,
			}
			if !r.Complete {
				complete := false
				rr.Complete = &complete
			}
			if cp := cpByRecord[r.ID]; cp != nil {
				rr.CheckpointID = cp.ID
			}
			out.Records = append(out.Records, rr)
		}
		doc.Segments = append(doc.Segments, out)
	}
	return jsonMarshal(doc)
}

func EncodeV2(p *model.Project) ([]byte, error) {
	doc := v2Document{
		Schema:         model.SchemaV2,
		EngineeringID:  p.ID,
		Seed:           p.Seed,
		TickIntervalMS: p.TickIntervalMS,
		SequenceBasis:  "segment_offset",
		TimeUnit:       "millisecond",
		SizeUnit:       "kibibyte",
	}
	latest := p.LatestCheckpoint
	for _, seg := range p.SortedSegments() {
		if seg.SizeBytes%KiB != 0 {
			return nil, model.NewError(model.EFieldAmbiguous, seg.ID+".sizeKiB", "size %d is not KiB aligned", seg.SizeBytes)
		}
		out := v2Segment{
			ID:        seg.ID,
			Index:     seg.Index,
			BaseLSN:   seg.BaseLSN,
			SizeKiB:   seg.SizeBytes / KiB,
			CreatedMS: seg.CreatedMS,
			DependsOn: append([]string(nil), seg.PredecessorIDs...),
		}
		for _, r := range seg.Records {
			if r.Op == model.OpCheckpoint && latest != nil && r.ID == latest.RecordID {
				continue
			}
			entry := v2Entry{
				RID:         r.ID,
				Offset:      r.Offset,
				TxID:        r.TransactionID,
				Kind:        string(r.Op),
				Key:         r.Key,
				Value:       r.Value,
				Version:     r.Version,
				TimestampMS: r.TimeMS,
			}
			if !r.Complete {
				complete := false
				entry.Complete = &complete
			}
			out.Entries = append(out.Entries, entry)
		}
		doc.Segments = append(doc.Segments, out)
	}
	if latest != nil {
		seg := p.Segment(latest.SegmentID)
		if seg == nil {
			return nil, model.NewError(model.EDepMissing, "$.checkpoint.segmentId", "checkpoint segment %q missing", latest.SegmentID)
		}
		offset := latest.LSN - seg.BaseLSN
		lsn := latest.LSN
		doc.Checkpoint = &v2Checkpoint{
			ID:        latest.ID,
			RecordID:  latest.RecordID,
			SegmentID: latest.SegmentID,
			Offset:    &offset,
			LSN:       &lsn,
			AtMS:      latest.AtMS,
		}
	}
	return jsonMarshal(doc)
}

func EncodeV3(p *model.Project) ([]byte, error) {
	doc := v3Document{
		SchemaVersion:  model.SchemaV3,
		ProjectName:    p.ID,
		Seed:           p.Seed,
		TickIntervalMS: p.TickIntervalMS,
		SequenceKind:   "relative_offset",
		TimeUnit:       "tick",
		SizeUnit:       "kib",
	}
	byID := map[string]*v3Node{}
	children := map[string][]*v3Node{}
	var roots []*v3Node
	for _, seg := range p.SortedSegments() {
		if len(seg.PredecessorIDs) > 1 {
			return nil, model.NewError(model.EFieldAmbiguous, seg.ID+".predecessor_ids", "schema v3 nested tree supports one parent")
		}
		if seg.SizeBytes%KiB != 0 {
			return nil, model.NewError(model.EFieldAmbiguous, seg.ID+".size_kib", "size %d is not KiB aligned", seg.SizeBytes)
		}
		node := &v3Node{
			SegmentName: seg.ID,
			SegmentNo:   seg.Index,
			BaseLSN:     seg.BaseLSN,
			SizeKiB:     seg.SizeBytes / KiB,
			BornTick:    seg.CreatedTick,
		}
		latest := p.LatestCheckpoint
		for _, r := range seg.Records {
			if r.Op == model.OpCheckpoint && latest != nil && r.ID == latest.RecordID {
				continue
			}
			rec := v3Record{
				SeqOffset: r.Offset,
				RID:       r.ID,
				Tx:        r.TransactionID,
				Op:        string(r.Op),
				DataKey:   r.Key,
				DataValue: r.Value,
				DataVer:   r.Version,
				Tick:      r.Tick,
			}
			if !r.Complete {
				complete := false
				rec.Complete = &complete
			}
			node.Records = append(node.Records, rec)
		}
		byID[seg.ID] = node
		pred := ""
		if len(seg.PredecessorIDs) == 1 {
			pred = seg.PredecessorIDs[0]
		}
		if pred == "" {
			roots = append(roots, node)
		} else {
			children[pred] = append(children[pred], node)
		}
	}
	for id, kids := range children {
		parent := byID[id]
		if parent == nil {
			return nil, model.NewError(model.EDepMissing, id, "missing tree parent")
		}
		parent.Children = kids
	}
	if len(roots) != 1 {
		return nil, model.NewError(model.EFieldAmbiguous, "$.segment_tree", "schema v3 requires exactly one root, got %d", len(roots))
	}
	doc.SegmentTree = roots[0]
	if cp := p.LatestCheckpoint; cp != nil {
		seg := p.Segment(cp.SegmentID)
		if seg == nil {
			return nil, model.NewError(model.EDepMissing, "$.checkpoint_marker.at_segment", "missing checkpoint segment")
		}
		doc.CheckpointMarker = &v3Checkpoint{
			Name:     cp.ID,
			RecordID: cp.RecordID,
			Segment:  cp.SegmentID,
			Offset:   cp.LSN - seg.BaseLSN,
			AtTick:   cp.AtMS / p.TickIntervalMS,
		}
	}
	return jsonMarshal(doc)
}
