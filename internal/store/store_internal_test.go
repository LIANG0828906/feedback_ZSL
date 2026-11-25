package store

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

func TestHashCorruptionInvalidatesWholeChain(t *testing.T) {
	ctx := context.Background()
	st, err := Open(filepath.Join(t.TempDir(), "corrupt.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	blob := []byte("canonical-mesh-bytes")
	cp := Checkpoint{
		SampleID: "cube", Stage: "diagnostic", StageIndex: 0, State: "committed",
		ParamVersion: "pv-test", AlgoVersion: "algo-test", ParamsJSON: "{}",
		MeshBlob: blob, MeshHash: HashBlob(blob),
		SnapshotJSON: []byte(`{"param_version":"pv-test","algo_version":"algo-test"}`),
	}
	if err := st.CommitCheckpoint(ctx, cp); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.LoadRecoveryChain(ctx, "cube", "pv-test", "algo-test"); err != nil {
		t.Fatalf("valid snapshot should load: %v", err)
	}
	if _, err := st.db.Exec(`UPDATE checkpoints SET mesh_blob=? WHERE sample_id='cube'`, []byte("tampered")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.LoadRecoveryChain(ctx, "cube", "pv-test", "algo-test"); !errors.Is(err, ErrInvalidSnapshot) {
		t.Fatalf("err=%v want ErrInvalidSnapshot", err)
	}
	var count int
	if err := st.db.QueryRow(`SELECT count(*) FROM checkpoints WHERE sample_id='cube'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("corrupt chain rows=%d want 0", count)
	}
}

func TestVersionMismatchInvalidatesChain(t *testing.T) {
	ctx := context.Background()
	st, err := Open(filepath.Join(t.TempDir(), "version.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	blob := []byte("mesh")
	if err := st.CommitCheckpoint(ctx, Checkpoint{
		SampleID: "cube", Stage: "weld", StageIndex: 1, State: "committed",
		ParamVersion: "pv-old", AlgoVersion: AlgoTestVersion, ParamsJSON: "{}",
		MeshBlob: blob, MeshHash: HashBlob(blob),
		SnapshotJSON: []byte(`{"param_version":"pv-old","algo_version":"algo-test"}`),
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.LoadRecoveryChain(ctx, "cube", "pv-new", "algo-test"); !errors.Is(err, ErrInvalidSnapshot) {
		t.Fatalf("err=%v want ErrInvalidSnapshot", err)
	}
}

const AlgoTestVersion = "algo-test"
