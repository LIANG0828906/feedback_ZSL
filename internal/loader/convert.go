package loader

import (
	"fmt"

	"enginelab.local/internal/model"
)

func intervalOrDefault(v int64) int64 {
	if v > 0 {
		return v
	}
	return 10
}

func finishProject(p *model.Project) (*model.Project, error) {
	if err := validateProject(p); err != nil {
		return nil, err
	}
	for _, seg := range p.Segments {
		if seg.Integrity == "" {
			seg.Integrity = model.IntegrityComplete
		}
		if seg.CompleteRecords == 0 && seg.Integrity == model.IntegrityComplete {
			seg.CompleteRecords = len(seg.Records)
		}
		if len(seg.Records) > 0 {
			seg.FirstLSN = seg.Records[0].LSN
			seg.LastLSN = seg.Records[len(seg.Records)-1].LSN
		} else {
			seg.FirstLSN = seg.BaseLSN
			seg.LastLSN = seg.BaseLSN
		}
	}
	cps := []*model.Checkpoint{}
	for _, cp := range p.Checkpoints {
		cps = append(cps, cp)
	}
	for _, seg := range p.Segments {
		for _, rec := range seg.Records {
			if rec.Op == model.OpCheckpoint {
				found := false
				for _, cp := range cps {
					if cp.ID == rec.ID || cp.RecordID == rec.ID {
						found = true
						break
					}
				}
				if !found {
					cp := &model.Checkpoint{
						ID:        rec.ID,
						RecordID:  rec.ID,
						LSN:       rec.LSN,
						SegmentID: rec.SegmentID,
						AtMS:      rec.TimeMS,
					}
					p.Checkpoints = append(p.Checkpoints, cp)
					cps = append(cps, cp)
				}
			}
		}
	}
	if len(p.Checkpoints) > 0 {
		sorted := p.SortedCheckpoints()
		for i, cp := range sorted {
			cp.Sequence = i + 1
		}
		p.LatestCheckpoint = sorted[len(sorted)-1]
	}
	return p, nil
}

func convertV1(doc *v1Document) (*model.Project, error) {
	interval := intervalOrDefault(doc.TickIntervalMS)
	p := &model.Project{
		ID:             doc.EngineeringID,
		Seed:           doc.Seed,
		SchemaVersion:  model.SchemaV1,
		TickIntervalMS: interval,
	}
	for si, in := range doc.Segments {
		seg := &model.Segment{
			ID:             in.ID,
			Index:          in.Index,
			BaseLSN:        in.FirstLSN,
			SizeBytes:      in.SizeBytes,
			CreatedTick:    in.CreatedTick,
			CreatedMS:      in.CreatedTick * interval,
			PredecessorIDs: append([]string(nil), in.PredecessorIDs...),
			Integrity:      model.IntegrityComplete,
		}
		if seg.Index == 0 {
			seg.Index = si
		}
		for ri, r := range in.Records {
			complete := boolValue(r.Complete)
			rec := &model.Record{
				ID:            r.RID,
				SegmentID:     seg.ID,
				Index:         ri,
				LSN:           r.LSN,
				Offset:        r.LSN - in.FirstLSN,
				TransactionID: r.TxID,
				Op:            model.OpKind(r.Type),
				Key:           r.Key,
				Value:         r.Value,
				Version:       r.Version,
				Tick:          r.Tick,
				TimeMS:        r.Tick * interval,
				Complete:      complete,
			}
			if r.CheckpointID != "" {
				rec.Op = model.OpCheckpoint
			}
			seg.Records = append(seg.Records, rec)
			if complete {
				seg.CompleteRecords++
			}
			if rec.Op == model.OpCheckpoint {
				cpID := r.CheckpointID
				if cpID == "" {
					cpID = r.RID
				}
				p.Checkpoints = append(p.Checkpoints, &model.Checkpoint{
					ID:        cpID,
					RecordID:  r.RID,
					LSN:       r.LSN,
					SegmentID: seg.ID,
					AtMS:      rec.TimeMS,
				})
			}
		}
		seg.RawRecordCount = len(seg.Records)
		p.Segments = append(p.Segments, seg)
	}
	return finishProject(p)
}

func convertV2(doc *v2Document) (*model.Project, error) {
	interval := intervalOrDefault(doc.TickIntervalMS)
	p := &model.Project{
		ID:             doc.EngineeringID,
		Seed:           doc.Seed,
		SchemaVersion:  model.SchemaV2,
		TickIntervalMS: interval,
	}
	cpBySeg := map[string]*v2Checkpoint{}
	if doc.Checkpoint != nil {
		cpBySeg[doc.Checkpoint.SegmentID] = doc.Checkpoint
	}
	for si, in := range doc.Segments {
		seg := &model.Segment{
			ID:             in.ID,
			Index:          in.Index,
			BaseLSN:        in.BaseLSN,
			SizeBytes:      in.SizeKiB * KiB,
			CreatedMS:      in.CreatedMS,
			CreatedTick:    in.CreatedMS / interval,
			PredecessorIDs: append([]string(nil), in.DependsOn...),
			Integrity:      model.IntegrityComplete,
		}
		if seg.Index == 0 {
			seg.Index = si
		}
		for ri, r := range in.Entries {
			complete := boolValue(r.Complete)
			lsn := in.BaseLSN + r.Offset
			seg.Records = append(seg.Records, &model.Record{
				ID:            r.RID,
				SegmentID:     seg.ID,
				Index:         ri,
				LSN:           lsn,
				Offset:        r.Offset,
				TransactionID: r.TxID,
				Op:            model.OpKind(r.Kind),
				Key:           r.Key,
				Value:         r.Value,
				Version:       r.Version,
				TimeMS:        r.TimestampMS,
				Tick:          r.TimestampMS / interval,
				Complete:      complete,
			})
			if complete {
				seg.CompleteRecords++
			}
		}
		if cp := cpBySeg[in.ID]; cp != nil {
			lsn := uint64(0)
			hasLSN := false
			if cp.Offset != nil {
				lsn = in.BaseLSN + *cp.Offset
				hasLSN = true
			}
			if cp.LSN != nil {
				if hasLSN && lsn != *cp.LSN {
					return nil, model.NewError(model.EFieldAmbiguous,
						fmt.Sprintf("$.checkpoint.lsn"),
						"checkpoint %s offset resolves to %d but lsn is %d", cp.ID, lsn, *cp.LSN).
						WithDetails(map[string]any{"resolved_lsn": lsn, "declared_lsn": *cp.LSN})
				}
				lsn = *cp.LSN
				hasLSN = true
			}
			if !hasLSN {
				return nil, model.NewError(model.EFieldRequired, "$.checkpoint.offset", "checkpoint requires offset or lsn")
			}
			recordID := cp.RecordID
			if recordID == "" {
				recordID = cp.ID
			}
			rec := &model.Record{
				ID:        recordID,
				SegmentID: in.ID,
				Index:     len(seg.Records),
				LSN:       lsn,
				Offset:    lsn - in.BaseLSN,
				Op:        model.OpCheckpoint,
				TimeMS:    cp.AtMS,
				Tick:      cp.AtMS / interval,
				Complete:  true,
			}
			seg.Records = append(seg.Records, rec)
			seg.CompleteRecords++
			p.Checkpoints = append(p.Checkpoints, &model.Checkpoint{
				ID:        cp.ID,
				RecordID:  recordID,
				LSN:       lsn,
				SegmentID: in.ID,
				AtMS:      cp.AtMS,
			})
		}
		seg.RawRecordCount = len(seg.Records)
		p.Segments = append(p.Segments, seg)
	}
	return finishProject(p)
}

func convertV3(doc *v3Document) (*model.Project, error) {
	interval := intervalOrDefault(doc.TickIntervalMS)
	p := &model.Project{
		ID:             doc.ProjectName,
		Seed:           doc.Seed,
		SchemaVersion:  model.SchemaV3,
		TickIntervalMS: interval,
	}
	if doc.SegmentTree == nil {
		return nil, model.NewError(model.EFieldRequired, "$.segment_tree", "nested segment tree is required")
	}
	var cp *v3Checkpoint
	if doc.CheckpointMarker != nil {
		cp = doc.CheckpointMarker
	}
	var walk func(node *v3Node, parentID string) error
	walk = func(node *v3Node, parentID string) error {
		if node == nil {
			return model.NewError(model.EFieldRequired, "$.segment_tree.node", "null segment node")
		}
		seg := &model.Segment{
			ID:          node.SegmentName,
			Index:       node.SegmentNo,
			BaseLSN:     node.BaseLSN,
			SizeBytes:   node.SizeKiB * KiB,
			CreatedTick: node.BornTick,
			CreatedMS:   node.BornTick * interval,
			Integrity:   model.IntegrityComplete,
		}
		if parentID != "" {
			seg.PredecessorIDs = []string{parentID}
		}
		for ri, r := range node.Records {
			complete := boolValue(r.Complete)
			lsn := node.BaseLSN + r.SeqOffset
			seg.Records = append(seg.Records, &model.Record{
				ID:            r.RID,
				SegmentID:     seg.ID,
				Index:         ri,
				LSN:           lsn,
				Offset:        r.SeqOffset,
				TransactionID: r.Tx,
				Op:            model.OpKind(r.Op),
				Key:           r.DataKey,
				Value:         r.DataValue,
				Version:       r.DataVer,
				Tick:          r.Tick,
				TimeMS:        r.Tick * interval,
				Complete:      complete,
			})
			if complete {
				seg.CompleteRecords++
			}
		}
		if cp != nil && cp.Segment == node.SegmentName {
			lsn := node.BaseLSN + cp.Offset
			recordID := cp.RecordID
			if recordID == "" {
				recordID = cp.Name
			}
			seg.Records = append(seg.Records, &model.Record{
				ID:        recordID,
				SegmentID: seg.ID,
				Index:     len(seg.Records),
				LSN:       lsn,
				Offset:    cp.Offset,
				Op:        model.OpCheckpoint,
				Tick:      cp.AtTick,
				TimeMS:    cp.AtTick * interval,
				Complete:  true,
			})
			seg.CompleteRecords++
			p.Checkpoints = append(p.Checkpoints, &model.Checkpoint{
				ID:        cp.Name,
				RecordID:  recordID,
				LSN:       lsn,
				SegmentID: seg.ID,
				AtMS:      cp.AtTick * interval,
			})
		}
		seg.RawRecordCount = len(seg.Records)
		p.Segments = append(p.Segments, seg)
		for _, child := range node.Children {
			if err := walk(child, seg.ID); err != nil {
				return err
			}
		}
		return nil
	}
	if err := walk(doc.SegmentTree, ""); err != nil {
		return nil, err
	}
	return finishProject(p)
}
