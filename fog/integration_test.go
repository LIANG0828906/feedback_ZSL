package fog

import (
	"bytes"
	"os"
	"path/filepath"
	"sort"
	"testing"
)

func TestThreePathsPixelAndCommitEquality(t *testing.T) {
	s := smallScene(t)
	rep, err := RunThreePath(s, t.TempDir(), ThreePathOptions{
		ResumeFraction: 0.5, ShadowBudget: 32, MaxParallel: 2,
	})
	if err != nil {
		t.Fatalf("three paths diverged: %v", err)
	}
	if !rep.PixelsEqual || !rep.SummariesEqual || !rep.CommitEqual {
		t.Fatalf("three path mismatch: %+v", rep)
	}
	if rep.ResumeCommitted < 1 || rep.ResumeReloaded < 1 {
		t.Fatalf("resume path did not exercise checkpoint reuse: phase1=%d cache=%d", rep.ResumeCommitted, rep.ResumeReloaded)
	}
	if rep.CacheHits[2] != 0 {
		t.Fatalf("full recompute path unexpectedly hit cache: %d", rep.CacheHits[2])
	}
	if len(rep.CommitSequence) != len(rep.RealTime.Tiles) {
		t.Fatalf("commit sequence length=%d tiles=%d", len(rep.CommitSequence), len(rep.RealTime.Tiles))
	}
}

func TestCheckpointRejectsCorruptAndVersionMismatch(t *testing.T) {
	s := smallScene(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "atomic.vfogj")
	store, err := OpenCheckpointStore(path)
	if err != nil {
		t.Fatal(err)
	}
	frame := renderExact(t, NewEngine(s, store), RenderOptions{Mode: RenderExact})
	firstID := frame.CommitSequence[0]
	firstSeq := frame.CommitSequence
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	// Append a checksum-valid transaction whose tile content hash is wrong.
	store, err = OpenCheckpointStore(path)
	if err != nil {
		t.Fatal(err)
	}
	bad := frame.Tiles[0]
	for _, tile := range frame.Tiles {
		if tile.ID == firstID {
			bad = tile
			break
		}
	}
	bad.ContentHash = "tampered-content"
	if _, err := store.commit("tile", &bad, nil, ""); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = OpenCheckpointStore(path)
	if err != nil {
		t.Fatal(err)
	}
	all := store.AllTiles()
	if len(all) != len(frame.Tiles)-1 {
		t.Fatalf("corrupt tile not isolated: got %d tiles want %d", len(all), len(frame.Tiles)-1)
	}
	for _, tile := range all {
		if tile.ID == firstID {
			t.Fatalf("corrupt tile was replayed")
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	// A truncated final transaction must be ignored, preserving committed
	// transactions and stable order.
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString(`{"magic":"vfog-checkpoint-v1","txn":999`); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = OpenCheckpointStore(path)
	if err != nil {
		t.Fatal(err)
	}
	diag := store.Diagnostics()
	if !diag.TruncatedTail {
		t.Fatalf("truncated tail not detected: %+v", diag)
	}
	if len(store.CommitSequence()) != len(frame.Tiles)-1 {
		t.Fatalf("truncated tail affected committed tiles")
	}
	// Appending after recovery must remain a valid complete transaction.
	appendTile := all[0]
	if _, err := store.CommitTile(appendTile); err != nil {
		t.Fatalf("commit after truncated-tail recovery: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = OpenCheckpointStore(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(store.CommitSequence()) != len(frame.Tiles)-1 {
		t.Fatalf("post-recovery commit not replayed: %v", store.CommitSequence())
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	// Version mismatch invalidates every related tile on load.
	store, err = OpenCheckpointStore(path)
	if err != nil {
		t.Fatal(err)
	}
	changed := s.Clone()
	changed.Recipe.Version = "recipe-2"
	loaded, faults, _ := store.LoadFor(changed, frame.Tiles[0].Profile)
	if len(loaded) != 0 || len(faults) != len(frame.Tiles)-1 {
		t.Fatalf("version mismatch did not invalidate all tiles: loaded=%d faults=%d", len(loaded), len(faults))
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	// Commit order remains lexicographic even if callers submit in another
	// order.
	orderPath := filepath.Join(dir, "order.vfogj")
	orderStore, err := OpenCheckpointStore(orderPath)
	if err != nil {
		t.Fatal(err)
	}
	tiles := append([]TileResult(nil), frame.Tiles...)
	sort.Slice(tiles, func(i, j int) bool { return tiles[i].ID > tiles[j].ID })
	if _, err := orderStore.CommitTilesStable(tiles); err != nil {
		t.Fatal(err)
	}
	gotOrder := orderStore.CommitSequence()
	want := append([]string(nil), firstSeq...)
	sort.Strings(want)
	if !stringSlicesEqual(gotOrder, want) {
		t.Fatalf("stable commit order=%v want=%v", gotOrder, want)
	}
	if err := orderStore.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestCheckpointRoundTripSurvivesRestart(t *testing.T) {
	s := smallScene(t)
	path := filepath.Join(t.TempDir(), "restart.vfogj")
	store, err := OpenCheckpointStore(path)
	if err != nil {
		t.Fatal(err)
	}
	frame := renderExact(t, NewEngine(s, store), RenderOptions{Mode: RenderExact})
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store2, err := OpenCheckpointStore(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store2.Close()
	loaded, faults, _ := store2.LoadFor(s, frame.Tiles[0].Profile)
	if len(faults) != 0 {
		t.Fatalf("valid checkpoint rejected: %v", faults)
	}
	if len(loaded) != len(frame.Tiles) {
		t.Fatalf("loaded %d/%d tiles", len(loaded), len(frame.Tiles))
	}
	frame2 := renderExact(t, NewEngine(s, store2), RenderOptions{Mode: RenderExact})
	if frame2.Stats.CacheHits != len(frame.Tiles) {
		t.Fatalf("restart cache hits=%d want=%d", frame2.Stats.CacheHits, len(frame.Tiles))
	}
	if !framesHaveSameSummaries(frame, frame2) {
		t.Fatal("restart changed ray/tile summaries")
	}
}

func TestFaultInjectionChains(t *testing.T) {
	s := smallScene(t)
	decode := RunInjection(s, InjectDecode, "", "")
	if decode.Code != EDecode || decode.Fault == nil || decode.Fault.Path == "" {
		t.Fatalf("decode injection: %+v", decode)
	}
	order := RunInjection(s, InjectOrder, "", "")
	if order.Code != EOrder || order.Fault == nil || order.Fault.Path != "cameras[1].logical_time" {
		t.Fatalf("order injection: %+v", order)
	}
	density := RunInjection(s, InjectDensity, "", "")
	if density.Code != EDens || density.Fault == nil || density.Fault.MediaID == "" {
		t.Fatalf("density injection: %+v", density)
	}
	block := RunInjection(s, InjectBlock, "tile-r00-c00", "haze-west")
	if block.Code != EBlock || block.Fault == nil || block.Fault.TileID != "tile-r00-c00" || block.Fault.MediaID != "haze-west" {
		t.Fatalf("block injection: %+v", block)
	}
	if block.Surviving != 3 || block.Failed != 1 {
		t.Fatalf("block failure spread beyond one tile: %+v", block)
	}
	full := renderExact(t, NewEngine(s, nil), RenderOptions{Mode: RenderExact})
	for _, surviving := range block.Frame.Tiles {
		found := false
		for _, normal := range full.Tiles {
			if surviving.ID == normal.ID {
				if surviving.ContentHash != normal.ContentHash || surviving.SegmentDigest != normal.SegmentDigest {
					t.Fatalf("failed block polluted surviving tile %s", surviving.ID)
				}
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("surviving tile %s missing from baseline", surviving.ID)
		}
	}
}

func TestDecodeSceneRejectsMalformedJSON(t *testing.T) {
	_, f := DecodeScene(bytes.NewBufferString(`{"width":`))
	if f == nil || f.Code != EDecode {
		t.Fatalf("malformed JSON fault=%v", f)
	}
}
