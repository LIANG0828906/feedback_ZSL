package queryopt

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

func TestSQLiteStateBeforeCursorAndRecovery(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "optimizer.db")
	sample, err := GenerateSample(DefaultSeed)
	if err != nil {
		t.Fatal(err)
	}
	est, plans, err := FullRecompute(sample.Project, sample.Bindings, OptionsFromProject(sample.Project))
	if err != nil {
		t.Fatal(err)
	}

	st, err := OpenStore(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	rec1 := CommittedQuery{Ordinal: 1, QueryID: "q1_sales_slice", Plan: plans["q1_sales_slice"], Estimates: est}
	if err := st.CommitQuery(ctx, rec1); err != nil {
		t.Fatal(err)
	}
	cursor, err := st.GlobalCommittedOrdinal(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if cursor != 1 {
		t.Fatalf("cursor=%d want1", cursor)
	}

	rec2 := CommittedQuery{Ordinal: 2, QueryID: "q2_account_txn", Plan: plans["q2_account_txn"], Estimates: est}
	err = st.CommitQueryWithCrashAfterState(ctx, rec2)
	if !errors.Is(err, ErrCrashSimulated) {
		t.Fatalf("crash hook err=%v", err)
	}

	st2, err := OpenStore(ctx, dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer st2.Close()
	before, err := st2.LoadCommitted(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(before) != 1 || before[0].QueryID != "q1_sales_slice" {
		t.Fatalf("reader observed future state: %+v", before)
	}

	recovered, err := st2.Recover(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(recovered) != 1 || recovered[0] != 2 {
		t.Fatalf("recovered=%v", recovered)
	}
	after, err := st2.LoadCommitted(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 2 {
		t.Fatalf("committed rows=%d", len(after))
	}
	if got := after[1].Plan.SelectedOrder; !equalStrings(got, plans["q2_account_txn"].SelectedOrder) {
		t.Fatalf("recovered order=%v want %v", got, plans["q2_account_txn"].SelectedOrder)
	}
	if len(after[0].Estimates.Predicates) == 0 {
		t.Fatal("estimate cache was not persisted")
	}
	if pe := after[0].Estimates.Predicates["pred_q1_region"]; pe.Selectivity != Fixed(pe.SelectivityRaw) {
		t.Fatalf("fixed selectivity not reconstructed: %+v", pe)
	}

	checkpointPayload := sample.Wire[SchemaV3]
	if err := st2.SaveCheckpoint(ctx, "engineering", SchemaV3, checkpointPayload); err != nil {
		t.Fatal(err)
	}
	loadedPayload, err := st2.LoadCheckpoint(ctx, "engineering", SchemaV3)
	if err != nil {
		t.Fatal(err)
	}
	if len(loadedPayload) != len(checkpointPayload) {
		t.Fatalf("checkpoint len=%d want %d", len(loadedPayload), len(checkpointPayload))
	}
	if err := st2.InjectTruncatedCheckpoint(ctx, "engineering"); err != nil {
		t.Fatal(err)
	}
	_, err = st2.LoadCheckpoint(ctx, "engineering", SchemaV3)
	if code := errorCode(err); code != CodeSnapshotTruncated {
		t.Fatalf("truncated checkpoint code=%s err=%v", code, err)
	}
	committedAfterFault, err := st2.LoadCommitted(ctx)
	if err != nil {
		t.Fatalf("committed query results changed after checkpoint fault: %v", err)
	}
	if len(committedAfterFault) != 2 || !equalStrings(committedAfterFault[1].Plan.SelectedOrder, plans["q2_account_txn"].SelectedOrder) {
		t.Fatalf("committed results not preserved: %+v", committedAfterFault)
	}

	if err := st2.SaveCheckpoint(ctx, "engineering-v2", SchemaV2, sample.Wire[SchemaV2]); err != nil {
		t.Fatal(err)
	}
	_, err = st2.LoadCheckpoint(ctx, "engineering-v2", SchemaV3)
	if code := errorCode(err); code != CodeSnapshotMismatch {
		t.Fatalf("version mismatch code=%s err=%v", code, err)
	}
}
