package acousticfield

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func TestStoreRejectsOutOfOrderCommitAndInvalidatesSnapshot(t *testing.T) {
	sample, err := Generate(808)
	if err != nil {
		t.Fatal(err)
	}
	opts := DefaultOptions()
	dbPath := filepath.Join(t.TempDir(), "store.db")
	store, err := OpenStore(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	snapshot := SnapshotID(sample.Level, opts)
	_, err = store.EnsureSnapshot(context.Background(), SnapshotMeta{
		ID: snapshot, LevelID: sample.Level.ID, LevelHash: LevelContentHash(sample.Level),
		TruthFingerprint: sample.Truth.Fingerprint, ParamsVersion: opts.Version(), AlgorithmVersion: AlgorithmVersion,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.BeginCommitRound(context.Background(), snapshot, 1); err != nil {
		t.Fatal(err)
	}
	res := ChunkResult{ChunkID: 0, ContentHash: ChunkContentHash(&sample.Level.Chunks[0]), ParamsVersion: opts.Version(), AlgorithmVersion: AlgorithmVersion}
	res.PathDigest = PathDigest(res.Paths)
	if err := store.CommitChunk(context.Background(), snapshot, 1, 1, res, false); !IsCode(err, CodeState) {
		t.Fatalf("expected sequence E_STATE, got %v", err)
	}
	if err := store.CommitChunk(context.Background(), snapshot, 1, 0, res, false); err != nil {
		t.Fatal(err)
	}

	edited := CloneLevel(sample.Level)
	edited.Chunks[0].Reflectors[0].Material = "changed"
	_, valid, err := store.ValidateLoadedSnapshot(context.Background(), snapshot, edited, opts, sample.Truth.Fingerprint)
	if err == nil || valid {
		t.Fatalf("expected content mismatch invalidation, valid=%v err=%v", valid, err)
	}
	loaded, err := store.LoadResults(context.Background(), snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded) != 0 {
		t.Fatalf("snapshot not fully invalidated: %d rows", len(loaded))
	}
}

func TestDiagnosticPageAndAPI(t *testing.T) {
	sample, err := Generate(909)
	if err != nil {
		t.Fatal(err)
	}
	opts := DefaultOptions()
	e := newTestEngine(t, sample, filepath.Join(t.TempDir(), "ui.db"), opts)
	run, err := e.Rebuild(context.Background(), BuildOptions{})
	if err != nil {
		t.Fatal(err)
	}
	diag := NewDiagnosticServer(e)
	diag.SetComparison("incremental/full", CompareRunReports(run, run))
	ts := httptest.NewServer(diag)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	text := string(body)
	for _, needle := range []string{"Per-band arrival energy heatmap", "Boundary transfer flow", "Invalidation propagation", "Conservation", "Queue and cache detail", "MATCH", "PASS"} {
		if !strings.Contains(text, needle) {
			t.Fatalf("dashboard missing %q", needle)
		}
	}
	if !strings.Contains(text, "125Hz") || !strings.Contains(text, "2000Hz") {
		t.Fatal("dashboard missing band columns")
	}

	resp2, err := http.Get(ts.URL + "/api/report")
	if err != nil {
		t.Fatal(err)
	}
	defer resp2.Body.Close()
	var payload map[string]any
	if err := json.NewDecoder(resp2.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if payload["report"] == nil || payload["state"] == nil || payload["comparison"] == nil {
		t.Fatalf("incomplete API payload: %+v", payload)
	}

	diag.SetComparison("incremental/full", diffError(&Diff{ChunkID: 7, BandID: "500Hz", Field: "arrival_energy"}))
	resp3, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp3.Body.Close()
	body3, _ := io.ReadAll(resp3.Body)
	if !strings.Contains(string(body3), "DIFF") {
		t.Fatal("dashboard did not show comparison diff")
	}
}

func TestErrAbortedUnwrap(t *testing.T) {
	if !errors.Is(ErrAborted, ErrAborted) {
		t.Fatal("sentinel error broken")
	}
}
