package store

import (
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"sync"

	_ "modernc.org/sqlite"

	"terrainkernel/internal/bake"
	"terrainkernel/internal/kerr"
	"terrainkernel/internal/model"
)

const AlgorithmVersion = "algo-1.0.0"

type Fingerprint struct {
	ProjectID        string
	ProjectHash      string
	NormVersion      string
	ParamVersion     string
	AlgorithmVersion string
}

type ChunkRecord struct {
	ChunkID       int
	WeightSummary string
	BlendSummary  string
	Dependencies  []string
	ContentHash   string
	NormVersion   string
	ParamVersion  string
	AlgorithmVer  string
	CommitSeq     int
	Dominants     []int
	Blends        []int64
	WeightRows    [][]int64
}

type Store struct {
	db      *sql.DB
	fp      Fingerprint
	writeMu sync.Mutex
}

func NormVersion(mode string) string {
	return "norm-" + mode + "-1"
}

func ParamVersion(p *model.Project) string {
	sum := bake.GlobalParamHash(p)
	if len(sum) > 16 {
		sum = sum[:16]
	}
	return "param-" + sum
}

func BuildFingerprint(p *model.Project) Fingerprint {
	return Fingerprint{
		ProjectID:        p.ProjectID,
		ProjectHash:      bake.GlobalParamHash(p),
		NormVersion:      NormVersion(p.NormMode),
		ParamVersion:     ParamVersion(p),
		AlgorithmVersion: AlgorithmVersion,
	}
}

func Open(path string, fingerprint Fingerprint) (*Store, error) {
	if path == "" {
		return nil, kerr.New(kerr.CodeSchema, "checkpoint_path", "checkpoint path is empty")
	}
	if dir := filepath.Dir(path); dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
	}
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_txlock=immediate", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	s := &Store{db: db, fp: fingerprint}
	if err := s.init(); err != nil {
		db.Close()
		return nil, err
	}
	stored, err := s.loadFingerprint()
	if err != nil {
		db.Close()
		return nil, err
	}
	if stored == nil || *stored != fingerprint {
		if err := s.resetSnapshot(); err != nil {
			db.Close()
			return nil, err
		}
		if err := s.saveFingerprint(fingerprint); err != nil {
			db.Close()
			return nil, err
		}
	}
	return s, nil
}

func (s *Store) init() error {
	statements := []string{
		`CREATE TABLE IF NOT EXISTS meta(
			key TEXT PRIMARY KEY,
			value TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS chunks(
			chunk_id INTEGER PRIMARY KEY,
			weight_summary TEXT NOT NULL,
			blend_summary TEXT NOT NULL,
			dependencies TEXT NOT NULL,
			content_hash TEXT NOT NULL,
			norm_version TEXT NOT NULL,
			param_version TEXT NOT NULL,
			algorithm_version TEXT NOT NULL,
			commit_seq INTEGER NOT NULL,
			dominants BLOB NOT NULL,
			blends BLOB NOT NULL,
			weights BLOB NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS chunks_commit_seq ON chunks(commit_seq)`,
	}
	for _, statement := range statements {
		if _, err := s.db.Exec(statement); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

func (s *Store) Fingerprint() Fingerprint {
	return s.fp
}

func (s *Store) loadFingerprint() (*Fingerprint, error) {
	rows, err := s.db.Query(`SELECT key, value FROM meta`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	values := make(map[string]string)
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			return nil, err
		}
		values[key] = value
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	keys := []string{"project_id", "project_hash", "norm_version", "param_version", "algorithm_version"}
	for _, key := range keys {
		if values[key] == "" {
			return nil, nil
		}
	}
	fp := Fingerprint{
		ProjectID:        values["project_id"],
		ProjectHash:      values["project_hash"],
		NormVersion:      values["norm_version"],
		ParamVersion:     values["param_version"],
		AlgorithmVersion: values["algorithm_version"],
	}
	return &fp, nil
}

func (s *Store) saveFingerprint(fp Fingerprint) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	values := map[string]string{
		"project_id":        fp.ProjectID,
		"project_hash":      fp.ProjectHash,
		"norm_version":      fp.NormVersion,
		"param_version":     fp.ParamVersion,
		"algorithm_version": fp.AlgorithmVersion,
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if _, err := tx.Exec(`INSERT INTO meta(key, value) VALUES(?, ?)
			ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, values[key]); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) resetSnapshot() error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM chunks`); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM meta`); err != nil {
		return err
	}
	return tx.Commit()
}

// ResetSnapshot discards every committed chunk. It is used when any checkpoint
// record cannot be proven to belong to the current global snapshot.
func (s *Store) ResetSnapshot() error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return s.resetSnapshotAndFingerprint()
}

func (s *Store) resetSnapshotAndFingerprint() error {
	if err := s.resetSnapshot(); err != nil {
		return err
	}
	return s.saveFingerprint(s.fp)
}

func (s *Store) InvalidateChunks(ids []int) error {
	if len(ids) == 0 {
		return nil
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, id := range ids {
		if _, err := tx.Exec(`DELETE FROM chunks WHERE chunk_id=?`, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) CommitChunk(record ChunkRecord) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var next sql.NullInt64
	if err := tx.QueryRow(`SELECT MAX(commit_seq) FROM chunks`).Scan(&next); err != nil {
		return err
	}
	seq := 0
	if next.Valid {
		seq = int(next.Int64) + 1
	}
	deps, err := json.Marshal(record.Dependencies)
	if err != nil {
		return err
	}
	dominants := encodeInts(record.Dominants)
	blends := encodeInt64s(record.Blends)
	weights := encodeRows(record.WeightRows)
	_, err = tx.Exec(`INSERT INTO chunks(
			chunk_id, weight_summary, blend_summary, dependencies, content_hash,
			norm_version, param_version, algorithm_version, commit_seq,
			dominants, blends, weights
		) VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(chunk_id) DO UPDATE SET
			weight_summary=excluded.weight_summary,
			blend_summary=excluded.blend_summary,
			dependencies=excluded.dependencies,
			content_hash=excluded.content_hash,
			norm_version=excluded.norm_version,
			param_version=excluded.param_version,
			algorithm_version=excluded.algorithm_version,
			commit_seq=excluded.commit_seq,
			dominants=excluded.dominants,
			blends=excluded.blends,
			weights=excluded.weights`,
		record.ChunkID, record.WeightSummary, record.BlendSummary, string(deps), record.ContentHash,
		record.NormVersion, record.ParamVersion, record.AlgorithmVer, seq,
		dominants, blends, weights)
	if err != nil {
		return err
	}
	return tx.Commit()
}

// LoadCommitted validates every row before returning any reusable result. A
// single mismatch discards the whole snapshot.
func (s *Store) LoadCommitted(expectedChunks int, expectedDeps map[int][]string, expectedParam map[int]string) (map[int]*bake.ChunkResult, []int, string, error) {
	rows, err := s.db.Query(`SELECT chunk_id, weight_summary, blend_summary, dependencies,
		content_hash, norm_version, param_version, algorithm_version, commit_seq,
		dominants, blends, weights FROM chunks ORDER BY commit_seq, chunk_id`)
	if err != nil {
		return nil, nil, "", err
	}
	defer rows.Close()
	results := make(map[int]*bake.ChunkResult)
	sequence := make([]int, 0)
	previousSeq := -1
	failReason := ""
	for rows.Next() {
		var record ChunkRecord
		var deps string
		var dominants, blends, weights []byte
		if err := rows.Scan(
			&record.ChunkID, &record.WeightSummary, &record.BlendSummary, &deps,
			&record.ContentHash, &record.NormVersion, &record.ParamVersion,
			&record.AlgorithmVer, &record.CommitSeq, &dominants, &blends, &weights,
		); err != nil {
			return nil, nil, "", err
		}
		if record.CommitSeq <= previousSeq {
			failReason = "commit sequence is not strictly increasing"
			continue
		}
		previousSeq = record.CommitSeq
		if err := json.Unmarshal([]byte(deps), &record.Dependencies); err != nil {
			failReason = fmt.Sprintf("chunk %d dependency JSON is corrupt", record.ChunkID)
			continue
		}
		if record.NormVersion != s.fp.NormVersion ||
			record.AlgorithmVer != s.fp.AlgorithmVersion ||
			(expectedParam != nil && record.ParamVersion != expectedParam[record.ChunkID]) {
			failReason = fmt.Sprintf("chunk %d version mismatch", record.ChunkID)
			continue
		}
		if expectedDeps != nil && !equalStrings(record.Dependencies, expectedDeps[record.ChunkID]) {
			failReason = fmt.Sprintf("chunk %d dependency set mismatch", record.ChunkID)
			continue
		}
		record.Dominants, err = decodeInts(dominants)
		if err != nil {
			failReason = fmt.Sprintf("chunk %d dominant data is corrupt", record.ChunkID)
			continue
		}
		record.Blends, err = decodeInt64s(blends)
		if err != nil {
			failReason = fmt.Sprintf("chunk %d blend data is corrupt", record.ChunkID)
			continue
		}
		record.WeightRows, err = decodeRows(weights)
		if err != nil {
			failReason = fmt.Sprintf("chunk %d weight data is corrupt", record.ChunkID)
			continue
		}
		if expectedChunks > 0 && (record.ChunkID < 0 || record.ChunkID >= expectedChunks) {
			failReason = fmt.Sprintf("chunk %d is outside project range", record.ChunkID)
			continue
		}
		if bake.WeightSummary(record.WeightRows) != record.WeightSummary ||
			bake.BlendSummary(record.Blends) != record.BlendSummary ||
			bake.ContentHash(record.WeightRows, record.Dominants, record.Blends) != record.ContentHash {
			failReason = fmt.Sprintf("chunk %d content hash mismatch", record.ChunkID)
			continue
		}
		size := int(math.Sqrt(float64(len(record.WeightRows))))
		if size*size != len(record.WeightRows) || len(record.Dominants) != len(record.WeightRows) || len(record.Blends) != len(record.WeightRows) {
			failReason = fmt.Sprintf("chunk %d shape mismatch", record.ChunkID)
			continue
		}
		result := &bake.ChunkResult{
			ChunkID:      record.ChunkID,
			Size:         size,
			ParamVersion: record.ParamVersion,
			WeightsHash:  record.WeightSummary,
			BlendHash:    record.BlendSummary,
			ContentHash:  record.ContentHash,
			Dominants:    record.Dominants,
			Blends:       record.Blends,
			WeightRows:   record.WeightRows,
			Dependencies: append([]string(nil), record.Dependencies...),
		}
		results[record.ChunkID] = result
		sequence = append(sequence, record.ChunkID)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, "", err
	}
	if failReason != "" {
		_ = rows.Close()
		s.writeMu.Lock()
		err := s.resetSnapshotAndFingerprint()
		s.writeMu.Unlock()
		if err != nil {
			return nil, nil, "", err
		}
		return map[int]*bake.ChunkResult{}, nil, failReason, nil
	}
	return results, sequence, "", nil
}

func (s *Store) CommitSequence() ([]int, error) {
	rows, err := s.db.Query(`SELECT chunk_id FROM chunks ORDER BY commit_seq, chunk_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var sequence []int
	for rows.Next() {
		var id int
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		sequence = append(sequence, id)
	}
	return sequence, rows.Err()
}

type CheckpointCommitter struct {
	store *Store
}

func NewCommitter(s *Store) *CheckpointCommitter {
	return &CheckpointCommitter{store: s}
}

func (c *CheckpointCommitter) Commit(chunkID int, result *bake.ChunkResult, dependencies []string) error {
	if result == nil {
		return kerr.New(kerr.CodeSchema, "chunk", "nil result")
	}
	paramVersion := result.ParamVersion
	if paramVersion == "" {
		paramVersion = c.store.fp.ParamVersion
	}
	return c.store.CommitChunk(ChunkRecord{
		ChunkID:       chunkID,
		WeightSummary: result.WeightsHash,
		BlendSummary:  result.BlendHash,
		Dependencies:  append([]string(nil), dependencies...),
		ContentHash:   result.ContentHash,
		NormVersion:   c.store.fp.NormVersion,
		ParamVersion:  paramVersion,
		AlgorithmVer:  c.store.fp.AlgorithmVersion,
		Dominants:     append([]int(nil), result.Dominants...),
		Blends:        append([]int64(nil), result.Blends...),
		WeightRows:    cloneRows(result.WeightRows),
	})
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	aa := append([]string(nil), a...)
	bb := append([]string(nil), b...)
	sort.Strings(aa)
	sort.Strings(bb)
	for i := range aa {
		if aa[i] != bb[i] {
			return false
		}
	}
	return true
}

func cloneRows(rows [][]int64) [][]int64 {
	out := make([][]int64, len(rows))
	for i := range rows {
		out[i] = append([]int64(nil), rows[i]...)
	}
	return out
}

func encodeInt64s(values []int64) []byte {
	var out []byte
	var buf [binary.MaxVarintLen64]byte
	n := binary.PutUvarint(buf[:], uint64(len(values)))
	out = append(out, buf[:n]...)
	for _, value := range values {
		n = binary.PutVarint(buf[:], value)
		out = append(out, buf[:n]...)
	}
	return out
}

func decodeInt64s(data []byte) ([]int64, error) {
	count, n := binary.Uvarint(data)
	if n <= 0 {
		return nil, fmt.Errorf("invalid length")
	}
	data = data[n:]
	out := make([]int64, 0, count)
	for i := uint64(0); i < count; i++ {
		value, n := binary.Varint(data)
		if n <= 0 {
			return nil, fmt.Errorf("invalid value")
		}
		data = data[n:]
		out = append(out, value)
	}
	if len(data) != 0 {
		return nil, fmt.Errorf("trailing bytes")
	}
	return out, nil
}

func encodeInts(values []int) []byte {
	converted := make([]int64, len(values))
	for i, value := range values {
		converted[i] = int64(value)
	}
	return encodeInt64s(converted)
}

func decodeInts(data []byte) ([]int, error) {
	values, err := decodeInt64s(data)
	if err != nil {
		return nil, err
	}
	out := make([]int, len(values))
	for i, value := range values {
		out[i] = int(value)
	}
	return out, nil
}

func encodeRows(rows [][]int64) []byte {
	var out []byte
	var buf [binary.MaxVarintLen64]byte
	n := binary.PutUvarint(buf[:], uint64(len(rows)))
	out = append(out, buf[:n]...)
	for _, row := range rows {
		n = binary.PutUvarint(buf[:], uint64(len(row)))
		out = append(out, buf[:n]...)
		for _, value := range row {
			n = binary.PutVarint(buf[:], value)
			out = append(out, buf[:n]...)
		}
	}
	return out
}

func decodeRows(data []byte) ([][]int64, error) {
	count, n := binary.Uvarint(data)
	if n <= 0 {
		return nil, fmt.Errorf("invalid row count")
	}
	data = data[n:]
	rows := make([][]int64, 0, count)
	for i := uint64(0); i < count; i++ {
		width, n := binary.Uvarint(data)
		if n <= 0 {
			return nil, fmt.Errorf("invalid row width")
		}
		data = data[n:]
		row := make([]int64, 0, width)
		for j := uint64(0); j < width; j++ {
			value, n := binary.Varint(data)
			if n <= 0 {
				return nil, fmt.Errorf("invalid weight")
			}
			data = data[n:]
			row = append(row, value)
		}
		rows = append(rows, row)
	}
	if len(data) != 0 {
		return nil, fmt.Errorf("trailing bytes")
	}
	return rows, nil
}
