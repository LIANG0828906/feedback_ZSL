package clientstate

import (
	"context"
	"path/filepath"
	"testing"

	"example.com/dualring-config/internal/model"
)

func TestSnapshotPersistsAcrossRestart(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "client.db")
	store, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	want := Snapshot{
		InstanceID: "sim-01",
		Namespace:  "payments",
		Version:    42,
		Ring:       model.RingCanary,
		ReleaseID:  "rel-42",
		Digest:     "digest-42",
		Items: []model.Item{
			{Key: "timeout", Value: "250ms", Version: 7},
		},
		LoadedAt: "2026-11-19T00:00:00Z",
	}
	if err := store.Save(ctx, want); err != nil {
		t.Fatalf("save: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	reopened, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	got, ok, err := reopened.Load(ctx, "sim-01", "payments")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !ok {
		t.Fatal("snapshot was not found after restart")
	}
	if got.Version != want.Version || got.Ring != want.Ring ||
		got.ReleaseID != want.ReleaseID || got.Digest != want.Digest ||
		len(got.Items) != 1 || got.Items[0].Value != "250ms" {
		t.Fatalf("snapshot mismatch: got=%+v want=%+v", got, want)
	}
}
