package bake_test

import (
	"sync"
	"testing"

	"terrain-bake/bake"
)

func TestConcurrentSnapshotWritesRemainWhole(t *testing.T) {
	p, _ := loadProject(t)
	g, e := bake.NewGraph(p)
	if e != nil {
		t.Fatal(e)
	}
	params := bake.ParamsFromManifest(p.Manifest)
	artifacts := bakeAll(t, p)
	order := g.OrderAll()
	store := bake.NewSnapshotStore(t.TempDir())

	const writers = 12
	errs := make(chan error, writers)
	var wg sync.WaitGroup
	for i := 1; i <= writers; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := store.Write(p, g, params, uint64(i), order, artifacts)
			if err != nil {
				errs <- err
			} else {
				errs <- nil
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent snapshot write: %v", err)
		}
	}
	loaded := store.Load(p, g, params)
	if !loaded.Valid || len(loaded.Artifacts) != 6 {
		t.Fatalf("final snapshot is not a complete valid snapshot: %+v", loaded)
	}
	if loaded.Generation < 1 || loaded.Generation > writers {
		t.Fatalf("generation %d came from no complete writer", loaded.Generation)
	}
}
