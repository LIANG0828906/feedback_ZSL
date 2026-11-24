package checkpoint

import (
	"path/filepath"
	"testing"

	"spatialir/internal/model"
)

func TestCommitOrderAndInvalidation(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "cp.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	params := model.FrameParams{FrameLength: 16, FrameHop: 8}
	resumed, err := st.InitSnapshot("hash-a", params, 3)
	if err != nil || resumed {
		t.Fatalf("first snapshot resumed=%v err=%v", resumed, err)
	}
	for i := 0; i < 3; i++ {
		if err := st.CommitFrame(model.FrameSnapshot{Index: i, Status: model.StatusCommitted}); err != nil {
			t.Fatal(err)
		}
	}
	snaps, err := st.LoadFrames()
	if err != nil || len(snaps) != 3 || snaps[2].Index != 2 {
		t.Fatalf("load frames failed: %d %v", len(snaps), err)
	}
	resumed, err = st.InitSnapshot("hash-a", params, 3)
	if err != nil || !resumed {
		t.Fatalf("same input must resume: %v %v", resumed, err)
	}
	// 哈希变化 => 整体作废。
	resumed, err = st.InitSnapshot("hash-b", params, 3)
	if err != nil || resumed {
		t.Fatalf("changed hash must invalidate: %v %v", resumed, err)
	}
	snaps, _ = st.LoadFrames()
	if len(snaps) != 0 {
		t.Fatal("no old frame may survive invalidation")
	}
	// 参数变化同样整体作废，禁止旧帧结果与新帧参数混用。
	p2 := params
	p2.FrameHop = 4
	_, _ = st.InitSnapshot("hash-b", p2, 3)
	_ = st.CommitFrame(model.FrameSnapshot{Index: 0, Status: model.StatusCommitted})
	resumed, err = st.InitSnapshot("hash-b", params, 3)
	if err != nil || resumed {
		t.Fatalf("param change must invalidate snapshot: %v %v", resumed, err)
	}
	snaps, _ = st.LoadFrames()
	if len(snaps) != 0 {
		t.Fatal("param change must discard all old frames")
	}
	// 算法版本变化同样整体作废。
	_, _ = st.InitSnapshot("hash-b", params, 3)
	_ = st.CommitFrame(model.FrameSnapshot{Index: 0, Status: model.StatusCommitted})
}
