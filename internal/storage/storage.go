package storage

import (
	"database/sql"
	"encoding/json"
	"fmt"

	_ "modernc.org/sqlite"

	"github.com/gameaudio/workbench/internal/engine"
	"github.com/gameaudio/workbench/internal/errors"
	"github.com/gameaudio/workbench/internal/model"
)

// Meta 是快照级元数据，任何一项不匹配则整个快照作废。
type Meta struct {
	ProjectHash   string
	AlgoVersion   string
	GeoScaleBits  int
	ParamVersions string // 区域参数版本映射的 JSON
	GeomVersions  string // 区域几何版本映射的 JSON
}

// WindowRecord 是一个检查点窗口的持久化记录。
type WindowRecord struct {
	WindowID    int
	PairKeys    []string
	InputHash   string
	Summary     string // 曲线采样摘要 JSON
	ProfileStat string // 剖面统计 JSON
}

// Store 封装 SQLite，所有写入由单一串行通道提交。
type Store struct {
	db *sql.DB
}

// Open 打开（必要时创建）数据库并初始化表结构。
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, errors.New(errors.EStorage, "open", err.Error())
	}
	db.SetMaxOpenConns(1) // 强制写入串行，杜绝交错
	s := &Store{db: db}
	if err := s.init(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) init() error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS meta (
			id INTEGER PRIMARY KEY CHECK (id = 1),
			project_hash TEXT NOT NULL,
			algo_version TEXT NOT NULL,
			geo_scale_bits INTEGER NOT NULL,
			param_versions TEXT NOT NULL,
			geom_versions TEXT NOT NULL,
			created_at TEXT NOT NULL DEFAULT (datetime('now'))
		)`,
		`CREATE TABLE IF NOT EXISTS windows (
			window_id INTEGER PRIMARY KEY,
			pair_keys TEXT NOT NULL,
			input_hash TEXT NOT NULL,
			summary TEXT NOT NULL,
			profile_stat TEXT NOT NULL,
			committed_at TEXT NOT NULL DEFAULT (datetime('now'))
		)`,
		`CREATE TABLE IF NOT EXISTS pair_results (
			pair_key TEXT PRIMARY KEY,
			window_id INTEGER NOT NULL,
			input_hash TEXT NOT NULL,
			payload TEXT NOT NULL
		)`,
	}
	for _, q := range stmts {
		if _, err := s.db.Exec(q); err != nil {
			return errors.New(errors.EStorage, "schema", err.Error())
		}
	}
	return nil
}

// Reset 清空全部快照数据（版本/哈希不匹配时整体作废，禁止混用旧窗口）。
func (s *Store) Reset() error {
	if _, err := s.db.Exec(`DELETE FROM pair_results`); err != nil {
		return errors.New(errors.EStorage, "reset", err.Error())
	}
	if _, err := s.db.Exec(`DELETE FROM windows`); err != nil {
		return errors.New(errors.EStorage, "reset", err.Error())
	}
	if _, err := s.db.Exec(`DELETE FROM meta`); err != nil {
		return errors.New(errors.EStorage, "reset", err.Error())
	}
	return nil
}

// SaveMeta 写入快照元数据（单例行）。
func (s *Store) SaveMeta(m Meta) error {
	_, err := s.db.Exec(
		`INSERT OR REPLACE INTO meta
		 (id, project_hash, algo_version, geo_scale_bits, param_versions, geom_versions)
		 VALUES (1, ?, ?, ?, ?, ?)`,
		m.ProjectHash, m.AlgoVersion, m.GeoScaleBits, m.ParamVersions, m.GeomVersions)
	if err != nil {
		return errors.New(errors.EStorage, "meta", err.Error())
	}
	return nil
}

// LoadMeta 读取元数据；无快照行时返回 false。
func (s *Store) LoadMeta() (Meta, bool, error) {
	var m Meta
	row := s.db.QueryRow(
		`SELECT project_hash, algo_version, geo_scale_bits, param_versions, geom_versions
		 FROM meta WHERE id = 1`)
	err := row.Scan(&m.ProjectHash, &m.AlgoVersion, &m.GeoScaleBits,
		&m.ParamVersions, &m.GeomVersions)
	if err == sql.ErrNoRows {
		return Meta{}, false, nil
	}
	if err != nil {
		return Meta{}, false, errors.New(errors.EStorage, "meta", err.Error())
	}
	return m, true, nil
}

// CommitWindow 在单个事务内原子提交一个窗口及其全部声源-听者对结果。
func (s *Store) CommitWindow(rec WindowRecord, results []model.Result) error {
	tx, err := s.db.Begin()
	if err != nil {
		return errors.New(errors.EStorage, "begin", err.Error())
	}
	defer func() {
		if tx != nil {
			_ = tx.Rollback()
		}
	}()

	keysJSON, _ := json.Marshal(rec.PairKeys)
	if _, err := tx.Exec(
		`INSERT OR REPLACE INTO windows
		 (window_id, pair_keys, input_hash, summary, profile_stat)
		 VALUES (?, ?, ?, ?, ?)`,
		rec.WindowID, string(keysJSON), rec.InputHash, rec.Summary, rec.ProfileStat); err != nil {
		return errors.New(errors.EStorage, "window", err.Error())
	}
	stmt, err := tx.Prepare(
		`INSERT OR REPLACE INTO pair_results
		 (pair_key, window_id, input_hash, payload) VALUES (?, ?, ?, ?)`)
	if err != nil {
		return errors.New(errors.EStorage, "prepare", err.Error())
	}
	defer stmt.Close()
	for _, r := range results {
		payload, err := json.Marshal(r)
		if err != nil {
			return errors.New(errors.EStorage, "marshal", err.Error())
		}
		if _, err := stmt.Exec(engine.PairKey(r.SourceID, r.ListenerID),
			rec.WindowID, rec.InputHash, string(payload)); err != nil {
			return errors.New(errors.EStorage, "pair", err.Error())
		}
	}
	if err := tx.Commit(); err != nil {
		return errors.New(errors.EStorage, "commit", err.Error())
	}
	tx = nil
	return nil
}

// LoadPair 读取单对缓存；不存在返回 false。
func (s *Store) LoadPair(pairKey, inputHash string) (model.Result, bool, error) {
	row := s.db.QueryRow(
		`SELECT payload FROM pair_results WHERE pair_key = ? AND input_hash = ?`,
		pairKey, inputHash)
	var payload string
	err := row.Scan(&payload)
	if err == sql.ErrNoRows {
		return model.Result{}, false, nil
	}
	if err != nil {
		return model.Result{}, false, errors.New(errors.EStorage, "loadPair", err.Error())
	}
	var r model.Result
	if err := json.Unmarshal([]byte(payload), &r); err != nil {
		return model.Result{}, false, errors.New(errors.EStorage, "unmarshal", err.Error())
	}
	r.CacheHit = true
	return r, true, nil
}

// LoadedWindows 返回已提交窗口的有序列表（用于断点恢复验证）。
func (s *Store) LoadedWindows() ([]int, error) {
	rows, err := s.db.Query(`SELECT window_id FROM windows ORDER BY window_id ASC`)
	if err != nil {
		return nil, errors.New(errors.EStorage, "windows", err.Error())
	}
	defer rows.Close()
	var ids []int
	for rows.Next() {
		var id int
		if err := rows.Scan(&id); err != nil {
			return nil, errors.New(errors.EStorage, "windows", err.Error())
		}
		ids = append(ids, id)
	}
	return ids, nil
}

// Close 关闭数据库。
func (s *Store) Close() error {
	if err := s.db.Close(); err != nil {
		return errors.New(errors.EStorage, "close", err.Error())
	}
	return nil
}

// Describe 返回存储状态摘要，便于工作台展示。
func (s *Store) Describe() string {
	var pairs, windows int
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM pair_results`).Scan(&pairs)
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM windows`).Scan(&windows)
	return fmt.Sprintf("windows=%d cachedPairs=%d", windows, pairs)
}
