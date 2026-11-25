package workspace

import (
	"context"
	"path/filepath"
	"testing"

	"atelier/internal/engine"
)

func TestBootstrapRecoverEditAndCompare(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "atelier-checkpoints.db")
	service, err := New(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	first := service.Bootstrap(ctx)
	if !first.OK {
		t.Fatalf("bootstrap failed: %+v", first)
	}
	firstSnapshot := first.Data.(engine.Snapshot)
	if firstSnapshot.Checkpoint == nil || firstSnapshot.Checkpoint.CommittedZones == 0 {
		t.Fatalf("bootstrap did not commit checkpoints: %+v", firstSnapshot.Checkpoint)
	}
	if err := service.Close(); err != nil {
		t.Fatal(err)
	}

	service, err = New(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer service.Close()
	restarted := service.Bootstrap(ctx)
	if !restarted.OK {
		t.Fatalf("restart bootstrap failed: %+v", restarted)
	}
	restartedSnapshot := restarted.Data.(engine.Snapshot)
	if restartedSnapshot.Recovery == nil || restartedSnapshot.Recovery.Source != "completed_checkpoint" {
		t.Fatalf("unexpected recovery source: %+v", restartedSnapshot.Recovery)
	}
	comparison := service.Compare(ctx)
	if !comparison.OK {
		t.Fatalf("compare failed: %+v", comparison)
	}
	payload := comparison.Data.(ComparePayload)
	if !payload.Result.Equal {
		t.Fatalf("restart output differs from full: %+v", payload.Result)
	}

	updated := service.UpdateZone(ctx, UpdateZoneRequest{
		ZoneID: "west-woods", DensityScale: 1.04, RarityShift: 0.05,
	})
	if !updated.OK {
		t.Fatalf("update failed: %+v", updated)
	}
	updatedSnapshot := updated.Data.(engine.Snapshot)
	if updatedSnapshot.Invalidation == nil || updatedSnapshot.Invalidation.ReusedZoneCount == 0 {
		t.Fatalf("incremental update did not reuse unaffected zones: %+v", updatedSnapshot.Invalidation)
	}
	comparison = service.Compare(ctx)
	if !comparison.OK {
		t.Fatalf("post-edit compare failed: %+v", comparison)
	}
	payload = comparison.Data.(ComparePayload)
	if !payload.Result.Equal {
		t.Fatalf("incremental output differs from full: %+v", payload.Result)
	}
}
