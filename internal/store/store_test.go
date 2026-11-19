package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"edgegateway/internal/model"
)

func testStore(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "edge.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st, path
}

func TestSessionTransactionsAndRecovery(t *testing.T) {
	ctx := context.Background()
	st, path := testStore(t)
	sess, reused, err := st.CreateOrGetSession(ctx, "dev-1", model.IngressTCP, "conn-1", "10.0.0.1:1", "10.0.0.1", 4)
	if err != nil {
		t.Fatal(err)
	}
	if reused {
		t.Fatal("new session reported as reused")
	}
	dup, reused, err := st.CreateOrGetSession(ctx, "dev-1", model.IngressTCP, "conn-2", "10.0.0.1:2", "10.0.0.1", 4)
	if err != nil {
		t.Fatal(err)
	}
	if !reused || dup.ID != sess.ID {
		t.Fatalf("duplicate establish created a new session: %s != %s", dup.ID, sess.ID)
	}

	stats := Stats{RTTSamples: []float64{1.2, 1.5}, BacklogPeak: 3, SendWindowPeak: 5}
	if _, _, err := st.RecordAccepted(ctx, sess.ID, 0, 1024, "a", []model.Range{{Start: 0, End: 0}}, stats); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.RecordAccepted(ctx, sess.ID, 1, 1024, "b", []model.Range{{Start: 0, End: 1}}, stats); err != nil {
		t.Fatal(err)
	}
	rejected, err := st.RecordRejected(ctx, sess.ID, 2, 1024, "c", "injected", "TEST_REJECT", stats)
	if err != nil {
		t.Fatal(err)
	}
	if rejected.AckThrough != 1 || len(rejected.AckRanges) != 1 || rejected.AckRanges[0].End != 1 {
		t.Fatalf("rejection rolled back ack state: %#v", rejected.AckRanges)
	}
	if rejected.FailureCount != 1 || rejected.RejectionCount != 1 {
		t.Fatalf("failure counters = %d/%d", rejected.FailureCount, rejected.RejectionCount)
	}
	accepted, duplicate, err := st.RecordAccepted(ctx, sess.ID, 2, 1024, "c", []model.Range{{Start: 0, End: 2}}, stats)
	if err != nil {
		t.Fatal(err)
	}
	if duplicate {
		t.Fatal("first successful retry reported duplicate")
	}
	if accepted.SuccessCount != 3 || accepted.AckThrough != 2 || accepted.Blocked {
		t.Fatalf("unexpected retry state: success=%d ack=%d blocked=%t", accepted.SuccessCount, accepted.AckThrough, accepted.Blocked)
	}
	again, duplicate, err := st.RecordAccepted(ctx, sess.ID, 2, 1024, "c", []model.Range{{Start: 0, End: 2}}, stats)
	if err != nil {
		t.Fatal(err)
	}
	if !duplicate || again.SuccessCount != 3 {
		t.Fatalf("retry was double counted: duplicate=%t success=%d", duplicate, again.SuccessCount)
	}
	if again.SendWindowPeak != 5 || again.BacklogPeak != 3 {
		t.Fatalf("window/backlog peaks not persisted: %#v", again)
	}

	ev, err := st.RecordFeedback(ctx, accepted, true, "rtt_median_threshold_exceeded", "rtt_median", 12, 2, 99)
	if err != nil {
		t.Fatal(err)
	}
	if ev.Basis != "rtt_median" {
		t.Fatalf("feedback basis = %q", ev.Basis)
	}
	migrated, mig, err := st.ObserveAddress(ctx, sess.ID, "10.9.0.1:9", "10.9.0.1", true, 2*time.Second, 2, "test")
	if err != nil {
		t.Fatal(err)
	}
	if mig == nil || migrated.MigrationCount != 1 {
		t.Fatalf("migration not recorded: %#v", migrated)
	}
	if !mig.PreservedLimited || mig.PreservedRate != 12 || !migrated.LimitActive {
		t.Fatalf("migration did not preserve feedback state: %#v %#v", mig, migrated)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	restored, err := reopened.GetSession(ctx, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if restored.AckThrough != 2 || restored.SuccessCount != 3 || restored.MigrationCount != 1 || !restored.LimitActive {
		t.Fatalf("state not recovered after restart: %#v", restored)
	}
	_, duplicate, err = reopened.RecordAccepted(ctx, sess.ID, 2, 1024, "c", nil, Stats{})
	if err != nil || !duplicate {
		t.Fatalf("recovered session resubmitted ack: duplicate=%t err=%v", duplicate, err)
	}
	if err := reopened.CompleteSession(ctx, sess.ID); err != nil {
		t.Fatal(err)
	}
	next, reused, err := reopened.CreateOrGetSession(ctx, "dev-1", model.IngressTCP, "conn-3", "10.9.0.1:9", "10.9.0.1", 4)
	if err != nil {
		t.Fatal(err)
	}
	if reused || next.ID == sess.ID {
		t.Fatalf("completed session was overwritten: reused=%t old=%s new=%s", reused, sess.ID, next.ID)
	}
}

func TestSessionLimitAndIngressUniqueness(t *testing.T) {
	ctx := context.Background()
	st, _ := testStore(t)
	if _, _, err := st.CreateOrGetSession(ctx, "d", model.IngressQUIC, "", "10.1.0.1:1", "10.1.0.1", 1); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.CreateOrGetSession(ctx, "e", model.IngressQUIC, "", "10.1.0.2:1", "10.1.0.2", 1); err != ErrSessionLimit {
		t.Fatalf("limit error = %v", err)
	}
	if _, _, err := st.CreateOrGetSession(ctx, "d", model.IngressTCP, "", "10.1.0.1:2", "10.1.0.1", 2); err != nil {
		t.Fatalf("second ingress should have its own active session: %v", err)
	}
}
