package recovery

import (
	"hash/fnv"
	"sort"
	"strings"
	"time"

	"enginelab.local/internal/model"
)

func SegmentCacheKey(seg *model.Segment) string {
	h := fnv.New64a()
	_, _ = h.Write([]byte(seg.ID))
	_, _ = h.Write([]byte{0})
	for _, pred := range seg.PredecessorIDs {
		_, _ = h.Write([]byte(pred))
		_, _ = h.Write([]byte{':'})
	}
	_, _ = h.Write([]byte{0, 1})
	for _, rec := range seg.Records {
		_, _ = h.Write([]byte(rec.ID))
		_, _ = h.Write([]byte{':'})
		_, _ = h.Write([]byte(rec.Op))
		_, _ = h.Write([]byte{':'})
		_, _ = h.Write([]byte(rec.Key))
		_, _ = h.Write([]byte{':'})
		_, _ = h.Write([]byte(rec.Value))
		_, _ = h.Write([]byte{':'})
		_, _ = h.Write([]byte(itoa(int(rec.LSN))))
		_, _ = h.Write([]byte{':'})
		if rec.Complete {
			_, _ = h.Write([]byte{'1'})
		} else {
			_, _ = h.Write([]byte{'0'})
		}
		_, _ = h.Write([]byte{'|'})
	}
	return fmtUint(h.Sum64())
}

func fmtUint(v uint64) string {
	const digits = "0123456789abcdef"
	var b [16]byte
	for i := 15; i >= 0; i-- {
		b[i] = digits[v&0xf]
		v >>= 4
	}
	return "k-" + string(b[:])
}

func ReplaySegment(seg *model.Segment, rng model.ReplayRange, budget model.Budget) (*model.SegmentReport, error) {
	started := time.Now()
	estimate := EstimateSegmentMemory(seg)
	report := &model.SegmentReport{
		SegmentID:            seg.ID,
		Range:                rng,
		Integrity:            seg.Integrity,
		EstimatedMemoryBytes: estimate,
		CacheKey:             SegmentCacheKey(seg),
		Diagnostics:          append([]model.Diagnostic(nil), seg.Diagnostics...),
	}
	if estimate > budget.MaxSegmentMemoryBytes {
		report.State = model.StateRejected
		report.ElapsedNanoseconds = time.Since(started).Nanoseconds()
		return report, model.NewError(model.EBudgetMemory, seg.ID,
			"segment replay estimate %d exceeds per-segment cap %d", estimate, budget.MaxSegmentMemoryBytes).
			WithDetails(map[string]any{"estimated_bytes": estimate, "limit_bytes": budget.MaxSegmentMemoryBytes})
	}
	if seg.Integrity != model.IntegrityComplete {
		report.State = model.StateDegraded
	} else {
		report.State = model.StateReplayed
	}
	if !rng.Replay {
		report.State = model.StateSkipped
		if seg.Integrity != model.IntegrityComplete {
			report.State = model.StateDegraded
		}
		report.LastRecovered = rng.End
		report.ElapsedNanoseconds = time.Since(started).Nanoseconds()
		return report, nil
	}

	txWrites := map[string][]model.Write{}
	txState := map[string]string{}
	txRecords := map[string][]string{}
	committed := map[string]bool{}
	aborted := map[string]bool{}

	end := rng.EndIndex
	if end >= len(seg.Records) {
		end = len(seg.Records) - 1
	}
	for i := rng.StartIndex; i <= end; i++ {
		rec := seg.Records[i]
		if rec == nil || !rec.Complete {
			break
		}
		report.AppliedRecords++
		report.LastRecovered = pointFromRecord(rec)
		if rec.TransactionID == "" {
			continue
		}
		tx := rec.TransactionID
		txRecords[tx] = append(txRecords[tx], rec.ID)
		switch rec.Op {
		case model.OpBegin:
			txState[tx] = "open"
		case model.OpPut, model.OpDelete:
			txState[tx] = "open"
			txWrites[tx] = append(txWrites[tx], model.Write{
				Key:       rec.Key,
				Value:     rec.Value,
				Version:   rec.Version,
				LSN:       rec.LSN,
				TxID:      tx,
				SegmentID: seg.ID,
				Deleted:   rec.Op == model.OpDelete,
			})
		case model.OpCommit:
			txState[tx] = "committed"
			committed[tx] = true
			for idx := range txWrites[tx] {
				txWrites[tx][idx].CommitLSN = rec.LSN
				report.Writes = append(report.Writes, txWrites[tx][idx])
			}
			report.CommittedTransactions = append(report.CommittedTransactions, tx)
		case model.OpAbort:
			txState[tx] = "aborted"
			aborted[tx] = true
			report.AbortedTransactions = append(report.AbortedTransactions, tx)
			delete(txWrites, tx)
		}
	}

	for tx := range txState {
		if committed[tx] {
			report.VisibleRecords = append(report.VisibleRecords, txRecords[tx]...)
		}
	}
	sort.Strings(report.VisibleRecords)
	sort.Strings(report.CommittedTransactions)
	sort.Strings(report.AbortedTransactions)
	sort.SliceStable(report.Writes, func(i, j int) bool {
		if report.Writes[i].CommitLSN != report.Writes[j].CommitLSN {
			return report.Writes[i].CommitLSN < report.Writes[j].CommitLSN
		}
		return report.Writes[i].LSN < report.Writes[j].LSN
	})
	if rng.End.RecordID != "" {
		report.LastRecovered = rng.End
	}
	report.ElapsedNanoseconds = time.Since(started).Nanoseconds()
	return report, nil
}

func AggregateItems(baseline map[string]model.DataItem, reports []*model.SegmentReport, batchSize int) []model.DataItem {
	items := map[string]model.DataItem{}
	for k, v := range baseline {
		items[k] = v
	}
	writes := []model.Write{}
	for _, rep := range reports {
		if rep == nil || !rep.Range.Replay {
			continue
		}
		writes = append(writes, rep.Writes...)
	}
	sort.SliceStable(writes, func(i, j int) bool {
		if writes[i].CommitLSN != writes[j].CommitLSN {
			return writes[i].CommitLSN < writes[j].CommitLSN
		}
		return writes[i].LSN < writes[j].LSN
	})
	if batchSize <= 0 {
		batchSize = len(writes) + 1
	}
	for start := 0; start < len(writes); start += batchSize {
		end := start + batchSize
		if end > len(writes) {
			end = len(writes)
		}
		for _, w := range writes[start:end] {
			if w.Deleted {
				delete(items, w.Key)
				continue
			}
			items[w.Key] = model.DataItem{
				Key:       w.Key,
				Value:     w.Value,
				Version:   w.Version,
				LSN:       w.CommitLSN,
				TxID:      w.TxID,
				SegmentID: w.SegmentID,
			}
		}
	}
	out := make([]model.DataItem, 0, len(items))
	for _, item := range items {
		out = append(out, item)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

func ComputeBaseline(p *model.Project, cp *model.Checkpoint) map[string]model.DataItem {
	if cp == nil {
		return map[string]model.DataItem{}
	}
	all := []*model.Record{}
	for _, seg := range p.Segments {
		for _, rec := range seg.Records {
			if rec.Complete && rec.LSN <= cp.LSN {
				all = append(all, rec)
			}
		}
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].LSN < all[j].LSN })
	writes := []model.Write{}
	state := map[string]string{}
	buffered := map[string][]model.Write{}
	for _, rec := range all {
		if rec.TransactionID == "" {
			continue
		}
		tx := rec.TransactionID
		switch rec.Op {
		case model.OpPut, model.OpDelete:
			state[tx] = "open"
			buffered[tx] = append(buffered[tx], model.Write{Key: rec.Key, Value: rec.Value, Version: rec.Version, LSN: rec.LSN, TxID: tx, SegmentID: rec.SegmentID, Deleted: rec.Op == model.OpDelete})
		case model.OpCommit:
			state[tx] = "committed"
			for _, w := range buffered[tx] {
				w.CommitLSN = rec.LSN
				writes = append(writes, w)
			}
		case model.OpAbort:
			state[tx] = "aborted"
			delete(buffered, tx)
		}
	}
	items := map[string]model.DataItem{}
	for _, w := range writes {
		if w.CommitLSN > cp.LSN {
			continue
		}
		if w.Deleted {
			delete(items, w.Key)
		} else {
			items[w.Key] = model.DataItem{Key: w.Key, Value: w.Value, Version: w.Version, LSN: w.CommitLSN, TxID: w.TxID, SegmentID: w.SegmentID}
		}
	}
	return items
}

func SortedReportIDs(reports []*model.SegmentReport) string {
	ids := make([]string, 0, len(reports))
	for _, r := range reports {
		ids = append(ids, r.SegmentID)
	}
	sort.Strings(ids)
	return strings.Join(ids, ",")
}
