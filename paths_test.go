package maskqa

import (
	"context"
	"path/filepath"
	"testing"
)

func runService(t *testing.T, d *Dataset, db string, mutate func(*Params), opts RunOptions) *RunReport {
	t.Helper()
	svc, err := NewService(d, db)
	if err != nil {
		t.Fatal(err)
	}
	if mutate != nil {
		p := svc.Params
		mutate(&p)
		if err := p.Validate(); err != nil {
			t.Fatal(err)
		}
		svc.Params = p
		svc.Scheduler = NewBoundedScheduler(p)
	}
	rep, err := svc.Run(context.Background(), opts)
	if err != nil {
		t.Fatalf("run %s: %v", db, err)
	}
	if err := svc.Close(); err != nil {
		t.Fatal(err)
	}
	return rep
}

func TestThreePathsPixelAndCommitConsistency(t *testing.T) {
	d := GenerateDataset()
	dir := t.TempDir()
	full := runService(t, d, filepath.Join(dir, "full.sqlite"), nil, RunOptions{Mode: "full"})
	realtime := runService(t, d, filepath.Join(dir, "realtime.sqlite"), func(p *Params) {
		p.MaxParallelBlocks = 3
		p.QueuedPixelBudget = 4096
	}, RunOptions{Mode: "realtime"})

	resumeDB := filepath.Join(dir, "resume.sqlite")
	first := runService(t, d, resumeDB, nil, RunOptions{Mode: "resume-interrupted", StopAfterCommits: 37})
	if !first.Incomplete || len(first.CommitSequence) != 37 {
		t.Fatalf("expected 37 committed prefix, got incomplete=%v commits=%d", first.Incomplete, len(first.CommitSequence))
	}
	resume := runService(t, d, resumeDB, nil, RunOptions{Mode: "resume"})

	if diff := CompareRunReports(full, realtime); diff != nil {
		t.Fatal(diff)
	}
	if diff := CompareRunReports(full, resume); diff != nil {
		t.Fatal(diff)
	}
	if len(resume.CommitSequence) != len(full.CommitSequence) {
		t.Fatalf("commit count %d != %d", len(resume.CommitSequence), len(full.CommitSequence))
	}
	for i, ref := range resume.CommitSequence {
		want := full.CommitSequence[i]
		if ref.SampleID != want.SampleID || ref.Stage != want.Stage || ref.BlockX != want.BlockX || ref.BlockY != want.BlockY || ref.ContentHash != want.ContentHash {
			t.Fatalf("commit %d mismatch: got %+v want %+v", i, ref, want)
		}
	}
}
