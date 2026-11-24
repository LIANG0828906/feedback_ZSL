package checkpoint

import (
	"context"
	"database/sql"
	"path/filepath"
	"reflect"
	"sync"
	"testing"

	"spatialworkbench/internal/acoustics"
	"spatialworkbench/internal/generator"
	"spatialworkbench/internal/model"
	"spatialworkbench/internal/werr"
)

func generationFor(sample model.Sample, params model.Params) Generation {
	return Generation{
		InputHash:  model.SampleHash(sample),
		ParamsHash: params.Hash(sample),
		Algorithm:  model.AlgorithmVersion,
		LayoutHash: model.LayoutHash(sample.Layout),
	}
}

func fakeResult(frame int, sample model.Sample, params model.Params) acoustics.FrameResult {
	return acoustics.FrameResult{
		Frame:      frame,
		Params:     params,
		Render:     [][]float64{{float64(frame) + 0.25}, {float64(frame) + 0.5}},
		InputHash:  model.SampleHash(sample),
		ParamsHash: params.Hash(sample),
		Algorithm:  model.AlgorithmVersion,
	}
}

func TestCommitOrderAndAtomicAppend(t *testing.T) {
	sample := generator.Generate(generator.Options{Seed: 1, Samples: 1}).Samples[0]
	params := model.DefaultParams(sample)
	store, err := Open(filepath.Join(t.TempDir(), "order.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	generation := generationFor(sample, params)
	if err := store.Commit(context.Background(), generation, fakeResult(1, sample, params)); err == nil || err.Code != werr.CodeState {
		t.Fatalf("out-of-order commit must fail, got %#v", err)
	}
	if err := store.Commit(context.Background(), generation, fakeResult(0, sample, params)); err != nil {
		t.Fatal(err)
	}
	if err := store.Commit(context.Background(), generation, fakeResult(0, sample, params)); err == nil || err.Code != werr.CodeState {
		t.Fatalf("duplicate commit must fail, got %#v", err)
	}
	loaded, err := store.Load(context.Background(), generation)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Records) != 1 || loaded.Records[0].Frame != 0 {
		t.Fatalf("records %#v", loaded.Records)
	}
}

func TestConcurrentCommitIsSerialized(t *testing.T) {
	sample := generator.Generate(generator.Options{Seed: 2, Samples: 1}).Samples[0]
	params := model.DefaultParams(sample)
	store, err := Open(filepath.Join(t.TempDir(), "concurrent.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	generation := generationFor(sample, params)
	var wg sync.WaitGroup
	results := make([]*werr.Error, 2)
	for i := 0; i < 2; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = store.Commit(context.Background(), generation, fakeResult(0, sample, params))
		}()
	}
	wg.Wait()
	successes := 0
	for _, result := range results {
		if result == nil {
			successes++
		}
	}
	if successes != 1 {
		t.Fatalf("successful concurrent commits = %d, want 1", successes)
	}
	loaded, err := store.Load(context.Background(), generation)
	if err != nil || len(loaded.Records) != 1 {
		t.Fatalf("load after concurrent commit: %#v %v", loaded, err)
	}
}

func TestVersionMismatchInvalidatesWholeCheckpoint(t *testing.T) {
	sample := generator.Generate(generator.Options{Seed: 3, Samples: 1}).Samples[0]
	params := model.DefaultParams(sample)
	generation := generationFor(sample, params)
	path := filepath.Join(t.TempDir(), "invalidate.sqlite")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Commit(context.Background(), generation, fakeResult(0, sample, params)); err != nil {
		t.Fatal(err)
	}
	if err := store.Commit(context.Background(), generation, fakeResult(1, sample, params)); err != nil {
		t.Fatal(err)
	}
	store.Close()

	paramChanged := params.Clone()
	paramChanged.InterpOrder = 2
	store, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := store.Load(context.Background(), generationFor(sample, paramChanged))
	if err != nil {
		t.Fatal(err)
	}
	if !loaded.Invalidated || len(loaded.Records) != 0 {
		t.Fatalf("expected full invalidation, got %#v", loaded)
	}
	loaded, err = store.Load(context.Background(), generation)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Records) != 0 {
		t.Fatalf("old rows survived invalidation: %#v", loaded)
	}
	store.Close()
}

func TestAlgorithmMismatchInvalidates(t *testing.T) {
	sample := generator.Generate(generator.Options{Seed: 4, Samples: 1}).Samples[0]
	params := model.DefaultParams(sample)
	generation := generationFor(sample, params)
	path := filepath.Join(t.TempDir(), "algorithm.sqlite")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Commit(context.Background(), generation, fakeResult(0, sample, params)); err != nil {
		t.Fatal(err)
	}
	store.Close()

	db, dbErr := sql.Open("sqlite", path)
	if dbErr != nil {
		t.Fatal(dbErr)
	}
	if _, dbErr := db.Exec(`UPDATE checkpoint_meta SET algorithm_version='other' WHERE id=1`); dbErr != nil {
		db.Close()
		t.Fatal(dbErr)
	}
	db.Close()

	store, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	loaded, err := store.Load(context.Background(), generation)
	if err != nil {
		t.Fatal(err)
	}
	if !loaded.Invalidated || len(loaded.Records) != 0 {
		t.Fatalf("expected algorithm invalidation, got %#v", loaded)
	}
}

func TestInputMismatchInvalidates(t *testing.T) {
	sample := generator.Generate(generator.Options{Seed: 5, Samples: 1}).Samples[0]
	params := model.DefaultParams(sample)
	generation := generationFor(sample, params)
	path := filepath.Join(t.TempDir(), "input.sqlite")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Commit(context.Background(), generation, fakeResult(0, sample, params)); err != nil {
		t.Fatal(err)
	}
	store.Close()

	changed := sample.Clone()
	changed.Dry.Samples[0] += 0.125
	store, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	loaded, err := store.Load(context.Background(), generationFor(changed, params))
	if err != nil {
		t.Fatal(err)
	}
	if !loaded.Invalidated || len(loaded.Records) != 0 {
		t.Fatalf("expected input hash invalidation, got %#v", loaded)
	}
}

func TestRenderBlobRoundTrip(t *testing.T) {
	render := [][]float64{{1.5, -2.25, 0}, {0.1, 0.2, 0.3}}
	blob, err := encodeRender(render)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := decodeRender(blob)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(render, decoded) {
		t.Fatalf("round trip changed samples: %#v != %#v", render, decoded)
	}
}
