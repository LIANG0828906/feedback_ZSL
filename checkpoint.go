package plancache

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	_ "modernc.org/sqlite"
)

type Checkpoint struct {
	db      *sql.DB
	path    string
	version string
}

type RecoveredCheckpoint struct {
	EngineeringVersion string                 `json:"engineering_version"`
	LastSeq            int                    `json:"last_seq"`
	Entries            []CacheEntry           `json:"entries"`
	RequestSeqs        []int                  `json:"request_seqs"`
	Progress           []InvalidationProgress `json:"progress"`
	DiscardedPending   int                    `json:"discarded_pending"`
	Rebuilt            bool                   `json:"rebuilt"`
}

func OpenCheckpoint(path, engineeringVersion string) (*Checkpoint, error) {
	if err := preflightCheckpointFile(path); err != nil {
		return nil, err
	}
	if dir := filepath.Dir(path); dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, withContext(newError(CodeCheckpointCorrupt, path, "cannot create checkpoint directory"), "detail", err.Error())
		}
	}
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(ON)")
	if err != nil {
		return nil, checkpointOpenError(path, err)
	}
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, checkpointOpenError(path, err)
	}
	cp := &Checkpoint{db: db, path: path, version: engineeringVersion}
	if err := cp.migrate(); err != nil {
		_ = db.Close()
		return nil, err
	}
	var savedVersion string
	err = db.QueryRow(`SELECT value FROM checkpoint_meta WHERE key='engineering_version'`).Scan(&savedVersion)
	if errors.Is(err, sql.ErrNoRows) {
		_, err = db.Exec(`INSERT INTO checkpoint_meta(key,value) VALUES ('engineering_version',?)`, engineeringVersion)
		if err != nil {
			_ = db.Close()
			return nil, withContext(newError(CodeCheckpointCorrupt, path, "cannot write checkpoint version"), "detail", err.Error())
		}
		savedVersion = engineeringVersion
	} else if err != nil {
		_ = db.Close()
		return nil, checkpointOpenError(path, err)
	}
	if savedVersion != engineeringVersion {
		_ = db.Close()
		return nil, withContext(newError(CodeCheckpointVersion, path, "checkpoint engineering version does not match current project"), "checkpoint_version", savedVersion, "project_version", engineeringVersion)
	}
	if err := cp.integrityCheck(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return cp, nil
}

func preflightCheckpointFile(path string) error {
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return withContext(newError(CodeCheckpointCorrupt, path, "cannot stat checkpoint"), "detail", err.Error())
	}
	if info.IsDir() {
		return newError(CodeCheckpointCorrupt, path, "checkpoint path is a directory")
	}
	if info.Size() < 16 {
		return withContext(newError(CodeCheckpointTruncated, path, "checkpoint file is empty or shorter than SQLite header"), "size_bytes", info.Size())
	}
	if info.Size() >= 16 {
		header := make([]byte, 16)
		f, err := os.Open(path)
		if err != nil {
			return withContext(newError(CodeCheckpointCorrupt, path, "cannot open checkpoint header"), "detail", err.Error())
		}
		_, readErr := f.Read(header)
		_ = f.Close()
		if readErr != nil {
			return withContext(newError(CodeCheckpointTruncated, path, "cannot read SQLite header"), "detail", readErr.Error())
		}
		if string(header) != "SQLite format 3\x00" {
			return withContext(newError(CodeCheckpointTruncated, path, "invalid SQLite header"), "header", fmt.Sprintf("% x", header))
		}
	}
	return nil
}

func checkpointOpenError(path string, err error) error {
	msg := strings.ToLower(err.Error())
	code := CodeCheckpointCorrupt
	if strings.Contains(msg, "malformed") || strings.Contains(msg, "not a database") || strings.Contains(msg, "disk image") || strings.Contains(msg, "eof") {
		code = CodeCheckpointTruncated
	}
	return withContext(newError(code, path, "cannot open SQLite checkpoint: %v", err), "detail", err.Error())
}

func (cp *Checkpoint) integrityCheck() error {
	var result string
	err := cp.db.QueryRow(`PRAGMA integrity_check`).Scan(&result)
	if err != nil {
		return withContext(newError(CodeCheckpointTruncated, cp.path, "checkpoint integrity check failed"), "detail", err.Error())
	}
	if result != "ok" {
		return withContext(newError(CodeCheckpointTruncated, cp.path, "checkpoint integrity check returned %q", result), "integrity", result)
	}
	return nil
}

func (cp *Checkpoint) migrate() error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS checkpoint_meta (key TEXT PRIMARY KEY, value TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS cache_entries (
			entry_id TEXT PRIMARY KEY,
			plan_id TEXT NOT NULL,
			statement_id TEXT NOT NULL,
			cache_key TEXT NOT NULL,
			seq INTEGER NOT NULL,
			payload TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS pending_entry_state (
			seq INTEGER NOT NULL,
			entry_id TEXT NOT NULL,
			payload TEXT NOT NULL,
			PRIMARY KEY(seq, entry_id)
		)`,
		`CREATE TABLE IF NOT EXISTS pending_invalidation_progress (
			seq INTEGER NOT NULL,
			event_id TEXT NOT NULL,
			payload TEXT NOT NULL,
			PRIMARY KEY(seq, event_id)
		)`,
		`CREATE TABLE IF NOT EXISTS request_cursor (id INTEGER PRIMARY KEY CHECK(id=1), last_seq INTEGER NOT NULL)`,
		`INSERT OR IGNORE INTO request_cursor(id,last_seq) VALUES(1,0)`,
		`CREATE TABLE IF NOT EXISTS committed_requests (
			seq INTEGER PRIMARY KEY,
			request_id TEXT NOT NULL,
			session_id TEXT NOT NULL,
			request_json TEXT NOT NULL,
			report_json TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS invalidation_progress (
			event_id TEXT PRIMARY KEY,
			payload TEXT NOT NULL
		)`,
	}
	for _, stmt := range stmts {
		if _, err := cp.db.Exec(stmt); err != nil {
			return withContext(newError(CodeCheckpointCorrupt, cp.path, "migration failed"), "detail", err.Error(), "sql", stmt)
		}
	}
	return nil
}

func (cp *Checkpoint) Close() error { return cp.db.Close() }

// SavePendingEntryState is phase one: durable cache entry state before the
// global request cursor is advanced. A crash after this call leaves only
// pending rows, never a partially committed request.
func (cp *Checkpoint) SavePendingEntryState(seq int, entries []CacheEntry, progress []InvalidationProgress) error {
	tx, err := cp.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, ent := range entries {
		b, mErr := json.Marshal(ent)
		if mErr != nil {
			return mErr
		}
		if _, err := tx.Exec(`INSERT OR REPLACE INTO pending_entry_state(seq,entry_id,payload) VALUES(?,?,?)`, seq, ent.ID, string(b)); err != nil {
			return err
		}
	}
	for _, p := range progress {
		b, mErr := json.Marshal(p)
		if mErr != nil {
			return mErr
		}
		if _, err := tx.Exec(`INSERT OR REPLACE INTO pending_invalidation_progress(seq,event_id,payload) VALUES(?,?,?)`, seq, p.EventID, string(b)); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// CommitPendingRequest is phase two: promote pending entries, persist the
// request/report and advance the global cursor in one atomic transaction.
func (cp *Checkpoint) CommitPendingRequest(req SessionRequest, report RequestReport) error {
	tx, err := cp.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	rows, err := tx.Query(`SELECT entry_id,payload FROM pending_entry_state WHERE seq=?`, req.Seq)
	if err != nil {
		return err
	}
	type pending struct{ id, payload string }
	var pendings []pending
	for rows.Next() {
		var p pending
		if err := rows.Scan(&p.id, &p.payload); err != nil {
			_ = rows.Close()
			return err
		}
		pendings = append(pendings, p)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	for _, p := range pendings {
		var ent CacheEntry
		if err := json.Unmarshal([]byte(p.payload), &ent); err != nil {
			return withContext(newError(CodeCheckpointCorrupt, cp.path, "pending entry payload is invalid"), "entry_id", p.id, "detail", err.Error())
		}
		if _, err := tx.Exec(`INSERT OR REPLACE INTO cache_entries(entry_id,plan_id,statement_id,cache_key,seq,payload) VALUES(?,?,?,?,?,?)`, ent.ID, ent.PlanID, ent.StatementID, ent.CacheKey, req.Seq, p.payload); err != nil {
			return err
		}
	}
	type pendingProgress struct{ eventID, payload string }
	progressRows, err := tx.Query(`SELECT event_id,payload FROM pending_invalidation_progress WHERE seq=?`, req.Seq)
	if err != nil {
		return err
	}
	var pendingProgresses []pendingProgress
	for progressRows.Next() {
		var p pendingProgress
		if err := progressRows.Scan(&p.eventID, &p.payload); err != nil {
			_ = progressRows.Close()
			return err
		}
		pendingProgresses = append(pendingProgresses, p)
	}
	if err := progressRows.Close(); err != nil {
		return err
	}
	for _, p := range pendingProgresses {
		if _, err := tx.Exec(`INSERT OR REPLACE INTO invalidation_progress(event_id,payload) VALUES(?,?)`, p.eventID, p.payload); err != nil {
			return err
		}
	}
	reqBytes, err := json.Marshal(req)
	if err != nil {
		return err
	}
	reportBytes, err := json.Marshal(report)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT OR REPLACE INTO committed_requests(seq,request_id,session_id,request_json,report_json) VALUES(?,?,?,?,?)`, req.Seq, req.RequestID, req.SessionID, string(reqBytes), string(reportBytes)); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE request_cursor SET last_seq=? WHERE id=1`, req.Seq); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM pending_entry_state WHERE seq=?`, req.Seq); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM pending_invalidation_progress WHERE seq=?`, req.Seq); err != nil {
		return err
	}
	return tx.Commit()
}

// CheckpointRequest performs both phases in order.
func (cp *Checkpoint) CheckpointRequest(req SessionRequest, report RequestReport, entries []CacheEntry) error {
	if err := cp.SavePendingEntryState(req.Seq, entries, nil); err != nil {
		return err
	}
	return cp.CommitPendingRequest(req, report)
}

func (cp *Checkpoint) LastSeq() (int, error) {
	var seq int
	err := cp.db.QueryRow(`SELECT last_seq FROM request_cursor WHERE id=1`).Scan(&seq)
	return seq, err
}

// Recover discards pending state above the durable cursor and returns only
// committed entries/requests.
func (cp *Checkpoint) Recover() (*RecoveredCheckpoint, error) {
	out := &RecoveredCheckpoint{EngineeringVersion: cp.version}
	res, err := cp.db.Exec(`DELETE FROM pending_entry_state WHERE seq > (SELECT last_seq FROM request_cursor WHERE id=1)`)
	if err != nil {
		return nil, withContext(newError(CodeCheckpointCorrupt, cp.path, "cannot discard unfinished pending state"), "detail", err.Error())
	}
	if n, err := res.RowsAffected(); err == nil {
		out.DiscardedPending = int(n)
	}
	if _, err := cp.db.Exec(`DELETE FROM pending_invalidation_progress WHERE seq > (SELECT last_seq FROM request_cursor WHERE id=1)`); err != nil {
		return nil, withContext(newError(CodeCheckpointCorrupt, cp.path, "cannot discard unfinished invalidation progress"), "detail", err.Error())
	}
	if err := cp.db.QueryRow(`SELECT last_seq FROM request_cursor WHERE id=1`).Scan(&out.LastSeq); err != nil {
		return nil, err
	}
	rows, err := cp.db.Query(`SELECT payload FROM cache_entries ORDER BY entry_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			return nil, err
		}
		var ent CacheEntry
		if err := json.Unmarshal([]byte(payload), &ent); err != nil {
			return nil, withContext(newError(CodeCheckpointCorrupt, cp.path, "committed entry payload is invalid"), "detail", err.Error())
		}
		out.Entries = append(out.Entries, ent)
	}
	reqRows, err := cp.db.Query(`SELECT seq FROM committed_requests WHERE seq <= ? ORDER BY seq`, out.LastSeq)
	if err != nil {
		return nil, err
	}
	defer reqRows.Close()
	for reqRows.Next() {
		var seq int
		if err := reqRows.Scan(&seq); err != nil {
			return nil, err
		}
		out.RequestSeqs = append(out.RequestSeqs, seq)
	}
	progRows, err := cp.db.Query(`SELECT payload FROM invalidation_progress ORDER BY event_id`)
	if err != nil {
		return nil, err
	}
	defer progRows.Close()
	for progRows.Next() {
		var payload string
		if err := progRows.Scan(&payload); err != nil {
			return nil, err
		}
		var prog InvalidationProgress
		if err := json.Unmarshal([]byte(payload), &prog); err != nil {
			return nil, err
		}
		out.Progress = append(out.Progress, prog)
	}
	return out, nil
}

// RecoverOrRebuild implements the version rule: mismatch means a full rebuild,
// never partial reuse.
func RecoverOrRebuild(path string, p *Project) (*Checkpoint, *RecoveredCheckpoint, bool, error) {
	cp, err := OpenCheckpoint(path, p.EngineeringVersion)
	if err != nil {
		if be, ok := AsBusinessError(err); ok && be.Code == CodeCheckpointVersion {
			if resetErr := ResetCheckpointFile(path); resetErr != nil {
				return nil, nil, false, resetErr
			}
			cp, err = OpenCheckpoint(path, p.EngineeringVersion)
			if err != nil {
				return nil, nil, true, err
			}
			rec, rerr := cp.Recover()
			if rerr != nil {
				_ = cp.Close()
				return nil, nil, true, rerr
			}
			rec.Rebuilt = true
			return cp, rec, true, nil
		}
		return nil, nil, false, err
	}
	rec, err := cp.Recover()
	if err != nil {
		_ = cp.Close()
		return nil, nil, false, err
	}
	return cp, rec, false, nil
}

func ResetCheckpointFile(path string) error {
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if err := os.Remove(path + suffix); err != nil && !errors.Is(err, os.ErrNotExist) {
			return withContext(newError(CodeCheckpointCorrupt, path+suffix, "cannot reset old checkpoint"), "detail", err.Error())
		}
	}
	return nil
}

// TruncateCheckpointForInjection truncates a valid database to a non-header
// length so integrity validation deterministically reports E_CHECKPOINT_TRUNCATED.
func TruncateCheckpointForInjection(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	target := info.Size() / 2
	if target < 32 {
		target = 32
	}
	if target >= info.Size() {
		target = info.Size() - 1
	}
	return os.Truncate(path, target)
}
