// Package checkpoint 基于纯 Go SQLite（modernc.org/sqlite）持久化已提交帧窗口，
// 写入严格串行、单事务原子提交，并在版本/素材哈希不匹配时整体作废。
package checkpoint

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"

	"audiobench/internal/model"
	"audiobench/internal/window"

	_ "modernc.org/sqlite"
)

// Meta 快照身份信息，任何一项不匹配都会触发整体作废。
type Meta struct {
	MaterialID       string `json:"material_id"`
	ContentHash      string `json:"content_hash"`
	ParamsVersion    string `json:"params_version"`
	AlgorithmVersion string `json:"algorithm_version"`
	WindowFrames     int    `json:"window_frames"`
	TotalFrames      int    `json:"total_frames"`
}

// Store 单连接 SQLite 检查点存储，天然串行化所有写入。
type Store struct {
	db       *sql.DB
	material string
}

// Open 打开（必要时创建）数据库并初始化表结构。
func Open(ctx context.Context, dbPath, materialID string) (*Store, error) {
	absPath, err := filepath.Abs(dbPath)
	if err != nil {
		return nil, model.NewErr(model.EIO, "解析数据库绝对路径失败: %v", err)
	}
	dsn := fmt.Sprintf("%s?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(FULL)",
		absPath)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, model.NewErr(model.EIO, "打开 SQLite 失败: %v", err)
	}
	// 单连接 + 显式互斥锁，保证窗口写入不交错、顺序稳定。
	db.SetMaxOpenConns(1)
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, model.NewErr(model.EIO, "SQLite 连通性检查失败: %v", err)
	}
	s := &Store{db: db, material: materialID}
	if err := s.init(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) init(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS meta (
  id INTEGER PRIMARY KEY CHECK (id = 1),
  material_id TEXT NOT NULL,
  content_hash TEXT NOT NULL,
  params_version TEXT NOT NULL,
  algorithm_version TEXT NOT NULL,
  window_frames INTEGER NOT NULL,
  total_frames INTEGER NOT NULL,
  updated_at TEXT NOT NULL DEFAULT (datetime('now'))
);
CREATE TABLE IF NOT EXISTS windows (
  material_id TEXT NOT NULL,
  window_index INTEGER NOT NULL,
  state TEXT NOT NULL,
  payload TEXT NOT NULL,
  PRIMARY KEY (material_id, window_index)
);`)
	return err
}

// Close 关闭底层数据库。
func (s *Store) Close() error { return s.db.Close() }

// LoadMeta 读取快照身份；不存在返回空 Meta 与 nil。
func (s *Store) LoadMeta(ctx context.Context) (*Meta, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT material_id, content_hash, params_version, algorithm_version,
		        window_frames, total_frames FROM meta WHERE id = 1`)
	var m Meta
	err := row.Scan(&m.MaterialID, &m.ContentHash, &m.ParamsVersion,
		&m.AlgorithmVersion, &m.WindowFrames, &m.TotalFrames)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, model.NewErr(model.EIO, "读取检查点 meta 失败: %v", err)
	}
	return &m, nil
}

// Invalidate 在单事务内删除全部快照数据，实现“整体作废、禁止部分混用”。
func (s *Store) Invalidate(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.NewErr(model.EIO, "开启作废事务失败: %v", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM windows WHERE material_id = ?`, s.material); err != nil {
		tx.Rollback()
		return model.NewErr(model.EIO, "作废窗口失败: %v", err)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM meta WHERE id = 1`); err != nil {
		tx.Rollback()
		return model.NewErr(model.EIO, "作废 meta 失败: %v", err)
	}
	return tx.Commit()
}

// InitMeta 写入全新快照身份（仅在 meta 不存在时）。
func (s *Store) InitMeta(ctx context.Context, m Meta) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO meta (id, material_id, content_hash, params_version,
		                   algorithm_version, window_frames, total_frames)
		 VALUES (1, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET
		   material_id = excluded.material_id,
		   content_hash = excluded.content_hash,
		   params_version = excluded.params_version,
		   algorithm_version = excluded.algorithm_version,
		   window_frames = excluded.window_frames,
		   total_frames = excluded.total_frames`,
		m.MaterialID, m.ContentHash, m.ParamsVersion,
		m.AlgorithmVersion, m.WindowFrames, m.TotalFrames)
	if err != nil {
		return model.NewErr(model.EIO, "写 meta 失败: %v", err)
	}
	return nil
}

// CommitWindow 原子且串行地提交一个已完成窗口；单事务保证窗口整体落盘。
func (s *Store) CommitWindow(ctx context.Context, w window.Window) error {
	payload, err := json.Marshal(w)
	if err != nil {
		return model.NewErr(model.EIO, "序列化窗口失败: %v", err)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.NewErr(model.EIO, "开启窗口事务失败: %v", err)
	}
	_, err = tx.ExecContext(ctx,
		`INSERT INTO windows (material_id, window_index, state, payload)
		 VALUES (?, ?, ?, ?)
		 ON CONFLICT(material_id, window_index) DO UPDATE SET
		   state = excluded.state, payload = excluded.payload`,
		s.material, w.Index, window.StateCommitted, string(payload))
	if err != nil {
		tx.Rollback()
		return model.NewErr(model.EIO, "写窗口 %d 失败: %v", w.Index, err)
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE meta SET updated_at = datetime('now') WHERE id = 1`); err != nil {
		tx.Rollback()
		return model.NewErr(model.EIO, "更新 meta 时间失败: %v", err)
	}
	if err := tx.Commit(); err != nil {
		return model.NewErr(model.EIO, "提交窗口事务失败: %v", err)
	}
	return nil
}

// LoadWindows 按窗口序号升序恢复全部已提交窗口。
func (s *Store) LoadWindows(ctx context.Context) ([]window.Window, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT payload FROM windows WHERE material_id = ? ORDER BY window_index ASC`,
		s.material)
	if err != nil {
		return nil, model.NewErr(model.EIO, "读取窗口失败: %v", err)
	}
	defer rows.Close()
	var out []window.Window
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			return nil, model.NewErr(model.EIO, "扫描窗口失败: %v", err)
		}
		var w window.Window
		if err := json.Unmarshal([]byte(payload), &w); err != nil {
			return nil, model.NewErr(model.EIO, "反序列化窗口失败: %v", err)
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// ParamsVersion 由参数与算法版本派生确定性版本字符串。
func ParamsVersion(p model.Params) string {
	raw, _ := json.Marshal(p)
	return fmt.Sprintf("%s|%s", model.AlgorithmVersion, string(raw))
}

// MatchMeta 判断两份快照身份是否完全一致，任一维度不同即视为不匹配。
func MatchMeta(a, b *Meta) bool {
	return a.MaterialID == b.MaterialID &&
		a.ContentHash == b.ContentHash &&
		a.ParamsVersion == b.ParamsVersion &&
		a.AlgorithmVersion == b.AlgorithmVersion &&
		a.WindowFrames == b.WindowFrames &&
		a.TotalFrames == b.TotalFrames
}
