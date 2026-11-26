package gen

import (
	"sort"

	"enginelab.local/internal/model"
)

type Tolerance = model.FixedTolerance

type Truth struct {
	Seed                int64                        `json:"seed"`
	Items               []model.DataItem             `json:"items"`
	ItemMap             map[string]model.DataItem    `json:"item_map"`
	CheckpointItems     map[string]model.DataItem    `json:"checkpoint_items"`
	VisibleRecordIDs    []string                     `json:"visible_record_ids"`
	CommittedTxIDs      []string                     `json:"committed_tx_ids"`
	AbortedTxIDs        []string                     `json:"aborted_tx_ids"`
	UncommittedTxIDs    []string                     `json:"uncommitted_tx_ids"`
	Ranges              map[string]model.ReplayRange `json:"ranges"`
	Tolerance           Tolerance                    `json:"tolerance"`
	LatestCheckpointLSN uint64                       `json:"latest_checkpoint_lsn"`
}

func BuildTruth(p *model.Project) *Truth {
	all := make([]*model.Record, 0)
	for _, seg := range p.Segments {
		all = append(all, seg.Records...)
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].LSN < all[j].LSN })

	state := map[string]string{}
	buffered := map[string][]model.Write{}
	items := map[string]model.DataItem{}
	checkpointItems := map[string]model.DataItem{}
	committed := map[string]bool{}
	aborted := map[string]bool{}

	var cpLSN uint64
	if cp := p.LatestCheckpoint; cp != nil {
		cpLSN = cp.LSN
	}

	for _, rec := range all {
		if rec.Op == model.OpCheckpoint || rec.TransactionID == "" {
			continue
		}
		tx := rec.TransactionID
		switch rec.Op {
		case model.OpBegin:
			state[tx] = "open"
		case model.OpPut, model.OpDelete:
			state[tx] = "open"
			buffered[tx] = append(buffered[tx], model.Write{
				Key:       rec.Key,
				Value:     rec.Value,
				Version:   rec.Version,
				LSN:       rec.LSN,
				TxID:      tx,
				SegmentID: rec.SegmentID,
				Deleted:   rec.Op == model.OpDelete,
			})
		case model.OpCommit:
			state[tx] = "committed"
			committed[tx] = true
			for _, w := range buffered[tx] {
				item := model.DataItem{
					Key:       w.Key,
					Value:     w.Value,
					Version:   w.Version,
					LSN:       rec.LSN,
					TxID:      tx,
					SegmentID: w.SegmentID,
					Deleted:   w.Deleted,
				}
				if w.Deleted {
					delete(items, w.Key)
				} else {
					items[w.Key] = item
				}
				if cpLSN != 0 && rec.LSN <= cpLSN {
					if w.Deleted {
						delete(checkpointItems, w.Key)
					} else {
						checkpointItems[w.Key] = item
					}
				}
			}
		case model.OpAbort:
			state[tx] = "aborted"
			aborted[tx] = true
		}
	}

	committedIDs := []string{}
	abortedIDs := []string{}
	openIDs := []string{}
	for tx, st := range state {
		switch st {
		case "committed":
			committedIDs = append(committedIDs, tx)
		case "aborted":
			abortedIDs = append(abortedIDs, tx)
		default:
			openIDs = append(openIDs, tx)
		}
	}
	sort.Strings(committedIDs)
	sort.Strings(abortedIDs)
	sort.Strings(openIDs)

	visible := []string{}
	for _, rec := range all {
		if rec.TransactionID != "" && committed[rec.TransactionID] {
			visible = append(visible, rec.ID)
		}
	}
	sort.Strings(visible)

	itemList := make([]model.DataItem, 0, len(items))
	for _, item := range items {
		itemList = append(itemList, item)
	}
	sort.Slice(itemList, func(i, j int) bool { return itemList[i].Key < itemList[j].Key })

	return &Truth{
		Seed:                p.Seed,
		Items:               itemList,
		ItemMap:             items,
		CheckpointItems:     checkpointItems,
		VisibleRecordIDs:    visible,
		CommittedTxIDs:      committedIDs,
		AbortedTxIDs:        abortedIDs,
		UncommittedTxIDs:    openIDs,
		Ranges:              BuildTruthRanges(p),
		Tolerance:           Tolerance{LSNAbs: 0, Offset: 0, Index: 0, TimeMS: 0, Tick: 0},
		LatestCheckpointLSN: cpLSN,
	}
}

func pointFromRecord(r *model.Record) model.Point {
	return model.Point{
		SegmentID: r.SegmentID,
		RecordID:  r.ID,
		LSN:       r.LSN,
		Offset:    r.Offset,
		Index:     r.Index,
		TimeMS:    r.TimeMS,
		Tick:      r.Tick,
	}
}

func BuildTruthRanges(p *model.Project) map[string]model.ReplayRange {
	var cpLSN uint64
	if cp := p.LatestCheckpoint; cp != nil {
		cpLSN = cp.LSN
	}
	out := map[string]model.ReplayRange{}
	for _, seg := range p.SortedSegments() {
		recs := seg.Records
		start := 0
		for start < len(recs) && (cpLSN != 0 && recs[start].LSN <= cpLSN) {
			start++
		}
		rng := model.ReplayRange{
			SegmentID:  seg.ID,
			StartIndex: start,
			EndIndex:   len(recs) - 1,
		}
		if start < len(recs) {
			first := recs[start]
			last := recs[len(recs)-1]
			rng.Replay = true
			rng.Reason = "after_checkpoint"
			rng.Start = pointFromRecord(first)
			rng.End = pointFromRecord(last)
			rng.StartLSN = first.LSN
			rng.EndLSN = last.LSN
			rng.RecordCount = len(recs) - start
		} else {
			rng.Replay = false
			rng.Reason = "at_or_before_checkpoint"
			rng.RecordCount = 0
			rng.StartIndex = len(recs)
			rng.EndIndex = len(recs) - 1
			if len(recs) > 0 {
				last := recs[len(recs)-1]
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
