package checkpoint

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sync"

	_ "modernc.org/sqlite"

	"terrain-occlusion/internal/model"
)

const SchemaVersion = "checkpoint-v1"

type Record struct {
	SampleIndex   int    `json:"sample_index"`
	LogicalTimeMs int64  `json:"logical_time_ms"`
	VisibleDigest string `json:"visible_digest"`
	UncertainJSON string `json:"uncertain_json"`
	ResidentJSON  string `json:"resident_json"`
	ShadowVersion string `json:"shadow_version"`
	CameraOffset  int    `json:"camera_offset"`
	ContentHash   string `json:"content_hash"`
	PrevHash      string `json:"prev_hash"`
	ResultJSON    string `json:"result_json"`
	EngineJSON    string `json:"engine_json"`
}

type Store struct {
	db   *sql.DB
	mu   sync.Mutex
	path string
}

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	s := &Store{db: db, path: path}
	if err := s.init(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) init() error {
	_, err := s.db.Exec(`
CREATE TABLE IF NOT EXISTS meta (
  key TEXT PRIMARY KEY,
  value TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS checkpoints (
  sample_index INTEGER PRIMARY KEY,
  logical_time_ms INTEGER NOT NULL,
  visible_digest TEXT NOT NULL,
  uncertain_json TEXT NOT NULL,
  resident_json TEXT NOT NULL,
  shadow_version TEXT NOT NULL,
  camera_offset INTEGER NOT NULL,
  content_hash TEXT NOT NULL,
  prev_hash TEXT NOT NULL,
  result_json TEXT NOT NULL,
  engine_json TEXT NOT NULL
);
`)
	if err != nil {
		return model.NewError(model.EHash, "sqlite.schema", "initialize checkpoint schema: %v", err)
	}
	_, err = s.db.Exec(`INSERT OR IGNORE INTO meta(key,value) VALUES('schema_version', ?)`, SchemaVersion)
	return err
}

func (s *Store) SetMeta(key, value string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`INSERT INTO meta(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value)
	return err
}

func (s *Store) GetMeta(key string) (string, error) {
	var v string
	err := s.db.QueryRow(`SELECT value FROM meta WHERE key=?`, key).Scan(&v)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return v, err
}

type hashPayload struct {
	SampleIndex   int    `json:"sample_index"`
	LogicalTimeMs int64  `json:"logical_time_ms"`
	VisibleDigest string `json:"visible_digest"`
	UncertainJSON string `json:"uncertain_json"`
	ResidentJSON  string `json:"resident_json"`
	ShadowVersion string `json:"shadow_version"`
	CameraOffset  int    `json:"camera_offset"`
	PrevHash      string `json:"prev_hash"`
	ResultJSON    string `json:"result_json"`
	EngineJSON    string `json:"engine_json"`
}

func ComputeHash(r Record, prevHash string) string {
	h := sha256.New()
	h.Write(model.CanonicalJSON(hashPayload{
		SampleIndex: r.SampleIndex, LogicalTimeMs: r.LogicalTimeMs,
		VisibleDigest: r.VisibleDigest, UncertainJSON: r.UncertainJSON,
		ResidentJSON: r.ResidentJSON, ShadowVersion: r.ShadowVersion,
		CameraOffset: r.CameraOffset, PrevHash: prevHash,
		ResultJSON: r.ResultJSON, EngineJSON: r.EngineJSON,
	}))
	return hex.EncodeToString(h.Sum(nil))
}

// Commit atomically appends one checkpoint. The mutex and one-connection pool
// guarantee that writers cannot interleave within this process.
func (s *Store) Commit(r Record) (string, error) {
	return s.CommitSnapshot("", r)
}

func (s *Store) CommitSnapshot(projectHash string, r Record) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.Begin()
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback() }()
	if projectHash != "" {
		if _, err := tx.Exec(`INSERT INTO meta(key,value) VALUES('project_hash',?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, projectHash); err != nil {
			return "", err
		}
	}

	var prev sql.NullString
	err = tx.QueryRow(`SELECT content_hash FROM checkpoints ORDER BY sample_index DESC LIMIT 1`).Scan(&prev)
	if err == sql.ErrNoRows {
		r.PrevHash = ""
	} else if err != nil {
		return "", err
	} else {
		r.PrevHash = prev.String
	}
	r.ContentHash = ComputeHash(r, r.PrevHash)
	_, err = tx.Exec(`INSERT INTO checkpoints
(sample_index,logical_time_ms,visible_digest,uncertain_json,resident_json,shadow_version,camera_offset,content_hash,prev_hash,result_json,engine_json)
VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		r.SampleIndex, r.LogicalTimeMs, r.VisibleDigest, r.UncertainJSON, r.ResidentJSON,
		r.ShadowVersion, r.CameraOffset, r.ContentHash, r.PrevHash, r.ResultJSON, r.EngineJSON)
	if err != nil {
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	return r.ContentHash, nil
}

func (s *Store) Records() ([]Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.Query(`SELECT sample_index,logical_time_ms,visible_digest,uncertain_json,resident_json,shadow_version,camera_offset,content_hash,prev_hash,result_json,engine_json FROM checkpoints ORDER BY sample_index ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Record{}
	for rows.Next() {
		var r Record
		if err := rows.Scan(&r.SampleIndex, &r.LogicalTimeMs, &r.VisibleDigest, &r.UncertainJSON, &r.ResidentJSON, &r.ShadowVersion, &r.CameraOffset, &r.ContentHash, &r.PrevHash, &r.ResultJSON, &r.EngineJSON); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// LoadVerified validates schema/project binding and the complete hash chain.
// Any failure discards the whole snapshot by returning nil records.
func (s *Store) LoadVerified(projectHash string) ([]Record, error) {
	version, err := s.GetMeta("schema_version")
	if err != nil {
		return nil, err
	}
	if version != SchemaVersion {
		return nil, &model.Error{Code: model.EHash, Field: "meta.schema_version", Message: "checkpoint schema version mismatch", Details: map[string]any{"want": SchemaVersion, "got": version}}
	}
	gotProject, err := s.GetMeta("project_hash")
	if err != nil {
		return nil, err
	}
	if projectHash != "" && gotProject != projectHash {
		return nil, &model.Error{Code: model.EHash, Field: "meta.project_hash", Message: "project hash mismatch; snapshot is void", Details: map[string]any{"want": projectHash, "got": gotProject}}
	}
	records, err := s.Records()
	if err != nil {
		return nil, &model.Error{Code: model.EHash, Field: "checkpoints", Message: "cannot read checkpoint path: %v", Details: map[string]any{"error": err.Error()}}
	}
	prev := ""
	expectedIndex := 0
	for _, r := range records {
		if r.SampleIndex != expectedIndex {
			return nil, &model.Error{Code: model.EHash, Field: "checkpoints.sample_index", Message: "checkpoint path is not contiguous", Details: map[string]any{"expected": expectedIndex, "got": r.SampleIndex}}
		}
		if r.PrevHash != prev {
			return nil, hashMismatch(r.SampleIndex, "prev_hash", prev, r.PrevHash)
		}
		want := ComputeHash(r, prev)
		if want != r.ContentHash {
			return nil, hashMismatch(r.SampleIndex, "content_hash", want, r.ContentHash)
		}
		prev = r.ContentHash
		expectedIndex++
	}
	return records, nil
}

func hashMismatch(sample int, field, want, got string) error {
	return &model.Error{Code: model.EHash, Field: "checkpoints." + field, Message: "hash/version mismatch at sample %d; whole snapshot void", Details: map[string]any{"sample_index": sample, "want": want, "got": got}}
}

func DigestIDs(ids []int) string {
	b, _ := json.Marshal(ids)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:16])
}

func ProjectHash(p *model.Project) string {
	cp := *p
	cp.GeneratedAt = ""
	cp.Truth.Projection = nil
	b, _ := json.Marshal(cp)
	sum := sha256.Sum256(b)
	return fmt.Sprintf("project-%s", hex.EncodeToString(sum[:16]))
}
