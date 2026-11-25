package store

import (
	"path/filepath"
	"testing"
)

func TestBeginSnapshotVoidsOnVersionMismatch(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "checkpoint.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	first, err := st.BeginSnapshot("snap", "identity-1", "algo-1", "time-1", "params-1")
	if err != nil {
		t.Fatal(err)
	}
	if first.Voided || first.Generation != 1 {
		t.Fatalf("first snapshot = %+v", first)
	}
	second, err := st.BeginSnapshot("snap", "identity-1", "algo-2", "time-1", "params-1")
	if err != nil {
		t.Fatal(err)
	}
	if !second.Voided || second.Generation != 2 || second.VoidReason == "" {
		t.Fatalf("version mismatch snapshot = %+v", second)
	}
}

func TestCommitIntervalIsOrderedAndAtomic(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "ordered.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	if _, err := st.BeginSnapshot("snap", "id", "algo", "time", "params"); err != nil {
		t.Fatal(err)
	}
	for _, no := range []int{1, 2, 3} {
		payload := []byte(`{"ok":true}`)
		if err := st.CommitInterval("snap", 1, IntervalRecord{IntervalNo: no, CacheKey: "k", Payload: payload, ContentHash: "h", AlgoVersion: "algo"}); err != nil {
			t.Fatal(err)
		}
	}
	numbers, err := st.CommittedIntervalNumbers("snap", 1)
	if err != nil {
		t.Fatal(err)
	}
	order, err := st.WriteOrder("snap", 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(numbers) != 3 || numbers[0] != 1 || numbers[2] != 3 {
		t.Fatalf("numbers=%v", numbers)
	}
	if len(order) != 3 || order[0] != 1 || order[2] != 3 {
		t.Fatalf("write order=%v", order)
	}
}
