package store_test

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"sync"
	"testing"

	"lipsync/domain"
	"lipsync/store"
)

func artifact(i int) domain.ShardArtifact {
	pkgID := fmt.Sprintf("pkg-%02d", i)
	return domain.ShardArtifact{
		ShardIndex: i, DialogueID: fmt.Sprintf("d%02d", i), State: "committed",
		SourceVersion: "dialogue/v3", ContentHash: fmt.Sprintf("hash-%02d", i),
		QuantVersion: "q1", AlgorithmVersion: "a1", TruthFingerprint: "truth",
		Packages: []domain.ScheduledPackage{{
			Package: domain.Package{
				ID: pkgID, DialogueID: fmt.Sprintf("d%02d", i), ShardIndex: i,
				Channel: 0, StartFrame: int64(i), EndFrame: int64(i + 2),
				PhonemeIDs: []string{fmt.Sprintf("p-%02d", i)},
			},
			Status: "accepted", DispatchStartFrame: int64(i), DispatchEndFrame: int64(i + 2),
		}},
		Frames: []domain.FrameRecord{
			{Frame: int64(i), PackageIDs: []string{pkgID}},
			{Frame: int64(i + 1), PackageIDs: []string{pkgID}},
		},
		Events: []domain.DispatchEvent{{
			Frame: int64(i), PackageID: pkgID, DialogueID: fmt.Sprintf("d%02d", i),
			ShardIndex: i, Channel: 0, Status: "accepted", Reason: "capacity available",
		}},
	}
}

func TestConcurrentCommitsDoNotInterleave(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "checkpoint.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	const n = 16
	var wg sync.WaitGroup
	errs := make(chan error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := st.CommitShard(context.Background(), artifact(i))
			errs <- err
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	seqs, err := st.CommitSequence(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(seqs) != n {
		t.Fatalf("commit count = %d", len(seqs))
	}
	for i, seq := range seqs {
		if seq != int64(i+1) {
			t.Fatalf("commit sequence is not contiguous/serialized: %v", seqs)
		}
	}
	all, err := st.LoadAllShards(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != n {
		t.Fatalf("loaded shards = %d", len(all))
	}
	for _, a := range all {
		if len(a.Frames) != 2 || len(a.Packages) != 1 {
			t.Fatalf("partial shard artifact: %+v", a)
		}
	}
}

func TestHashMismatchInvalidatesWholeShard(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "checkpoint.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if _, err := st.CommitShard(ctx, artifact(i)); err != nil {
			t.Fatal(err)
		}
	}
	expected := []store.ExpectedShard{
		{ShardIndex: 0, DialogueID: "d00", ContentHash: "hash-00", SourceVersion: "dialogue/v3", QuantVersion: "q1", AlgorithmVersion: "a1", TruthFingerprint: "truth"},
		{ShardIndex: 1, DialogueID: "d01", ContentHash: "tampered", SourceVersion: "dialogue/v3", QuantVersion: "q1", AlgorithmVersion: "a1", TruthFingerprint: "truth"},
		{ShardIndex: 2, DialogueID: "d02", ContentHash: "hash-02", SourceVersion: "dialogue/v3", QuantVersion: "q1", AlgorithmVersion: "a1", TruthFingerprint: "truth"},
	}
	invalid, err := st.ValidateExpected(ctx, expected)
	if err != nil {
		t.Fatal(err)
	}
	if len(invalid) != 1 || invalid[0].ShardIndex != 1 ||
		len(invalid[0].Fields) != 1 || invalid[0].Fields[0] != "content_hash" {
		t.Fatalf("invalidated = %+v", invalid)
	}
	count, err := st.Count(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("count after invalidation = %d", count)
	}
}

func TestDuplicateCommitRollsBackAndDoesNotConsumeSequence(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "checkpoint.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	a := artifact(1)
	if _, err := st.CommitShard(ctx, a); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CommitShard(ctx, a); domain.ErrorCode(err) != domain.CodeState {
		t.Fatalf("duplicate code = %s: %v", domain.ErrorCode(err), err)
	}
	next := artifact(2)
	committed, err := st.CommitShard(ctx, next)
	if err != nil {
		t.Fatal(err)
	}
	seqs, _ := st.CommitSequence(ctx)
	sort.Slice(seqs, func(i, j int) bool { return seqs[i] < seqs[j] })
	if len(seqs) != 2 || committed.CommitSeq != 2 || seqs[0] != 1 || seqs[1] != 2 {
		t.Fatalf("sequence after rollback = %v, committed=%d", seqs, committed.CommitSeq)
	}
}
