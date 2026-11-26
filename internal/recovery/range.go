package recovery

import "enginelab.local/internal/model"

func usableRecordCount(seg *model.Segment) int {
	count := 0
	for _, rec := range seg.Records {
		if !rec.Complete {
			break
		}
		count++
	}
	return count
}

func pointFromRecord(rec *model.Record) model.Point {
	return model.Point{
		SegmentID: rec.SegmentID,
		RecordID:  rec.ID,
		LSN:       rec.LSN,
		Offset:    rec.Offset,
		Index:     rec.Index,
		TimeMS:    rec.TimeMS,
		Tick:      rec.Tick,
	}
}

func BuildRanges(p *model.Project) map[string]model.ReplayRange {
	var cpLSN uint64
	if cp := p.LatestCheckpoint; cp != nil {
		cpLSN = cp.LSN
	}
	out := map[string]model.ReplayRange{}
	for _, seg := range p.SortedSegments() {
		usable := usableRecordCount(seg)
		start := 0
		for start < usable {
			lsn := seg.Records[start].LSN
			if cpLSN != 0 && lsn <= cpLSN {
				start++
				continue
			}
			break
		}
		rng := model.ReplayRange{SegmentID: seg.ID}
		if start < usable {
			first := seg.Records[start]
			last := seg.Records[usable-1]
			rng.Replay = true
			rng.Reason = "after_checkpoint"
			rng.Start = pointFromRecord(first)
			rng.End = pointFromRecord(last)
			rng.StartLSN = first.LSN
			rng.EndLSN = last.LSN
			rng.StartIndex = start
			rng.EndIndex = usable - 1
			rng.RecordCount = usable - start
		} else {
			rng.Replay = false
			rng.Reason = "at_or_before_checkpoint"
			rng.StartIndex = usable
			rng.EndIndex = usable - 1
			if usable > 0 {
				last := seg.Records[usable-1]
				rng.Start = pointFromRecord(last)
				rng.End = pointFromRecord(last)
				rng.StartLSN = last.LSN
				rng.EndLSN = last.LSN
			}
		}
		out[seg.ID] = rng
	}
	return out
}
