package orchestrator

import (
	"database/sql"
	"encoding/json"
	"path/filepath"
	"testing"
)

func TestVerifierAcceptsCleanRun(t *testing.T) {
	p := genProject(t)
	store := openStore(t)
	if _, err := RunLive(p, store); err != nil {
		t.Fatalf("live run: %v", err)
	}
	if v := VerifyFromStore(p, store); v != nil {
		t.Fatalf("clean run must verify, got %v", v)
	}
}

func TestVerifierDetectsTamperedResult(t *testing.T) {
	p := genProject(t)
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "och.db")
	store, err := OpenCheckpointStore(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if _, lerr := RunLive(p, store); lerr != nil {
		t.Fatalf("live run: %v", lerr)
	}
	store.Close()

	// Tamper one persisted character interval directly in SQLite.
	raw, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	victim := p.Policy.QueueOrder[0]
	var payload string
	if err := raw.QueryRow(`SELECT payload FROM results WHERE dialogue_id=?`, victim).Scan(&payload); err != nil {
		t.Fatalf("read payload: %v", err)
	}
	var res DialogueResult
	if err := json.Unmarshal([]byte(payload), &res); err != nil {
		t.Fatalf("decode: %v", err)
	}
	res.Schedule.Chars[0].End++
	rawPayload, _ := json.Marshal(&res)
	if _, err := raw.Exec(`UPDATE results SET payload=? WHERE dialogue_id=?`, string(rawPayload), victim); err != nil {
		t.Fatalf("tamper: %v", err)
	}
	raw.Close()

	store2, err := OpenCheckpointStore(dbPath)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer store2.Close()
	v := VerifyFromStore(p, store2)
	if v == nil {
		t.Fatal("verifier must reject tampered result")
	}
	if v.DialogueID != victim {
		t.Fatalf("violation must name dialogue %s, got %s", victim, v.DialogueID)
	}
	if v.Item == "" {
		t.Fatal("violation must name the offending item")
	}
}

func TestVerifierDetectsQueueReorder(t *testing.T) {
	p := genProject(t)
	store := openStore(t)
	results, _, err := FullRecompute(p)
	if err != nil {
		t.Fatalf("full recompute: %v", err)
	}
	// Persist in reversed order to break the queue constraint.
	order := p.Policy.QueueOrder
	for i := len(order) - 1; i >= 0; i-- {
		if werr := store.WriteResult(results[order[i]]); werr != nil {
			t.Fatalf("write: %v", werr)
		}
	}
	v := VerifyFromStore(p, store)
	if v == nil || v.Item != "queue_order" {
		t.Fatalf("want queue_order violation, got %v", v)
	}
}
