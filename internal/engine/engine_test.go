package engine_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	_ "modernc.org/sqlite"

	"hdrproof/internal/apperr"
	"hdrproof/internal/compare"
	"hdrproof/internal/engine"
	"hdrproof/internal/model"
	"hdrproof/internal/sample"
	"hdrproof/internal/snapshot"
)

func setupSamples(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if _, err := sample.Generate(sample.GenerateOptions{Dir: dir, Seed: 42}); err != nil {
		t.Fatal(err)
	}
	return dir
}

func execute(t *testing.T, cfg engine.Config) (*engine.RunResult, error) {
	t.Helper()
	if cfg.Params == (model.Params{}) {
		cfg.Params = model.DefaultParams()
	}
	if cfg.MaxWorkers == 0 {
		cfg.MaxWorkers = 4
	}
	if cfg.FrameBufferPerWorker == 0 {
		cfg.FrameBufferPerWorker = 2
	}
	return engine.Execute(context.Background(), cfg)
}

func TestThreePathsEqualAndResumeIsFrozen(t *testing.T) {
	dir := setupSamples(t)
	tmp := t.TempDir()
	realtimeDB := filepath.Join(tmp, "realtime.db")
	resumeDB := filepath.Join(tmp, "resume.db")
	fullDB := filepath.Join(tmp, "full.db")

	rt, err := execute(t, engine.Config{DatasetDir: dir, DBPath: realtimeDB, Mode: engine.ModeRealtime})
	if err != nil {
		t.Fatal(err)
	}
	interrupted, err := execute(t, engine.Config{DatasetDir: dir, DBPath: resumeDB, Mode: engine.ModeResume, CancelAfterCommits: 5})
	if err != nil {
		t.Fatal(err)
	}
	if !interrupted.Cancelled || interrupted.Progress.SegmentsCommitted < 5 {
		t.Fatalf("interruption did not preserve checkpoints: %+v", interrupted)
	}
	resumed, err := execute(t, engine.Config{DatasetDir: dir, DBPath: resumeDB, Mode: engine.ModeResume})
	if err != nil {
		t.Fatal(err)
	}
	if !resumed.SnapshotReused {
		t.Fatalf("resume did not reuse snapshot: %+v", resumed)
	}
	full, err := execute(t, engine.Config{DatasetDir: dir, DBPath: fullDB, Mode: engine.ModeFull})
	if err != nil {
		t.Fatal(err)
	}
	for _, pair := range [][2]string{{realtimeDB, fullDB}, {resumeDB, fullDB}, {realtimeDB, resumeDB}} {
		result, err := compare.Snapshots(context.Background(), pair[0], pair[1])
		if err != nil {
			t.Fatalf("compare %v: %v", pair, err)
		}
		if !result.Equal || result.FramesCompared != 95 {
			t.Fatalf("paths differ: %+v", result)
		}
	}
	if rt.Progress.SegmentsCommitted != 18 || full.Progress.SegmentsCommitted != 18 ||
		resumed.Progress.SegmentsCommitted != 18 {
		t.Fatal("not all segments committed")
	}

	converted := 0
	frozen, err := execute(t, engine.Config{
		DatasetDir: dir, DBPath: fullDB, Mode: engine.ModeResume,
		EventSink: func(ev engine.Event) {
			if ev.Kind == "converted" {
				converted++
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !frozen.SnapshotReused || converted != 0 {
		t.Fatalf("committed frames were reprocessed: reused=%v converted=%d", frozen.SnapshotReused, converted)
	}
}

func TestInjectionChainsAndIsolation(t *testing.T) {
	dir := setupSamples(t)
	injectionsDir := filepath.Join(dir, "injections")
	cases := []struct {
		name string
		code apperr.Code
	}{
		{"bad_frame.json", apperr.CodeFrame},
		{"meta_conflict.json", apperr.CodeMeta},
		{"parameter_out_of_range.json", apperr.CodePara},
		{"segment_failure.json", apperr.CodeSeg},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			db := filepath.Join(t.TempDir(), "inject.db")
			result, err := execute(t, engine.Config{
				DatasetDir: dir, DBPath: db, Mode: engine.ModeFull,
				InjectionPath:            filepath.Join(injectionsDir, tc.name),
				AutoRetryInjectedFailure: true,
			})
			if err == nil {
				t.Fatal("expected controlled error")
			}
			if got := apperr.CodeOf(err); got != tc.code {
				t.Fatalf("expected %s received %s: %v", tc.code, got, err)
			}
			info, _ := apperr.As(err)
			if info.RouteID == "" || info.Field == "" {
				t.Fatalf("missing injection context: %+v", info)
			}
			if result == nil {
				t.Fatal("missing run result")
			}
			if tc.code == apperr.CodeSeg {
				if result.Progress.SegmentsCommitted != result.Progress.SegmentsTotal ||
					result.Progress.RoutesCommitted != result.Progress.RoutesTotal {
					t.Fatalf("retry did not complete: %+v", result.Progress)
				}
			} else {
				if result.Progress.SegmentsCommitted >= result.Progress.SegmentsTotal {
					t.Fatalf("injected failure did not isolate a segment: %+v", result.Progress)
				}
				if result.Progress.RoutesCommitted < 3 {
					t.Fatalf("other routes were polluted: %+v", result.Progress)
				}
			}
		})
	}
}

func TestVersionAndContentMismatchInvalidatesWholeSnapshot(t *testing.T) {
	dir := setupSamples(t)
	t.Run("version", func(t *testing.T) {
		dbPath := filepath.Join(t.TempDir(), "version.db")
		if _, err := execute(t, engine.Config{DatasetDir: dir, DBPath: dbPath, Mode: engine.ModeFull}); err != nil {
			t.Fatal(err)
		}
		db, err := sql.Open("sqlite", dbPath)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(`UPDATE snapshot_meta SET value='tampered' WHERE key='algorithm_version'`); err != nil {
			t.Fatal(err)
		}
		db.Close()
		result, err := execute(t, engine.Config{DatasetDir: dir, DBPath: dbPath, Mode: engine.ModeResume})
		if err != nil {
			t.Fatal(err)
		}
		if result.SnapshotReused || result.SnapshotReason != "version_mismatch" {
			t.Fatalf("snapshot was not invalidated: reused=%v reason=%s", result.SnapshotReused, result.SnapshotReason)
		}
		if result.Frames != 95 {
			t.Fatalf("full recompute produced %d frames", result.Frames)
		}
	})
	t.Run("content_hash", func(t *testing.T) {
		dbPath := filepath.Join(t.TempDir(), "content.db")
		if _, err := execute(t, engine.Config{DatasetDir: dir, DBPath: dbPath, Mode: engine.ModeFull}); err != nil {
			t.Fatal(err)
		}
		db, err := sql.Open("sqlite", dbPath)
		if err != nil {
			t.Fatal(err)
		}
		var pixels []byte
		if err := db.QueryRow(`SELECT pixels FROM frames WHERE route_id='arena-linear-bt2020' AND frame_index=0`).Scan(&pixels); err != nil {
			t.Fatal(err)
		}
		pixels[0] ^= 0xff
		if _, err := db.Exec(`UPDATE frames SET pixels=? WHERE route_id='arena-linear-bt2020' AND frame_index=0`, pixels); err != nil {
			t.Fatal(err)
		}
		db.Close()
		result, err := execute(t, engine.Config{DatasetDir: dir, DBPath: dbPath, Mode: engine.ModeResume})
		if err != nil {
			t.Fatal(err)
		}
		if result.SnapshotReused || !strings.Contains(result.SnapshotReason, "mismatch") {
			t.Fatalf("content mismatch did not invalidate: reused=%v reason=%s", result.SnapshotReused, result.SnapshotReason)
		}
		if result.Frames != 95 {
			t.Fatalf("full recompute produced %d frames", result.Frames)
		}
	})
}

func TestConcurrencyLimitsAndStableCommitOrder(t *testing.T) {
	dir := setupSamples(t)
	dbPath := filepath.Join(t.TempDir(), "parallel.db")
	result, err := execute(t, engine.Config{
		DatasetDir: dir, DBPath: dbPath, Mode: engine.ModeFull,
		MaxWorkers: 2, FrameBufferPerWorker: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.MaxWorkers != 2 || result.FrameBufferLimit != 1 {
		t.Fatalf("limits not reported: %+v", result)
	}
	if result.BufferBlocks == 0 {
		t.Fatal("bounded frame buffer never blocked")
	}
	store, err := snapshot.OpenReadOnly(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	segments, _, err := store.ReadAll(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ds, err := sample.LoadWithOptions(dir, sample.LoadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for routeOrder, rd := range ds.Routes {
		for seg := 0; seg < model.SegmentCount(rd.Route); seg++ {
			stored, ok := segments[snapshot.SegmentKey{RouteID: rd.Route.ID, Segment: seg}]
			if !ok {
				continue
			}
			want := int64(routeOrder)*1_000_000 + int64(seg)
			if stored.CommitOrder != want {
				t.Fatalf("route %s segment %d commit order %d != %d", rd.Route.ID, seg, stored.CommitOrder, want)
			}
		}
	}
}

func TestParamScheduleFreezesCommittedSegments(t *testing.T) {
	dir := setupSamples(t)
	dbPath := filepath.Join(t.TempDir(), "schedule.db")
	base := model.DefaultParams()
	updated := base
	updated.TargetPeakNits = 150
	schedule := []engine.ParamUpdate{{RouteID: "arena-linear-bt2020", FromSegment: 2, Params: updated}}
	result, err := execute(t, engine.Config{
		DatasetDir: dir, DBPath: dbPath, Mode: engine.ModeFull,
		Params: base, ParamSchedule: schedule,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Frames != 95 {
		t.Fatalf("unexpected frames: %d", result.Frames)
	}
	store, err := snapshot.OpenReadOnly(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	_, frames, err := store.ReadAll(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	store.Close()
	readProof := func(frame int) model.Proof {
		var proof model.Proof
		if err := json.Unmarshal(frames[snapshot.FrameKey{RouteID: "arena-linear-bt2020", Frame: frame}].ProofJSON, &proof); err != nil {
			t.Fatal(err)
		}
		return proof
	}
	if got, want := readProof(0).ParamVersion, model.ParamVersion(base); got != want {
		t.Fatalf("committed seg0 was rewritten: %s != %s", got, want)
	}
	if got, want := readProof(12).ParamVersion, model.ParamVersion(updated); got != want {
		t.Fatalf("uncommitted seg2 did not use updated params: %s != %s", got, want)
	}
	reused, err := execute(t, engine.Config{
		DatasetDir: dir, DBPath: dbPath, Mode: engine.ModeResume,
		Params: base, ParamSchedule: schedule,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reused.SnapshotReused {
		t.Fatalf("same parameter plan should reuse snapshot: %+v", reused)
	}
	invalidated, err := execute(t, engine.Config{
		DatasetDir: dir, DBPath: dbPath, Mode: engine.ModeResume, Params: base,
	})
	if err != nil {
		t.Fatal(err)
	}
	if invalidated.SnapshotReused || invalidated.SnapshotReason != "version_mismatch" {
		t.Fatalf("mixed parameter snapshot should invalidate: %+v", invalidated)
	}
}

func TestMissingFilesAndBadParams(t *testing.T) {
	dir := setupSamples(t)
	if _, err := execute(t, engine.Config{DatasetDir: filepath.Join(dir, "missing"), DBPath: filepath.Join(t.TempDir(), "x.db")}); err == nil {
		t.Fatal("expected missing dataset error")
	}
	p := model.DefaultParams()
	p.OutputBits = 10
	_, err := execute(t, engine.Config{DatasetDir: dir, DBPath: filepath.Join(t.TempDir(), "x.db"), Params: p})
	if err == nil || apperr.CodeOf(err) != apperr.CodePara {
		t.Fatalf("expected E_PARA, got %v", err)
	}
	_ = os.Remove(filepath.Join(dir, "injections", "bad_frame.json"))
}
