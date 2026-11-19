package store

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"driftconverge/internal/model"
)

func TestCommittedRoundIsAtomicImmutableAndSurvivesReopen(t *testing.T) {
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	stateStore, err := Open(ctx, databaseURL)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer stateStore.Close()
	if _, err := stateStore.pool.Exec(ctx, `DROP SCHEMA public CASCADE; CREATE SCHEMA public`); err != nil {
		t.Fatalf("reset schema: %v", err)
	}
	if err := stateStore.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	round := sampleRound("round-test-1")
	if err := stateStore.CommitRound(ctx, round); err != nil {
		t.Fatalf("CommitRound: %v", err)
	}
	if err := stateStore.CommitRound(ctx, round); !errors.Is(err, ErrRoundExists) {
		t.Fatalf("expected ErrRoundExists, got %v", err)
	}
	if _, err := stateStore.pool.Exec(
		ctx,
		`UPDATE drift_rounds SET duration_ms = 99 WHERE round_id = $1`,
		round.RoundID,
	); err == nil {
		t.Fatal("expected committed round update to be rejected")
	}

	status, err := stateStore.LatestClusterStatus(ctx, "cluster-prod")
	if err != nil {
		t.Fatalf("LatestClusterStatus: %v", err)
	}
	if status.RoundID != round.RoundID || status.DiffCount != 1 || status.PolicyBlockCount != 1 {
		t.Fatalf("unexpected status: %+v", status)
	}
	if len(status.Differences) != 1 || status.Differences[0].Expected != "prod" || status.Differences[0].Actual != "unexpected" {
		t.Fatalf("unexpected differences: %+v", status.Differences)
	}
	if len(status.PolicyBlocks) != 1 || len(status.ApplyResults) != 1 {
		t.Fatalf("unexpected child records: %+v", status)
	}

	stateStore.Close()
	reopened, err := Open(ctx, databaseURL)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	reopenedStatus, err := reopened.LatestClusterStatus(ctx, "cluster-prod")
	if err != nil {
		t.Fatalf("LatestClusterStatus after reopen: %v", err)
	}
	if reopenedStatus.RoundID != round.RoundID || reopenedStatus.Convergence != model.ConclusionPartial {
		t.Fatalf("reopened status changed: %+v", reopenedStatus)
	}
}

func TestCommitRollsBackWhenAChildInsertFails(t *testing.T) {
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	stateStore, err := Open(ctx, databaseURL)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer stateStore.Close()
	if _, err := stateStore.pool.Exec(ctx, `DROP SCHEMA public CASCADE; CREATE SCHEMA public`); err != nil {
		t.Fatalf("reset schema: %v", err)
	}
	if err := stateStore.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if _, err := stateStore.pool.Exec(ctx, `
		CREATE FUNCTION fail_policy_insert() RETURNS trigger LANGUAGE plpgsql AS $$
		BEGIN
			RAISE EXCEPTION 'injected child failure';
		END;
		$$;
		CREATE TRIGGER fail_policy_insert_trigger
		BEFORE INSERT ON drift_policy_blocks
		FOR EACH ROW EXECUTE FUNCTION fail_policy_insert();
	`); err != nil {
		t.Fatalf("install failure trigger: %v", err)
	}
	if err := stateStore.CommitRound(ctx, sampleRound("round-test-rollback")); err == nil {
		t.Fatal("expected injected commit failure")
	}
	var count int
	if err := stateStore.pool.QueryRow(ctx, `SELECT count(*) FROM drift_rounds`).Scan(&count); err != nil {
		t.Fatalf("count rounds: %v", err)
	}
	if count != 0 {
		t.Fatalf("transaction was not rolled back; %d round(s) visible", count)
	}
}

func TestLastObservableIgnoresFailedComparison(t *testing.T) {
	databaseURL := os.Getenv("TEST_DATABASE_URL")
	if databaseURL == "" {
		t.Skip("TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	stateStore, err := Open(ctx, databaseURL)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer stateStore.Close()
	if _, err := stateStore.pool.Exec(ctx, `DROP SCHEMA public CASCADE; CREATE SCHEMA public`); err != nil {
		t.Fatalf("reset schema: %v", err)
	}
	if err := stateStore.Migrate(ctx); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	successful := sampleRound("round-successful")
	if err := stateStore.CommitRound(ctx, successful); err != nil {
		t.Fatalf("commit successful round: %v", err)
	}
	failed := sampleRound("round-manifest-failure")
	failed.CommittedAt = failed.CommittedAt.Add(time.Second)
	for index := range failed.Clusters {
		failed.Clusters[index].ComparisonSuccess = false
		failed.Clusters[index].DiffCount = 0
		failed.Clusters[index].Differences = []model.Difference{}
		failed.Clusters[index].Convergence = model.ConclusionFailed
		failed.Clusters[index].ErrorCode = model.CodeManifest
	}
	if err := stateStore.CommitRound(ctx, failed); err != nil {
		t.Fatalf("commit failed round: %v", err)
	}
	latest, err := stateStore.LatestClusterStatus(ctx, "cluster-prod")
	if err != nil {
		t.Fatalf("latest: %v", err)
	}
	if latest.RoundID != failed.RoundID {
		t.Fatalf("expected latest failed round, got %s", latest.RoundID)
	}
	observable, err := stateStore.LastObservable(ctx, "cluster-prod")
	if err != nil {
		t.Fatalf("last observable: %v", err)
	}
	if observable.RoundID != successful.RoundID || observable.DiffCount != 1 {
		t.Fatalf("failed comparison overwrote preserved summary: %+v", observable)
	}
}

func sampleRound(roundID string) model.RoundReport {
	now := time.Now().UTC()
	return model.RoundReport{
		RoundID:     roundID,
		StartedAt:   now.Add(-time.Second),
		CommittedAt: now,
		DurationMS:  1000,
		Clusters: []model.ClusterReport{
			{
				Cluster:            "cluster-dev",
				Branch:             "cluster-dev",
				GitCommit:          "dev-sha",
				Observable:         true,
				ComparisonSuccess:  true,
				ExpectedCount:      1,
				DiffCount:          0,
				ConvergedResources: 1,
				Convergence:        model.ConclusionConverged,
				Differences:        []model.Difference{},
				PolicyBlocks:       []model.PolicyBlock{},
				ApplyResults:       []model.ApplyResult{},
			},
			{
				Cluster:           "cluster-prod",
				Branch:            "cluster-prod",
				GitCommit:         "prod-sha",
				Observable:        true,
				ComparisonSuccess: true,
				ExpectedCount:     1,
				DiffCount:         1,
				PolicyBlockCount:  1,
				BlockedCount:      1,
				Convergence:       model.ConclusionPartial,
				ErrorCode:         model.CodeWait,
				Differences: []model.Difference{{
					ResourceKey: "cluster-prod/cluster-prod/app",
					Field:       "env",
					Component:   "app",
					EnvKey:      "MODE",
					Expected:    "prod",
					Actual:      "unexpected",
					Resolution:  model.ResolutionBlocked,
					Reason:      "test",
				}},
				PolicyBlocks: []model.PolicyBlock{{
					ResourceKey:      "cluster-prod/cluster-prod/app",
					Field:            "env:DEBUG_MODE",
					Value:            "true",
					SourceBranch:     "cluster-prod",
					DeclaredInBranch: "cluster-dev",
					Reason:           "test",
				}},
				ApplyResults: []model.ApplyResult{{
					ResourceKey: "cluster-prod/cluster-prod/app",
					Action:      "wait",
					Status:      "blocked",
					ErrorCode:   model.CodeWait,
					Message:     "test",
					AppliedAt:   now,
				}},
			},
		},
	}
}
