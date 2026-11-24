package store

import (
	"database/sql"
	"encoding/json"
	"fmt"

	_ "modernc.org/sqlite"
)

// SchemaVersion 是检查点表结构版本，结构变更必须升级。
const SchemaVersion = 1

// SegmentRecord 是一行已提交分段检查点。
type SegmentRecord struct {
	ChannelID     string
	SegmentIndex  int
	Start         int64
	End           int64
	DriftJSON     string
	Output        []byte // float64le 的对齐输出
	ContentHash   string
	ParamVersion  string
	AlgoVersion   string
	SchemaVersion int
}

// Store 封装 SQLite 检查点。所有写操作经单一写互斥串行执行，保证不交错。
type Store struct {
	db      *sql.DB
	writer  chan func()
	stopped chan struct{}
}

// Open 打开（必要时创建）检查点数据库。
func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=synchronous(FULL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // 单连接，从根上保证写入串行
	s := &Store{db: db, writer: make(chan func(), 1024), stopped: make(chan struct{})}
	if err := s.init(); err != nil {
		db.Close()
		return nil, err
	}
	go s.writeLoop()
	return s, nil
}

func (s *Store) init() error {
	_, err := s.db.Exec(`
CREATE TABLE IF NOT EXISTS meta (
  key TEXT PRIMARY KEY,
  value TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS segments (
  channel_id TEXT NOT NULL,
  segment_index INTEGER NOT NULL,
  start_sample INTEGER NOT NULL,
  end_sample INTEGER NOT NULL,
  drift_json TEXT NOT NULL,
  output BLOB NOT NULL,
  content_hash TEXT NOT NULL,
  param_version TEXT NOT NULL,
  algo_version TEXT NOT NULL,
  schema_version INTEGER NOT NULL,
  seq INTEGER NOT NULL,
  PRIMARY KEY (channel_id, segment_index)
);
CREATE TABLE IF NOT EXISTS commit_log (
  seq INTEGER PRIMARY KEY AUTOINCREMENT,
  channel_id TEXT NOT NULL,
  segment_index INTEGER NOT NULL,
  content_hash TEXT NOT NULL
);`)
	return err
}

// writeLoop 是唯一的写入协程：所有提交按入队顺序串行原子落盘。
func (s *Store) writeLoop() {
	for fn := range s.writer {
		fn()
	}
	close(s.stopped)
}

// Close 等待排队写入落盘后关闭数据库。
func (s *Store) Close() error {
	close(s.writer)
	<-s.stopped
	return s.db.Close()
}

// PutMeta 写入元信息（会话指纹、参数版本等）。
func (s *Store) PutMeta(key, value string) error {
	errCh := make(chan error, 1)
	s.writer <- func() {
		_, err := s.db.Exec(
			`INSERT INTO meta(key,value) VALUES(?,?)
			 ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value)
		errCh <- err
	}
	return <-errCh
}

// GetMeta 读取元信息。
func (s *Store) GetMeta(key string) (string, error) {
	var v string
	row := s.db.QueryRow(`SELECT value FROM meta WHERE key=?`, key)
	if err := row.Scan(&v); err == sql.ErrNoRows {
		return "", nil
	} else if err != nil {
		return "", err
	}
	return v, nil
}

// CommitSegment 在单个事务内原子写入分段与提交日志，保证提交序列稳定。
func (s *Store) CommitSegment(r SegmentRecord) (int64, error) {
	errCh := make(chan error, 1)
	var seq int64
	s.writer <- func() {
		tx, err := s.db.Begin()
		if err != nil {
			errCh <- err
			return
		}
		res, err := tx.Exec(`INSERT INTO commit_log(channel_id,segment_index,content_hash)
			VALUES(?,?,?)`, r.ChannelID, r.SegmentIndex, r.ContentHash)
		if err != nil {
			tx.Rollback()
			errCh <- err
			return
		}
		seq, _ = res.LastInsertId()
		r.SchemaVersion = SchemaVersion
		_, err = tx.Exec(`INSERT INTO segments
			(channel_id,segment_index,start_sample,end_sample,drift_json,output,
			 content_hash,param_version,algo_version,schema_version,seq)
			VALUES(?,?,?,?,?,?,?,?,?,?,?)
			ON CONFLICT(channel_id,segment_index) DO UPDATE SET
			 start_sample=excluded.start_sample, end_sample=excluded.end_sample,
			 drift_json=excluded.drift_json, output=excluded.output,
			 content_hash=excluded.content_hash, param_version=excluded.param_version,
			 algo_version=excluded.algo_version, schema_version=excluded.schema_version,
			 seq=excluded.seq`,
			r.ChannelID, r.SegmentIndex, r.Start, r.End, r.DriftJSON, r.Output,
			r.ContentHash, r.ParamVersion, r.AlgoVersion, r.SchemaVersion, seq)
		if err != nil {
			tx.Rollback()
			errCh <- err
			return
		}
		errCh <- tx.Commit()
	}
	if err := <-errCh; err != nil {
		return 0, err
	}
	return seq, nil
}

// LoadAll 按提交序列顺序读取全部分段与元信息，用于恢复。
func (s *Store) LoadAll() (map[string]map[int]SegmentRecord, []string, error) {
	rows, err := s.db.Query(`SELECT channel_id,segment_index,start_sample,end_sample,
		drift_json,output,content_hash,param_version,algo_version,schema_version
		FROM segments ORDER BY seq ASC, channel_id ASC, segment_index ASC`)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	out := map[string]map[int]SegmentRecord{}
	var order []string
	for rows.Next() {
		var r SegmentRecord
		if err := rows.Scan(&r.ChannelID, &r.SegmentIndex, &r.Start, &r.End,
			&r.DriftJSON, &r.Output, &r.ContentHash, &r.ParamVersion,
			&r.AlgoVersion, &r.SchemaVersion); err != nil {
			return nil, nil, err
		}
		if out[r.ChannelID] == nil {
			out[r.ChannelID] = map[int]SegmentRecord{}
		}
		out[r.ChannelID][r.SegmentIndex] = r
		order = append(order, fmt.Sprintf("%s#%d", r.ChannelID, r.SegmentIndex))
	}
	return out, order, rows.Err()
}

// Reset 清空全部检查点（整体快照作废时使用）。
func (s *Store) Reset() error {
	errCh := make(chan error, 1)
	s.writer <- func() {
		_, err := s.db.Exec(`DELETE FROM segments; DELETE FROM commit_log; DELETE FROM meta;`)
		errCh <- err
	}
	return <-errCh
}

// MarshalDrift 序列化漂移参数。
func MarshalDrift(v any) (string, error) {
	b, err := json.Marshal(v)
	return string(b), err
}
