package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"archive/internal/model"
)

func TestQuotaReservationCompleteAndRelease(t *testing.T) {
	db, err := OpenSQLite(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	day := "2026-11-19"
	mk := func(id string) model.RepairJob {
		return model.RepairJob{JobID: id, StripeID: "s-" + id, ObjectID: "o1", MissingBlocks: []int{0},
			State: "running", ReservedBytes: 100, Day: day, StartedAt: time.Now().UTC()}
	}
	ok, usage, err := db.ReserveRepair(ctx, mk("j1"), 1000, "{}")
	if err != nil || !ok || usage.UsedBytes != 100 {
		t.Fatalf("reserve1 ok=%v usage=%+v err=%v", ok, usage, err)
	}
	ok, usage, err = db.ReserveRepair(ctx, mk("j2"), 1000, "{}")
	if err != nil || !ok || usage.UsedBytes != 200 {
		t.Fatalf("reserve2 ok=%v usage=%+v err=%v", ok, usage, err)
	}
	if err := db.FailRepair(ctx, "j2", "s-j2", "o1", "test_failure", "{}", "{}", true); err != nil {
		t.Fatal(err)
	}
	usage, _ = db.QuotaUsage(ctx, day, 1000)
	if usage.UsedBytes != 100 || usage.ReservedBytes != 100 {
		t.Fatalf("after release usage=%+v", usage)
	}
	if err := db.CompleteRepair(ctx, "j1", "s-j1", "o1", `{"blocks":[]}`, "{}"); err != nil {
		t.Fatal(err)
	}
	if err := db.CompleteRepair(ctx, "j1", "s-j1", "o1", `{"blocks":[]}`, "{}"); err != nil {
		t.Fatal(err)
	}
	usage, _ = db.QuotaUsage(ctx, day, 1000)
	if usage.UsedBytes != 100 || usage.CommittedBytes != 100 {
		t.Fatalf("after complete usage=%+v", usage)
	}
	row, err := db.RepairJob(ctx, "j1")
	if err != nil || row.State != "completed" {
		t.Fatalf("job=%+v err=%v", row, err)
	}
	nextDay, err := db.QuotaUsage(ctx, "2026-11-20", 1000)
	if err != nil {
		t.Fatal(err)
	}
	if nextDay.UsedBytes != 0 || nextDay.RemainingBytes != 1000 {
		t.Fatalf("local-day rollover not isolated: %+v", nextDay)
	}
}

func TestQuotaRejectsOverLimit(t *testing.T) {
	db, err := OpenSQLite(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	job := model.RepairJob{JobID: "j1", StripeID: "s1", ObjectID: "o1", MissingBlocks: []int{0},
		State: "running", ReservedBytes: 101, Day: "2026-11-19", StartedAt: time.Now().UTC()}
	ok, usage, err := db.ReserveRepair(ctx, job, 100, "{}")
	if err != nil {
		t.Fatal(err)
	}
	if ok || usage.UsedBytes != 0 || usage.RemainingBytes != 100 {
		t.Fatalf("expected quota rejection, ok=%v usage=%+v", ok, usage)
	}
}
