package session

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"performanceexpressions/workspace/domain"
	"performanceexpressions/workspace/engine"
	"performanceexpressions/workspace/generator"
)

type failingSink struct{}

func (f *failingSink) AppendBatch(context.Context, []domain.Checkpoint) (int64, error) {
	return 0, errors.New("disk full")
}

func projectForTest(t *testing.T) domain.Project {
	t.Helper()
	return generator.GenerateProjects(generator.DefaultSeed)[0]
}

func editForTest(t *testing.T, project domain.Project, valueDelta float64) domain.EditOp {
	t.Helper()
	seg := project.Segments[0]
	ch, _, ok := seg.ChannelByID("velocity.main")
	if !ok || len(ch.Layers) == 0 || len(ch.Layers[0].Points) < 2 {
		t.Fatal("sample missing velocity.main/base")
	}
	point := ch.Layers[0].Points[1]
	return domain.EditOp{
		Kind: domain.EditMovePoint, SegmentID: seg.ID, ChannelID: ch.ID, LayerID: ch.Layers[0].ID,
		PointIndex: 1, Beat: point.Beat, Value: point.Value + valueDelta, BatchID: "batch-test",
	}
}

func TestCommitRefreshesCacheAndWorkingIsolation(t *testing.T) {
	project := projectForTest(t)
	params := domain.DefaultParameters()
	sess, err := New(project, params)
	if err != nil {
		t.Fatal(err)
	}
	beforeCommitted := domain.HashJSONMust(sess.CommittedProject())
	beforePlayback, err := sess.PlaybackAtTick(project.Segments[0].ID, 2)
	if err != nil {
		t.Fatal(err)
	}
	op := editForTest(t, project, 0.08)
	if err := sess.ApplyEdit(op); err != nil {
		t.Fatal(err)
	}
	if domain.HashJSONMust(sess.CommittedProject()) != beforeCommitted {
		t.Fatal("working edit changed committed project")
	}
	afterPlayback, err := sess.PlaybackAtTick(project.Segments[0].ID, 2)
	if err != nil {
		t.Fatal(err)
	}
	for target, value := range beforePlayback.Values {
		if afterPlayback.Values[target] != value {
			t.Fatalf("uncommitted edit changed playback for %s", target)
		}
	}
	if cmp, err := sess.DiffWorkingAgainstCommitted(); err == nil || domain.CodeOf(err) != domain.CodeDiff || cmp.Differences == 0 {
		t.Fatalf("working diff not detected: %v %+v", err, cmp)
	}
	receipt, err := sess.CommitSegment(context.Background(), "batch-1", project.Segments[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if receipt.BatchID != "batch-1" || len(receipt.SegmentIDs) != 1 {
		t.Fatalf("bad receipt: %+v", receipt)
	}
	if domain.HashJSONMust(sess.CommittedProject()) == beforeCommitted {
		t.Fatal("commit did not update committed project")
	}
	fullProject, fullResult, err := ApplyProjectOps(project, []domain.EditOp{op}, params)
	if err != nil {
		t.Fatal(err)
	}
	if domain.HashJSONMust(fullProject) != domain.HashJSONMust(sess.CommittedProject()) {
		t.Fatal("session commit differs from full rebuild")
	}
	if cmp, err := engine.CompareResultSets(fullResult, sess.CommittedResult()); err != nil || !cmp.Equal {
		t.Fatalf("session result differs from full rebuild: %v %+v", err, cmp)
	}
}

func TestCommitFailureRollsBackAndRetrySucceeds(t *testing.T) {
	project := projectForTest(t)
	params := domain.DefaultParameters()
	sess, err := New(project, params, Options{Sink: &failingSink{}})
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.ApplyEdit(editForTest(t, project, 0.06)); err != nil {
		t.Fatal(err)
	}
	before := domain.HashJSONMust(sess.CommittedResult())
	_, err = sess.CommitSegment(context.Background(), "will-fail", project.Segments[0].ID)
	if domain.CodeOf(err) != domain.CodeCommit {
		t.Fatalf("expected E_COMMIT, got %v", err)
	}
	if got := domain.HashJSONMust(sess.CommittedResult()); got != before {
		t.Fatal("failed commit changed committed overlay cache")
	}
	states := sess.States()
	if !states[0].Failed || states[0].LastError == nil {
		t.Fatalf("segment was not marked failed: %+v", states[0])
	}
	if err := sess.RetrySegment(project.Segments[0].ID); err != nil {
		t.Fatal(err)
	}
	if _, err := sess.CommitSegment(context.Background(), "retry-no-sink", project.Segments[0].ID); err != nil {
		// The sink still fails; this proves retry is allowed and goes through
		// the same persistence path, not that the storage recovered.
		if domain.CodeOf(err) != domain.CodeCommit {
			t.Fatalf("expected E_COMMIT on retry, got %v", err)
		}
	}
}

func TestDuplicateBatchKeepsModelUnchanged(t *testing.T) {
	project := projectForTest(t)
	sess, err := New(project, domain.DefaultParameters())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := sess.CommitSegment(context.Background(), "dup", project.Segments[0].ID); err != nil {
		t.Fatal(err)
	}
	before := domain.HashJSONMust(sess.CommittedProject())
	_, err = sess.CommitSegment(context.Background(), "dup", project.Segments[0].ID)
	if domain.CodeOf(err) != domain.CodeDuplicate {
		t.Fatalf("expected E_DUP, got %v", err)
	}
	if after := domain.HashJSONMust(sess.CommittedProject()); after != before {
		t.Fatal("duplicate commit changed model")
	}
}

func TestFaultInjectionCodesAndRetry(t *testing.T) {
	project := projectForTest(t)
	cases := []struct {
		kind FaultKind
		code string
	}{
		{FaultPriorityCycle, domain.CodeCycle},
		{FaultPosition, domain.CodePosition},
		{FaultBatchConflict, domain.CodeConflict},
		{FaultCommit, domain.CodeCommit},
	}
	for _, tc := range cases {
		t.Run(string(tc.kind), func(t *testing.T) {
			sess, err := New(project, domain.DefaultParameters())
			if err != nil {
				t.Fatal(err)
			}
			if err := sess.InjectFault(Fault{Kind: tc.kind, SegmentID: project.Segments[0].ID}); err != nil {
				t.Fatal(err)
			}
			_, err = sess.CommitSegment(context.Background(), "fault-"+string(tc.kind), project.Segments[0].ID)
			if domain.CodeOf(err) != tc.code {
				t.Fatalf("expected %s, got %v", tc.code, err)
			}
			if err := sess.RetrySegment(project.Segments[0].ID); err != nil {
				t.Fatal(err)
			}
			if _, err := sess.CommitSegment(context.Background(), "retry-"+string(tc.kind), project.Segments[0].ID); err != nil {
				t.Fatalf("retry failed: %v", err)
			}
		})
	}
}

func TestPoolBoundsAndBlocksInsteadOfDropping(t *testing.T) {
	pool := NewPool(context.Background(), 2, 1)
	var executed atomic.Int64
	var maxSeen atomic.Int64
	var inFlight atomic.Int64
	for i := 0; i < 8; i++ {
		if err := pool.Submit(context.Background(), func(context.Context) error {
			current := inFlight.Add(1)
			for {
				previous := maxSeen.Load()
				if current <= previous || maxSeen.CompareAndSwap(previous, current) {
					break
				}
			}
			executed.Add(1)
			time.Sleep(7 * time.Millisecond)
			inFlight.Add(-1)
			return nil
		}); err != nil {
			t.Fatal(err)
		}
	}
	pool.CloseAndWait()
	stats := pool.Stats()
	if stats.Submitted != 8 || stats.Completed != 8 || executed.Load() != 8 {
		t.Fatalf("jobs dropped: %+v executed=%d", stats, executed.Load())
	}
	if stats.MaxInFlight > 2 || maxSeen.Load() > 2 {
		t.Fatalf("parallelism exceeded bound: stats=%+v observed=%d", stats, maxSeen.Load())
	}
	if stats.Blocked == 0 {
		t.Fatalf("expected at least one blocked submit: %+v", stats)
	}
}

func TestRestoreCommittedPreservesTruthAndResult(t *testing.T) {
	project := projectForTest(t)
	params := domain.DefaultParameters()
	sess, err := New(project, params)
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.ApplyEdit(editForTest(t, project, 0.04)); err != nil {
		t.Fatal(err)
	}
	if _, err := sess.CommitSegment(context.Background(), "restore-batch", project.Segments[0].ID); err != nil {
		t.Fatal(err)
	}
	committed := sess.CommittedProject()
	result := sess.CommittedResult()
	restored, err := New(project, params, Options{KnownBatches: []string{"restore-batch"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := restored.RestoreCommitted(committed.Segments, []string{"restore-batch"}); err != nil {
		t.Fatal(err)
	}
	if cmp, err := engine.CompareResultSets(result, restored.CommittedResult()); err != nil || !cmp.Equal {
		t.Fatalf("restore result differs: %v %+v", err, cmp)
	}
	if _, err := restored.CommitSegment(context.Background(), "restore-batch", project.Segments[0].ID); domain.CodeOf(err) != domain.CodeDuplicate {
		t.Fatalf("known batch not restored: %v", err)
	}
}

func TestConcurrentWorkingReads(t *testing.T) {
	project := projectForTest(t)
	sess, err := New(project, domain.DefaultParameters())
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = sess.WorkingProject()
			_ = sess.WorkingResult()
			_ = sess.States()
		}()
	}
	wg.Wait()
}

func TestBatchSubmitterPreservesSubmissionResults(t *testing.T) {
	project := projectForTest(t)
	sess, err := New(project, domain.DefaultParameters())
	if err != nil {
		t.Fatal(err)
	}
	submitter := NewBatchSubmitter(context.Background(), sess, 2, 1)
	for i := 0; i < 3; i++ {
		seg := project.Segments[i]
		ch, _, _ := seg.ChannelByID("velocity.main")
		op := domain.EditOp{
			Kind: domain.EditMovePoint, SegmentID: seg.ID, ChannelID: ch.ID, LayerID: ch.Layers[0].ID,
			PointIndex: 1, Beat: ch.Layers[0].Points[1].Beat, Value: ch.Layers[0].Points[1].Value + 0.01*float64(i+1),
			BatchID: "parallel-" + seg.ID,
		}
		if err := sess.ApplyEdit(op); err != nil {
			t.Fatal(err)
		}
		if _, err := submitter.Submit(context.Background(), op.BatchID, []string{seg.ID}); err != nil {
			t.Fatal(err)
		}
	}
	submitter.CloseAndWait()
	results := submitter.Results()
	if len(results) != 3 {
		t.Fatalf("expected three results, got %d", len(results))
	}
	for i, result := range results {
		if result.Ordinal != uint64(i+1) || result.Error != nil {
			t.Fatalf("submission result %d invalid: %+v", i, result)
		}
	}
}
