package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	_ "modernc.org/sqlite"

	"occlusionlab/internal/model"
	"occlusionlab/internal/schema"
	"occlusionlab/internal/streaming"
)

const StoreVersion = "occlusionlab-store/1"

type Header struct {
	SchemaVersion int    `json:"schema_version"`
	StoreVersion  string `json:"store_version"`
	SceneHash     string `json:"scene_hash"`
}

type CheckpointStore struct {
	mu     sync.Mutex
	db     *sql.DB
	path   string
	header Header
}

type LoadResult struct {
	Snapshots   []streaming.Snapshot
	Invalidated bool
	Reason      string
	Count       int
}

func Open(path string, header Header) (*CheckpointStore, *model.AppError) {
	if header.SchemaVersion != model.SchemaVersion || header.SceneHash == "" {
		return nil, model.VersionError("检查点头版本或场景哈希无效")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, model.IOError("checkpoint_path", "无法创建检查点目录", err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, model.IOError("sqlite", "无法打开 SQLite", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if _, err := db.Exec(`PRAGMA busy_timeout=5000`); err != nil {
		db.Close()
		return nil, model.IOError("sqlite.busy_timeout", "无法设置 SQLite 忙等待", err)
	}
	if _, err := db.Exec(`PRAGMA journal_mode=WAL`); err != nil {
		db.Close()
		return nil, model.IOError("sqlite.journal_mode", "无法启用 WAL", err)
	}
	if _, err := db.Exec(`PRAGMA synchronous=FULL`); err != nil {
		db.Close()
		return nil, model.IOError("sqlite.synchronous", "无法启用同步写入", err)
	}
	statements := []string{
		`CREATE TABLE IF NOT EXISTS checkpoint_meta (
			id INTEGER PRIMARY KEY CHECK (id = 1),
			schema_version INTEGER NOT NULL,
			store_version TEXT NOT NULL,
			scene_hash TEXT NOT NULL,
			created_at TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS window_checkpoints (
			window_index INTEGER PRIMARY KEY,
			result_json BLOB NOT NULL,
			params_json BLOB NOT NULL,
			scene_hash TEXT NOT NULL,
			params_hash TEXT NOT NULL,
			params_version INTEGER NOT NULL,
			content_hash TEXT NOT NULL,
			previous_hash TEXT NOT NULL,
			written_at TEXT NOT NULL
		)`,
	}
	for _, statement := range statements {
		if _, err := db.Exec(statement); err != nil {
			db.Close()
			return nil, model.IOError("sqlite.schema", "无法初始化检查点表", err)
		}
	}
	return &CheckpointStore{db: db, path: path, header: header}, nil
}

func (s *CheckpointStore) AppendSnapshot(snapshot streaming.Snapshot) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.appendLocked(snapshot)
}

func (s *CheckpointStore) appendLocked(snapshot streaming.Snapshot) error {
	if snapshot.Frame == nil || snapshot.Window != snapshot.Frame.Window {
		return fmt.Errorf("窗口编号与结果不一致")
	}
	if snapshot.SceneHash != s.header.SceneHash || snapshot.Frame.SceneHash != s.header.SceneHash {
		return fmt.Errorf("场景哈希不匹配，快照版本失效")
	}
	if err := verifyFrame(snapshot.Frame); err != nil {
		return err
	}
	if snapshot.ParamsHash != snapshot.Frame.ParamsHash ||
		snapshot.ParamsVersion != snapshot.Frame.ParamsVersion {
		return fmt.Errorf("参数版本与窗口结果不一致")
	}
	resultJSON, err := json.Marshal(snapshot.Frame)
	if err != nil {
		return err
	}
	paramsJSON, err := json.Marshal(snapshot.Params)
	if err != nil {
		return err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := s.ensureHeaderTx(tx); err != nil {
		return err
	}
	var count int
	var maxIndex sql.NullInt64
	if err := tx.QueryRow(`SELECT COUNT(*), MAX(window_index) FROM window_checkpoints`).Scan(&count, &maxIndex); err != nil {
		return err
	}
	expected := 0
	prevHash := zeroHash()
	if count > 0 {
		if !maxIndex.Valid || int(maxIndex.Int64)+1 != count {
			return fmt.Errorf("检查点窗口不连续")
		}
		expected = count
		if err := tx.QueryRow(`SELECT content_hash FROM window_checkpoints WHERE window_index = ?`,
			maxIndex.Int64).Scan(&prevHash); err != nil {
			return err
		}
	}
	if snapshot.Window != expected {
		return fmt.Errorf("窗口顺序不稳定：期望 %d，收到 %d", expected, snapshot.Window)
	}
	contentHash := checkpointHash(s.header, snapshot, resultJSON, paramsJSON, prevHash)
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err = tx.Exec(`INSERT INTO window_checkpoints (
		window_index, result_json, params_json, scene_hash, params_hash,
		params_version, content_hash, previous_hash, written_at
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		snapshot.Window, resultJSON, paramsJSON, s.header.SceneHash,
		snapshot.ParamsHash, snapshot.ParamsVersion, contentHash, prevHash, now)
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (s *CheckpointStore) ensureHeaderTx(tx *sql.Tx) error {
	var schemaVersion int
	var storeVersion, sceneHash string
	err := tx.QueryRow(`SELECT schema_version, store_version, scene_hash FROM checkpoint_meta WHERE id = 1`).
		Scan(&schemaVersion, &storeVersion, &sceneHash)
	if errors.Is(err, sql.ErrNoRows) {
		_, err = tx.Exec(`INSERT INTO checkpoint_meta
			(id, schema_version, store_version, scene_hash, created_at) VALUES (1, ?, ?, ?, ?)`,
			s.header.SchemaVersion, s.header.StoreVersion, s.header.SceneHash,
			time.Now().UTC().Format(time.RFC3339Nano))
		return err
	}
	if err != nil {
		return err
	}
	if schemaVersion != s.header.SchemaVersion || storeVersion != s.header.StoreVersion ||
		sceneHash != s.header.SceneHash {
		return model.VersionError("检查点整体版本或场景哈希不匹配")
	}
	return nil
}

func (s *CheckpointStore) LoadAll() (*LoadResult, *model.AppError) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return nil, model.IOError("sqlite", "无法读取检查点事务", err)
	}
	defer tx.Rollback()
	var schemaVersion int
	var storeVersion, sceneHash string
	err = tx.QueryRow(`SELECT schema_version, store_version, scene_hash FROM checkpoint_meta WHERE id = 1`).
		Scan(&schemaVersion, &storeVersion, &sceneHash)
	if errors.Is(err, sql.ErrNoRows) {
		return &LoadResult{}, nil
	}
	if err != nil {
		return nil, model.IOError("checkpoint_meta", "无法读取检查点头", err)
	}
	if schemaVersion != s.header.SchemaVersion || storeVersion != s.header.StoreVersion ||
		sceneHash != s.header.SceneHash {
		reason := "检查点头版本或场景哈希不匹配"
		if err := s.invalidateTx(tx); err != nil {
			return nil, model.IOError("checkpoint_reset", "无法作废旧快照", err)
		}
		if err := tx.Commit(); err != nil {
			return nil, model.IOError("checkpoint_reset", "无法提交快照作废", err)
		}
		return &LoadResult{Invalidated: true, Reason: reason}, nil
	}
	rows, err := tx.Query(`SELECT window_index, result_json, params_json, scene_hash, params_hash,
		params_version, content_hash, previous_hash FROM window_checkpoints ORDER BY window_index`)
	if err != nil {
		return nil, model.IOError("window_checkpoints", "无法读取窗口检查点", err)
	}
	defer rows.Close()
	var snapshots []streaming.Snapshot
	prevHash := zeroHash()
	for rows.Next() {
		var window int
		var resultJSON, paramsJSON []byte
		var rowSceneHash, paramsHash, contentHash, previousHash string
		var paramsVersion int
		if err := rows.Scan(&window, &resultJSON, &paramsJSON, &rowSceneHash, &paramsHash,
			&paramsVersion, &contentHash, &previousHash); err != nil {
			return nil, model.IOError("window_checkpoints", "无法扫描窗口检查点", err)
		}
		reason := ""
		if window != len(snapshots) {
			reason = "窗口顺序不连续"
		} else if rowSceneHash != s.header.SceneHash {
			reason = fmt.Sprintf("窗口 %d 场景哈希不匹配", window)
		} else if previousHash != prevHash {
			reason = fmt.Sprintf("窗口 %d 哈希链断裂", window)
		}
		var frame streaming.WindowFrame
		if reason == "" {
			if err := json.Unmarshal(resultJSON, &frame); err != nil {
				reason = fmt.Sprintf("窗口 %d 结果无法解码", window)
			}
		}
		var params model.Params
		if reason == "" {
			if err := json.Unmarshal(paramsJSON, &params); err != nil {
				reason = fmt.Sprintf("窗口 %d 参数无法解码", window)
			}
		}
		if reason == "" {
			if err := schema.ValidateParams(params); err != nil {
				reason = fmt.Sprintf("窗口 %d 参数版本无效: %s", window, err.Field)
			}
		}
		if reason == "" {
			if frame.SceneHash != s.header.SceneHash || frame.Window != window ||
				frame.ParamsHash != paramsHash || frame.ParamsVersion != paramsVersion {
				reason = fmt.Sprintf("窗口 %d 内容或参数版本不匹配", window)
			}
		}
		if reason == "" {
			if err := verifyFrame(&frame); err != nil {
				reason = fmt.Sprintf("窗口 %d 内容哈希不匹配: %v", window, err)
			}
		}
		if reason != "" {
			if err := s.invalidateTx(tx); err != nil {
				return nil, model.IOError("checkpoint_reset", "无法作废旧快照", err)
			}
			if err := tx.Commit(); err != nil {
				return nil, model.IOError("checkpoint_reset", "无法提交快照作废", err)
			}
			return &LoadResult{Invalidated: true, Reason: reason}, nil
		}
		wantHash := checkpointHash(s.header, streaming.Snapshot{
			Window: window, Frame: &frame, Params: params, SceneHash: rowSceneHash,
			ParamsHash: paramsHash, ParamsVersion: paramsVersion,
		}, resultJSON, paramsJSON, prevHash)
		if contentHash != wantHash {
			reason = fmt.Sprintf("窗口 %d 检查点内容哈希不匹配", window)
			if err := s.invalidateTx(tx); err != nil {
				return nil, model.IOError("checkpoint_reset", "无法作废旧快照", err)
			}
			if err := tx.Commit(); err != nil {
				return nil, model.IOError("checkpoint_reset", "无法提交快照作废", err)
			}
			return &LoadResult{Invalidated: true, Reason: reason}, nil
		}
		snapshots = append(snapshots, streaming.Snapshot{
			Window: window, Frame: &frame, Params: params, SceneHash: rowSceneHash,
			ParamsHash: paramsHash, ParamsVersion: paramsVersion,
		})
		prevHash = contentHash
	}
	if err := rows.Err(); err != nil {
		return nil, model.IOError("window_checkpoints", "遍历窗口检查点失败", err)
	}
	return &LoadResult{Snapshots: snapshots, Count: len(snapshots)}, nil
}

func (s *CheckpointStore) Invalidate(reason string) *model.AppError {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return model.IOError("checkpoint_reset", "无法开始作废事务", err)
	}
	defer tx.Rollback()
	if err := s.invalidateTx(tx); err != nil {
		return model.IOError("checkpoint_reset", "无法作废检查点", err)
	}
	if err := tx.Commit(); err != nil {
		return model.IOError("checkpoint_reset", "无法提交作废事务", err)
	}
	return nil
}

func (s *CheckpointStore) invalidateTx(tx *sql.Tx) error {
	if _, err := tx.Exec(`DELETE FROM window_checkpoints`); err != nil {
		return err
	}
	_, err := tx.Exec(`DELETE FROM checkpoint_meta`)
	return err
}

func (s *CheckpointStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.db == nil {
		return nil
	}
	err := s.db.Close()
	s.db = nil
	return err
}

func (s *CheckpointStore) Path() string { return s.path }

func verifyFrame(frame *streaming.WindowFrame) error {
	if frame.VisibleDigest != model.CanonicalHash(frame.Visible) {
		return fmt.Errorf("可见集合摘要不匹配")
	}
	copy := *frame
	copy.ResultHash = ""
	if frame.ResultHash != model.CanonicalHash(copy) {
		return fmt.Errorf("窗口结果哈希不匹配")
	}
	return nil
}

func checkpointHash(header Header, snapshot streaming.Snapshot, resultJSON, paramsJSON []byte,
	prevHash string) string {

	return model.CanonicalHash(struct {
		Header        Header `json:"header"`
		Window        int    `json:"window"`
		SceneHash     string `json:"scene_hash"`
		ParamsHash    string `json:"params_hash"`
		ParamsVersion int    `json:"params_version"`
		Result        []byte `json:"result"`
		Params        []byte `json:"params"`
		PreviousHash  string `json:"previous_hash"`
	}{
		Header: header, Window: snapshot.Window, SceneHash: snapshot.SceneHash,
		ParamsHash: snapshot.ParamsHash, ParamsVersion: snapshot.ParamsVersion,
		Result: resultJSON, Params: paramsJSON, PreviousHash: prevHash,
	})
}

func zeroHash() string { return "0000000000000000000000000000000000000000000000000000000000000000" }
