package checkpoint

import (
	"context"
	"path/filepath"
	"testing"
)

func TestSQLiteStageTransactionHistory(t *testing.T) {
	ctx := context.Background()
	store, err := OpenSQLite(filepath.Join(t.TempDir(), "checkpoint.sqlite"))
	if err != nil {
		t.Skipf("sqlite adapter unavailable in this build: %v", err)
	}
	defer store.Close()
	seq, err := store.CommitStage(ctx, StageCommit{
		ProjectID: "p", BlockKey: "b", Stage: "gradient", Status: "committed",
		Summary: "summary", Threshold: 123, CacheKey: "cache",
		InputHash: "input", ContentHash: "content", ParamVersion: "param",
		AlgoVersion: "sobel-v1",
	})
	if err != nil || seq != 1 {
		t.Fatalf("commit seq=%d err=%v", seq, err)
	}
	history, err := store.StageHistory(ctx, "p")
	if err != nil {
		t.Fatal(err)
	}
	if len(history) != 1 || history[0].Threshold != 123 || history[0].CacheKey != "cache" {
		t.Fatalf("unexpected history: %+v", history)
	}
}
