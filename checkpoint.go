package orchestrator

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"sync"

	_ "modernc.org/sqlite"
)

// AlgoVersion identifies the scheduling algorithm; bumping it
// invalidates all persisted checkpoints.
const AlgoVersion = "1.0.0"

// PolicyVersion identifies the strategy parameter schema.
const PolicyVersion = "1"

// CheckpointStore persists per-dialogue orchestration results in
// SQLite. All writes are serialized through a mutex and committed in
// a stable sequence; each dialogue is written atomically in its own
// transaction.
type CheckpointStore struct {
	db  *sql.DB
	mu  sync.Mutex
	seq int64
}

// OpenCheckpointStore opens (and initializes) the checkpoint database.
func OpenCheckpointStore(path string) (*CheckpointStore, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // serialize all access at the driver level
	s := &CheckpointStore{db: db}
	if err := s.init(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *CheckpointStore) init() error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS meta (key TEXT PRIMARY KEY, value TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS results (
			dialogue_id TEXT PRIMARY KEY,
			queue_pos INTEGER NOT NULL,
			seq INTEGER NOT NULL,
			payload TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS commit_log (
			seq INTEGER PRIMARY KEY,
			dialogue_id TEXT NOT NULL
		)`,
	}
	for _, q := range stmts {
		if _, err := s.db.Exec(q); err != nil {
			return err
		}
	}
	var maxSeq sql.NullInt64
	if err := s.db.QueryRow(`SELECT MAX(seq) FROM commit_log`).Scan(&maxSeq); err != nil {
		return err
	}
	if maxSeq.Valid {
		s.seq = maxSeq.Int64
	}
	return nil
}

// Close closes the underlying database.
func (s *CheckpointStore) Close() error { return s.db.Close() }

// SaveMeta records the content hash and version stamps.
func (s *CheckpointStore) SaveMeta(contentHash string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	kv := map[string]string{
		"content_hash":   contentHash,
		"policy_version": PolicyVersion,
		"algo_version":   AlgoVersion,
	}
	for k, v := range kv {
		if _, err := tx.Exec(`INSERT INTO meta(key,value) VALUES(?,?)
			ON CONFLICT(key) DO UPDATE SET value=excluded.value`, k, v); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// LoadMeta returns the stored meta values.
func (s *CheckpointStore) LoadMeta() (map[string]string, error) {
	rows, err := s.db.Query(`SELECT key, value FROM meta`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, rows.Err()
}

// CheckValidity compares stored hash/versions with the current
// project. Any mismatch invalidates the whole snapshot: partial
// mixing of stale results is forbidden.
func (s *CheckpointStore) CheckValidity(contentHash string) (bool, error) {
	meta, err := s.LoadMeta()
	if err != nil {
		return false, err
	}
	valid := meta["content_hash"] == contentHash &&
		meta["policy_version"] == PolicyVersion &&
		meta["algo_version"] == AlgoVersion &&
		contentHash != ""
	if !valid {
		if err := s.Invalidate(); err != nil {
			return false, err
		}
	}
	return valid, nil
}

// Invalidate drops all persisted results and the commit log.
func (s *CheckpointStore) Invalidate() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, q := range []string{`DELETE FROM results`, `DELETE FROM commit_log`} {
		if _, err := tx.Exec(q); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.seq = 0
	return nil
}

// WriteResult atomically appends one dialogue result. Writes are
// serialized and the commit sequence is stable.
func (s *CheckpointStore) WriteResult(res *DialogueResult) error {
	payload, err := json.Marshal(res)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	s.seq++
	if _, err := tx.Exec(`INSERT INTO results(dialogue_id, queue_pos, seq, payload)
		VALUES(?,?,?,?)
		ON CONFLICT(dialogue_id) DO UPDATE SET queue_pos=excluded.queue_pos,
		seq=excluded.seq, payload=excluded.payload`,
		res.DialogueID, res.QueuePos, s.seq, string(payload)); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO commit_log(seq, dialogue_id) VALUES(?,?)`,
		s.seq, res.DialogueID); err != nil {
		return err
	}
	return tx.Commit()
}

// ReadResults loads all persisted results ordered by commit sequence.
func (s *CheckpointStore) ReadResults() ([]*DialogueResult, error) {
	rows, err := s.db.Query(`SELECT payload FROM results ORDER BY seq`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*DialogueResult
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			return nil, err
		}
		var res DialogueResult
		if err := json.Unmarshal([]byte(payload), &res); err != nil {
			return nil, fmt.Errorf("decode result: %w", err)
		}
		out = append(out, &res)
	}
	return out, rows.Err()
}

// CommitLog returns the persisted commit sequence.
func (s *CheckpointStore) CommitLog() ([]string, error) {
	rows, err := s.db.Query(`SELECT dialogue_id FROM commit_log ORDER BY seq`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// RunLive orchestrates every dialogue in queue order, checkpointing
// each result as soon as it is submitted and verified.
func RunLive(p *Project, store *CheckpointStore) (map[string]*DialogueResult, *Error) {
	if err := store.Invalidate(); err != nil {
		return nil, newError(EState, "", "checkpoint", err.Error())
	}
	if err := store.SaveMeta(p.ContentHash()); err != nil {
		return nil, newError(EState, "", "checkpoint", err.Error())
	}
	o, err := NewOrchestrator(p)
	if err != nil {
		return nil, err
	}
	var first *Error
	for _, id := range o.Queue.Order() {
		if oerr := o.OrchestrateOne(id); oerr != nil {
			if first == nil {
				first = oerr
			}
			continue
		}
		if werr := store.WriteResult(o.Results[id]); werr != nil {
			return nil, newError(EState, id, "checkpoint", werr.Error())
		}
	}
	return o.Results, first
}

// RunBatch computes all results first, then checkpoints them in one
// stable queue-ordered sequence (one-shot full orchestration).
func RunBatch(p *Project, store *CheckpointStore) (map[string]*DialogueResult, *Error) {
	if err := store.Invalidate(); err != nil {
		return nil, newError(EState, "", "checkpoint", err.Error())
	}
	if err := store.SaveMeta(p.ContentHash()); err != nil {
		return nil, newError(EState, "", "checkpoint", err.Error())
	}
	results, order, runErr := FullRecompute(p)
	for _, id := range order {
		if werr := store.WriteResult(results[id]); werr != nil {
			return nil, newError(EState, id, "checkpoint", werr.Error())
		}
	}
	return results, runErr
}

// RunResumable orchestrates until crashAfter dialogues have been
// committed, simulating a restart. ContinueFromCheckpoint then
// validates the snapshot and finishes the remaining dialogues.
func RunResumable(p *Project, store *CheckpointStore, crashAfter int) *Error {
	if err := store.Invalidate(); err != nil {
		return newError(EState, "", "checkpoint", err.Error())
	}
	if err := store.SaveMeta(p.ContentHash()); err != nil {
		return newError(EState, "", "checkpoint", err.Error())
	}
	o, err := NewOrchestrator(p)
	if err != nil {
		return err
	}
	count := 0
	for _, id := range o.Queue.Order() {
		if count >= crashAfter {
			break
		}
		if oerr := o.OrchestrateOne(id); oerr != nil {
			continue
		}
		if werr := store.WriteResult(o.Results[id]); werr != nil {
			return newError(EState, id, "checkpoint", werr.Error())
		}
		count++
	}
	return nil
}

// ContinueFromCheckpoint resumes after a restart. If the content hash
// or any version mismatches, the snapshot is discarded wholesale and
// a full recompute runs instead.
func ContinueFromCheckpoint(p *Project, store *CheckpointStore) (map[string]*DialogueResult, *Error) {
	valid, err := store.CheckValidity(p.ContentHash())
	if err != nil {
		return nil, newError(EState, "", "checkpoint", err.Error())
	}
	if !valid {
		if serr := store.SaveMeta(p.ContentHash()); serr != nil {
			return nil, newError(EState, "", "checkpoint", serr.Error())
		}
	}
	persisted, err := store.ReadResults()
	if err != nil {
		return nil, newError(EState, "", "checkpoint", err.Error())
	}
	results := map[string]*DialogueResult{}
	done := map[string]bool{}
	for _, r := range persisted {
		results[r.DialogueID] = r
		done[r.DialogueID] = true
	}
	o, oerr := NewOrchestrator(p)
	if oerr != nil {
		return nil, oerr
	}
	var first *Error
	for _, id := range o.Queue.Order() {
		if done[id] {
			continue
		}
		if err := o.OrchestrateOne(id); err != nil {
			if first == nil {
				first = err
			}
			continue
		}
		if werr := store.WriteResult(o.Results[id]); werr != nil {
			return nil, newError(EState, id, "checkpoint", werr.Error())
		}
		results[id] = o.Results[id]
	}
	return results, first
}
