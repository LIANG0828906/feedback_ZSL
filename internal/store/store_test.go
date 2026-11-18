package store

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"configcenter/internal/model"
)

func TestStoreAtomicVersionChainAndSnapshot(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	configStore, err := Open(filepath.Join(t.TempDir(), "config.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer configStore.Close()

	sqliteVersion, err := configStore.SQLiteVersion(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if sqliteVersion < "3.40" {
		t.Fatalf("sqlite version %s is older than 3.40", sqliteVersion)
	}

	initial := map[string]model.Change{
		"order.timeout_ms": {Key: "order.timeout_ms", Value: "800"},
		"order.retry":      {Key: "order.retry", Value: "3"},
	}
	version, created, err := configStore.EnsureNamespace(ctx, "orders", initial)
	if err != nil {
		t.Fatal(err)
	}
	if !created || version != 1 {
		t.Fatalf("seed returned created=%v version=%d", created, version)
	}
	version, created, err = configStore.EnsureNamespace(ctx, "orders", initial)
	if err != nil {
		t.Fatal(err)
	}
	if created || version != 1 {
		t.Fatalf("idempotent seed returned created=%v version=%d", created, version)
	}

	version, err = configStore.Publish(ctx, "orders", []model.Change{
		{Key: "order.timeout_ms", Value: "1200"},
		{Key: "order.retry", Value: "4"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if version != 2 {
		t.Fatalf("atomic publish version=%d; want 2", version)
	}

	version, err = configStore.Publish(ctx, "orders", []model.Change{
		{Key: "order.retry", Value: "5"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if version != 3 {
		t.Fatalf("second publish version=%d; want 3", version)
	}

	changes, err := configStore.ChangesSince(ctx, "orders", 1, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 2 {
		t.Fatalf("changes since v1 = %d; want 2", len(changes))
	}
	if changes[0].Key != "order.timeout_ms" && changes[0].Key != "order.retry" {
		t.Fatalf("unexpected change ordering: %+v", changes)
	}
	for _, change := range changes {
		if change.Version <= 1 || change.Version > 3 {
			t.Fatalf("change outside range: %+v", change)
		}
	}

	config, versions, err := configStore.SnapshotAt(ctx, "orders", 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(config) != 2 || config["order.timeout_ms"] != "1200" || config["order.retry"] != "4" {
		t.Fatalf("v2 snapshot = %#v", config)
	}
	if versions["order.timeout_ms"] != 2 || versions["order.retry"] != 2 {
		t.Fatalf("v2 item versions = %#v", versions)
	}

	config, versions, err = configStore.SnapshotAt(ctx, "orders", 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(config) != 2 || config["order.retry"] != "5" {
		t.Fatalf("v3 snapshot = %#v", config)
	}
	if versions["order.timeout_ms"] != 2 || versions["order.retry"] != 3 {
		t.Fatalf("v3 item versions = %#v", versions)
	}
}

func TestStoreRejectsInvalidPublishWithoutVersion(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	configStore, err := Open(filepath.Join(t.TempDir(), "config.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer configStore.Close()

	version, _, err := configStore.EnsureNamespace(ctx, "orders", map[string]model.Change{
		"order.timeout_ms": {Key: "order.timeout_ms", Value: "800"},
	})
	if err != nil {
		t.Fatal(err)
	}

	_, err = configStore.Publish(ctx, "orders", []model.Change{
		{Key: "order.timeout_ms", Value: "1200"},
		{Key: "order.timeout_ms", Value: "1300"},
	})
	if err == nil || !strings.Contains(err.Error(), "duplicated") {
		t.Fatalf("expected duplicate validation error, got %v", err)
	}
	latest, err := configStore.LatestVersion(ctx, "orders")
	if err != nil {
		t.Fatal(err)
	}
	if latest != version {
		t.Fatalf("invalid publish advanced version from %d to %d", version, latest)
	}
	changes, err := configStore.ChangesSince(ctx, "orders", version, latest)
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 0 {
		t.Fatalf("invalid publish inserted history: %+v", changes)
	}
}
