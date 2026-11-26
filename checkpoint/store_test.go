package checkpoint_test

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"

	"maskqc/checkpoint"
	"maskqc/model"
)

func testStamp(projectID string) checkpoint.Stamp {
	return checkpoint.Stamp{
		ProjectID: projectID, ContentHash: "content-" + projectID,
		ConversionVersion: model.ConversionVersion, AlgorithmVersion: model.AlgorithmVersion,
		Width: 8, Height: 8,
	}
}

func testCommit(projectID, strokeID string, sequence int) checkpoint.StrokeCommit {
	coverage := []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}
	return checkpoint.StrokeCommit{
		Stamp: testStamp(projectID), StrokeID: strokeID, Seq: sequence, Status: "committed",
		InputHash: "input-" + strokeID, CacheKey: "stroke-key-" + strokeID,
		ComponentDigest: model.HashStrings(model.HashBytes(coverage)),
		ResidualDigest:  "residual", OrderVersion: "order", CommitSequence: sequence,
		Components: []checkpoint.ComponentCommit{{
			BlockIndex: sequence, BlockX: sequence, BlockY: 0, ContentHash: "block-content",
			CacheKey: "component-key-" + strokeID, ComponentDigest: model.HashBytes(coverage),
			Coverage: coverage, ResidualDigest: "residual", OrderVersion: "order",
		}},
	}
}

func testBlock(projectID string, index int, sequence int) checkpoint.BlockCommit {
	mask := []byte{byte(index), 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}
	return checkpoint.BlockCommit{
		Stamp: testStamp(projectID), BlockIndex: index, BlockX: index, BlockY: 0,
		ContentHash: "block-content", CacheKey: fmt.Sprintf("block-key-%d", index),
		CompositeDigest: model.HashBytes(mask), StrokeIDs: []string{fmt.Sprintf("s%d", index)},
		Mask: mask, OrderVersion: "order", CommitSequence: sequence,
	}
}

func TestCheckpointTransactionRestoreAndInvalidation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	store, err := checkpoint.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	projectID := "project-a"
	if err := store.CommitStroke(testCommit(projectID, "s1", 1)); err != nil {
		t.Fatal(err)
	}
	if err := store.CommitBlock(testBlock(projectID, 1, 1)); err != nil {
		t.Fatal(err)
	}
	strokes, blocks, err := store.Counts(projectID)
	if err != nil || strokes != 1 || blocks != 1 {
		t.Fatalf("unexpected counts strokes=%d blocks=%d err=%v", strokes, blocks, err)
	}
	restored, err := store.Restore(testStamp(projectID))
	if err != nil {
		t.Fatal(err)
	}
	if !restored.Valid || len(restored.Strokes) != 1 || len(restored.Blocks) != 1 {
		t.Fatalf("restore is incomplete: %+v", restored)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = checkpoint.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	restored, err = store.Restore(testStamp(projectID))
	if err != nil || !restored.Valid {
		t.Fatalf("reopen restore failed: %+v %v", restored, err)
	}
	if _, err := store.DB().Exec(`UPDATE project_meta SET algorithm_version='maskqc-engine/old' WHERE project_id=?`, projectID); err != nil {
		t.Fatal(err)
	}
	invalid, err := store.Restore(testStamp(projectID))
	if err != nil {
		t.Fatal(err)
	}
	if invalid.Valid || !invalid.Invalidated || invalid.Reason == "" {
		t.Fatalf("version mismatch did not invalidate: %+v", invalid)
	}
	strokes, blocks, err = store.Counts(projectID)
	if err != nil || strokes != 0 || blocks != 0 {
		t.Fatalf("invalidation did not clear state: %d %d %v", strokes, blocks, err)
	}
}

func TestSingleTransactionRollbackAndFailureIsolation(t *testing.T) {
	store, err := checkpoint.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	projectID := "project-b"
	bad := testCommit(projectID, "bad", 1)
	bad.Components[0].ComponentDigest = "not-a-real-digest"
	if err := store.CommitStroke(bad); err == nil {
		t.Fatal("bad digest was accepted")
	}
	strokes, blocks, err := store.Counts(projectID)
	if err != nil || strokes != 0 || blocks != 0 {
		t.Fatalf("failed transaction leaked rows: %d %d %v", strokes, blocks, err)
	}
	if err := store.CommitStroke(testCommit(projectID, "s1", 1)); err != nil {
		t.Fatal(err)
	}
	if err := store.CommitStroke(checkpoint.StrokeCommit{
		Stamp: testStamp(projectID), StrokeID: "s2", Seq: 2, Status: "failed",
		InputHash: "input-s2", CommitSequence: 2, ErrorMessage: "injected failure",
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.CommitStroke(testCommit(projectID, "s3", 3)); err != nil {
		t.Fatal(err)
	}
	restored, err := store.Restore(testStamp(projectID))
	if err != nil || !restored.Valid {
		t.Fatalf("restore failed: %v", err)
	}
	if restored.Strokes["s1"].Status != "committed" || restored.Strokes["s2"].Status != "failed" ||
		restored.Strokes["s3"].Status != "committed" {
		t.Fatalf("failure polluted other strokes: %+v", restored.Strokes)
	}
	if len(restored.Strokes["s2"].Components) != 0 || len(restored.Strokes["s1"].Components) != 1 ||
		len(restored.Strokes["s3"].Components) != 1 {
		t.Fatalf("failed stroke retained components or successful components were lost")
	}
}

func TestContentMismatchInvalidatesWholeProject(t *testing.T) {
	store, err := checkpoint.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	projectID := "project-c"
	if err := store.CommitStroke(testCommit(projectID, "s1", 1)); err != nil {
		t.Fatal(err)
	}
	if err := store.CommitBlock(testBlock(projectID, 1, 1)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().Exec(`UPDATE block_checkpoint SET mask=zeroblob(length(mask)) WHERE project_id=?`, projectID); err != nil {
		t.Fatal(err)
	}
	invalid, err := store.Restore(testStamp(projectID))
	if err != nil {
		t.Fatal(err)
	}
	if invalid.Valid || !invalid.Invalidated || invalid.Reason == "" {
		t.Fatalf("content mismatch did not invalidate: %+v", invalid)
	}
}

func TestStrokeVersionMismatchInvalidatesWholeProject(t *testing.T) {
	store, err := checkpoint.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	projectID := "project-version"
	if err := store.CommitStroke(testCommit(projectID, "s1", 1)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().Exec(`UPDATE stroke_checkpoint SET conversion_version='old' WHERE project_id=?`, projectID); err != nil {
		t.Fatal(err)
	}
	invalid, err := store.Restore(testStamp(projectID))
	if err != nil {
		t.Fatal(err)
	}
	if invalid.Valid || !invalid.Invalidated || invalid.Reason == "" {
		t.Fatalf("stroke version mismatch did not invalidate: %+v", invalid)
	}
}

func TestConcurrentCommitSerialization(t *testing.T) {
	store, err := checkpoint.Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	projectID := "project-d"
	var wait sync.WaitGroup
	errs := make(chan error, 16)
	for index := 1; index <= 16; index++ {
		wait.Add(1)
		go func(sequence int) {
			defer wait.Done()
			commit := testCommit(projectID, fmt.Sprintf("s%02d", sequence), sequence)
			commit.Components[0].BlockIndex = sequence
			errs <- store.CommitStroke(commit)
		}(index)
	}
	wait.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	strokes, _, err := store.Counts(projectID)
	if err != nil || strokes != 16 {
		t.Fatalf("concurrent commits lost rows: %d %v", strokes, err)
	}
}

func TestAbruptProcessExitRecoversCommittedState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "abrupt.db")
	command := exec.Command(os.Args[0], "-test.run=TestAbruptExitHelper")
	command.Env = append(os.Environ(), "MASKQC_ABRUPT_DB="+path)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("abrupt helper failed: %v\n%s", err, output)
	}
	store, err := checkpoint.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	restored, err := store.Restore(testStamp("abrupt-project"))
	if err != nil {
		t.Fatal(err)
	}
	if !restored.Valid || len(restored.Strokes) != 1 || len(restored.Blocks) != 1 {
		t.Fatalf("committed state was not recovered: %+v", restored)
	}
}

func TestAbruptExitHelper(t *testing.T) {
	path := os.Getenv("MASKQC_ABRUPT_DB")
	if path == "" {
		return
	}
	store, err := checkpoint.Open(path)
	if err != nil {
		os.Exit(2)
	}
	projectID := "abrupt-project"
	if err := store.CommitStroke(testCommit(projectID, "s1", 1)); err != nil {
		os.Exit(3)
	}
	if err := store.CommitBlock(testBlock(projectID, 1, 1)); err != nil {
		os.Exit(4)
	}
	os.Exit(0)
}
