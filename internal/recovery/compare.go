package recovery

import (
	"fmt"
	"reflect"

	"enginelab.local/internal/model"
)

type Comparison struct {
	Equal              bool     `json:"equal"`
	ItemsEqual         bool     `json:"items_equal"`
	RangesEqual        bool     `json:"ranges_equal"`
	TransactionsEqual  bool     `json:"transactions_equal"`
	SegmentWritesEqual bool     `json:"segment_writes_equal"`
	Differences        []string `json:"differences,omitempty"`
}

func CompareResults(a, b *model.RecoveryResult) Comparison {
	cmp := Comparison{Equal: true, ItemsEqual: true, RangesEqual: true, TransactionsEqual: true, SegmentWritesEqual: true}
	if !reflect.DeepEqual(a.Items, b.Items) {
		cmp.ItemsEqual = false
		cmp.Equal = false
		cmp.Differences = append(cmp.Differences, fmt.Sprintf("items differ: incremental=%d full=%d", len(a.Items), len(b.Items)))
		for i := 0; i < len(a.Items) || i < len(b.Items); i++ {
			var ai, bi *model.DataItem
			if i < len(a.Items) {
				ai = &a.Items[i]
			}
			if i < len(b.Items) {
				bi = &b.Items[i]
			}
			if !reflect.DeepEqual(ai, bi) {
				cmp.Differences = append(cmp.Differences, fmt.Sprintf("item[%d] incremental=%+v full=%+v", i, ai, bi))
				break
			}
		}
	}
	if !reflect.DeepEqual(a.Ranges, b.Ranges) {
		cmp.RangesEqual = false
		cmp.Equal = false
		for id, ar := range a.Ranges {
			br := b.Ranges[id]
			if !reflect.DeepEqual(ar, br) {
				cmp.Differences = append(cmp.Differences, fmt.Sprintf("range %s differs: incremental=%+v full=%+v", id, ar, br))
			}
		}
	}
	if !reflect.DeepEqual(a.CommittedTxIDs, b.CommittedTxIDs) || !reflect.DeepEqual(a.AbortedTxIDs, b.AbortedTxIDs) {
		cmp.TransactionsEqual = false
		cmp.Equal = false
		cmp.Differences = append(cmp.Differences, "transaction visibility sets differ")
	}
	ids := map[string]bool{}
	for id := range a.SegmentMap {
		ids[id] = true
	}
	for id := range b.SegmentMap {
		ids[id] = true
	}
	for id := range ids {
		as := a.SegmentMap[id]
		bs := b.SegmentMap[id]
		if as == nil || bs == nil {
			cmp.SegmentWritesEqual = false
			cmp.Equal = false
			cmp.Differences = append(cmp.Differences, fmt.Sprintf("segment %s missing on one side", id))
			continue
		}
		if as.AppliedRecords != bs.AppliedRecords || as.State != bs.State || as.Integrity != bs.Integrity {
			cmp.SegmentWritesEqual = false
			cmp.Equal = false
			cmp.Differences = append(cmp.Differences, fmt.Sprintf("segment %s state/count differs: (%s,%d,%s) vs (%s,%d,%s)", id, as.State, as.AppliedRecords, as.Integrity, bs.State, bs.AppliedRecords, bs.Integrity))
		}
		if !reflect.DeepEqual(as.Writes, bs.Writes) {
			cmp.SegmentWritesEqual = false
			cmp.Equal = false
			cmp.Differences = append(cmp.Differences, fmt.Sprintf("segment %s writes differ", id))
		}
		if as.LastRecovered.LSN != bs.LastRecovered.LSN || as.LastRecovered.RecordID != bs.LastRecovered.RecordID {
			cmp.SegmentWritesEqual = false
			cmp.Equal = false
			cmp.Differences = append(cmp.Differences, fmt.Sprintf("segment %s last recovered differs: %+v vs %+v", id, as.LastRecovered, bs.LastRecovered))
		}
	}
	return cmp
}

func EqualCorrectness(a, b *model.RecoveryResult) bool {
	return CompareResults(a, b).Equal
}
