// Package checkpoint 基于 database/sql 与 modernc.org/sqlite 实现原子、串行的窗口检查点。
// 每个窗口的技能状态、资源余量、冷却剩余、意图偏移、内容哈希、源版本与算法版本
// 在单个事务内整体提交；重启时任一版本不匹配则整窗作废（E_ENTRY）。
package checkpoint

import (
	"database/sql"
	"encoding/json"
	"fmt"

	"skillsched/internal/engine"
	"skillsched/internal/errx"
	"skillsched/internal/model"

	_ "modernc.org/sqlite"
)

// AlgorithmVersion 是排程算法版本，变更会使历史检查点作废。
const AlgorithmVersion = "skillsched-algo-1"

// WindowRecord 是一个窗口的完整检查点。
type WindowRecord struct {
	WindowIndex   int               `json:"window_index"`
	TickFrom      int               `json:"tick_from"`
	TickTo        int               `json:"tick_to"`
	WindowState   string            `json:"window_state"`
	ContentHash   string            `json:"content_hash"`
	SourceVersion string            `json:"source_version"`
	AlgorithmVer  string            `json:"algorithm_ver"`
	ParamVersion  int               `json:"param_version"`
	IntentOffset  int               `json:"intent_offset"`
	Runtime       *engine.Runtime   `json:"runtime"`
	Truth         []model.TickTruth `json:"truth"`
}

// Store 封装 sqlite 连接；单连接 + 互斥保证写入不交错、窗口顺序稳定。
type Store struct {
	db *sql.DB
	mu chan struct{}
}

// Open 打开（或创建）检查点库。
func Open(dsn string) (*Store, error) {
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, errx.New(errx.EStore, "open sqlite").Wrap(err)
	}
	// 单连接串行化所有写入，杜绝交错。
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		return nil, errx.New(errx.EStore, "ping sqlite").Wrap(err)
	}
	s := &Store{db: db, mu: make(chan struct{}, 1)}
	if err := s.init(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) init() error {
	_, err := s.db.Exec(`
CREATE TABLE IF NOT EXISTS windows (
  window_index INTEGER PRIMARY KEY,
  tick_from INTEGER NOT NULL,
  tick_to INTEGER NOT NULL,
  window_state TEXT NOT NULL,
  content_hash TEXT NOT NULL,
  source_version TEXT NOT NULL,
  algorithm_ver TEXT NOT NULL,
  param_version INTEGER NOT NULL,
  intent_offset INTEGER NOT NULL,
  runtime_json TEXT NOT NULL,
  truth_json TEXT NOT NULL
);`)
	return err
}

// Close 关闭底层连接。
func (s *Store) Close() error { return s.db.Close() }

// CommitWindow 在单个事务内原子写入整窗，并校验窗口顺序稳定。
func (s *Store) CommitWindow(rec *WindowRecord) error {
	s.mu <- struct{}{}
	defer func() { <-s.mu }()

	tx, err := s.db.Begin()
	if err != nil {
		return errx.New(errx.EStore, "begin tx").Wrap(err)
	}
	defer func() { _ = tx.Rollback() }()

	var prev int
	row := tx.QueryRow(`SELECT COALESCE(MAX(window_index), -1) FROM windows`)
	if err := row.Scan(&prev); err != nil {
		return errx.New(errx.EStore, "scan max window").Wrap(err)
	}
	if rec.WindowIndex != prev+1 {
		return errx.New(errx.EState, "non-monotonic window index").
			With("expected", fmt.Sprint(prev+1), "got", fmt.Sprint(rec.WindowIndex))
	}

	rtJSON, err := json.Marshal(rec.Runtime)
	if err != nil {
		return errx.New(errx.EStore, "marshal runtime").Wrap(err)
	}
	truthJSON, err := json.Marshal(rec.Truth)
	if err != nil {
		return errx.New(errx.EStore, "marshal truth").Wrap(err)
	}
	_, err = tx.Exec(`INSERT INTO windows
(window_index,tick_from,tick_to,window_state,content_hash,source_version,algorithm_ver,param_version,intent_offset,runtime_json,truth_json)
VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
		rec.WindowIndex, rec.TickFrom, rec.TickTo, rec.WindowState, rec.ContentHash,
		rec.SourceVersion, rec.AlgorithmVer, rec.ParamVersion, rec.IntentOffset,
		string(rtJSON), string(truthJSON))
	if err != nil {
		return errx.New(errx.EStore, "insert window").Wrap(err)
	}
	if err := tx.Commit(); err != nil {
		return errx.New(errx.EStore, "commit tx").Wrap(err)
	}
	return nil
}

// LoadAll 读取全部窗口并做版本校验；任一窗口版本/哈希不匹配则整窗作废（返回 E_ENTRY）。
func (s *Store) LoadAll(expectedHash string, expectedSource model.Version, expectedParam int) ([]*WindowRecord, error) {
	s.mu <- struct{}{}
	defer func() { <-s.mu }()

	rows, err := s.db.Query(`SELECT window_index,tick_from,tick_to,window_state,content_hash,source_version,
algorithm_ver,param_version,intent_offset,runtime_json,truth_json FROM windows ORDER BY window_index ASC`)
	if err != nil {
		return nil, errx.New(errx.EStore, "query windows").Wrap(err)
	}
	defer rows.Close()

	var out []*WindowRecord
	for rows.Next() {
		rec := &WindowRecord{}
		var rtJSON, truthJSON string
		if err := rows.Scan(&rec.WindowIndex, &rec.TickFrom, &rec.TickTo, &rec.WindowState,
			&rec.ContentHash, &rec.SourceVersion, &rec.AlgorithmVer, &rec.ParamVersion,
			&rec.IntentOffset, &rtJSON, &truthJSON); err != nil {
			return nil, errx.New(errx.EStore, "scan window").Wrap(err)
		}
		// 重启版本校验：任一不匹配整窗作废。
		if rec.AlgorithmVer != AlgorithmVersion {
			return nil, errx.New(errx.EEntry, "algorithm version mismatch; window invalidated").
				With("window_index", fmt.Sprint(rec.WindowIndex), "expected", AlgorithmVersion, "actual", rec.AlgorithmVer)
		}
		if rec.ContentHash != expectedHash {
			return nil, errx.New(errx.EEntry, "content hash mismatch; window invalidated").
				With("window_index", fmt.Sprint(rec.WindowIndex))
		}
		if rec.SourceVersion != string(expectedSource) {
			return nil, errx.New(errx.EEntry, "source version mismatch; window invalidated").
				With("window_index", fmt.Sprint(rec.WindowIndex))
		}
		if rec.ParamVersion != expectedParam {
			return nil, errx.New(errx.EEntry, "param version mismatch; window invalidated").
				With("window_index", fmt.Sprint(rec.WindowIndex))
		}
		rec.Runtime = &engine.Runtime{}
		if err := json.Unmarshal([]byte(rtJSON), rec.Runtime); err != nil {
			return nil, errx.New(errx.EEntry, "runtime corrupt").Wrap(err)
		}
		if err := json.Unmarshal([]byte(truthJSON), &rec.Truth); err != nil {
			return nil, errx.New(errx.EEntry, "truth corrupt").Wrap(err)
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

// LastRuntime 返回最新已提交窗口的运行态，用于断点恢复；无窗口时返回 nil。
func (s *Store) LastRuntime() (*engine.Runtime, error) {
	recs, err := s.loadRaw()
	if err != nil || len(recs) == 0 {
		return nil, err
	}
	return recs[len(recs)-1].Runtime, nil
}

func (s *Store) loadRaw() ([]*WindowRecord, error) {
	rows, err := s.db.Query(`SELECT runtime_json FROM windows ORDER BY window_index ASC`)
	if err != nil {
		return nil, errx.New(errx.EStore, "query runtime").Wrap(err)
	}
	defer rows.Close()
	var out []*WindowRecord
	for rows.Next() {
		var rtJSON string
		if err := rows.Scan(&rtJSON); err != nil {
			return nil, err
		}
		rt := &engine.Runtime{}
		if err := json.Unmarshal([]byte(rtJSON), rt); err != nil {
			return nil, err
		}
		out = append(out, &WindowRecord{Runtime: rt})
	}
	return out, rows.Err()
}

// Reset 清空检查点（测试与重算场景）。
func (s *Store) Reset() error {
	s.mu <- struct{}{}
	defer func() { <-s.mu }()
	_, err := s.db.Exec(`DELETE FROM windows`)
	return err
}
