// Package checkpoint 用 SQLite 实现每提交一帧原子、串行写入的检查点。
// 写入在单连接上由互斥锁串行化，帧序稳定；重启时输入哈希或算法版本不匹配，
// 整体快照作废并全量重算，禁止混用旧帧。
package checkpoint

import (
	"database/sql"
	"encoding/json"
	"fmt"

	_ "github.com/mattn/go-sqlite3"

	"spatialir/internal/model"
)

// AlgorithmVersion 与分析/渲染算法绑定，算法语义变化必须提升该版本，
// 重启加载将据此判定旧快照作废。
const AlgorithmVersion = "sir-algo/1.0.0"

// Store 封装单个 SQLite 连接与写互斥。
type Store struct {
	db   *sql.DB
	path string
}

// Open 打开（必要时初始化）检查点库。
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite3", path+"?_busy_timeout=5000&_journal_mode=WAL&_synchronous=FULL")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // 串行写入，杜绝交错
	s := &Store{db: db, path: path}
	if err := s.init(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) init() error {
	_, err := s.db.Exec(`
CREATE TABLE IF NOT EXISTS snapshot_meta (
  id INTEGER PRIMARY KEY CHECK (id = 1),
  input_hash TEXT NOT NULL,
  algo_version TEXT NOT NULL,
  params_json TEXT NOT NULL,
  total_frames INTEGER NOT NULL,
  created_at TEXT NOT NULL DEFAULT (datetime('now'))
);
CREATE TABLE IF NOT EXISTS frames (
  frame_index INTEGER PRIMARY KEY,
  status TEXT NOT NULL,
  payload_json TEXT NOT NULL
);`)
	return err
}

func (s *Store) Close() error { return s.db.Close() }

// InitSnapshot 在一次流水线开始时登记输入哈希与参数。
// 若已有快照的哈希/版本不匹配，则整体清空（全量重算，不保留任何旧帧）。
func (s *Store) InitSnapshot(inputHash string, params model.FrameParams, totalFrames int) (resumed bool, err error) {
	paramsJSON, _ := json.Marshal(params)
	row := s.db.QueryRow(`SELECT input_hash, algo_version, params_json FROM snapshot_meta WHERE id = 1`)
	var oldHash, oldAlgo, oldParams string
	scanErr := row.Scan(&oldHash, &oldAlgo, &oldParams)
	if scanErr == sql.ErrNoRows {
		_, err = s.db.Exec(`INSERT INTO snapshot_meta(id,input_hash,algo_version,params_json,total_frames)
			VALUES(1,?,?,?,?)`, inputHash, AlgorithmVersion, string(paramsJSON), totalFrames)
		return false, err
	}
	if scanErr != nil {
		return false, scanErr
	}
	if oldHash != inputHash || oldAlgo != AlgorithmVersion || oldParams != string(paramsJSON) {
		if _, err := s.db.Exec(`DELETE FROM frames; DELETE FROM snapshot_meta;`); err != nil {
			return false, err
		}
		_, err = s.db.Exec(`INSERT INTO snapshot_meta(id,input_hash,algo_version,params_json,total_frames)
			VALUES(1,?,?,?,?)`, inputHash, AlgorithmVersion, string(paramsJSON), totalFrames)
		return false, err
	}
	return true, nil
}

// CommitFrame 在单个事务里原子写入一帧，调用方必须保证按帧序提交。
func (s *Store) CommitFrame(snap model.FrameSnapshot) error {
	payload, err := json.Marshal(snap)
	if err != nil {
		return err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`INSERT INTO frames(frame_index,status,payload_json) VALUES(?,?,?)
		ON CONFLICT(frame_index) DO UPDATE SET status=excluded.status, payload_json=excluded.payload_json`,
		snap.Index, string(snap.Status), string(payload)); err != nil {
		return err
	}
	return tx.Commit()
}

// LoadFrames 按帧序读出全部已提交帧；未提交帧不出现在结果中。
func (s *Store) LoadFrames() ([]model.FrameSnapshot, error) {
	rows, err := s.db.Query(`SELECT payload_json FROM frames WHERE status = ? ORDER BY frame_index ASC`,
		string(model.StatusCommitted))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.FrameSnapshot
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			return nil, err
		}
		var snap model.FrameSnapshot
		if err := json.Unmarshal([]byte(raw), &snap); err != nil {
			return nil, err
		}
		out = append(out, snap)
	}
	return out, rows.Err()
}

// Meta 返回当前快照元数据（诊断/导出用）。
func (s *Store) Meta() (inputHash, algoVersion, paramsJSON string, total int, err error) {
	err = s.db.QueryRow(`SELECT input_hash, algo_version, params_json, total_frames FROM snapshot_meta WHERE id=1`).
		Scan(&inputHash, &algoVersion, &paramsJSON, &total)
	return
}

// Reset 清空整个快照（测试与手动全量重算入口）。
func (s *Store) Reset() error {
	_, err := s.db.Exec(`DELETE FROM frames; DELETE FROM snapshot_meta;`)
	return err
}

func (s *Store) String() string {
	return fmt.Sprintf("checkpoint(%s)", s.path)
}
