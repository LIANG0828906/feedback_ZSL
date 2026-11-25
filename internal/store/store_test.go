package store_test

import (
	"sync"
	"testing"

	"spectralforge/internal/domain"
	"spectralforge/internal/errs"
	"spectralforge/internal/sample"
	"spectralforge/internal/store"
)

func testManifest(t *testing.T) (store.Manifest, string) {
	t.Helper()
	dir := t.TempDir()
	b, err := sample.Generate(dir, 1)
	if err != nil {
		t.Fatal(err)
	}
	cfg := *b.Config
	m := store.Manifest{
		SchemaVersion:   1,
		ContentHash:     b.ContentHash,
		SceneHash:       b.SceneHash,
		SpectralHash:    b.SpectralHash,
		SamplingVersion: domain.SamplingVersion(b.Spectral, cfg),
		ParamVersion:    domain.ConfigVersion(cfg),
		Width:           16,
		Height:          16,
		TileSize:        8,
		TileCount:       8,
		MaterialKeys:    map[string]string{"mesh": "key"},
		Config:          cfg,
	}
	return m, b.ContentHash
}

func TestConcurrentTileCommitsAreValidJSONAndAtomic(t *testing.T) {
	m, _ := testManifest(t)
	root := t.TempDir()
	st, _, err := store.Prepare(root, m, false)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errsCh := make(chan error, m.TileCount)
	for i := 0; i < m.TileCount; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			linear := make([]float64, 8*8*3)
			for p := range linear {
				linear[p] = float64(index) + float64(p)*0.001
			}
			tile := store.TileSnapshot{
				SchemaVersion:   1,
				CommitSeq:       index,
				TileIndex:       index,
				TileX:           (index % 2) * 8,
				TileY:           (index / 2) * 8,
				TileW:           8,
				TileH:           8,
				ContentHash:     m.ContentHash,
				SceneHash:       m.SceneHash,
				SamplingVersion: m.SamplingVersion,
				ParamVersion:    m.ParamVersion,
				MaterialKeys:    m.MaterialKeys,
				LinearRGB:       linear,
				ObjectIDs:       make([]uint32, 64),
				PeakRadiance:    make([]float64, 64),
				Digest:          store.TileDigest(8, 8, linear),
			}
			errsCh <- st.CommitTile(tile)
		}(i)
	}
	wg.Wait()
	close(errsCh)
	for err := range errsCh {
		if err != nil {
			t.Fatalf("commit: %v", err)
		}
	}
	tiles, err := store.ReadTiles(root, m)
	if err != nil {
		t.Fatalf("tiles were not atomically readable: %v", err)
	}
	if len(tiles) != m.TileCount {
		t.Fatalf("got %d tiles", len(tiles))
	}
	commits, err := store.ReadCommits(root)
	if err != nil {
		t.Fatalf("commit log is not valid JSON lines: %v", err)
	}
	if len(commits) != m.TileCount {
		t.Fatalf("got %d commit records", len(commits))
	}
}

func TestPrepareInvalidatesOnParameterVersionMismatch(t *testing.T) {
	m, _ := testManifest(t)
	root := t.TempDir()
	if _, _, err := store.Prepare(root, m, false); err != nil {
		t.Fatal(err)
	}
	changed := m
	changed.ParamVersion = "different-param-version"
	_, result, err := store.Prepare(root, changed, true)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Invalidated || result.Reused != 0 || result.Reason == "" {
		t.Fatalf("expected full invalidation, got %+v", result)
	}
}

func TestCommitRejectsMixedVersionTile(t *testing.T) {
	m, _ := testManifest(t)
	root := t.TempDir()
	st, _, err := store.Prepare(root, m, false)
	if err != nil {
		t.Fatal(err)
	}
	linear := make([]float64, 8*8*3)
	tile := store.TileSnapshot{
		SchemaVersion:   1,
		CommitSeq:       0,
		TileIndex:       0,
		TileW:           8,
		TileH:           8,
		ContentHash:     m.ContentHash,
		SceneHash:       m.SceneHash,
		SamplingVersion: m.SamplingVersion,
		ParamVersion:    "other-version",
		MaterialKeys:    m.MaterialKeys,
		LinearRGB:       linear,
		ObjectIDs:       make([]uint32, 64),
		PeakRadiance:    make([]float64, 64),
		Digest:          store.TileDigest(8, 8, linear),
	}
	err = st.CommitTile(tile)
	e, ok := errs.As(err)
	if !ok || e.Code != errs.CodeVersion || e.ExpectedVersion != m.ParamVersion || e.ActualVersion != "other-version" {
		t.Fatalf("expected E_VER with both versions, got %v", err)
	}
}

func TestCommitRejectsMaterialKeyMix(t *testing.T) {
	m, _ := testManifest(t)
	root := t.TempDir()
	st, _, err := store.Prepare(root, m, false)
	if err != nil {
		t.Fatal(err)
	}
	linear := make([]float64, 8*8*3)
	tile := store.TileSnapshot{
		SchemaVersion:   1,
		CommitSeq:       0,
		TileIndex:       0,
		TileW:           8,
		TileH:           8,
		ContentHash:     m.ContentHash,
		SceneHash:       m.SceneHash,
		SamplingVersion: m.SamplingVersion,
		ParamVersion:    m.ParamVersion,
		MaterialKeys:    map[string]string{"mesh": "stale-quantization-key"},
		LinearRGB:       linear,
		ObjectIDs:       make([]uint32, 64),
		PeakRadiance:    make([]float64, 64),
		Digest:          store.TileDigest(8, 8, linear),
	}
	err = st.CommitTile(tile)
	e, ok := errs.As(err)
	if !ok || e.Code != errs.CodeQMix {
		t.Fatalf("expected E_QMIX, got %v", err)
	}
}
