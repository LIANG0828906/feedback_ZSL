package pipeline_test

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"

	"lipsync/domain"
	"lipsync/edit"
	"lipsync/generate"
	"lipsync/pipeline"
	"lipsync/schedule"
	"lipsync/schema"
	"lipsync/store"
	"lipsync/truth"
	"lipsync/verify"
)

func setup(t *testing.T) (*domain.Project, *truth.Manifest) {
	t.Helper()
	rep, err := generate.Generate(generate.Config{OutDir: t.TempDir(), Seed: 42})
	if err != nil {
		t.Fatal(err)
	}
	p, m, err := schema.LoadBundle(rep.Projects["v3"])
	if err != nil {
		t.Fatal(err)
	}
	return p, m
}

func runWithStore(t *testing.T, p *domain.Project, path string, mode pipeline.Mode,
	fp string, opts ...func(*pipeline.Options)) *pipeline.Result {
	t.Helper()
	st, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	o := pipeline.Options{Mode: mode, TruthFingerprint: fp}
	for _, fn := range opts {
		fn(&o)
	}
	res, err := pipeline.Run(context.Background(), p, st, o)
	if err != nil {
		t.Fatalf("run %s: %v", mode, err)
	}
	return res
}

func TestThreePathsAndCheckpointRecoveryAreIdentical(t *testing.T) {
	p, m := setup(t)
	dir := t.TempDir()
	realtimeDB := filepath.Join(dir, "realtime.db")
	fullDB := filepath.Join(dir, "full.db")
	resumeDB := filepath.Join(dir, "resume.db")
	ctx := context.Background()

	realtime := runWithStore(t, p, realtimeDB, pipeline.ModeRealtime, m.Fingerprint)
	full := runWithStore(t, p, fullDB, pipeline.ModeFull, m.Fingerprint)

	st, err := store.Open(resumeDB)
	if err != nil {
		t.Fatal(err)
	}
	_, err = pipeline.Run(ctx, p, st, pipeline.Options{
		Mode: pipeline.ModeFull, TruthFingerprint: m.Fingerprint, StopAfterCommits: 2,
	})
	if !pipeline.IsCanceled(err) {
		t.Fatalf("partial run error = %v", err)
	}
	_ = st.Close()
	resumed := runWithStore(t, p, resumeDB, pipeline.ModeRecovery, m.Fingerprint)
	if len(resumed.Shards) != len(p.Dialogues) {
		t.Fatalf("resumed shards = %d", len(resumed.Shards))
	}

	for _, path := range []string{realtimeDB, fullDB, resumeDB} {
		if _, err := verify.VerifyDB(ctx, path, m); err != nil {
			t.Fatalf("verify %s: %v", path, err)
		}
	}
	if _, err := verify.CompareDBs(ctx, realtimeDB, fullDB, resumeDB); err != nil {
		t.Fatalf("compare paths: %v", err)
	}
	if len(realtime.CommitSequence) != len(full.CommitSequence) ||
		len(full.CommitSequence) != len(resumed.CommitSequence) {
		t.Fatalf("commit sequences differ")
	}

	// A fully valid checkpoint run must reuse every committed shard without
	// appending new commits.
	recovered := runWithStore(t, p, realtimeDB, pipeline.ModeRecovery, m.Fingerprint)
	if fmt.Sprint(recovered.CommitSequence) != fmt.Sprint(realtime.CommitSequence) {
		t.Fatalf("all-valid recovery re-committed shards: %v != %v",
			recovered.CommitSequence, realtime.CommitSequence)
	}
}

func TestInjectedDomainErrorsLocateShardAndPhoneme(t *testing.T) {
	p, m := setup(t)
	cases := []struct {
		name  string
		code  domain.Code
		shard int
		mut   func(*domain.Project)
		opts  []func(*pipeline.Options)
	}{
		{
			name: "order", code: domain.CodeOrder, shard: 1,
			mut: func(p *domain.Project) {
				ph := &p.Dialogues[1].Phonemes[0]
				ph.StartFrame = ph.EndFrame
				ph.SourceStart = ph.SourceEnd
				ph.SampleStart = ph.SampleEnd
			},
		},
		{
			name: "rate", code: domain.CodeRate, shard: 0,
			mut: func(p *domain.Project) { p.Render.SamplesPerFrame = 961 },
		},
		{
			name: "class", code: domain.CodeClass, shard: 2,
			mut: func(p *domain.Project) {
				p.Dialogues[2].Phonemes[0].Category = "whistle"
			},
		},
		{
			name: "stage", code: domain.CodeStage, shard: 2,
			opts: []func(*pipeline.Options){func(o *pipeline.Options) {
				o.FailShard = map[int]error{2: errors.New("injected shard fault")}
			}},
		},
		{
			name: "affinity", code: domain.CodeParam, shard: 3,
			mut: func(p *domain.Project) {
				p.Dialogues[3].Phonemes[0].Affinity = 1.5
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			proj := p.Clone()
			if tc.mut != nil {
				tc.mut(proj)
			}
			st, err := store.Open(filepath.Join(t.TempDir(), "db.sqlite"))
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			opts := pipeline.Options{
				Mode: pipeline.ModeFull, TruthFingerprint: m.Fingerprint,
			}
			for _, fn := range tc.opts {
				fn(&opts)
			}
			_, err = pipeline.Run(context.Background(), proj, st, opts)
			if domain.ErrorCode(err) != tc.code {
				t.Fatalf("code = %s, want %s: %v", domain.ErrorCode(err), tc.code, err)
			}
			de, _ := domain.AsError(err)
			if de.ShardIndex != tc.shard {
				t.Fatalf("shard = %d, want %d: %+v", de.ShardIndex, tc.shard, de)
			}
			if de.PhonemeID == "" {
				t.Fatalf("phoneme id missing: %+v", de)
			}
			if tc.code == domain.CodeRate || tc.code == domain.CodeStage ||
				tc.code == domain.CodeOrder || tc.code == domain.CodeClass ||
				tc.code == domain.CodeParam {
				count, err := st.Count(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				if tc.code == domain.CodeRate && count != 0 {
					t.Fatalf("rate failure committed %d shards", count)
				}
				if tc.code != domain.CodeRate && count != len(p.Dialogues)-1 {
					t.Fatalf("isolated failure committed %d shards, want %d", count, len(p.Dialogues)-1)
				}
			}
		})
	}
}

func TestCheckpointHashMismatchRecomputesShard(t *testing.T) {
	p, m := setup(t)
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "db.sqlite")
	runWithStore(t, p, dbPath, pipeline.ModeFull, m.Fingerprint)

	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB().Exec(`UPDATE shards SET content_hash='tampered' WHERE shard_index=1`); err != nil {
		t.Fatal(err)
	}
	_, err = pipeline.Run(context.Background(), p, st, pipeline.Options{
		Mode: pipeline.ModeRecovery, TruthFingerprint: m.Fingerprint,
	})
	if err != nil {
		t.Fatalf("recovery after tamper: %v", err)
	}
	_ = st.Close()
	if _, err := verify.VerifyDB(context.Background(), dbPath, m); err != nil {
		t.Fatalf("verify after tamper recovery: %v", err)
	}
}

func TestChangedSchedulingParametersInvalidateCheckpoints(t *testing.T) {
	p, m := setup(t)
	dbPath := filepath.Join(t.TempDir(), "db.sqlite")
	runWithStore(t, p, dbPath, pipeline.ModeFull, m.Fingerprint)
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	changed := p.Clone()
	changed.Render.FrameBudget++
	expected, err := pipeline.ExpectedShards(changed, m.Fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	invalid, err := st.ValidateExpected(context.Background(), expected)
	if err != nil {
		t.Fatal(err)
	}
	if len(invalid) != len(p.Dialogues) {
		t.Fatalf("parameter change invalidated %d shards, want %d", len(invalid), len(p.Dialogues))
	}
	for _, inv := range invalid {
		found := false
		for _, field := range inv.Fields {
			if field == "quantization_version" {
				found = true
			}
		}
		if !found {
			t.Fatalf("parameter mismatch did not report quantization_version: %+v", inv)
		}
	}
	count, _ := st.Count(context.Background())
	if count != 0 {
		t.Fatalf("checkpoints remain after parameter mismatch: %d", count)
	}
}

func TestSchedulerBuildRemainsTruthSourceForPipeline(t *testing.T) {
	p, m := setup(t)
	pkgs, err := schedule.Build(p)
	if err != nil {
		t.Fatal(err)
	}
	got, err := schedule.Schedule(p, pkgs)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Packages) != len(m.Packages) || len(got.Frames) != len(m.Frames) {
		t.Fatalf("pipeline kernel and truth diverged")
	}
}

func TestUnaffectedPackagesReusePersistedResults(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "reuse.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	a := domain.ShardArtifact{
		ShardIndex: 0, DialogueID: "d01", SourceVersion: "dialogue/v3",
		ContentHash: "hash", QuantVersion: "q", AlgorithmVersion: "a",
		TruthFingerprint: "truth",
		Packages: []domain.ScheduledPackage{
			{Package: domain.Package{ID: "keep", DialogueID: "d01", ShardIndex: 0}, Status: "accepted", DispatchStartFrame: 0, DispatchEndFrame: 2},
			{Package: domain.Package{ID: "drop", DialogueID: "d01", ShardIndex: 0}, Status: "accepted", DispatchStartFrame: 2, DispatchEndFrame: 4},
		},
		Frames: []domain.FrameRecord{
			{Frame: 0, PackageIDs: []string{"keep"}},
			{Frame: 1, PackageIDs: []string{"keep"}},
			{Frame: 2, PackageIDs: []string{"drop"}},
			{Frame: 3, PackageIDs: []string{"drop"}},
		},
	}
	if _, err := st.CommitShard(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	reuse, err := pipeline.ReusePersisted(context.Background(), st, &domain.Invalidation{
		ReusedPackages: []string{"keep"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(reuse.Packages) != 1 || reuse.Packages[0].ID != "keep" {
		t.Fatalf("reused packages = %+v", reuse.Packages)
	}
	if len(reuse.Frames) != 2 || reuse.Frames[0].PackageIDs[0] != "keep" {
		t.Fatalf("reused frames = %+v", reuse.Frames)
	}
}

func TestEditedSequenceIsIdenticalAcrossThreePaths(t *testing.T) {
	p, _ := setup(t)
	edited, err := edit.ApplySequence(p,
		domain.Edit{Kind: domain.EditBoundary, DialogueID: "d01", PhonemeID: "d01-p01", NewStartFrame: 0, NewEndFrame: 7},
		domain.Edit{Kind: domain.EditBoundary, DialogueID: "d01", PhonemeID: "d01-p02", NewStartFrame: 7, NewEndFrame: 13},
		domain.Edit{Kind: domain.EditWeight, DialogueID: "d02", PhonemeID: "d02-p01", NewWeight: 1500},
		domain.Edit{Kind: domain.EditFPS, NewFPS: 40},
	)
	if err != nil {
		t.Fatal(err)
	}
	editedFP, err := domain.Hash(edited)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	realtimeDB := filepath.Join(dir, "realtime.db")
	fullDB := filepath.Join(dir, "full.db")
	resumeDB := filepath.Join(dir, "resume.db")
	runWithStore(t, edited, realtimeDB, pipeline.ModeRealtime, editedFP)
	runWithStore(t, edited, fullDB, pipeline.ModeFull, editedFP)

	st, err := store.Open(resumeDB)
	if err != nil {
		t.Fatal(err)
	}
	_, err = pipeline.Run(context.Background(), edited, st, pipeline.Options{
		Mode: pipeline.ModeFull, TruthFingerprint: editedFP, StopAfterCommits: 2,
	})
	if !pipeline.IsCanceled(err) {
		t.Fatalf("edited partial run = %v", err)
	}
	_ = st.Close()
	runWithStore(t, edited, resumeDB, pipeline.ModeRecovery, editedFP)
	if _, err := verify.CompareDBs(context.Background(), realtimeDB, fullDB, resumeDB); err != nil {
		t.Fatalf("edited three-path comparison: %v", err)
	}
}
