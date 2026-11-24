package spectralrender

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func mustRender(t *testing.T, s loadedSamples, p RenderParams, opts RenderOptions) (*Frame, *RenderReport) {
	t.Helper()
	frame, report, err := Render(s.scene, s.hash, p, opts)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	return frame, report
}

func TestSerialParallelAndIncrementalPathsMatch(t *testing.T) {
	s := loadSamples(t)
	base := DefaultRenderParams()
	base.Version = 7

	serialParams := base
	serialParams.MaxParallel = 1
	serial, serialReport := mustRender(t, s, serialParams, RenderOptions{})
	if serialReport.ParallelSlots != 1 {
		t.Fatalf("serial report slots=%d", serialReport.ParallelSlots)
	}

	parallelParams := base
	parallelParams.MaxParallel = 4
	parallel, _ := mustRender(t, s, parallelParams, RenderOptions{})
	if !sameFloatBits(serial.LinearRGB, parallel.LinearRGB) ||
		!sameBytes(serial.EncodedRGB, parallel.EncodedRGB) {
		t.Fatal("parallel render differs from serial render")
	}

	store, err := OpenCheckpointStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	_, partialReport, partialErr := Render(s.scene, s.hash, parallelParams, RenderOptions{
		Store:   store,
		Context: ctx,
		OnTileCommitted: func(index int) {
			if index == 4 {
				cancel()
			}
		},
	})
	if CodeOf(partialErr) != ErrCancel {
		t.Fatalf("expected E_CANCEL from partial render, got %v", partialErr)
	}
	if partialReport == nil || len(partialReport.RenderedTiles) != 5 {
		t.Fatalf("expected five committed tiles before cancellation, got %+v", partialReport)
	}
	data, err := LoadSnapshot(store.Root())
	if err != nil {
		t.Fatal(err)
	}
	if len(data.Tiles) != 5 || data.Image != nil {
		t.Fatalf("partial checkpoint tiles=%d image=%v", len(data.Tiles), data.Image != nil)
	}

	incremental, incReport := mustRender(t, s, parallelParams, RenderOptions{Store: store})
	if len(incReport.ResumedTiles) != 5 {
		t.Fatalf("resumed tiles=%v, want 5", incReport.ResumedTiles)
	}
	if len(incReport.RenderedTiles) != 19 {
		t.Fatalf("rendered tiles=%v, want 19", incReport.RenderedTiles)
	}
	if !sameFloatBits(serial.LinearRGB, incremental.LinearRGB) ||
		!sameBytes(serial.EncodedRGB, incremental.EncodedRGB) {
		t.Fatal("incremental resume differs from full serial render")
	}
	check, cerr := CheckTruthFrame(incremental, s.truth)
	if cerr != nil || !check.Pass {
		t.Fatalf("truth check failed: %v %+v", cerr, check)
	}
}

func TestCheckpointInvalidationAndCommittedImmutability(t *testing.T) {
	s := loadSamples(t)
	store, err := OpenCheckpointStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	p1 := DefaultRenderParams()
	p1.Version = 1
	ctx, cancel := context.WithCancel(context.Background())
	_, firstReport, firstErr := Render(s.scene, s.hash, p1, RenderOptions{
		Store:   store,
		Context: ctx,
		OnTileCommitted: func(index int) {
			if index == 1 {
				cancel()
			}
		},
	})
	if CodeOf(firstErr) != ErrCancel {
		t.Fatalf("expected cancellation, got %v", firstErr)
	}
	cancel()
	oldID := store.SnapshotID()
	oldDir := store.SnapshotDir()
	oldManifest, err := readManifest(filepath.Join(oldDir, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(oldManifest.Tiles) != 2 {
		t.Fatalf("expected two committed old tiles, got %d", len(oldManifest.Tiles))
	}
	oldTilePath := filepath.Join(oldDir, filepath.FromSlash(oldManifest.Tiles[0].File))
	oldTileBytes, err := os.ReadFile(oldTilePath)
	if err != nil {
		t.Fatal(err)
	}
	oldKey := firstReport.MaterialCacheKey

	p2 := p1
	p2.Version = 2
	p2.VertexBitsOverride = 9
	frame2, report2 := mustRender(t, s, p2, RenderOptions{Store: store})
	if report2.Invalidation == nil {
		t.Fatal("expected snapshot invalidation after quantization/parameter change")
	}
	if report2.Invalidation.PreviousParamVersion != 1 || report2.Invalidation.NewParamVersion != 2 {
		t.Fatalf("bad invalidation versions: %+v", report2.Invalidation)
	}
	if report2.MaterialCacheKey == oldKey {
		t.Fatal("material cache key did not change with quantization parameters")
	}
	afterBytes, err := os.ReadFile(oldTilePath)
	if err != nil {
		t.Fatal(err)
	}
	if string(afterBytes) != string(oldTileBytes) {
		t.Fatal("a committed tile from the previous snapshot was silently rewritten")
	}
	if store.SnapshotID() == oldID {
		t.Fatal("new parameters reused the old snapshot")
	}
	if frame2.MaterialCacheKey != report2.MaterialCacheKey {
		t.Fatal("frame material key mismatch")
	}
}

func TestTileFailureCanBeRetriedWithoutCorruption(t *testing.T) {
	s := loadSamples(t)
	store, err := OpenCheckpointStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	p := DefaultRenderParams()
	p.Version = 3
	failing := errors.New("injected tile failure")
	_, failReport, failErr := Render(s.scene, s.hash, p, RenderOptions{
		Store: store,
		FailTile: func(tile Tile) error {
			if tile.Index == 3 {
				return failing
			}
			return nil
		},
	})
	if failErr == nil || failReport == nil || len(failReport.FailedTiles) != 1 || failReport.FailedTiles[0].Tile != 3 {
		t.Fatalf("expected failed tile 3, err=%v report=%+v", failErr, failReport)
	}
	data, err := LoadSnapshot(store.Root())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := data.Tiles[3]; ok {
		t.Fatal("failed tile was committed")
	}
	if len(data.Tiles) < 3 {
		t.Fatalf("successful earlier tiles were not committed: %d", len(data.Tiles))
	}

	frame, retryReport := mustRender(t, s, p, RenderOptions{Store: store})
	if len(retryReport.ResumedTiles) < 3 {
		t.Fatalf("retry did not resume earlier tiles: %v", retryReport.ResumedTiles)
	}
	check, cerr := CheckTruthFrame(frame, s.truth)
	if cerr != nil || !check.Pass {
		t.Fatalf("retry frame failed truth: %v %+v", cerr, check)
	}
}

func TestMemoryBudgetQueuesAndRecordsReason(t *testing.T) {
	s := loadSamples(t)
	normal := DefaultRenderParams()
	normal.Version = 4
	base, _ := mustRender(t, s, normal, RenderOptions{})
	limited := normal
	limited.MaxParallel = 4
	limited.MemoryBudgetBytes = 1
	frame, report := mustRender(t, s, limited, RenderOptions{})
	if !sameFloatBits(base.LinearRGB, frame.LinearRGB) {
		t.Fatal("memory budget affected pixel output")
	}
	if len(report.SchedulerEvents) == 0 {
		t.Fatal("expected scheduler budget events")
	}
	found := false
	for _, ev := range report.SchedulerEvents {
		if ev.Waited && strings.Contains(ev.Reason, "memory") {
			found = true
		}
	}
	if !found {
		t.Fatalf("no memory-budget waiting reason recorded: %+v", report.SchedulerEvents)
	}
	if report.MemorySlots != 1 {
		t.Fatalf("memory slots=%d, want clamped to 1", report.MemorySlots)
	}
}

func TestVerifierRecomputesAndLocatesMismatches(t *testing.T) {
	s := loadSamples(t)
	store, err := OpenCheckpointStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	p := DefaultRenderParams()
	p.Version = 5
	mustRender(t, s, p, RenderOptions{Store: store})

	report, verr := VerifyCheckpoint(s.scene, s.hash, s.truth, store.Root(), DefaultBoundModel())
	if verr != nil {
		t.Fatalf("verify failed: %v", verr)
	}
	if !report.OK || report.TilesChecked != 24 || report.BoundCheck == nil || !report.BoundCheck.Passed {
		t.Fatalf("unexpected verify report: %+v", report)
	}
	if report.BoundCheck.TruthObservedP95 > report.BoundCheck.Bound.LinearRGBAbs {
		t.Fatal("derived bound is below the observed truth quantile")
	}

	// Tamper a tile but recompute its digest so loading succeeds. The verifier
	// must recompute the spectral accumulation and locate the first RGB field.
	data, err := LoadSnapshot(store.Root())
	if err != nil {
		t.Fatal(err)
	}
	tileEntry := data.Manifest.Tiles[0]
	tilePath := filepath.Join(store.Root(), store.SnapshotID(), filepath.FromSlash(tileEntry.File))
	var tile TileResult
	raw, err := os.ReadFile(tilePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &tile); err != nil {
		t.Fatal(err)
	}
	tile.LinearRGB[0] += 0.25
	tile.Digest = ComputeTileDigest(&tile)
	raw, err = json.MarshalIndent(&tile, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tilePath, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	badReport, badErr := VerifyCheckpoint(s.scene, s.hash, s.truth, store.Root(), DefaultBoundModel())
	if badErr == nil || CodeOf(badErr) != ErrState {
		t.Fatalf("expected mismatch error, got %v", badErr)
	}
	if badReport == nil || badReport.FirstMismatch == nil {
		t.Fatal("missing first mismatch details")
	}
	m := badReport.FirstMismatch
	if m.Tile != 0 || m.Field != "linear_rgb[0]" || m.X != 0 || m.Y != 0 {
		t.Fatalf("bad mismatch: tile=%d field=%s pixel=(%d,%d)", m.Tile, m.Field, m.X, m.Y)
	}
}

func TestVerifierFindsFinalImageMismatchAndBounds(t *testing.T) {
	s := loadSamples(t)
	store, err := OpenCheckpointStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	p := DefaultRenderParams()
	p.Version = 6
	mustRender(t, s, p, RenderOptions{Store: store})

	imagePath := filepath.Join(store.Root(), store.SnapshotID(), "image.json")
	var img Frame
	raw, err := os.ReadFile(imagePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &img); err != nil {
		t.Fatal(err)
	}
	img.LinearRGB[1] += 0.125
	raw, _ = json.MarshalIndent(&img, "", "  ")
	if err := os.WriteFile(imagePath, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	report, verr := VerifyCheckpoint(s.scene, s.hash, s.truth, store.Root(), DefaultBoundModel())
	if verr == nil || report == nil || report.FirstMismatch == nil {
		t.Fatalf("expected final image mismatch, got %v %+v", verr, report)
	}
	if !strings.HasPrefix(report.FirstMismatch.Field, "image.") {
		t.Fatalf("expected an image field, got %s", report.FirstMismatch.Field)
	}

	// A deliberately impossible bound must return E_BOUND, not a silent pass.
	store2, err := OpenCheckpointStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	mustRender(t, s, p, RenderOptions{Store: store2})
	zeroModel := BoundModel{}
	_, boundErr := VerifyCheckpoint(s.scene, s.hash, s.truth, store2.Root(), zeroModel)
	if CodeOf(boundErr) != ErrBound {
		t.Fatalf("expected E_BOUND, got %v", boundErr)
	}
}

func TestIncompleteCheckpointCannotBeVerified(t *testing.T) {
	s := loadSamples(t)
	store, err := OpenCheckpointStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	p := DefaultRenderParams()
	ctx, cancel := context.WithCancel(context.Background())
	_, _, _ = Render(s.scene, s.hash, p, RenderOptions{
		Store:   store,
		Context: ctx,
		OnTileCommitted: func(index int) {
			if index == 2 {
				cancel()
			}
		},
	})
	_, verr := VerifyCheckpoint(s.scene, s.hash, s.truth, store.Root(), DefaultBoundModel())
	if CodeOf(verr) != ErrState {
		t.Fatalf("expected E_STATE for incomplete snapshot, got %v", verr)
	}
}

func TestCorruptCheckpointReturnsDecode(t *testing.T) {
	root := t.TempDir()
	store, err := OpenCheckpointStore(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "current.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = LoadSnapshot(store.Root())
	if CodeOf(err) != ErrDecode {
		t.Fatalf("expected E_DECODE, got %v", err)
	}
}
