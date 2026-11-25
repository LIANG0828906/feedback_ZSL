package pipeline

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"testing"

	"meshdoctor/internal/sample"
	"meshdoctor/internal/store"
)

func TestThreePathsByteIdentical(t *testing.T) {
	samples, err := sample.GenerateAll(sample.DefaultSeed)
	if err != nil {
		t.Fatal(err)
	}
	for _, sm := range samples {
		sm := sm
		t.Run(sm.Manifest.ID, func(t *testing.T) {
			ctx := context.Background()
			dir := t.TempDir()
			params := DefaultParams()
			orch := NewOrchestrator()

			oneShot, err := orch.RunOneShot(sm.Manifest.ID, &sm.Mesh, &sm.Manifest, params)
			if err != nil {
				t.Fatal(err)
			}

			segStore, err := store.Open(filepath.Join(dir, "segmented.db"))
			if err != nil {
				t.Fatal(err)
			}
			segmented, err := orch.RunOrResume(ctx, sm.Manifest.ID, &sm.Mesh, &sm.Manifest, params, segStore, RunOptions{Path: "segmented"})
			if err != nil {
				t.Fatal(err)
			}
			if err := segStore.Close(); err != nil {
				t.Fatal(err)
			}

			resumeDB := filepath.Join(dir, "resume.db")
			prefixStore, err := store.Open(resumeDB)
			if err != nil {
				t.Fatal(err)
			}
			prefix, err := orch.RunOrResume(ctx, sm.Manifest.ID, &sm.Mesh, &sm.Manifest, params, prefixStore, RunOptions{
				Path: "resume-prefix", StopAfter: 2,
			})
			if err != nil {
				t.Fatal(err)
			}
			if len(prefix.CommittedSequence) != 2 {
				t.Fatalf("prefix sequence=%v", prefix.CommittedSequence)
			}
			if err := prefixStore.Close(); err != nil {
				t.Fatal(err)
			}

			resumeStore, err := store.Open(resumeDB)
			if err != nil {
				t.Fatal(err)
			}
			resume, err := orch.RunOrResume(ctx, sm.Manifest.ID, &sm.Mesh, &sm.Manifest, params, resumeStore, RunOptions{Path: "resume"})
			if err != nil {
				t.Fatal(err)
			}
			if resume.CacheHits != 2 {
				t.Fatalf("cache hits=%d want 2", resume.CacheHits)
			}
			if err := resumeStore.Close(); err != nil {
				t.Fatal(err)
			}

			diff, err := CompareThree(segmented, resume, oneShot)
			if err != nil {
				t.Fatalf("CompareThree: %v report=%+v", err, diff)
			}
			for i := range Stages {
				a := oneShot.Stages[i].Mesh.CanonicalBytes()
				b := segmented.Stages[i].Mesh.CanonicalBytes()
				c := resume.Stages[i].Mesh.CanonicalBytes()
				if !bytes.Equal(a, b) || !bytes.Equal(a, c) {
					t.Fatalf("stage %s canonical bytes differ", Stages[i])
				}
			}
			if oneShot.FinalHash != segmented.FinalHash || oneShot.FinalHash != resume.FinalHash {
				t.Fatalf("final hashes differ: %s %s %s", oneShot.FinalHash, segmented.FinalHash, resume.FinalHash)
			}
		})
	}
}

func TestCanceledContextDoesNotAdvance(t *testing.T) {
	samples, _ := sample.GenerateAll(sample.DefaultSeed)
	sm := samples[0]
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := NewOrchestrator().RunOrResume(ctx, sm.Manifest.ID, &sm.Mesh, &sm.Manifest, DefaultParams(), nil, RunOptions{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err=%v want context.Canceled", err)
	}
}

func TestBoundedParallelMatchesSerial(t *testing.T) {
	samples, err := sample.GenerateAll(sample.DefaultSeed)
	if err != nil {
		t.Fatal(err)
	}
	params := DefaultParams()
	params.MaxParallelism = 2
	params.BufferMeshesPerWorker = 1
	names := []string{samples[0].Manifest.ID, samples[1].Manifest.ID, samples[2].Manifest.ID}
	results, runErrs := RunOrdered(context.Background(), names, params, func(ctx context.Context, i int, name string) (*PathResult, error) {
		return NewEngine(samples[i].Manifest.ID, &samples[i].Manifest, params).RunAll(&samples[i].Mesh)
	})
	for i, err := range runErrs {
		if err != nil {
			t.Fatalf("job %d: %v", i, err)
		}
	}
	for i, r := range results {
		serial, err := NewEngine(samples[i].Manifest.ID, &samples[i].Manifest, params).RunAll(&samples[i].Mesh)
		if err != nil {
			t.Fatal(err)
		}
		if r.FinalHash != serial.FinalHash {
			t.Fatalf("sample %s parallel hash %s != serial %s", names[i], r.FinalHash, serial.FinalHash)
		}
		for stage := range Stages {
			if !bytes.Equal(r.Stages[stage].Mesh.CanonicalBytes(), serial.Stages[stage].Mesh.CanonicalBytes()) {
				t.Fatalf("sample %s stage %s differs under parallel execution", names[i], Stages[stage])
			}
		}
	}
}

func TestStateMachineLegalAndFrozen(t *testing.T) {
	if err := Transition(StatePending, StateRunning); err != nil {
		t.Fatal(err)
	}
	if err := Transition(StateRunning, StateCommitted); err != nil {
		t.Fatal(err)
	}
	if err := Transition(StateCommitted, StateRunning); err == nil {
		t.Fatal("committed -> running must be rejected")
	}
	if err := Transition(StateFailed, StateRunning); err != nil {
		t.Fatal(err)
	}
}
