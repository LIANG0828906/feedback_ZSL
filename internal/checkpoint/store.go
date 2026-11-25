package checkpoint

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	_ "modernc.org/sqlite"

	"battlekernel/internal/canonical"
	"battlekernel/internal/errs"
	"battlekernel/internal/model"
)

type Store struct {
	db   *sql.DB
	path string
	mu   sync.Mutex
}

func Open(path string) (*Store, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
	}
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(ON)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	store := &Store{db: db, path: path}
	if err := store.initSchema(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

func OpenMemory() (*Store, error) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	store := &Store{db: db, path: ":memory:"}
	if err := store.initSchema(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

func (s *Store) initSchema(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS meta (
  project_id TEXT NOT NULL,
  key TEXT NOT NULL,
  value TEXT NOT NULL,
  PRIMARY KEY(project_id, key)
);
CREATE TABLE IF NOT EXISTS windows (
  project_id TEXT NOT NULL,
  window_index INTEGER NOT NULL,
  start_round INTEGER NOT NULL,
  end_round INTEGER NOT NULL,
  content_hash TEXT NOT NULL,
  format_version TEXT NOT NULL,
  algorithm_version TEXT NOT NULL,
  reorder_offsets TEXT NOT NULL,
  round_count INTEGER NOT NULL,
  status TEXT NOT NULL,
  committed_at TEXT NOT NULL,
  PRIMARY KEY(project_id, window_index)
);
CREATE TABLE IF NOT EXISTS rounds (
  project_id TEXT NOT NULL,
  round INTEGER NOT NULL,
  window_index INTEGER NOT NULL,
  status TEXT NOT NULL,
  error_code TEXT NOT NULL DEFAULT '',
  error_message TEXT NOT NULL DEFAULT '',
  result_json TEXT NOT NULL DEFAULT '',
  content_hash TEXT NOT NULL DEFAULT '',
  updated_at TEXT NOT NULL,
  PRIMARY KEY(project_id, round)
);
CREATE TABLE IF NOT EXISTS consumed_reorders (
  project_id TEXT NOT NULL,
  offset INTEGER NOT NULL,
  window_index INTEGER NOT NULL,
  round INTEGER NOT NULL,
  PRIMARY KEY(project_id, offset)
);
`)
	return err
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) InitializeProject(ctx context.Context, p *model.Project, semanticHash string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	projectJSON, err := json.Marshal(p)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	rows := [][3]string{
		{"project_json", string(projectJSON)},
		{"semantic_hash", semanticHash},
		{"format_version", model.CanonicalFormatVersion},
		{"algorithm_version", model.AlgorithmVersion},
		{"source_format", p.SourceFormat},
		{"initialized_at", now},
	}
	for _, row := range rows {
		if _, err := tx.ExecContext(ctx, `INSERT INTO meta(project_id,key,value) VALUES(?,?,?)
			ON CONFLICT(project_id,key) DO UPDATE SET value=excluded.value`, p.ID, row[0], row[1]); err != nil {
			return err
		}
	}
	return tx.Commit()
}

type CommitWindowInput struct {
	ProjectID       string
	Window          model.WindowRecord
	Rounds          []model.RoundResult
	ConsumedOffsets []int
}

func (s *Store) CommitWindow(ctx context.Context, in CommitWindowInput) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	offsetsJSON, err := json.Marshal(in.ConsumedOffsets)
	if err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := tx.ExecContext(ctx, `INSERT INTO windows
	(project_id,window_index,start_round,end_round,content_hash,format_version,algorithm_version,reorder_offsets,round_count,status,committed_at)
	VALUES(?,?,?,?,?,?,?,?,?,'committed',?)
	ON CONFLICT(project_id,window_index) DO UPDATE SET
		start_round=excluded.start_round,end_round=excluded.end_round,content_hash=excluded.content_hash,
		format_version=excluded.format_version,algorithm_version=excluded.algorithm_version,
		reorder_offsets=excluded.reorder_offsets,round_count=excluded.round_count,status='committed',committed_at=excluded.committed_at`,
		in.ProjectID, in.Window.WindowIndex, in.Window.StartRound, in.Window.EndRound, in.Window.ContentHash,
		in.Window.FormatVersion, in.Window.AlgorithmVersion, string(offsetsJSON), in.Window.RoundCount, now); err != nil {
		return err
	}
	for _, round := range in.Rounds {
		data, err := json.Marshal(round)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO rounds
		(project_id,round,window_index,status,error_code,error_message,result_json,content_hash,updated_at)
		VALUES(?,?,?,'committed','','',?,?,?)
		ON CONFLICT(project_id,round) DO UPDATE SET
			window_index=excluded.window_index,status='committed',error_code='',error_message='',
			result_json=excluded.result_json,content_hash=excluded.content_hash,updated_at=excluded.updated_at`,
			in.ProjectID, round.Round, in.Window.WindowIndex, string(data), round.ContentHash, now); err != nil {
			return err
		}
		for _, offset := range in.ConsumedOffsets {
			if _, err := tx.ExecContext(ctx, `INSERT INTO consumed_reorders(project_id,offset,window_index,round)
			VALUES(?,?,?,?) ON CONFLICT(project_id,offset) DO UPDATE SET window_index=excluded.window_index,round=excluded.round`,
				in.ProjectID, offset, in.Window.WindowIndex, in.Window.EndRound); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

func (s *Store) MarkRoundFailed(ctx context.Context, projectID string, windowIndex int, round int64, code errs.Code, message string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err := s.db.ExecContext(ctx, `INSERT INTO rounds
	(project_id,round,window_index,status,error_code,error_message,result_json,content_hash,updated_at)
	VALUES(?,?,?,'failed',?,?,'','',?)
	ON CONFLICT(project_id,round) DO UPDATE SET
		window_index=excluded.window_index,status='failed',error_code=excluded.error_code,
		error_message=excluded.error_message,updated_at=excluded.updated_at`,
		projectID, round, windowIndex, string(code), message, now)
	return err
}

func (s *Store) LoadCommittedRounds(ctx context.Context, projectID string) ([]model.RoundResult, []model.WindowRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.QueryContext(ctx, `SELECT result_json FROM rounds WHERE project_id=? AND status='committed' ORDER BY round ASC`, projectID)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var rounds []model.RoundResult
	for rows.Next() {
		var data string
		if err := rows.Scan(&data); err != nil {
			return nil, nil, err
		}
		var round model.RoundResult
		if err := json.Unmarshal([]byte(data), &round); err != nil {
			return nil, nil, errs.Wrap(errs.ECheck, "rounds.result_json", err, "committed snapshot cannot be decoded")
		}
		rounds = append(rounds, round)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}

	wrows, err := s.db.QueryContext(ctx, `SELECT window_index,start_round,end_round,content_hash,format_version,algorithm_version,reorder_offsets,round_count
	FROM windows WHERE project_id=? AND status='committed' ORDER BY window_index ASC`, projectID)
	if err != nil {
		return nil, nil, err
	}
	defer wrows.Close()
	var windows []model.WindowRecord
	for wrows.Next() {
		var w model.WindowRecord
		var offsets string
		if err := wrows.Scan(&w.WindowIndex, &w.StartRound, &w.EndRound, &w.ContentHash, &w.FormatVersion, &w.AlgorithmVersion, &offsets, &w.RoundCount); err != nil {
			return nil, nil, err
		}
		if err := json.Unmarshal([]byte(offsets), &w.ReorderOffsets); err != nil {
			return nil, nil, err
		}
		windows = append(windows, w)
	}
	return rounds, windows, wrows.Err()
}

func (s *Store) LatestCommittedRound(ctx context.Context, projectID string) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var round int64
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(round),0) FROM rounds WHERE project_id=? AND status='committed'`, projectID).Scan(&round)
	return round, err
}

func (s *Store) ValidateSnapshot(ctx context.Context, projectID string) error {
	rounds, windows, err := s.LoadCommittedRounds(ctx, projectID)
	if err != nil {
		return err
	}
	for _, w := range windows {
		if w.AlgorithmVersion != model.AlgorithmVersion {
			return &errs.Error{Code: errs.ECheck, Field: "windows.algorithm_version", Values: []string{w.AlgorithmVersion, model.AlgorithmVersion}, Message: "algorithm version mismatch"}
		}
		if w.FormatVersion != model.CanonicalFormatVersion {
			return &errs.Error{Code: errs.ECheck, Field: "windows.format_version", Values: []string{w.FormatVersion, model.CanonicalFormatVersion}, Message: "format version mismatch"}
		}
	}
	for _, r := range rounds {
		expected, err := HashRoundResult(r)
		if err != nil {
			return err
		}
		if r.ContentHash == "" || r.ContentHash != expected {
			return &errs.Error{Code: errs.ECheck, Field: fmt.Sprintf("rounds[%d].content_hash", r.Round), Round: r.Round, Values: []string{r.ContentHash, expected}, Message: "round content hash mismatch"}
		}
		var stored string
		s.mu.Lock()
		err = s.db.QueryRowContext(ctx, `SELECT content_hash FROM rounds WHERE project_id=? AND round=?`, projectID, r.Round).Scan(&stored)
		s.mu.Unlock()
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if stored != expected {
			return &errs.Error{Code: errs.ECheck, Field: fmt.Sprintf("rounds[%d].stored_hash", r.Round), Round: r.Round, Values: []string{stored, expected}, Message: "stored hash mismatch"}
		}
	}
	return nil
}

func HashRoundResult(r model.RoundResult) (string, error) {
	r.ContentHash = ""
	return canonical.JSONHash(r)
}

func (s *Store) ResetProject(ctx context.Context, projectID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, table := range []string{"windows", "rounds", "consumed_reorders", "meta"} {
		if _, err := tx.ExecContext(ctx, "DELETE FROM "+table+" WHERE project_id=?", projectID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) DeleteFromRound(ctx context.Context, projectID string, windowStartRound int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM rounds WHERE project_id=? AND round>=?`, projectID, windowStartRound); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM windows WHERE project_id=? AND start_round>=?`, projectID, windowStartRound); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM consumed_reorders WHERE project_id=? AND round>=?`, projectID, windowStartRound); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) ResetAll(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.ExecContext(ctx, `DELETE FROM windows; DELETE FROM rounds; DELETE FROM consumed_reorders; DELETE FROM meta;`)
	return err
}

func (s *Store) LoadProjectJSON(ctx context.Context, projectID string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var data string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM meta WHERE project_id=? AND key='project_json'`, projectID).Scan(&data)
	if err != nil {
		return nil, err
	}
	return []byte(data), nil
}

func (s *Store) SetMeta(ctx context.Context, projectID, key, value string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.ExecContext(ctx, `INSERT INTO meta(project_id,key,value) VALUES(?,?,?)
	ON CONFLICT(project_id,key) DO UPDATE SET value=excluded.value`, projectID, key, value)
	return err
}

func (s *Store) GetMeta(ctx context.Context, projectID, key string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var value string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM meta WHERE project_id=? AND key=?`, projectID, key).Scan(&value)
	return value, err
}
