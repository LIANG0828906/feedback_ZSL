package persist

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	_ "modernc.org/sqlite"

	"occlbench/internal/core"
	"occlbench/internal/streaming"
)

// Store 基于 SQLite 的窗口级检查点存储。
// 所有写操作经单一 mutex 串行化，并在单事务内原子提交，杜绝交错与乱序。
type Store struct {
	mu  sync.Mutex
	db  *sql.DB
	dir string
}

// Open 打开（必要时创建）检查点数据库。
func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, core.NewError(core.EIO, "mkdir checkpoint dir", map[string]any{"err": err.Error()})
	}
	path := filepath.Join(dir, "checkpoints.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, core.NewError(core.EIO, "open sqlite", map[string]any{"err": err.Error()})
	}
	db.SetMaxOpenConns(1) // 强制连接级串行，配合 mutex 双保险。
	s := &Store{db: db, dir: dir}
	if err := s.init(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) init() error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS meta (
			key TEXT PRIMARY KEY,
			value TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS windows (
			window_index INTEGER PRIMARY KEY,
			payload TEXT NOT NULL,
			committed_at INTEGER NOT NULL
		)`,
	}
	for _, st := range stmts {
		if _, err := s.db.Exec(st); err != nil {
			return core.NewError(core.EIO, "init schema", map[string]any{"err": err.Error()})
		}
	}
	return nil
}

// Close 关闭数据库。
func (s *Store) Close() error { return s.db.Close() }

// Reset 整体作废快照：清空所有窗口与身份元数据（版本失配时调用）。
func (s *Store) Reset(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return core.NewError(core.EIO, "begin reset", map[string]any{"err": err.Error()})
	}
	if _, err := tx.Exec(`DELETE FROM windows`); err != nil {
		tx.Rollback()
		return core.NewError(core.EIO, "reset windows", map[string]any{"err": err.Error()})
	}
	if _, err := tx.Exec(`DELETE FROM meta`); err != nil {
		tx.Rollback()
		return core.NewError(core.EIO, "reset meta", map[string]any{"err": err.Error()})
	}
	return tx.Commit()
}

// SaveIdentity 写入当前快照身份（先 Reset 再写入，保证全量一致）。
func (s *Store) SaveIdentity(ctx context.Context, id Identity) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := json.Marshal(id)
	if err != nil {
		return core.NewError(core.EIO, "marshal identity", map[string]any{"err": err.Error()})
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return core.NewError(core.EIO, "begin identity", map[string]any{"err": err.Error()})
	}
	if _, err := tx.Exec(`INSERT INTO meta(key,value) VALUES('identity',?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, string(data)); err != nil {
		tx.Rollback()
		return core.NewError(core.EIO, "write identity", map[string]any{"err": err.Error()})
	}
	return tx.Commit()
}

// LoadIdentity 读取已持久化身份；不存在返回 ok=false。
func (s *Store) LoadIdentity(ctx context.Context) (Identity, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var raw string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM meta WHERE key='identity'`).Scan(&raw)
	if err == sql.ErrNoRows {
		return Identity{}, false, nil
	}
	if err != nil {
		return Identity{}, false, core.NewError(core.EIO, "load identity", map[string]any{"err": err.Error()})
	}
	var id Identity
	if err := json.Unmarshal([]byte(raw), &id); err != nil {
		return Identity{}, false, core.NewError(core.EIO, "unmarshal identity", map[string]any{"err": err.Error()})
	}
	return id, true, nil
}

// CommitWindow 串行且原子地提交一个已冻结窗口，强制窗口顺序稳定。
func (s *Store) CommitWindow(ctx context.Context, win *streaming.Window) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	// 顺序校验：只允许提交当前最大已提交窗口的下一个，防止乱序/交错。
	var maxIdx sql.NullInt64
	if err := s.db.QueryRowContext(ctx, `SELECT MAX(window_index) FROM windows`).Scan(&maxIdx); err != nil {
		return core.NewError(core.EIO, "query max window", map[string]any{"err": err.Error()})
	}
	expected := 0
	if maxIdx.Valid {
		expected = int(maxIdx.Int64) + 1
	}
	if win.Index != expected {
		return core.NewError(core.EState, "window commit out of order",
			map[string]any{"expected": expected, "got": win.Index})
	}

	payload, err := json.Marshal(win)
	if err != nil {
		return core.NewError(core.EIO, "marshal window", map[string]any{"window": win.Index})
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return core.NewError(core.EIO, "begin window", map[string]any{"err": err.Error()})
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO windows(window_index,payload,committed_at) VALUES(?,?,?)`,
		win.Index, string(payload), win.ConsumedOffset); err != nil {
		tx.Rollback()
		return core.NewError(core.EIO, "insert window", map[string]any{"window": win.Index, "err": err.Error()})
	}
	return tx.Commit()
}

// LoadWindows 按窗口顺序读取全部已提交窗口。
func (s *Store) LoadWindows(ctx context.Context) ([]*streaming.Window, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.QueryContext(ctx, `SELECT payload FROM windows ORDER BY window_index ASC`)
	if err != nil {
		return nil, core.NewError(core.EIO, "query windows", map[string]any{"err": err.Error()})
	}
	defer rows.Close()
	out := []*streaming.Window{}
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, core.NewError(core.EIO, "scan window", map[string]any{"err": err.Error()})
		}
		var win streaming.Window
		if err := json.Unmarshal([]byte(raw), &win); err != nil {
			return nil, core.NewError(core.EIO, "unmarshal window", map[string]any{"err": err.Error()})
		}
		out = append(out, &win)
	}
	return out, rows.Err()
}

// CommittedCount 返回已持久化的窗口数。
func (s *Store) CommittedCount(ctx context.Context) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM windows`).Scan(&n); err != nil {
		return 0, core.NewError(core.EIO, "count windows", map[string]any{"err": err.Error()})
	}
	return n, nil
}

// String 用于日志。
func (s *Store) String() string {
	return fmt.Sprintf("sqlite@%s", filepath.Join(s.dir, "checkpoints.db"))
}
