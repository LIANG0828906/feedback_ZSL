// Package store 基于 modernc.org/sqlite 纯 Go 驱动实现原子、串行的窗口检查点。
package store

import (
	"database/sql"
	"fmt"
	"sync"

	_ "modernc.org/sqlite"
)

// Store 封装单个 SQLite 检查点库。
// 所有写操作经 mu 串行化，保证写入不交错、窗口顺序稳定。
type Store struct {
	db *sql.DB
	mu sync.Mutex
}

// Open 打开（或创建）检查点库并初始化表结构。
func Open(path string) (*Store, error) {
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(FULL)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // 单连接串行写入，杜绝交错
	s := &Store{db: db}
	if err := s.init(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) init() error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS meta (
			match_id TEXT PRIMARY KEY,
			replay_hash TEXT NOT NULL,
			truth_hash TEXT NOT NULL,
			param_version TEXT NOT NULL,
			algo_version TEXT NOT NULL,
			seed INTEGER NOT NULL,
			window_size INTEGER NOT NULL,
			algorithm TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS windows (
			match_id TEXT NOT NULL,
			window_no INTEGER NOT NULL,
			from_turn INTEGER NOT NULL,
			to_turn INTEGER NOT NULL,
			status TEXT NOT NULL,
			action_offset INTEGER NOT NULL,
			rng_draws INTEGER NOT NULL,
			rng_seed INTEGER NOT NULL,
			content_hash TEXT NOT NULL,
			chain_hash TEXT NOT NULL,
			param_version TEXT NOT NULL,
			algo_version TEXT NOT NULL,
			PRIMARY KEY (match_id, window_no)
		)`,
		`CREATE TABLE IF NOT EXISTS snapshots (
			match_id TEXT NOT NULL,
			turn_no INTEGER NOT NULL,
			unit_id TEXT NOT NULL,
			hp INTEGER NOT NULL,
			mp INTEGER NOT NULL,
			alive INTEGER NOT NULL,
			cooldown_json TEXT NOT NULL,
			winner INTEGER NOT NULL,
			PRIMARY KEY (match_id, turn_no, unit_id)
		)`,
		`CREATE TABLE IF NOT EXISTS rulings (
			match_id TEXT NOT NULL,
			window_no INTEGER NOT NULL,
			turn_no INTEGER NOT NULL,
			unit_id TEXT NOT NULL,
			skill_id TEXT NOT NULL,
			violations_json TEXT NOT NULL,
			reason TEXT NOT NULL,
			allowed INTEGER NOT NULL,
			seq INTEGER NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS decisions (
			match_id TEXT PRIMARY KEY,
			algorithm TEXT NOT NULL,
			window_size INTEGER NOT NULL,
			turns INTEGER NOT NULL,
			units INTEGER NOT NULL,
			est_mem INTEGER NOT NULL,
			mem_budget INTEGER NOT NULL,
			est_time INTEGER NOT NULL,
			time_budget INTEGER NOT NULL,
			basis TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS transitions (
			match_id TEXT NOT NULL,
			window_no INTEGER NOT NULL,
			from_status TEXT NOT NULL,
			to_status TEXT NOT NULL,
			seq INTEGER NOT NULL
		)`,
	}
	for _, st := range stmts {
		if _, err := s.db.Exec(st); err != nil {
			return fmt.Errorf("init schema: %w", err)
		}
	}
	return nil
}

// Close 关闭底层连接。
func (s *Store) Close() error { return s.db.Close() }

// DB 暴露只读查询入口给独立复核工具（复核不经过结算主路径）。
func (s *Store) DB() *sql.DB { return s.db }
