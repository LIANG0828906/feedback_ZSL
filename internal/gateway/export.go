package gateway

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
)

func BuildBatches(p *Project, windows []*Aggregate) ([]*ExportBatch, error) {
	candidates := make([]*Aggregate, 0, len(windows))
	for _, agg := range windows {
		if agg != nil && agg.Committed && agg.RecordCount > 0 {
			candidates = append(candidates, agg)
		}
	}
	sort.Slice(candidates, func(i, j int) bool {
		a, b := candidates[i], candidates[j]
		if a.Priority != b.Priority {
			return a.Priority < b.Priority
		}
		if a.EndMS != b.EndMS {
			return a.EndMS < b.EndMS
		}
		return CompareWindowKey(a.Key, b.Key) < 0
	})

	maxWindows := p.Export.MaxWindowsPerBatch
	maxRecords := p.Export.MaxRecordsPerBatch
	if maxWindows <= 0 {
		return nil, NewError(CodeExportConstraint, "export.max_windows_per_batch", "must be positive")
	}
	if maxRecords <= 0 {
		return nil, NewError(CodeExportConstraint, "export.max_records_per_batch", "must be positive")
	}

	recordByID := map[string]Record{}
	for _, record := range p.Records {
		recordByID[record.ID] = record
	}

	batches := []*ExportBatch{}
	var current []*Aggregate
	var currentRecords int64
	batchNo := 0

	flush := func() error {
		if len(current) == 0 {
			return nil
		}
		batchNo++
		batch, err := makeBatch(p, batchNo, current, currentRecords, recordByID)
		if err != nil {
			return err
		}
		batches = append(batches, batch)
		current = nil
		currentRecords = 0
		return nil
	}

	for _, agg := range candidates {
		if agg.RecordCount > int64(maxRecords) {
			return nil, NewError(CodeWindowOversized, "export.max_records_per_batch", "window is atomic but exceeds batch record limit").
				WithContext("window_key", agg.Key.String()).
				WithContext("record_count", agg.RecordCount).
				WithContext("max_records", maxRecords)
		}
		if len(current) >= maxWindows || (len(current) > 0 && currentRecords+agg.RecordCount > int64(maxRecords)) {
			if err := flush(); err != nil {
				return nil, err
			}
		}
		current = append(current, agg)
		currentRecords += agg.RecordCount
		if len(current) >= maxWindows {
			if err := flush(); err != nil {
				return nil, err
			}
		}
	}
	if err := flush(); err != nil {
		return nil, err
	}
	return batches, nil
}

func makeBatch(p *Project, no int, windows []*Aggregate, recordMemberships int64, recordByID map[string]Record) (*ExportBatch, error) {
	keys := make([]string, 0, len(windows))
	priorityCodes := map[string]int{}
	digestParts := make([]string, 0, len(windows))
	uniqueRecords := map[string]RecordSummary{}
	var reqBytes, respBytes int64
	bucketTotals := make([]int64, len(p.HistogramBoundariesUS)+1)

	for _, agg := range windows {
		keys = append(keys, agg.Key.String())
		priorityCodes[agg.Key.String()] = agg.Priority
		digestParts = append(digestParts, AggregateDigest(agg))
		reqBytes += agg.RequestBytes
		respBytes += agg.ResponseBytes
		for i, count := range agg.BucketCounts {
			if i < len(bucketTotals) {
				bucketTotals[i] += count
			}
		}
		for _, id := range agg.RecordIDs {
			record, ok := recordByID[id]
			if !ok {
				return nil, NewError(CodeFieldRequired, "export.records", "aggregate references missing record").
					WithContext("record_id", id).WithContext("window_key", agg.Key.String())
			}
			if _, exists := uniqueRecords[id]; !exists {
				class, _ := p.RouteByID(record.RouteID).Classify(record.StatusCode, record.DurationUS)
				if record.ErrorClass != "" {
					class = record.ErrorClass
				}
				uniqueRecords[id] = RecordSummary{
					RecordID: record.ID, ArrivalSeq: record.ArrivalSeq, ServiceID: record.ServiceID,
					RouteID: record.RouteID, StatusCode: record.StatusCode, ErrorClass: class,
					EventTimeMS: record.EventTimeMS, DurationUS: record.DurationUS,
					RequestBytes: record.RequestBytes, ResponseBytes: record.ResponseBytes,
				}
			}
		}
	}
	recordIDs := make([]string, 0, len(uniqueRecords))
	for id := range uniqueRecords {
		recordIDs = append(recordIDs, id)
	}
	sort.Strings(recordIDs)
	summaries := make([]RecordSummary, 0, len(recordIDs))
	for _, id := range recordIDs {
		summaries = append(summaries, uniqueRecords[id])
	}

	sum := sha256.Sum256([]byte(strings.Join(digestParts, "\n")))
	digest := hex.EncodeToString(sum[:])
	priorityCode := windows[0].Priority
	for _, agg := range windows {
		if agg.Priority < priorityCode {
			priorityCode = agg.Priority
		}
	}
	summary := fmt.Sprintf("windows=%d membership_records=%d unique_records=%d buckets=%v request_bytes=%d response_bytes=%d",
		len(windows), recordMemberships, len(summaries), bucketTotals, reqBytes, respBytes)

	return &ExportBatch{
		BatchNo: no, BatchID: fmt.Sprintf("batch-%04d", no), PriorityCode: priorityCode,
		WindowKeys: keys, WindowCount: len(windows), RecordCount: recordMemberships,
		PriorityCodes: priorityCodes, SnapshotDigest: digest, SnapshotSummary: summary,
		Snapshots: append([]*Aggregate(nil), windows...), Records: summaries,
	}, nil
}

func BatchIdentity(batch *ExportBatch) string {
	if batch == nil {
		return ""
	}
	return fmt.Sprintf("%s|%s|%d|%s", batch.BatchID, strings.Join(batch.WindowKeys, ","), batch.RecordCount, batch.SnapshotDigest)
}

func CompareBatches(a, b []*ExportBatch) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if BatchIdentity(a[i]) != BatchIdentity(b[i]) {
			return false
		}
		if CanonicalJSON(a[i].PriorityCodes) != CanonicalJSON(b[i].PriorityCodes) {
			return false
		}
	}
	return true
}
