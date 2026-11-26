// Package checkpoint persists per-block stage checkpoints.
//
// The task allows only the Go standard library and repository-local pure-Go
// persistence. FileStore therefore implements the same one-transaction-per-
// stage contract with atomic temp-file rename. SQLiteStore provides the
// database/sql adapter and SQLite DDL for builds that register a SQLite driver.
package checkpoint

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"msworkbench/internal/domain"
	"msworkbench/internal/e"
)

const (
	StageQuant = "quant"
	StageRecon = "recon"
)

type StageRecord struct {
	RunID          string            `json:"run_id"`
	Seq            int64             `json:"seq"`
	Block          string            `json:"block"`
	Stage          string            `json:"stage"`
	OutputDigest   string            `json:"output_digest"`
	SelectedMethod string            `json:"selected_method"`
	ErrorBoundBP   int64             `json:"error_bound_bp"`
	CacheKey       string            `json:"cache_key"`
	BandCacheKeys  map[string]string `json:"band_cache_keys,omitempty"`
	ContentHash    string            `json:"content_hash"`
	ParamVersion   string            `json:"param_version"`
	AlgoVersion    string            `json:"algo_version"`
	Committed      bool              `json:"committed"`
}

type Transaction struct {
	Record  StageRecord     `json:"record"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

type fileData struct {
	Driver        string        `json:"driver"`
	SchemaVersion int           `json:"schema_version"`
	NextSeq       int64         `json:"next_seq"`
	Transactions  []Transaction `json:"transactions"`
}

type FileStore struct {
	path string
	mu   sync.Mutex
	data fileData
}

func OpenFile(path string) (*FileStore, error) {
	s := &FileStore{path: path, data: fileData{Driver: "msworkbench-purego-atomic-file", SchemaVersion: domain.SQLSchemaVersion, NextSeq: 1}}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, &s.data); err != nil {
		return nil, e.New(e.Ver, "checkpoint file is corrupt: %v", err)
	}
	if s.data.SchemaVersion != domain.SQLSchemaVersion {
		return nil, &e.Error{Code: e.Ver, Field: "schema_version", Expected: fmt.Sprint(domain.SQLSchemaVersion), Actual: fmt.Sprint(s.data.SchemaVersion), Message: "checkpoint schema version mismatch"}
	}
	if s.data.NextSeq == 0 {
		s.data.NextSeq = int64(len(s.data.Transactions)) + 1
	}
	return s, nil
}

func (s *FileStore) Path() string { return s.path }

// CommitStage appends exactly one stage transaction and atomically renames the
// temporary database into place. Calls are serialized by a mutex.
func (s *FileStore) CommitStage(rec StageRecord, payload any) (StageRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if rec.RunID == "" || rec.Block == "" || rec.Stage == "" {
		return StageRecord{}, e.New(e.Ver, "checkpoint record missing run/block/stage")
	}
	if rec.ParamVersion == "" {
		rec.ParamVersion = domain.ParamVersion
	}
	if rec.AlgoVersion == "" {
		rec.AlgoVersion = domain.AlgorithmVersion
	}
	for _, tx := range s.data.Transactions {
		if tx.Record.RunID == rec.RunID && tx.Record.Block == rec.Block && tx.Record.Stage == rec.Stage {
			return StageRecord{}, e.New(e.Ver, "duplicate committed stage %s/%s/%s", rec.RunID, rec.Block, rec.Stage)
		}
	}
	var raw json.RawMessage
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return StageRecord{}, err
		}
		raw = b
	}
	rec.Seq = s.data.NextSeq
	rec.Committed = true
	s.data.NextSeq++
	s.data.Transactions = append(s.data.Transactions, Transaction{Record: rec, Payload: raw})
	if err := s.persistLocked(); err != nil {
		return StageRecord{}, err
	}
	return rec, nil
}

func (s *FileStore) persistLocked() error {
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(&s.data, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".checkpoint-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, s.path); err != nil {
		return err
	}
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

func (s *FileStore) Transactions() []Transaction {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Transaction(nil), s.data.Transactions...)
}

func (s *FileStore) FindCache(runIndependentCacheKey string) (json.RawMessage, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := len(s.data.Transactions) - 1; i >= 0; i-- {
		tx := s.data.Transactions[i]
		if tx.Record.CacheKey == runIndependentCacheKey && len(tx.Payload) > 0 {
			return append(json.RawMessage(nil), tx.Payload...), true
		}
	}
	return nil, false
}

func (s *FileStore) FindBandCache(bandCacheKey string) (Transaction, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := len(s.data.Transactions) - 1; i >= 0; i-- {
		tx := s.data.Transactions[i]
		for _, key := range tx.Record.BandCacheKeys {
			if key == bandCacheKey && len(tx.Payload) > 0 {
				return tx, true
			}
		}
	}
	return Transaction{}, false
}

type ExpectedBlock struct {
	ContentHash string
	QuantKey    string
	ReconKey    string
}

// Recovery is the verified resume boundary. Any matching-run record whose
// content hash or version differs rejects the whole checkpoint with E_VER.
func (s *FileStore) RecoverRun(runID string, expectedHashes map[string]string) ([]Transaction, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []Transaction{}
	for _, tx := range s.data.Transactions {
		r := tx.Record
		if r.RunID != runID {
			continue
		}
		if r.ParamVersion != domain.ParamVersion || r.AlgoVersion != domain.AlgorithmVersion {
			return nil, &e.Error{Code: e.Ver, Block: r.Block, Field: "version", Expected: domain.ParamVersion + "/" + domain.AlgorithmVersion, Actual: r.ParamVersion + "/" + r.AlgoVersion, Message: "checkpoint version mismatch; whole recovery rejected"}
		}
		if expected, ok := expectedHashes[r.Block]; ok && r.ContentHash != "" && expected != "" && r.ContentHash != expected {
			return nil, &e.Error{Code: e.Ver, Block: r.Block, Field: "content_hash", Expected: expected, Actual: r.ContentHash, Message: "block content hash mismatch; whole recovery rejected"}
		}
		out = append(out, tx)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Record.Seq < out[j].Record.Seq })
	return out, nil
}

func (s *FileStore) LastCommittedBlockStage(runID string) (string, string, bool) {
	txs, err := s.RecoverRun(runID, nil)
	if err != nil || len(txs) == 0 {
		return "", "", false
	}
	last := txs[len(txs)-1].Record
	return last.Block, last.Stage, true
}
