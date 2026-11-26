package maskqc

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func testProject(t *testing.T) *Project {
	t.Helper()
	p, err := GenerateSample(DefaultSeed)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestSchemaVersionsAndRoundTrip(t *testing.T) {
	canonical := testProject(t)
	// Use a non-10-aligned weight so the persisted remainder path is exercised.
	canonical.Icon(HarnessIconID).Strokes[1].WeightPermyriad = 1234
	for _, version := range []string{SchemaV1, SchemaV2, SchemaV3} {
		data, report, err := ExportProject(canonical, version)
		if err != nil {
			t.Fatalf("export %s: %v", version, err)
		}
		if version != SchemaV3 && len(report.Lossy) == 0 {
			t.Fatalf("expected lossy trace for %s", version)
		}
		loaded, err := LoadProject(data)
		if err != nil {
			t.Fatalf("load %s: %v", version, err)
		}
		if len(loaded.Icons) != len(canonical.Icons) {
			t.Fatalf("%s icons = %d, want %d", version, len(loaded.Icons), len(canonical.Icons))
		}
		for _, wantIcon := range canonical.Icons {
			gotIcon := loaded.Icon(wantIcon.ID)
			if gotIcon == nil {
				t.Fatalf("%s missing icon %s", version, wantIcon.ID)
			}
			for _, wantStroke := range wantIcon.Strokes {
				var got *Stroke
				for _, s := range gotIcon.Strokes {
					if s.ID == wantStroke.ID {
						got = s
					}
				}
				if got == nil {
					t.Fatalf("%s missing stroke %s", version, wantStroke.ID)
				}
				if got.Start != wantStroke.Start || got.End != wantStroke.End {
					t.Fatalf("%s stroke %s coordinates changed: %+v -> %+v", version, wantStroke.ID, wantStroke, got)
				}
				if got.Seq != wantStroke.Seq || got.WeightPermyriad != wantStroke.WeightPermyriad {
					t.Fatalf("%s stroke %s seq/weight changed: seq %d/%d weight %d/%d", version, wantStroke.ID, got.Seq, wantStroke.Seq, got.WeightPermyriad, wantStroke.WeightPermyriad)
				}
			}
		}
	}
}

func TestSchemaErrorPaths(t *testing.T) {
	p := testProject(t)
	data, _, err := ExportProject(p, SchemaV1)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	doc["schema"] = "icon-project/v9"
	bad, _ := json.Marshal(doc)
	_, err = LoadProject(bad)
	var de *Error
	if !errors.As(err, &de) || de.Code != ESchema || de.FieldPath != "schema" {
		t.Fatalf("unknown schema error = %v", err)
	}

	data, _, err = ExportProject(p, SchemaV1)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	images := doc["images"].([]any)
	first := images[0].(map[string]any)
	delete(first, "image_id")
	bad, _ = json.Marshal(doc)
	_, err = LoadProject(bad)
	if !errors.As(err, &de) || de.Code != ESchema {
		t.Fatalf("missing field expected E_SCHEMA, got %v", err)
	}
	if de.FieldPath == "" || !contains(de.FieldPath, "image_id") {
		t.Fatalf("field path should identify image_id, got %q", de.FieldPath)
	}

	truthData, _, err := ExportProject(p, SchemaV1)
	if err != nil {
		t.Fatal(err)
	}
	var truthDoc map[string]any
	if err := json.Unmarshal(truthData, &truthDoc); err != nil {
		t.Fatal(err)
	}
	truthImage := truthDoc["images"].([]any)[0].(map[string]any)
	blocks := truthImage["truth_blocks"].([]any)
	block0 := blocks[0].(map[string]any)
	block0["sha256"] = "000000"
	badTruth, _ := json.Marshal(truthDoc)
	_, err = LoadProject(badTruth)
	if !errors.As(err, &de) || de.Code != ESchema || !contains(de.FieldPath, "sha256") {
		t.Fatalf("expected E_SCHEMA truth tile hash path, got %v", err)
	}
}

func TestIntegerConversionsRecordRemainderAndOverflow(t *testing.T) {
	q, rem, tr, err := ScalePermyriadToPermille(1234, "weight")
	if err != nil || q != 123 || rem != 4 || !tr.Truncated || tr.Residual != 4 {
		t.Fatalf("permille conversion q=%d rem=%d trace=%+v err=%v", q, rem, tr, err)
	}
	back, _, err := ScalePermilleToPermyriad(q, "weight")
	if err != nil || back != 1230 {
		t.Fatalf("reverse scale = %d, %v", back, err)
	}
	if _, _, _, err := AbsoluteToSequenceOffset(10, 1<<62, 4, "seq"); err == nil {
		t.Fatal("expected overflow for huge sequence")
	} else if de, ok := AsError(err); !ok || de.Code != EOVF || len(de.Operands) != 2 {
		t.Fatalf("expected E_OVF with operands, got %v", err)
	}
}

func TestInvalidEditDoesNotMutateStroke(t *testing.T) {
	p, err := HarnessProject(DefaultSeed)
	if err != nil {
		t.Fatal(err)
	}
	icon := p.Icon(HarnessIconID)
	var before int
	for _, s := range icon.Strokes {
		if s.ID == "bars-mid" {
			before = s.WeightPermyriad
		}
	}
	bad := 99999
	_, _, err = ApplyEdit(p, HarnessIconID, Edit{Type: EditUpdate, StrokeID: "bars-mid", NewWeight: &bad})
	if de, ok := AsError(err); !ok || de.Code != EPara {
		t.Fatalf("expected E_PARA, got %v", err)
	}
	for _, s := range icon.Strokes {
		if s.ID == "bars-mid" && s.WeightPermyriad != before {
			t.Fatalf("invalid edit mutated weight %d -> %d", before, s.WeightPermyriad)
		}
	}
}

func TestThreePathsPixelAndSequenceConsistent(t *testing.T) {
	workDir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	report, err := RunThreePaths(ctx, ThreePathOptions{
		WorkDir: workDir, CrashAfter: 1,
		Budget: BudgetConfig{MaxParallel: 2, MaxPixelsPerWorker: 4096, MaxQueuedTiles: 4},
	})
	if err != nil {
		t.Fatalf("three paths failed: %v\nreport diff=%+v", err, report.Diff)
	}
	if report.Diff != nil {
		t.Fatalf("unexpected diff: %+v", report.Diff)
	}
	full := report.Full.Icons[HarnessIconID]
	if full.Accumulated == nil || len(full.Accumulated.Alpha) == 0 {
		t.Fatal("full path missing accumulated mask")
	}
	if len(report.Incremental.EventSequence) != len(report.Recovery.EventSequence) || len(report.Full.EventSequence) != 6 {
		t.Fatalf("event lengths inc=%d rec=%d full=%d", len(report.Incremental.EventSequence), len(report.Recovery.EventSequence), len(report.Full.EventSequence))
	}

	// Incremental edits must actually reuse unaffected persisted tiles.
	lastStep := report.Incremental.Steps[len(report.Incremental.Steps)-1]
	if len(lastStep.Invalidation.ReusableTiles) == 0 {
		t.Fatalf("expected reusable tiles, invalidation=%+v", lastStep.Invalidation)
	}
	reused := 0
	for _, tile := range report.Incremental.Icons[HarnessIconID].Tiles {
		if tile.Reused {
			reused++
		}
	}
	if reused == 0 {
		t.Fatal("expected at least one persisted tile reuse on final incremental build")
	}

	// DB files are real SQLite databases and commit history is readable.
	if _, err := os.Stat(filepath.Join(workDir, "incremental-checkpoint.db")); err != nil {
		t.Fatal(err)
	}
	if len(report.Recovery.Commits) != 6 {
		t.Fatalf("recovery commits = %d, want 6", len(report.Recovery.Commits))
	}
}

func TestComparePathResultsLocatesEDiff(t *testing.T) {
	ctx := context.Background()
	p, err := HarnessProject(DefaultSeed)
	if err != nil {
		t.Fatal(err)
	}
	engine := NewEngine(nil, DefaultBudgetConfig(), p.ConvertTrace)
	icon := p.Icon(HarnessIconID)
	expected, err := engine.BuildIconResult(ctx, icon, false)
	if err != nil {
		t.Fatal(err)
	}
	actualMask := expected.Accumulated.Clone()
	actualMask.Alpha[10*icon.Width+10] ^= 255
	tx, ty := 10/icon.TileWidth, 10/icon.TileHeight
	tileID := TileID(icon.ID, tx, ty)
	r := TileRect(icon, tx, ty)
	tiles := map[string]*TileOutput{}
	for id, tile := range expected.Tiles {
		if id == tileID {
			copied := *tile
			crop, _ := CropMask(actualMask, r)
			copied.Mask = crop
			tiles[id] = &copied
		} else {
			tiles[id] = tile
		}
	}
	actual := &PathResult{IconOrder: []string{icon.ID}, Icons: map[string]*IconResult{
		icon.ID: {
			IconID: icon.ID, TileOrder: expected.TileOrder, Tiles: tiles,
			Accumulated: actualMask, Analysis: expected.Analysis,
		},
	}}
	de := ComparePathResults(&PathResult{IconOrder: []string{icon.ID}, Icons: map[string]*IconResult{icon.ID: expected}}, actual)
	if de == nil || de.Code != EDiff {
		t.Fatalf("expected E_DIFF, got %v", de)
	}
	if de.Details["field"] != "alpha" || de.Details["x"] != 10 || de.Details["y"] != 10 {
		t.Fatalf("diff location details wrong: %+v", de.Details)
	}
}

func TestBudgetQueueBlocksRecordsWaitAndIsolatesFailures(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{}, 1)
	s := NewScheduler(BudgetConfig{MaxParallel: 1, MaxPixelsPerWorker: 64, MaxQueuedTiles: 1})
	jobs := []TileJob{
		{ID: "tile-blocked", Pixels: 1, Run: func(context.Context) error {
			started <- struct{}{}
			<-release
			return nil
		}},
		{ID: "tile-ok-1", Pixels: 1, Run: func(context.Context) error { return nil }},
		{ID: "tile-fail", Pixels: 1, Run: func(context.Context) error { return errors.New("boom") }},
		{ID: "tile-ok-2", Pixels: 1, Run: func(context.Context) error { return nil }},
	}
	done := make(chan []JobResult, 1)
	go func() { done <- s.Run(context.Background(), jobs) }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("blocked job did not start")
	}
	time.Sleep(30 * time.Millisecond)
	close(release)
	select {
	case results := <-done:
		failures := 0
		success := 0
		for _, r := range results {
			if r.OK {
				success++
			} else {
				failures++
			}
		}
		if failures != 1 || success != 3 {
			t.Fatalf("results success=%d failures=%d: %+v", success, failures, results)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("scheduler did not finish after release")
	}
	snap := s.Snapshot()
	if len(snap.Waits) == 0 {
		t.Fatal("expected queue/parallel wait records under exhausted budget")
	}

	oversized := s.Run(context.Background(), []TileJob{{ID: "big", Pixels: 128, Run: func(context.Context) error { return nil }}})
	if oversized[0].OK || oversized[0].Code != string(EBudget) {
		t.Fatalf("expected E_BUDGET buffer cap failure, got %+v", oversized[0])
	}
}

func TestCheckpointCorruptionRejected(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "cp.db")
	store, err := OpenStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	meta := RecoveryMeta{ProjectHash: "hash", OrderVersion: OrderRulesVersion, ConversionVersion: ConversionParamsVersion, AlgorithmVersion: AlgorithmVersion}
	if err := store.InitializeMeta(meta); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CommitStroke(context.Background(), StrokeCommit{
		EventKey: "evt-0001:s", IconID: "icon", StrokeID: "s", StrokeVersion: "h", OrderIndex: 1,
		Components: []ComponentRecord{{TileID: "t", StrokeID: "s", CacheKey: "k", ComponentHash: HashBytes([]byte("alpha")), InputHash: "in", ResidualDigest: "r", OrderVersion: OrderRulesVersion, ConversionVersion: ConversionParamsVersion, AlgorithmVersion: AlgorithmVersion, Width: 1, Height: 1, Alpha: []byte("alpha")}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store2, err := OpenStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store2.Close()
	if err := store2.ValidateRecovery(meta); err != nil {
		t.Fatalf("valid checkpoint rejected: %v", err)
	}
	if err := store2.CorruptComponent(); err != nil {
		t.Fatal(err)
	}
	err = store2.ValidateRecovery(meta)
	if de, ok := AsError(err); !ok || de.Code != ECorrupt || de.Details["rebuild_required"] != true {
		t.Fatalf("expected E_CORRUPT rebuild requirement, got %v", err)
	}
}

func TestTopologyHoleMismatchReturnsETop(t *testing.T) {
	p := testProject(t)
	icon := p.Icon("glyph-ring-32")
	acc := icon.Truth.Clone()
	// Fill the central hole and overlap the ring wall, changing holes 1 -> 0.
	for y := 11; y < 22; y++ {
		for x := 11; x < 22; x++ {
			acc.Alpha[y*acc.Width+x] = 255
		}
	}
	rep, err := CompareTopology(acc, icon.Truth, icon.Strokes, DefaultTopologyTolerance())
	if err != nil {
		t.Fatal(err)
	}
	topErr := rep.Error()
	if de, ok := AsError(topErr); !ok || de.Code != ETop {
		t.Fatalf("expected E_TOP, got %v", topErr)
	}
	foundHole := false
	for _, issue := range rep.Issues {
		if issue.Field == "hole_count" {
			foundHole = true
			if issue.DomainNo == 0 || issue.Actual != 0 {
				t.Fatalf("bad hole issue: %+v", issue)
			}
		}
	}
	if !foundHole {
		t.Fatalf("expected hole_count issue, got %+v", rep.Issues)
	}
}

func TestEngineFailedTileIsolatedAndRetryPersists(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenStore(filepath.Join(dir, "tile.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	p, err := HarnessProject(DefaultSeed)
	if err != nil {
		t.Fatal(err)
	}
	icon := p.Icon(HarnessIconID)
	if err := store.InitializeMeta(metaFor(p)); err != nil {
		t.Fatal(err)
	}
	engine := NewEngine(store, BudgetConfig{MaxParallel: 2, MaxPixelsPerWorker: 4096, MaxQueuedTiles: 2}, p.ConvertTrace)
	tileID := icon.TileIDs()[0]
	engine.FailTileOnce(tileID)
	res, err := engine.BuildIconResult(context.Background(), icon, false)
	if err == nil {
		t.Fatal("expected tile failure")
	}
	if res == nil || len(res.TileErrors) != 1 {
		t.Fatalf("expected exactly one isolated tile error, got %+v err=%v", res, err)
	}
	if res.TileErrors[0].TileID != tileID {
		t.Fatalf("failed tile = %q, want %q", res.TileErrors[0].TileID, tileID)
	}
	retry, err := engine.BuildIconResult(context.Background(), icon, false)
	if err != nil {
		t.Fatalf("retry should succeed: %v", err)
	}
	if len(retry.TileErrors) != 0 || retry.Accumulated == nil {
		t.Fatalf("retry result bad: errors=%+v mask=%v", retry.TileErrors, retry.Accumulated != nil)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
