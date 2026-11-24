package checkpoint

import (
	"context"
	"path/filepath"
	"sync"
	"testing"

	"economycore/model"
)

func TestMixedIdentityRejectedAndGloballyInvalidated(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "checkpoints.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	fpA := model.Fingerprint{ContentHash: "a", SourceVersion: "v1", ConversionVersion: "c", AlgorithmVersion: "a"}
	fpB := model.Fingerprint{ContentHash: "b", SourceVersion: "v1", ConversionVersion: "c", AlgorithmVersion: "a"}
	if _, err := store.CommitWindow(context.Background(), Record{Window: 0, Fingerprint: fpA, StateJSON: []byte(`{}`), ResultJSON: []byte(`{}`)}); err != nil {
		t.Fatal(err)
	}
	_, err = store.CommitWindow(context.Background(), Record{Window: 1, Fingerprint: fpB, StateJSON: []byte(`{}`), ResultJSON: []byte(`{}`)})
	if !model.IsCode(err, model.EVer) {
		t.Fatalf("expected E_VER while mixing identities, got %v", err)
	}
	recovery, err := store.LoadCompatible(context.Background(), fpB, 2)
	if err != nil {
		t.Fatal(err)
	}
	if !recovery.Invalidated || recovery.ValidWindows != 0 {
		t.Fatalf("expected global invalidation, got %+v", recovery)
	}
	if count, _ := store.Count(context.Background()); count != 0 {
		t.Fatalf("expected zero checkpoints, got %d", count)
	}
}

func TestFailureWritesAreSerialized(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "checkpoints.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	fp := model.Fingerprint{ContentHash: "a", SourceVersion: "v1", ConversionVersion: "c", AlgorithmVersion: "a"}
	var wg sync.WaitGroup
	for window := 0; window < 32; window++ {
		wg.Add(1)
		go func(window int) {
			defer wg.Done()
			if err := store.RecordFailure(context.Background(), window, fp, "E_UNIT", "failure"); err != nil {
				t.Errorf("window %d: %v", window, err)
			}
		}(window)
	}
	wg.Wait()
	failures, err := store.Failures(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(failures) != 32 {
		t.Fatalf("expected 32 failures, got %d", len(failures))
	}
	for i, failure := range failures {
		if failure.Window != i {
			t.Fatalf("failure order unstable at %d: %+v", i, failure)
		}
	}
}
