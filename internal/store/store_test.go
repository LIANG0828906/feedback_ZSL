package store

import (
	"path/filepath"
	"testing"
)

func TestCommitAndLoadOrder(t *testing.T) {
	db := filepath.Join(t.TempDir(), "cp.db")
	st, err := Open(db)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.PutMeta("session_fingerprint", "s1"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 10; i++ {
		rec := SegmentRecord{
			ChannelID: "ch1", SegmentIndex: i, Start: int64(i * 100),
			End: int64((i + 1) * 100), DriftJSON: "{}",
			Output: make([]byte, 800), ContentHash: "hash",
			ParamVersion: "p1", AlgoVersion: "a1",
		}
		if _, err := st.CommitSegment(rec); err != nil {
			t.Fatal(err)
		}
	}
	recs, order, err := st.LoadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(order) != 10 || len(recs["ch1"]) != 10 {
		t.Fatalf("unexpected counts order=%d recs=%d", len(order), len(recs["ch1"]))
	}
	// 提交顺序稳定。
	for i, name := range order {
		want := "ch1#" + string(rune('0'+i))
		if name != want {
			t.Fatalf("order[%d]=%s want %s", i, name, want)
		}
	}
	if v, _ := st.GetMeta("session_fingerprint"); v != "s1" {
		t.Fatalf("meta roundtrip failed: %q", v)
	}
	if err := st.Reset(); err != nil {
		t.Fatal(err)
	}
	recs, order, _ = st.LoadAll()
	if len(order) != 0 || len(recs) != 0 {
		t.Fatal("reset must clear all rows")
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
}
