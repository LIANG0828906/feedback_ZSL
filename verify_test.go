package textureconv

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestIndependentVerifierDetectsFirstTruthViolation(t *testing.T) {
	b := testBundle(t, SamplePlan())
	rec := b.Manifest.Images[0]
	store, _ := openTestStore(t, t.TempDir(), "verify.db")
	e, err := NewEngine(b.Images[rec.ID], b.Plan, EngineOptions{
		ImageID: rec.ID, SourceHash: rec.SHA256, ParamsVersion: "vtest",
		TileSize: 24, Workers: 2, Store: store,
	})
	if err != nil {
		t.Fatal(err)
	}
	processAll(t, e)
	if _, err := VerifyPersisted(b, rec.ID, store, VerifyOptions{
		ExpectedParamsVersion: "vtest", ExpectedAlgorithmVersion: AlgorithmVersion,
	}); err != nil {
		store.Close()
		t.Fatal(err)
	}
	it, _ := b.ImageTruth(rec.ID)
	var sample TruthSample
	for _, s := range it.Samples {
		if s.Entry == 3 {
			sample = s
			break
		}
	}
	tiles, _ := MakeTiles(rec.Width, rec.Height, 24)
	tile, _ := tileContaining(tiles, sample.X, sample.Y)
	version, commits, err := store.ReadAll()
	if err != nil {
		store.Close()
		t.Fatal(err)
	}
	var target *TileCommit
	for i := range commits {
		if commits[i].Tile == tile.Index {
			target = &commits[i]
			break
		}
	}
	if target == nil {
		store.Close()
		t.Fatal("missing commit")
	}
	var recRow *StageRecord
	for i := range target.Stages {
		if target.Stages[i].Entry == 3 {
			recRow = &target.Stages[i]
			break
		}
	}
	if recRow == nil {
		store.Close()
		t.Fatal("missing stage row")
	}
	lx, ly := sample.X-tile.X, sample.Y-tile.Y
	off := (ly*tile.W + lx) * recRow.Channels
	if recRow.Pixels[off] == recRow.BitDepth.Max() {
		recRow.Pixels[off]--
	} else {
		recRow.Pixels[off]++
	}
	recRow.ContentHash = stageTileHash(recRow.StageImage(), tile)
	target.CommitHash = commitHash(target.Tile, target.Stages)
	if _, err := store.db.Exec(`UPDATE stage_results SET pixels=?,content_hash=? WHERE tile=? AND entry_seq=?`,
		pixelsToBytes(recRow.Pixels), recRow.ContentHash, target.Tile, 3); err != nil {
		store.Close()
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`UPDATE chunk_commits SET commit_hash=? WHERE tile=?`,
		target.CommitHash, target.Tile); err != nil {
		store.Close()
		t.Fatal(err)
	}
	_ = version
	err = func() error {
		_, err := VerifyPersisted(b, rec.ID, store, VerifyOptions{
			ExpectedParamsVersion: "vtest", ExpectedAlgorithmVersion: AlgorithmVersion,
		})
		return err
	}()
	eDiff := mustCode(t, err, CodeDiff)
	if eDiff.Entry != 3 || !eDiff.HasPixel || eDiff.Pixel.X != sample.X || eDiff.Pixel.Y != sample.Y ||
		eDiff.Field != "channels[0]" {
		store.Close()
		t.Fatalf("unexpected verifier diff context: %+v", eDiff)
	}
	store.Close()
}

func TestCompareSnapshotsReportsPixelEntryAndTile(t *testing.T) {
	b := testBundle(t, SamplePlan())
	rec := b.Manifest.Images[0]
	store, _ := openTestStore(t, t.TempDir(), "cmp.db")
	e, err := NewEngine(b.Images[rec.ID], b.Plan, EngineOptions{
		ImageID: rec.ID, SourceHash: rec.SHA256, ParamsVersion: "cmp",
		TileSize: 24, Workers: 2, Store: store,
	})
	if err != nil {
		t.Fatal(err)
	}
	processAll(t, e)
	a, err := e.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	store.Close()
	bb := *a
	bb.Stages = make([]*StageImage, len(a.Stages))
	for i, s := range a.Stages {
		bb.Stages[i] = s.Clone()
	}
	bb.Entries = append([]EntrySnapshot(nil), a.Entries...)
	bb.Submissions = append([]Submission(nil), a.Submissions...)
	bb.Tiles = append([]Tile(nil), a.Tiles...)
	bad := bb.Stages[1]
	x, y := 5, 7
	off := (y*bad.Width + x) * bad.Channels
	bad.Pixels[off] ^= 1
	err = CompareSnapshots(a, &bb)
	diff := mustCode(t, err, CodeDiff)
	if diff.Entry != 2 || diff.Pixel.X != x || diff.Pixel.Y != y || diff.Tile != tileAt(a.Tiles, x, y) {
		t.Fatalf("unexpected compare context: %+v", diff)
	}
	if len(diff.Left) == 0 || len(diff.Right) == 0 {
		t.Fatalf("missing sampled values: %+v", diff)
	}
}

func TestSampleManifestMismatchRejected(t *testing.T) {
	b := testBundle(t, SamplePlan())
	path := filepath.Join(b.Dir, "manifest.json")
	var m SampleManifest
	data := fileBytes(t, path)
	if err := json.Unmarshal(data, &m); err != nil {
		t.Fatal(err)
	}
	m.Images[0].Width++
	bad, _ := json.Marshal(m)
	if err := os.WriteFile(path, bad, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := LoadSample(b.Dir)
	e := mustCode(t, err, CodeParam)
	if e.Field != "geometry" {
		t.Fatalf("unexpected manifest error: %+v", e)
	}
}

func TestStageExportRoundTrip(t *testing.T) {
	b := testBundle(t, SamplePlan())
	rec := b.Manifest.Images[0]
	store, _ := openTestStore(t, t.TempDir(), "export.db")
	e, err := NewEngine(b.Images[rec.ID], b.Plan, EngineOptions{
		ImageID: rec.ID, SourceHash: rec.SHA256, ParamsVersion: "exp",
		TileSize: 24, Store: store,
	})
	if err != nil {
		t.Fatal(err)
	}
	processAll(t, e)
	final, err := e.FinalImage()
	if err != nil {
		store.Close()
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "final.tstage")
	if err := SaveStageFile(path, final); err != nil {
		store.Close()
		t.Fatal(err)
	}
	loaded, err := LoadStageFile(path)
	if err != nil {
		store.Close()
		t.Fatal(err)
	}
	store.Close()
	if HashStageImage(final) != HashStageImage(loaded) {
		t.Fatal("stage export round-trip changed pixels")
	}
}

func TestBufferLimiterBlocksAndRecordsWait(t *testing.T) {
	l := NewBufferLimiter(10)
	if err := l.Acquire(context.Background(), 8, "initial"); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		done <- l.Acquire(context.Background(), 6, "pixel scratch exhausted")
	}()
	// Let the waiter enqueue and record its reason.
	waitFor(t, func() bool { return len(l.Waits()) == 1 })
	l.Release(8)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	waits := l.Waits()
	if waits[0].Reason != "pixel scratch exhausted" || waits[0].Bytes != 6 {
		t.Fatalf("unexpected wait record: %+v", waits)
	}
	l.Release(6)
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	for i := 0; i < 1000; i++ {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("condition not reached")
}
