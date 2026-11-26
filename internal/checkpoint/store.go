// Package checkpoint implements the repository-local atomic transaction store.
//
// The task permits an equivalent in-repository persistence scheme when no
// third-party SQLite driver may be added. CommitChunk appends one checksummed
// transaction record and fsyncs the append-only log; torn tails are ignored on
// recovery. Small JSON snapshots are written through temp-file + rename.
package checkpoint

import (
	"bufio"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"acousticfield/internal/acoustic"
)

const TxnFileName = "transactions.jsonl"

type CommitRecord struct {
	StoreSchema      string               `json:"store_schema"`
	RecordType       string               `json:"record_type"`
	RoundID          string               `json:"round_id"`
	TxnSeq           int64                `json:"txn_seq"`
	OrderIndex       int                  `json:"order_index"`
	ChunkID          string               `json:"chunk_id"`
	State            string               `json:"state"`
	PathDigest       string               `json:"path_digest"`
	ContentHash      string               `json:"content_hash"`
	CacheKey         string               `json:"cache_key"`
	ParamVersion     string               `json:"param_version"`
	AlgorithmVersion string               `json:"algorithm_version"`
	Dependencies     []string             `json:"dependencies"`
	Result           acoustic.ChunkResult `json:"result"`
	Checksum         string               `json:"checksum"`
}

type Snapshot struct {
	SchemaVersion    string          `json:"schema_version"`
	Level            acoustic.Level  `json:"level"`
	Params           acoustic.Params `json:"params"`
	TruthFingerprint string          `json:"truth_fingerprint"`
}

type Store struct {
	dir string
	mu  sync.Mutex
}

func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(filepath.Join(dir, "chunks"), 0o755); err != nil {
		return nil, acoustic.NewError(acoustic.CodeCheckpoint, dir, "directory", "cannot create checkpoint dir: %v", err)
	}
	return &Store{dir: dir}, nil
}

func (s *Store) Dir() string { return s.dir }

func (s *Store) TxnPath() string { return filepath.Join(s.dir, TxnFileName) }

func checksumRecord(r CommitRecord) string {
	r.Checksum = ""
	return acoustic.HashValue("commit-record:", r)
}

// CommitChunk serializes one chunk transaction. The store mutex guarantees no
// interleaved writes from this process; O_APPEND plus checksum framing makes a
// force-killed partial tail recoverable.
func (s *Store) CommitChunk(roundID string, txnSeq int64, orderIndex int, result acoustic.ChunkResult) (CommitRecord, error) {
	rec := CommitRecord{
		StoreSchema:      acoustic.StoreSchema,
		RecordType:       "chunk-commit",
		RoundID:          roundID,
		TxnSeq:           txnSeq,
		OrderIndex:       orderIndex,
		ChunkID:          result.ChunkID,
		State:            string("committed"),
		PathDigest:       result.PathDigest,
		ContentHash:      result.ContentHash,
		CacheKey:         result.CacheKey,
		ParamVersion:     result.ParamVersion,
		AlgorithmVersion: result.AlgorithmVersion,
		Dependencies:     append([]string(nil), result.Dependencies...),
		Result:           result,
	}
	rec.Checksum = checksumRecord(rec)
	line := append(acoustic.CanonicalJSON(rec), '\n')

	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := os.OpenFile(s.TxnPath(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return CommitRecord{}, acoustic.NewError(acoustic.CodeCheckpoint, result.ChunkID, "transactions", "open failed: %v", err)
	}
	defer f.Close()
	if _, err := f.Write(line); err != nil {
		return CommitRecord{}, acoustic.NewError(acoustic.CodeCheckpoint, result.ChunkID, "transactions", "atomic append failed: %v", err)
	}
	if err := f.Sync(); err != nil {
		return CommitRecord{}, acoustic.NewError(acoustic.CodeCheckpoint, result.ChunkID, "transactions", "fsync failed: %v", err)
	}
	if err := syncDir(s.dir); err != nil {
		return CommitRecord{}, err
	}
	return rec, nil
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return nil // Directory fsync is best-effort on some WSL mounts.
	}
	defer d.Close()
	_ = d.Sync()
	return nil
}

// ReadCommitted returns all complete checksum-valid records. A malformed final
// record is treated as a torn force-kill tail and ignored.
func (s *Store) ReadCommitted() ([]CommitRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := os.Open(s.TxnPath())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var out []CommitRecord
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 1024*1024), 16*1024*1024)
	lineNo := 0
	for scanner.Scan() {
		lineNo++
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var rec CommitRecord
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			break // torn tail
		}
		want := rec.Checksum
		if want == "" || checksumRecord(rec) != want {
			break // torn/altered tail; earlier transactions remain valid
		}
		out = append(out, rec)
	}
	if err := scanner.Err(); err != nil && !errors.Is(err, io.EOF) {
		return out, acoustic.NewError(acoustic.CodeCheckpoint, "transactions", "read", "scan failed: %v", err)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].TxnSeq != out[j].TxnSeq {
			return out[i].TxnSeq < out[j].TxnSeq
		}
		return out[i].ChunkID < out[j].ChunkID
	})
	return out, nil
}

// LatestMatching returns the highest-seq valid record per chunk for matching versions.
func (s *Store) LatestMatching(algorithmVersion, paramVersion string) (map[string]CommitRecord, []CommitRecord, error) {
	records, err := s.ReadCommitted()
	if err != nil {
		return nil, nil, err
	}
	latest := map[string]CommitRecord{}
	for _, rec := range records {
		if rec.AlgorithmVersion != algorithmVersion || rec.ParamVersion != paramVersion {
			continue
		}
		old, ok := latest[rec.ChunkID]
		if !ok || rec.TxnSeq > old.TxnSeq {
			latest[rec.ChunkID] = rec
		}
	}
	ordered := make([]CommitRecord, 0, len(latest))
	for _, rec := range latest {
		ordered = append(ordered, rec)
	}
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].ChunkID < ordered[j].ChunkID })
	return latest, ordered, nil
}

func (s *Store) SaveParams(p acoustic.Params) error {
	return WriteJSONAtomic(filepath.Join(s.dir, "params.json"), p)
}

func (s *Store) SaveSnapshot(level *acoustic.Level, truthFingerprint string) error {
	snapshot := Snapshot{
		SchemaVersion:    "acoustic-snapshot-v1",
		Level:            *level,
		Params:           level.Params,
		TruthFingerprint: truthFingerprint,
	}
	return WriteJSONAtomic(filepath.Join(s.dir, "snapshot.json"), snapshot)
}

func LoadSnapshot(dir string) (*Snapshot, error) {
	var snapshot Snapshot
	if err := ReadJSON(filepath.Join(dir, "snapshot.json"), &snapshot); err != nil {
		return nil, err
	}
	return &snapshot, nil
}

func (s *Store) LoadParams() (acoustic.Params, error) {
	var p acoustic.Params
	if err := ReadJSON(filepath.Join(s.dir, "params.json"), &p); err != nil {
		return p, err
	}
	return p, nil
}

func (s *Store) SaveTruth(t *acoustic.Truth) error {
	if err := WriteJSONAtomic(filepath.Join(s.dir, "truth.json"), t); err != nil {
		return err
	}
	return WriteAtomic(filepath.Join(s.dir, "truth.sha256"), []byte(t.Fingerprint+"  truth.json\n"))
}

func (s *Store) LoadTruth() (*acoustic.Truth, error) {
	var t acoustic.Truth
	if err := ReadJSON(filepath.Join(s.dir, "truth.json"), &t); err != nil {
		return nil, err
	}
	return &t, nil
}

func (s *Store) SaveReport(r *acoustic.Report) error {
	return WriteJSONAtomic(filepath.Join(s.dir, "report.json"), r)
}

func LoadReport(dir string) (*acoustic.Report, error) {
	var r acoustic.Report
	if err := ReadJSON(filepath.Join(dir, "report.json"), &r); err != nil {
		return nil, err
	}
	return &r, nil
}

func WriteJSONAtomic(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	return WriteAtomic(path, b)
}

func ReadJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, v); err != nil {
		return acoustic.NewError(acoustic.CodeCheckpoint, path, "json", "invalid persisted JSON: %v", err)
	}
	return nil
}

// WriteAtomic writes temp file, fsyncs, renames and fsyncs directory.
func WriteAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
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
	if err := os.Rename(tmpName, path); err != nil {
		return err
	}
	return syncDir(dir)
}

// CorruptTailForTest appends a torn transaction line.
func (s *Store) CorruptTailForTest() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := os.OpenFile(s.TxnPath(), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = f.WriteString(`{"record_type":"chunk-commit","checksum":"broken"` + "\n")
	return err
}
