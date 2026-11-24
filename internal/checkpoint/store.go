package checkpoint

import (
	"bytes"
	"compress/gzip"
	"context"
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	_ "modernc.org/sqlite"

	"terrainnormalverify/internal/bake"
	"terrainnormalverify/internal/model"
)

type Checkpoint struct {
	BlockID          int             `json:"blockId"`
	Revision         int64           `json:"revision"`
	ContentHash      string          `json:"contentHash"`
	NormalHash       string          `json:"normalHash"`
	SlopeHash        string          `json:"slopeHash"`
	ParameterVersion string          `json:"parameterVersion"`
	AlgorithmVersion string          `json:"algorithmVersion"`
	Dependencies     json.RawMessage `json:"dependencies"`
	SeamStats        json.RawMessage `json:"seamStats"`
	CommitSeq        int64           `json:"commitSeq"`
	Result           *bake.BlockResult
}

type Meta struct {
	Revision         int64  `json:"revision"`
	ProjectHash      string `json:"projectHash"`
	ParameterVersion string `json:"parameterVersion"`
	AlgorithmVersion string `json:"algorithmVersion"`
	Complete         bool   `json:"complete"`
	SnapshotOrder    []int  `json:"snapshotOrder"`
}

type Store struct {
	db   *sql.DB
	mu   sync.Mutex
	path string
}

func Open(path string) (*Store, error) {
	if dir := filepath.Dir(path); dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, model.NewError(model.ErrIO, -1, "dbPath", err.Error())
		}
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, model.NewError(model.ErrIO, -1, "db", err.Error())
	}
	db.SetMaxOpenConns(1)
	store := &Store{db: db, path: path}
	if err := store.init(); err != nil {
		db.Close()
		return nil, err
	}
	return store, nil
}

func (s *Store) init() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	statements := []string{
		`PRAGMA journal_mode=WAL`,
		`PRAGMA synchronous=FULL`,
		`PRAGMA busy_timeout=10000`,
		`CREATE TABLE IF NOT EXISTS meta (key TEXT PRIMARY KEY, value TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS blocks (
			block_id INTEGER PRIMARY KEY,
			revision INTEGER NOT NULL,
			content_hash TEXT NOT NULL,
			normal_hash TEXT NOT NULL,
			slope_hash TEXT NOT NULL,
			parameter_version TEXT NOT NULL,
			algorithm_version TEXT NOT NULL,
			dependencies TEXT NOT NULL,
			seam_stats TEXT NOT NULL,
			width INTEGER NOT NULL,
			height INTEGER NOT NULL,
			origin_x INTEGER NOT NULL,
			origin_y INTEGER NOT NULL,
			bbox TEXT NOT NULL,
			normals BLOB NOT NULL,
			slopes BLOB NOT NULL,
			raw_normals BLOB NOT NULL,
			raw_slopes BLOB NOT NULL,
			commit_seq INTEGER NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS commits (
			seq INTEGER PRIMARY KEY AUTOINCREMENT,
			block_id INTEGER NOT NULL,
			revision INTEGER NOT NULL,
			content_hash TEXT NOT NULL,
			normal_hash TEXT NOT NULL,
			slope_hash TEXT NOT NULL,
			parameter_version TEXT NOT NULL,
			algorithm_version TEXT NOT NULL,
			committed_at TEXT NOT NULL
		)`,
	}
	for _, statement := range statements {
		if _, err := s.db.ExecContext(ctx, statement); err != nil {
			return model.NewError(model.ErrIO, -1, "sqlite", err.Error())
		}
	}
	return nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) Path() string {
	return s.path
}

func (s *Store) CommitBlocks(cps []Checkpoint) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, cp := range cps {
		if err := s.commitOne(cp); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) ReplaceBlocks(deleteIDs []int, cps []Checkpoint) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return model.NewError(model.ErrIO, -1, "sqlite.begin", err.Error())
	}
	for _, id := range deleteIDs {
		if _, err := tx.Exec(`DELETE FROM blocks WHERE block_id=?`, id); err != nil {
			tx.Rollback()
			return model.NewError(model.ErrIO, id, "sqlite.delete", err.Error())
		}
	}
	if err := tx.Commit(); err != nil {
		return model.NewError(model.ErrIO, -1, "sqlite.txCommit", err.Error())
	}
	for _, cp := range cps {
		if err := s.commitOne(cp); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) commitOne(cp Checkpoint) error {
	if cp.Result == nil {
		return model.NewError(model.ErrBlock, cp.BlockID, "result", "checkpoint has no block result")
	}
	normalsBlob, err := encodeVec3s(cp.Result.Normals)
	if err != nil {
		return err
	}
	slopesBlob, err := encodeFloat64s(cp.Result.Slopes)
	if err != nil {
		return err
	}
	rawNormalsBlob, err := encodeVec3s(cp.Result.RawNormals)
	if err != nil {
		return err
	}
	rawSlopesBlob, err := encodeFloat64s(cp.Result.RawSlopes)
	if err != nil {
		return err
	}
	bboxJSON, _ := json.Marshal(cp.Result.BBox)
	ctx := context.Background()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.NewError(model.ErrIO, cp.BlockID, "sqlite.begin", err.Error())
	}
	defer tx.Rollback()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	result, err := tx.ExecContext(ctx, `INSERT INTO commits
		(block_id, revision, content_hash, normal_hash, slope_hash, parameter_version, algorithm_version, committed_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		cp.BlockID, cp.Revision, cp.ContentHash, cp.NormalHash, cp.SlopeHash,
		cp.ParameterVersion, cp.AlgorithmVersion, now)
	if err != nil {
		return model.NewError(model.ErrIO, cp.BlockID, "sqlite.commit", err.Error())
	}
	seq, err := result.LastInsertId()
	if err != nil {
		return model.NewError(model.ErrIO, cp.BlockID, "sqlite.commitSeq", err.Error())
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO blocks
		(block_id, revision, content_hash, normal_hash, slope_hash, parameter_version, algorithm_version,
		 dependencies, seam_stats, width, height, origin_x, origin_y, bbox, normals, slopes, raw_normals, raw_slopes, commit_seq)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(block_id) DO UPDATE SET
			revision=excluded.revision,
			content_hash=excluded.content_hash,
			normal_hash=excluded.normal_hash,
			slope_hash=excluded.slope_hash,
			parameter_version=excluded.parameter_version,
			algorithm_version=excluded.algorithm_version,
			dependencies=excluded.dependencies,
			seam_stats=excluded.seam_stats,
			width=excluded.width,
			height=excluded.height,
			origin_x=excluded.origin_x,
			origin_y=excluded.origin_y,
			bbox=excluded.bbox,
			normals=excluded.normals,
			slopes=excluded.slopes,
			raw_normals=excluded.raw_normals,
			raw_slopes=excluded.raw_slopes,
			commit_seq=excluded.commit_seq`,
		cp.BlockID, cp.Revision, cp.ContentHash, cp.NormalHash, cp.SlopeHash,
		cp.ParameterVersion, cp.AlgorithmVersion, string(cp.Dependencies), string(cp.SeamStats),
		cp.Result.Width, cp.Result.Height, cp.Result.OriginX, cp.Result.OriginY, string(bboxJSON),
		normalsBlob, slopesBlob, rawNormalsBlob, rawSlopesBlob, seq)
	if err != nil {
		return model.NewError(model.ErrIO, cp.BlockID, "sqlite.write", err.Error())
	}
	if err := tx.Commit(); err != nil {
		return model.NewError(model.ErrIO, cp.BlockID, "sqlite.txCommit", err.Error())
	}
	return nil
}

func (s *Store) DeleteBlocks(blockIDs []int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return model.NewError(model.ErrIO, -1, "sqlite.begin", err.Error())
	}
	defer tx.Rollback()
	for _, id := range blockIDs {
		if _, err := tx.Exec(`DELETE FROM blocks WHERE block_id=?`, id); err != nil {
			return model.NewError(model.ErrIO, id, "sqlite.delete", err.Error())
		}
	}
	return tx.Commit()
}

func (s *Store) Wipe() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return model.NewError(model.ErrIO, -1, "sqlite.begin", err.Error())
	}
	defer tx.Rollback()
	for _, statement := range []string{
		`DELETE FROM blocks`,
		`DELETE FROM commits`,
		`DELETE FROM meta`,
	} {
		if _, err := tx.Exec(statement); err != nil {
			return model.NewError(model.ErrIO, -1, "sqlite.wipe", err.Error())
		}
	}
	return tx.Commit()
}

func (s *Store) LoadAll(expectedHashes map[int]string, parameterVersion, algorithmVersion string) (map[int]*Checkpoint, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.Query(`SELECT block_id, revision, content_hash, normal_hash, slope_hash,
		parameter_version, algorithm_version, dependencies, seam_stats, width, height,
		origin_x, origin_y, bbox, normals, slopes, raw_normals, raw_slopes, commit_seq
		FROM blocks ORDER BY block_id`)
	if err != nil {
		return nil, model.NewError(model.ErrIO, -1, "sqlite.query", err.Error())
	}
	defer rows.Close()
	out := make(map[int]*Checkpoint)
	invalidReason := ""
	invalidField := "checkpoint"
	var invalidBlock int
	for rows.Next() {
		cp := &Checkpoint{Result: &bake.BlockResult{}}
		var dependencies, seamStats, bboxJSON string
		var normalsBlob, slopesBlob, rawNormalsBlob, rawSlopesBlob []byte
		if err := rows.Scan(&cp.BlockID, &cp.Revision, &cp.ContentHash, &cp.NormalHash, &cp.SlopeHash,
			&cp.ParameterVersion, &cp.AlgorithmVersion, &dependencies, &seamStats,
			&cp.Result.Width, &cp.Result.Height, &cp.Result.OriginX, &cp.Result.OriginY,
			&bboxJSON, &normalsBlob, &slopesBlob, &rawNormalsBlob, &rawSlopesBlob, &cp.CommitSeq); err != nil {
			return nil, model.NewError(model.ErrIO, -1, "sqlite.scan", err.Error())
		}
		cp.Dependencies = json.RawMessage(dependencies)
		cp.SeamStats = json.RawMessage(seamStats)
		cp.Result.BlockID = cp.BlockID
		expectedHash, exists := expectedHashes[cp.BlockID]
		switch {
		case cp.ParameterVersion != parameterVersion:
			invalidReason = "parameter version mismatch"
			invalidBlock = cp.BlockID
			invalidField = "parameterVersion"
		case cp.AlgorithmVersion != algorithmVersion:
			invalidReason = "algorithm version mismatch"
			invalidBlock = cp.BlockID
			invalidField = "algorithmVersion"
		case !exists:
			invalidReason = "checkpoint block is not part of current project"
			invalidBlock = cp.BlockID
			invalidField = "blockId"
		case cp.ContentHash != expectedHash:
			invalidReason = "content hash mismatch"
			invalidBlock = cp.BlockID
			invalidField = "contentHash"
		}
		if invalidReason != "" {
			break
		}
		normals, err := decodeVec3s(normalsBlob)
		if err != nil {
			invalidReason = "normal blob decode failure: " + err.Error()
			invalidBlock = cp.BlockID
			invalidField = "normals"
			break
		}
		slopes, err := decodeFloat64s(slopesBlob)
		if err != nil {
			invalidReason = "slope blob decode failure: " + err.Error()
			invalidBlock = cp.BlockID
			invalidField = "slopes"
			break
		}
		rawNormals, err := decodeVec3s(rawNormalsBlob)
		if err != nil {
			invalidReason = "raw normal blob decode failure: " + err.Error()
			invalidBlock = cp.BlockID
			invalidField = "rawNormals"
			break
		}
		rawSlopes, err := decodeFloat64s(rawSlopesBlob)
		if err != nil {
			invalidReason = "raw slope blob decode failure: " + err.Error()
			invalidBlock = cp.BlockID
			invalidField = "rawSlopes"
			break
		}
		cp.Result.Normals = normals
		cp.Result.Slopes = slopes
		cp.Result.RawNormals = rawNormals
		cp.Result.RawSlopes = rawSlopes
		cp.Result.Colors = make([]uint8, len(normals)*3)
		for i, normal := range normals {
			color := model.ColorCode(normal)
			copy(cp.Result.Colors[i*3:i*3+3], color[:])
		}
		if err := json.Unmarshal([]byte(bboxJSON), &cp.Result.BBox); err != nil {
			invalidReason = "bounding box decode failure: " + err.Error()
			invalidBlock = cp.BlockID
			invalidField = "bbox"
			break
		}
		if model.HashNormals(normals) != cp.NormalHash {
			invalidReason = "normal summary hash mismatch"
			invalidBlock = cp.BlockID
			invalidField = "normalHash"
			break
		}
		if model.HashFloat64(slopes) != cp.SlopeHash {
			invalidReason = "slope summary hash mismatch"
			invalidBlock = cp.BlockID
			invalidField = "slopeHash"
			break
		}
		if len(normals) != cp.Result.Width*cp.Result.Height || len(rawNormals) != len(normals) {
			invalidReason = "checkpoint pixel dimensions mismatch"
			invalidBlock = cp.BlockID
			invalidField = "dimensions"
			break
		}
		out[cp.BlockID] = cp
	}
	if err := rows.Err(); err != nil {
		return nil, model.NewError(model.ErrIO, -1, "sqlite.rows", err.Error())
	}
	if invalidReason != "" {
		rows.Close()
		if err := s.wipeLocked(); err != nil {
			return nil, err
		}
		e := model.NewError(model.ErrSnapshot, invalidBlock, invalidField, invalidReason)
		e.Value = invalidReason
		return nil, e
	}
	return out, nil
}

func (s *Store) wipeLocked() error {
	for _, statement := range []string{`DELETE FROM blocks`, `DELETE FROM commits`, `DELETE FROM meta`} {
		if _, err := s.db.Exec(statement); err != nil {
			return model.NewError(model.ErrIO, -1, "sqlite.wipe", err.Error())
		}
	}
	return nil
}

func (s *Store) Finalize(meta Meta) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return model.NewError(model.ErrIO, -1, "sqlite.begin", err.Error())
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE blocks SET revision=?`, meta.Revision); err != nil {
		return model.NewError(model.ErrIO, -1, "sqlite.revision", err.Error())
	}
	values := map[string]string{
		"revision":          fmt.Sprintf("%d", meta.Revision),
		"project_hash":      meta.ProjectHash,
		"parameter_version": meta.ParameterVersion,
		"algorithm_version": meta.AlgorithmVersion,
		"complete":          fmt.Sprintf("%t", meta.Complete),
		"snapshot_order":    mustJSON(meta.SnapshotOrder),
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if _, err := tx.Exec(`INSERT INTO meta(key, value) VALUES(?, ?)
			ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, values[key]); err != nil {
			return model.NewError(model.ErrIO, -1, "sqlite.meta", err.Error())
		}
	}
	return tx.Commit()
}

func (s *Store) MarkIncomplete(revision int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return model.NewError(model.ErrIO, -1, "sqlite.begin", err.Error())
	}
	defer tx.Rollback()
	for key, value := range map[string]string{
		"revision": fmt.Sprintf("%d", revision),
		"complete": "false",
	} {
		if _, err := tx.Exec(`INSERT INTO meta(key, value) VALUES(?, ?)
			ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value); err != nil {
			return model.NewError(model.ErrIO, -1, "sqlite.meta", err.Error())
		}
	}
	return tx.Commit()
}

func (s *Store) LoadMeta() (Meta, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.Query(`SELECT key, value FROM meta`)
	if err != nil {
		return Meta{}, model.NewError(model.ErrIO, -1, "sqlite.meta", err.Error())
	}
	defer rows.Close()
	values := make(map[string]string)
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			return Meta{}, model.NewError(model.ErrIO, -1, "sqlite.meta", err.Error())
		}
		values[key] = value
	}
	var meta Meta
	if value := values["revision"]; value != "" {
		fmt.Sscanf(value, "%d", &meta.Revision)
	}
	meta.ProjectHash = values["project_hash"]
	meta.ParameterVersion = values["parameter_version"]
	meta.AlgorithmVersion = values["algorithm_version"]
	meta.Complete = values["complete"] == "true"
	if values["snapshot_order"] != "" {
		_ = json.Unmarshal([]byte(values["snapshot_order"]), &meta.SnapshotOrder)
	}
	return meta, nil
}

func (s *Store) CommitSequence() ([]int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.Query(`SELECT seq FROM commits ORDER BY seq`)
	if err != nil {
		return nil, model.NewError(model.ErrIO, -1, "sqlite.commits", err.Error())
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var seq int64
		if err := rows.Scan(&seq); err != nil {
			return nil, model.NewError(model.ErrIO, -1, "sqlite.commits", err.Error())
		}
		out = append(out, seq)
	}
	return out, rows.Err()
}

func (s *Store) BlockCommitOrder() ([]int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.Query(`SELECT block_id FROM commits ORDER BY seq`)
	if err != nil {
		return nil, model.NewError(model.ErrIO, -1, "sqlite.commits", err.Error())
	}
	defer rows.Close()
	var out []int
	for rows.Next() {
		var id int
		if err := rows.Scan(&id); err != nil {
			return nil, model.NewError(model.ErrIO, -1, "sqlite.commits", err.Error())
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

func (s *Store) CountBlocks() (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM blocks`).Scan(&count); err != nil {
		return 0, model.NewError(model.ErrIO, -1, "sqlite.count", err.Error())
	}
	return count, nil
}

func (s *Store) LatestCommitOrder(revision int64) ([]int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.Query(`SELECT seq, block_id FROM commits WHERE revision=? ORDER BY seq`, revision)
	if err != nil {
		return nil, model.NewError(model.ErrIO, -1, "sqlite.commits", err.Error())
	}
	defer rows.Close()
	type entry struct {
		seq     int64
		blockID int
	}
	last := make(map[int]int64)
	for rows.Next() {
		var seq int64
		var blockID int
		if err := rows.Scan(&seq, &blockID); err != nil {
			return nil, model.NewError(model.ErrIO, -1, "sqlite.commits", err.Error())
		}
		last[blockID] = seq
	}
	if err := rows.Err(); err != nil {
		return nil, model.NewError(model.ErrIO, -1, "sqlite.commits", err.Error())
	}
	entries := make([]entry, 0, len(last))
	for blockID, seq := range last {
		entries = append(entries, entry{seq: seq, blockID: blockID})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].seq < entries[j].seq })
	order := make([]int, len(entries))
	for i, item := range entries {
		order[i] = item.blockID
	}
	return order, nil
}

func encodeVec3s(values []model.Vec3) ([]byte, error) {
	raw := make([]byte, len(values)*24)
	for i, value := range values {
		binary.LittleEndian.PutUint64(raw[i*24:i*24+8], math.Float64bits(value.X))
		binary.LittleEndian.PutUint64(raw[i*24+8:i*24+16], math.Float64bits(value.Y))
		binary.LittleEndian.PutUint64(raw[i*24+16:i*24+24], math.Float64bits(value.Z))
	}
	return gzipBytes(raw)
}

func decodeVec3s(data []byte) ([]model.Vec3, error) {
	raw, err := gunzipBytes(data)
	if err != nil {
		return nil, err
	}
	if len(raw)%24 != 0 {
		return nil, fmt.Errorf("invalid vec3 blob length %d", len(raw))
	}
	out := make([]model.Vec3, len(raw)/24)
	for i := range out {
		out[i] = model.Vec3{
			X: math.Float64frombits(binary.LittleEndian.Uint64(raw[i*24 : i*24+8])),
			Y: math.Float64frombits(binary.LittleEndian.Uint64(raw[i*24+8 : i*24+16])),
			Z: math.Float64frombits(binary.LittleEndian.Uint64(raw[i*24+16 : i*24+24])),
		}
	}
	return out, nil
}

func encodeFloat64s(values []float64) ([]byte, error) {
	raw := make([]byte, len(values)*8)
	for i, value := range values {
		binary.LittleEndian.PutUint64(raw[i*8:i*8+8], math.Float64bits(value))
	}
	return gzipBytes(raw)
}

func decodeFloat64s(data []byte) ([]float64, error) {
	raw, err := gunzipBytes(data)
	if err != nil {
		return nil, err
	}
	if len(raw)%8 != 0 {
		return nil, fmt.Errorf("invalid float64 blob length %d", len(raw))
	}
	out := make([]float64, len(raw)/8)
	for i := range out {
		out[i] = math.Float64frombits(binary.LittleEndian.Uint64(raw[i*8 : i*8+8]))
	}
	return out, nil
}

func gzipBytes(raw []byte) ([]byte, error) {
	var buf bytes.Buffer
	writer := gzip.NewWriter(&buf)
	if _, err := writer.Write(raw); err != nil {
		return nil, err
	}
	if err := writer.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func gunzipBytes(data []byte) ([]byte, error) {
	reader, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	return io.ReadAll(reader)
}

func mustJSON(value any) string {
	data, _ := json.Marshal(value)
	return string(data)
}
