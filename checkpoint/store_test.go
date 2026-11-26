package checkpoint_test

import (
	"context"
	"testing"

	"dialoguefit/checkpoint"
)

func TestSegmentCheckpointCommitAndLoad(t *testing.T) {
	store, err := checkpoint.Open(t.TempDir() + "/nested/checkpoints.db")
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		rec := checkpoint.SegmentRecord{
			DialogueID: "dlg-1", PhonemeIndex: i, PhonemeID: "p", CommitSeq: int64(i + 1),
			ContentHash: "h", ParamVersion: "pv", AlgorithmVersion: "v1", TruthFingerprint: "fp",
			State: "committed", EnvelopeDigest: "digest", PayloadJSON: `{"ok":true}`,
		}
		if err := store.CommitSegment(ctx, rec); err != nil {
			t.Fatal(err)
		}
	}
	recs, err := store.LoadDialogueSegments(ctx, "dlg-1")
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 3 || recs[2].PhonemeIndex != 2 || recs[1].ParamVersion != "pv" {
		t.Fatalf("unexpected records: %+v", recs)
	}
	all, err := store.LoadAllOrdered(ctx)
	if err != nil || len(all) != 3 {
		t.Fatalf("LoadAllOrdered n=%d err=%v", len(all), err)
	}
}
