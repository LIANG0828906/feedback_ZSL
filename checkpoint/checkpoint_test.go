package checkpoint

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"proxydiag/bizerr"
	"proxydiag/domain"
)

func testUpstreams() []domain.Upstream {
	return []domain.Upstream{
		{ID: "u-blue", Healthy: true},
		{ID: "u-green", Healthy: false},
	}
}

func decision(seq int, listener string) Decision {
	return Decision{
		Seq: seq, ListenerID: listener, RouteID: "R-" + listener,
		UpstreamID: "u-blue", RequestLine: "GET /backend HTTP/1.1",
		Terminal: "T_SUCCESS", ResultDigest: "digest",
	}
}

func TestCommitRecoveryOrdering(t *testing.T) {
	path := filepath.Join(t.TempDir(), "checkpoint.jsonl")
	store, err := NewStore(path, "v1", 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Commit(1, decision(1, "edge-api"), testUpstreams()); err != nil {
		t.Fatal(err)
	}
	if err := store.Commit(2, decision(2, "edge-ops"), testUpstreams()); err != nil {
		t.Fatal(err)
	}
	store.Close()

	f, _ := os.Open(path)
	scanner := bufio.NewScanner(f)
	var types []string
	for scanner.Scan() {
		var rec struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &rec); err != nil {
			t.Fatal(err)
		}
		types = append(types, rec.Type)
	}
	f.Close()
	want := []string{"prepare", "commit", "prepare", "commit"}
	if strings.Join(types, ",") != strings.Join(want, ",") {
		t.Fatalf("record order = %v, want %v", types, want)
	}

	state, err := Recover(path, "v1")
	if err != nil {
		t.Fatal(err)
	}
	if state.LastCommittedSeq != 2 || len(state.Decisions) != 2 {
		t.Fatalf("unexpected recovered state: %+v", state)
	}
	if state.ListenerCaches["edge-api"][1].RouteID != "R-edge-api" {
		t.Fatal("listener cache was not recovered")
	}
	if state.UpstreamHealth["u-green"] {
		t.Fatal("upstream health snapshot was not recovered")
	}
}

func TestUncommittedPrepareIsIgnored(t *testing.T) {
	path := filepath.Join(t.TempDir(), "checkpoint.jsonl")
	store, _ := NewStore(path, "v1", 0)
	if err := store.Commit(1, decision(1, "edge-api"), testUpstreams()); err != nil {
		t.Fatal(err)
	}
	// Simulate termination after the listener/upstream state write but before
	// the global sequence commit.
	if err := store.commitPrepareOnly(2, decision(2, "edge-ops"), testUpstreams()); err != nil {
		t.Fatal(err)
	}
	store.Close()
	state, err := Recover(path, "v1")
	if err != nil {
		t.Fatal(err)
	}
	if state.LastCommittedSeq != 1 {
		t.Fatalf("uncommitted prepare advanced global sequence: %d", state.LastCommittedSeq)
	}
	if _, ok := state.Decisions[2]; ok {
		t.Fatal("uncommitted request is visible")
	}
}

func TestTruncatedTailKeepsPriorCommitsVisible(t *testing.T) {
	path := filepath.Join(t.TempDir(), "checkpoint.jsonl")
	store, _ := NewStore(path, "v1", 0)
	if err := store.Commit(1, decision(1, "edge-api"), testUpstreams()); err != nil {
		t.Fatal(err)
	}
	store.Close()
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	if _, err := f.WriteString(`{"type":"prepare","version":"v1"`); err != nil {
		t.Fatal(err)
	}
	f.Close()
	state, err := Recover(path, "v1")
	if got := bizerr.Code(err); got != bizerr.CheckpointTruncated {
		t.Fatalf("expected %s, got %s (%v)", bizerr.CheckpointTruncated, got, err)
	}
	if state.LastCommittedSeq != 1 || len(state.Decisions) != 1 {
		t.Fatal("truncation made prior committed state unusable")
	}
}

func TestChecksumDamageKeepsPriorCommits(t *testing.T) {
	path := filepath.Join(t.TempDir(), "checkpoint.jsonl")
	store, _ := NewStore(path, "v1", 0)
	if err := store.Commit(1, decision(1, "edge-api"), testUpstreams()); err != nil {
		t.Fatal(err)
	}
	if err := store.commitPrepareOnly(2, decision(2, "edge-ops"), testUpstreams()); err != nil {
		t.Fatal(err)
	}
	store.Close()

	b, _ := os.ReadFile(path)
	lines := strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
	var prep prepareRecord
	if err := json.Unmarshal([]byte(lines[2]), &prep); err != nil {
		t.Fatal(err)
	}
	prep.Checksum = strings.Repeat("0", len(prep.Checksum))
	encoded, _ := json.Marshal(prep)
	lines[2] = string(encoded)
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	state, err := Recover(path, "v1")
	if got := bizerr.Code(err); got != bizerr.CheckpointChecksum {
		t.Fatalf("expected %s, got %s (%v)", bizerr.CheckpointChecksum, got, err)
	}
	if state.LastCommittedSeq != 1 {
		t.Fatal("checksum damage discarded prior commits")
	}
}

func TestVersionMismatchRejectsWholeCheckpoint(t *testing.T) {
	path := filepath.Join(t.TempDir(), "checkpoint.jsonl")
	store, _ := NewStore(path, "v1", 0)
	if err := store.Commit(1, decision(1, "edge-api"), testUpstreams()); err != nil {
		t.Fatal(err)
	}
	store.Close()
	state, err := Recover(path, "v2")
	if got := bizerr.Code(err); got != bizerr.CheckpointVersion {
		t.Fatalf("expected %s, got %s (%v)", bizerr.CheckpointVersion, got, err)
	}
	if len(state.Decisions) != 0 || state.LastCommittedSeq != 0 {
		t.Fatal("version-mismatched checkpoint was partially accepted")
	}
}

func TestSequenceMustAdvance(t *testing.T) {
	path := filepath.Join(t.TempDir(), "checkpoint.jsonl")
	store, _ := NewStore(path, "v1", 0)
	if err := store.Commit(1, decision(1, "edge-api"), testUpstreams()); err != nil {
		t.Fatal(err)
	}
	if err := store.Commit(1, decision(1, "edge-api"), testUpstreams()); bizerr.Code(err) != bizerr.CheckpointSequence {
		t.Fatalf("expected sequence error, got %v", err)
	}
	store.Close()
}
