package checkpoint

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"screenrelay/internal/litedb"
	"screenrelay/internal/model"
)

type Commit struct {
	WindowSeq        int
	WindowID         string
	Tick             int64
	SourceVersion    string
	AlgorithmVersion string
	ContentHash      string
	PrevHash         string
	Payload          any
}

type Record struct {
	WindowSeq        int64  `json:"window_seq"`
	WindowID         string `json:"window_id"`
	Tick             int64  `json:"tick"`
	SourceVersion    string `json:"source_version"`
	AlgorithmVersion string `json:"algorithm_version"`
	ContentHash      string `json:"content_hash"`
	PrevHash         string `json:"prev_hash"`
	PayloadJSON      string `json:"payload_json"`
}

type Store struct {
	db   *sql.DB
	path string
}

func Open(path string) (*Store, error) {
	db, err := sql.Open(litedb.DriverName, path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	store := &Store{db: db, path: path}
	if err := store.init(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

func (s *Store) init() error {
	_, err := s.db.Exec(`CREATE TABLE IF NOT EXISTS checkpoint_windows (
		window_seq INTEGER PRIMARY KEY,
		window_id TEXT,
		tick INTEGER,
		source_version TEXT,
		algorithm_version TEXT,
		content_hash TEXT,
		prev_hash TEXT,
		payload_json TEXT
	)`)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`CREATE TABLE IF NOT EXISTS checkpoint_meta (
		meta_key TEXT PRIMARY KEY,
		meta_value TEXT
	)`)
	return err
}

func (s *Store) Close() error { return s.db.Close() }

func HashPayload(payload any) (string, []byte, error) {
	data, err := json.Marshal(payload)
	if err != nil {
		return "", nil, model.NewError(model.EInternal, "checkpoint marshal: %v", err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), data, nil
}

func (s *Store) Commit(c Commit) (*Record, error) {
	hash, data, err := HashPayload(c.Payload)
	if err != nil {
		return nil, err
	}
	if c.ContentHash == "" {
		c.ContentHash = hash
	}
	if c.ContentHash != hash {
		return nil, model.NewError(model.EInternal, "checkpoint content hash mismatch before commit seq=%d", c.WindowSeq)
	}
	record := Record{
		WindowSeq: int64(c.WindowSeq), WindowID: c.WindowID, Tick: c.Tick,
		SourceVersion: c.SourceVersion, AlgorithmVersion: c.AlgorithmVersion,
		ContentHash: c.ContentHash, PrevHash: c.PrevHash, PayloadJSON: string(data),
	}
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	_, err = tx.Exec(`INSERT OR REPLACE INTO checkpoint_windows
		(window_seq, window_id, tick, source_version, algorithm_version, content_hash, prev_hash, payload_json)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		record.WindowSeq, record.WindowID, record.Tick, record.SourceVersion,
		record.AlgorithmVersion, record.ContentHash, record.PrevHash, record.PayloadJSON)
	if err != nil {
		return nil, err
	}
	_, err = tx.Exec(`INSERT OR REPLACE INTO checkpoint_meta (meta_key, meta_value) VALUES (?, ?)`,
		"latest_seq", fmt.Sprint(record.WindowSeq))
	if err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &record, nil
}

func (s *Store) Records() ([]Record, error) {
	rows, err := s.db.Query(`SELECT window_seq, window_id, tick, source_version, algorithm_version, content_hash, prev_hash, payload_json
		FROM checkpoint_windows ORDER BY window_seq ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Record{}
	for rows.Next() {
		var r Record
		if err := rows.Scan(&r.WindowSeq, &r.WindowID, &r.Tick, &r.SourceVersion, &r.AlgorithmVersion, &r.ContentHash, &r.PrevHash, &r.PayloadJSON); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

type Recovery struct {
	Record        *Record `json:"record"`
	InvalidSeq    int64   `json:"invalid_seq,omitempty"`
	InvalidReason string  `json:"invalid_reason,omitempty"`
}

func (s *Store) LatestValid(expectedAlgo string) (Recovery, error) {
	return s.LatestValidVersions(expectedAlgo, "")
}

func (s *Store) LatestValidVersions(expectedAlgo, expectedSource string) (Recovery, error) {
	records, err := s.Records()
	if err != nil {
		return Recovery{}, err
	}
	if len(records) == 0 {
		return Recovery{}, nil
	}
	var last *Record
	expectedPrev := ""
	for i := range records {
		r := records[i]
		sum := sha256.Sum256([]byte(r.PayloadJSON))
		actualHash := hex.EncodeToString(sum[:])
		invalidReason := ""
		if actualHash != r.ContentHash {
			invalidReason = "content hash mismatch"
		} else if r.AlgorithmVersion != expectedAlgo {
			invalidReason = fmt.Sprintf("algorithm version mismatch: checkpoint=%s expected=%s", r.AlgorithmVersion, expectedAlgo)
		} else if r.SourceVersion == "" {
			invalidReason = "missing source version"
		} else if expectedSource != "" && r.SourceVersion != expectedSource {
			invalidReason = fmt.Sprintf("source version mismatch: checkpoint=%s expected=%s", r.SourceVersion, expectedSource)
		} else if r.PrevHash != expectedPrev {
			invalidReason = fmt.Sprintf("prev hash mismatch: checkpoint=%s expected=%s", r.PrevHash, expectedPrev)
		}
		if invalidReason != "" {
			seq := r.WindowSeq
			return Recovery{Record: last, InvalidSeq: seq, InvalidReason: invalidReason}, nil
		}
		copyRecord := r
		last = &copyRecord
		expectedPrev = r.ContentHash
	}
	return Recovery{Record: last}, nil
}

func (s *Store) LoadPayload(expectedAlgo string, target any) (Recovery, error) {
	recovery, err := s.LatestValid(expectedAlgo)
	return s.loadPayload(recovery, err, target)
}

func (s *Store) LoadPayloadVersions(expectedAlgo, expectedSource string, target any) (Recovery, error) {
	recovery, err := s.LatestValidVersions(expectedAlgo, expectedSource)
	return s.loadPayload(recovery, err, target)
}

func (s *Store) loadPayload(recovery Recovery, err error, target any) (Recovery, error) {
	if err != nil {
		return recovery, err
	}
	if recovery.Record == nil {
		return recovery, nil
	}
	if err := json.Unmarshal([]byte(recovery.Record.PayloadJSON), target); err != nil {
		return recovery, model.NewError(model.EDecode, "decode committed payload seq=%d: %v", recovery.Record.WindowSeq, err)
	}
	return recovery, nil
}

func IsNoRows(err error) bool { return errors.Is(err, sql.ErrNoRows) }
