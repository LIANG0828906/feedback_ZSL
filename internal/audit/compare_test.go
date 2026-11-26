package audit_test

import (
	"testing"

	"acousticfield/internal/acoustic"
	"acousticfield/internal/audit"
)

func TestCompareReportsLocatesFirstBandField(t *testing.T) {
	base := &acoustic.Report{
		ChunkOrder:     []string{"C00"},
		CommitSequence: []acoustic.CommitInfo{{ChunkID: "C00", Seq: 1}},
		Chunks: []acoustic.ChunkResult{{
			ChunkID: "C00", ListenerArrivalQ: []int64{10, 20, 30},
			InboundQ: []int64{0, 0, 0}, RetainedQ: []int64{10, 20, 30},
		}},
		Conservation: acoustic.ConservationReport{ResidualQ: []int64{0, 0, 0}},
	}
	other := *base
	otherChunks := base.Chunks[0]
	otherChunks.ListenerArrivalQ = []int64{10, 21, 30}
	other.Chunks = []acoustic.ChunkResult{otherChunks}
	cmp := audit.CompareReports([]*acoustic.Report{base, &other}, []string{"base", "other"})
	if cmp.OK || cmp.First == nil {
		t.Fatal("expected mismatch")
	}
	if cmp.First.ChunkID != "C00" || cmp.First.BandID != 1 || cmp.First.Field != "listener_arrival_q" {
		t.Fatalf("unexpected first mismatch: %+v", cmp.First)
	}
}

func TestCompareReportsCommitSequenceMismatch(t *testing.T) {
	base := &acoustic.Report{ChunkOrder: []string{"C00", "C01"}, CommitSequence: []acoustic.CommitInfo{{ChunkID: "C00"}, {ChunkID: "C01"}}}
	other := &acoustic.Report{ChunkOrder: []string{"C00", "C01"}, CommitSequence: []acoustic.CommitInfo{{ChunkID: "C01"}, {ChunkID: "C00"}}}
	cmp := audit.CompareReports([]*acoustic.Report{base, other}, []string{"a", "b"})
	if cmp.OK || cmp.First == nil || cmp.First.Field != "commit_sequence" {
		t.Fatalf("expected commit_sequence mismatch, got %+v", cmp.First)
	}
}
