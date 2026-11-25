package checkpoint

import (
	"context"
	"path/filepath"
	"testing"

	"replaybench/internal/model"
)

func TestRecoverInvalidatesBadWindowAndSuffix(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "suffix.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	identity := Identity{
		ContentHash: "hash", SourceFormatVersion: "rf1/test",
		ParamsVersion: "p1", AlgorithmVersion: model.AlgorithmVersion,
	}
	for i := 1; i <= 3; i++ {
		w := &model.Window{
			FirstRound: i, LastRound: i, ConsumedOffset: i, NextOffset: i,
			StateDigest: "digest", ContentHash: identity.ContentHash,
			SourceFormatVer: identity.SourceFormatVersion, ParamsVersion: identity.ParamsVersion,
			AlgorithmVersion: identity.AlgorithmVersion,
			Snapshots:        []model.RoundSnapshot{{Round: i, States: []model.UnitSnapshot{{UnitID: "u"}}}},
		}
		if err := store.CommitWindow(ctx, w); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.db.Exec(`UPDATE checkpoint_window SET params_version=? WHERE window_index=2`, "p2-bad"); err != nil {
		t.Fatal(err)
	}
	last, err := store.Recover(identity)
	if err != nil {
		t.Fatal(err)
	}
	if last == nil || last.Index != 1 {
		t.Fatalf("last = %+v, want window 1", last)
	}
	indexes, err := store.ListWindowIndexes()
	if err != nil {
		t.Fatal(err)
	}
	if len(indexes) != 1 || indexes[0] != 1 {
		t.Fatalf("indexes = %v want [1]", indexes)
	}
}
