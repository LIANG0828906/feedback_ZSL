package checkpoint

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"time"

	_ "modernc.org/sqlite"

	"iconseg/internal/core"
	"iconseg/internal/layers"
)

type SnapshotRecord struct {
	ID               string              `json:"id"`
	ImageID          string              `json:"image_id"`
	Seed             int64               `json:"seed"`
	ParamVersion     string              `json:"param_version"`
	AlgorithmVersion string              `json:"algorithm_version"`
	Params           core.SegmentParams  `json:"params"`
	TruthFingerprint string              `json:"truth_fingerprint"`
	RegionTable      []core.RegionRecord `json:"region_table"`
	MaskHash         string              `json:"mask_hash"`
	StatsHash        string              `json:"stats_hash"`
	Status           string              `json:"status"`
	UpdatedNS        int64               `json:"updated_ns"`
}

type Row struct {
	SnapshotID       string `json:"snapshot_id"`
	BlockIndex       int    `json:"block_index"`
	BlockX           int    `json:"block_x"`
	BlockY           int    `json:"block_y"`
	BlockW           int    `json:"block_w"`
	BlockH           int    `json:"block_h"`
	Layer            string `json:"layer"`
	State            State  `json:"state"`
	Summary          string `json:"summary"`
	ContentHash      string `json:"content_hash"`
	Content          []byte `json:"-"`
	ParamVersion     string `json:"param_version"`
	AlgorithmVersion string `json:"algorithm_version"`
	Seq              int64  `json:"seq"`
	UpdatedNS        int64  `json:"updated_ns"`
}

type Store struct {
	db      *sql.DB
	path    string
	writeMu sync.Mutex
}

func Open(path string) (*Store, error) {
	if path == "" {
		return nil, core.ParaError("database_path", "non-empty", "SQLite database path is required")
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create checkpoint directory: %w", err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	for _, pragma := range []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA synchronous=FULL",
		"PRAGMA foreign_keys=ON",
		"PRAGMA busy_timeout=5000",
	} {
		if _, err := db.Exec(pragma); err != nil {
			db.Close()
			return nil, fmt.Errorf("%s: %w", pragma, err)
		}
	}
	s := &Store{db: db, path: path}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) Path() string {
	return s.path
}

func (s *Store) migrate() error {
	_, err := s.db.Exec(`
CREATE TABLE IF NOT EXISTS snapshots (
  snapshot_id TEXT PRIMARY KEY,
  image_id TEXT NOT NULL,
  seed INTEGER NOT NULL,
  param_version TEXT NOT NULL,
  algorithm_version TEXT NOT NULL,
  params_json TEXT NOT NULL,
  truth_fingerprint TEXT NOT NULL,
  region_table_json TEXT NOT NULL DEFAULT '[]',
  mask_hash TEXT NOT NULL DEFAULT '',
  stats_hash TEXT NOT NULL DEFAULT '',
  status TEXT NOT NULL,
  updated_ns INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS blocks (
  snapshot_id TEXT NOT NULL,
  block_index INTEGER NOT NULL,
  block_x INTEGER NOT NULL,
  block_y INTEGER NOT NULL,
  block_w INTEGER NOT NULL,
  block_h INTEGER NOT NULL,
  layer TEXT NOT NULL,
  state TEXT NOT NULL,
  summary TEXT NOT NULL DEFAULT '',
  content_hash TEXT NOT NULL DEFAULT '',
  content BLOB,
  param_version TEXT NOT NULL,
  algorithm_version TEXT NOT NULL,
  seq INTEGER NOT NULL DEFAULT 0,
  updated_ns INTEGER NOT NULL,
  PRIMARY KEY (snapshot_id, block_index, layer),
  FOREIGN KEY (snapshot_id) REFERENCES snapshots(snapshot_id) ON DELETE CASCADE
);
CREATE INDEX IF NOT EXISTS blocks_commit_order ON blocks(snapshot_id, seq);
`)
	return err
}

func SnapshotID(snap core.Snapshot, truthFingerprint string) string {
	material := snap.ImageID + "|" + strconv.FormatInt(snap.Seed, 10) + "|" +
		snap.ParamVersion + "|" + snap.AlgorithmVersion + "|" + truthFingerprint
	return core.BytesHash([]byte(material))[:32]
}

func (s *Store) EnsureSnapshot(snap core.Snapshot, truthFingerprint string, blocks []layers.Block) (SnapshotRecord, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	id := SnapshotID(snap, truthFingerprint)
	paramsJSON, err := json.Marshal(snap.Params)
	if err != nil {
		return SnapshotRecord{}, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return SnapshotRecord{}, err
	}
	defer tx.Rollback()
	var existing SnapshotRecord
	var regionJSON, paramsStored string
	err = tx.QueryRow(`SELECT snapshot_id,image_id,seed,param_version,algorithm_version,params_json,
truth_fingerprint,region_table_json,mask_hash,stats_hash,status,updated_ns FROM snapshots WHERE snapshot_id=?`, id).
		Scan(&existing.ID, &existing.ImageID, &existing.Seed, &existing.ParamVersion,
			&existing.AlgorithmVersion, &paramsStored, &existing.TruthFingerprint,
			&regionJSON, &existing.MaskHash, &existing.StatsHash, &existing.Status, &existing.UpdatedNS)
	if errors.Is(err, sql.ErrNoRows) {
		now := time.Now().UnixNano()
		_, err = tx.Exec(`INSERT INTO snapshots(snapshot_id,image_id,seed,param_version,algorithm_version,
params_json,truth_fingerprint,region_table_json,mask_hash,stats_hash,status,updated_ns)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`, id, snap.ImageID, snap.Seed, snap.ParamVersion,
			snap.AlgorithmVersion, string(paramsJSON), truthFingerprint, "[]", "", "", "building", now)
		if err != nil {
			return SnapshotRecord{}, err
		}
		existing = SnapshotRecord{
			ID: id, ImageID: snap.ImageID, Seed: snap.Seed, ParamVersion: snap.ParamVersion,
			AlgorithmVersion: snap.AlgorithmVersion, Params: snap.Params, TruthFingerprint: truthFingerprint,
			Status: "building", UpdatedNS: now,
		}
	} else if err != nil {
		return SnapshotRecord{}, err
	} else {
		if err := json.Unmarshal([]byte(paramsStored), &existing.Params); err != nil {
			return SnapshotRecord{}, err
		}
		if existing.ImageID != snap.ImageID || existing.Seed != snap.Seed ||
			existing.ParamVersion != snap.ParamVersion || existing.AlgorithmVersion != snap.AlgorithmVersion ||
			existing.TruthFingerprint != truthFingerprint {
			_, _ = tx.Exec(`UPDATE snapshots SET status='invalid',updated_ns=? WHERE snapshot_id=?`,
				time.Now().UnixNano(), id)
			if err := tx.Commit(); err != nil {
				return SnapshotRecord{}, err
			}
			return SnapshotRecord{}, core.VersionError(id, snap.ParamVersion, existing.ParamVersion)
		}
		if existing.Status == "invalid" {
			return existing, core.VersionError(id, snap.ParamVersion, "invalid")
		}
		if err := json.Unmarshal([]byte(regionJSON), &existing.RegionTable); err != nil {
			return SnapshotRecord{}, err
		}
	}
	now := time.Now().UnixNano()
	for _, block := range blocks {
		for _, layer := range layers.Order {
			_, err := tx.Exec(`INSERT OR IGNORE INTO blocks(snapshot_id,block_index,block_x,block_y,block_w,
block_h,layer,state,updated_ns,param_version,algorithm_version)
VALUES(?,?,?,?,?,?,?,?,?,?,?)`, id, block.Index, block.X, block.Y, block.W, block.H,
				string(layer), string(Pending), now, snap.ParamVersion, snap.AlgorithmVersion)
			if err != nil {
				return SnapshotRecord{}, err
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return SnapshotRecord{}, err
	}
	return s.GetSnapshot(id)
}

func (s *Store) GetSnapshot(id string) (SnapshotRecord, error) {
	var record SnapshotRecord
	var paramsJSON, regionJSON string
	err := s.db.QueryRow(`SELECT snapshot_id,image_id,seed,param_version,algorithm_version,params_json,
truth_fingerprint,region_table_json,mask_hash,stats_hash,status,updated_ns FROM snapshots WHERE snapshot_id=?`, id).
		Scan(&record.ID, &record.ImageID, &record.Seed, &record.ParamVersion,
			&record.AlgorithmVersion, &paramsJSON, &record.TruthFingerprint,
			&regionJSON, &record.MaskHash, &record.StatsHash, &record.Status, &record.UpdatedNS)
	if errors.Is(err, sql.ErrNoRows) {
		return record, core.VersionError(id, "existing snapshot", "missing")
	}
	if err != nil {
		return record, err
	}
	if err := json.Unmarshal([]byte(paramsJSON), &record.Params); err != nil {
		return record, err
	}
	if err := json.Unmarshal([]byte(regionJSON), &record.RegionTable); err != nil {
		return record, err
	}
	return record, nil
}

func (s *Store) AllSnapshots() ([]SnapshotRecord, error) {
	rows, err := s.db.Query(`SELECT snapshot_id FROM snapshots ORDER BY snapshot_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	records := make([]SnapshotRecord, 0, len(ids))
	for _, id := range ids {
		record, err := s.GetSnapshot(id)
		if err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	return records, nil
}

func (s *Store) Transition(snapshotID string, blockIndex int, layer string, to State) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var from State
	err = tx.QueryRow(`SELECT state FROM blocks WHERE snapshot_id=? AND block_index=? AND layer=?`,
		snapshotID, blockIndex, layer).Scan(&from)
	if err != nil {
		return err
	}
	if err := ValidateTransition(strconv.Itoa(blockIndex), layer, from, to); err != nil {
		return err
	}
	_, err = tx.Exec(`UPDATE blocks SET state=?,updated_ns=? WHERE snapshot_id=? AND block_index=? AND layer=?`,
		string(to), time.Now().UnixNano(), snapshotID, blockIndex, layer)
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) CommitLayer(snapshotID string, blockIndex int, layer string, summary string, content []byte) (Row, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return Row{}, err
	}
	defer tx.Rollback()
	var row Row
	err = tx.QueryRow(`SELECT state,param_version,algorithm_version FROM blocks
WHERE snapshot_id=? AND block_index=? AND layer=?`, snapshotID, blockIndex, layer).
		Scan(&row.State, &row.ParamVersion, &row.AlgorithmVersion)
	if err != nil {
		return Row{}, err
	}
	if row.State != Processing {
		return Row{}, core.StateError(strconv.Itoa(blockIndex), layer, string(row.State), string(Committed))
	}
	snapshot, err := getSnapshotTx(tx, snapshotID)
	if err != nil {
		return Row{}, err
	}
	if row.ParamVersion != snapshot.ParamVersion || row.AlgorithmVersion != snapshot.AlgorithmVersion {
		_ = tx.Rollback()
		_ = s.invalidate(snapshotID, "layer version mismatch")
		return Row{}, core.VersionError(snapshotID, snapshot.ParamVersion+"|"+snapshot.AlgorithmVersion,
			row.ParamVersion+"|"+row.AlgorithmVersion)
	}
	hash := core.BytesHash(content)
	var seq int64
	if err := tx.QueryRow(`SELECT COALESCE(MAX(seq),0)+1 FROM blocks WHERE snapshot_id=?`, snapshotID).Scan(&seq); err != nil {
		return Row{}, err
	}
	now := time.Now().UnixNano()
	_, err = tx.Exec(`UPDATE blocks SET state=?,summary=?,content_hash=?,content=?,seq=?,updated_ns=?
WHERE snapshot_id=? AND block_index=? AND layer=?`,
		string(Committed), summary, hash, content, seq, now, snapshotID, blockIndex, layer)
	if err != nil {
		return Row{}, err
	}
	if err := tx.Commit(); err != nil {
		return Row{}, err
	}
	return Row{
		SnapshotID: snapshotID, BlockIndex: blockIndex, Layer: layer, State: Committed,
		Summary: summary, ContentHash: hash, Content: content, ParamVersion: row.ParamVersion,
		AlgorithmVersion: row.AlgorithmVersion, Seq: seq, UpdatedNS: now,
	}, nil
}

func getSnapshotTx(tx *sql.Tx, id string) (SnapshotRecord, error) {
	var record SnapshotRecord
	var paramsJSON, regionJSON string
	err := tx.QueryRow(`SELECT snapshot_id,image_id,seed,param_version,algorithm_version,params_json,
truth_fingerprint,region_table_json,mask_hash,stats_hash,status,updated_ns FROM snapshots WHERE snapshot_id=?`, id).
		Scan(&record.ID, &record.ImageID, &record.Seed, &record.ParamVersion,
			&record.AlgorithmVersion, &paramsJSON, &record.TruthFingerprint,
			&regionJSON, &record.MaskHash, &record.StatsHash, &record.Status, &record.UpdatedNS)
	if err != nil {
		return record, err
	}
	_ = json.Unmarshal([]byte(paramsJSON), &record.Params)
	_ = json.Unmarshal([]byte(regionJSON), &record.RegionTable)
	return record, nil
}

func (s *Store) InvalidateSnapshot(id, reason string) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	return s.invalidate(id, reason)
}

func (s *Store) invalidate(id, reason string) error {
	res, err := s.db.Exec(`UPDATE snapshots SET status='invalid',updated_ns=? WHERE snapshot_id=?`,
		time.Now().UnixNano(), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("snapshot %s not found while invalidating: %s", id, reason)
	}
	return nil
}

func (s *Store) ResetSnapshot(id string) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM blocks WHERE snapshot_id=?`, id); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE snapshots SET status='building',region_table_json='[]',mask_hash='',
stats_hash='',updated_ns=? WHERE snapshot_id=?`, time.Now().UnixNano(), id); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) LoadRows(snapshotID string) ([]Row, error) {
	rows, err := s.db.Query(`SELECT snapshot_id,block_index,block_x,block_y,block_w,block_h,layer,state,
summary,content_hash,content,param_version,algorithm_version,seq,updated_ns FROM blocks
WHERE snapshot_id=? ORDER BY block_index,
CASE layer WHEN 'gradient' THEN 0 WHEN 'marker' THEN 1 WHEN 'flood_merge' THEN 2 WHEN 'stats' THEN 3 ELSE 4 END`,
		snapshotID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Row
	for rows.Next() {
		var row Row
		if err := rows.Scan(&row.SnapshotID, &row.BlockIndex, &row.BlockX, &row.BlockY, &row.BlockW,
			&row.BlockH, &row.Layer, &row.State, &row.Summary, &row.ContentHash, &row.Content,
			&row.ParamVersion, &row.AlgorithmVersion, &row.Seq, &row.UpdatedNS); err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

func (s *Store) CommitHistory(snapshotID string) ([]Row, error) {
	rows, err := s.db.Query(`SELECT snapshot_id,block_index,block_x,block_y,block_w,block_h,layer,state,
summary,content_hash,content,param_version,algorithm_version,seq,updated_ns FROM blocks
WHERE snapshot_id=? AND state='committed' ORDER BY seq`, snapshotID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Row
	for rows.Next() {
		var row Row
		if err := rows.Scan(&row.SnapshotID, &row.BlockIndex, &row.BlockX, &row.BlockY, &row.BlockW,
			&row.BlockH, &row.Layer, &row.State, &row.Summary, &row.ContentHash, &row.Content,
			&row.ParamVersion, &row.AlgorithmVersion, &row.Seq, &row.UpdatedNS); err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

func (s *Store) ValidateSnapshot(snapshotID string) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	snapshot, err := s.GetSnapshot(snapshotID)
	if err != nil {
		return err
	}
	if snapshot.Status == "invalid" {
		return core.VersionError(snapshotID, "valid snapshot", "invalid")
	}
	rows, err := s.LoadRows(snapshotID)
	if err != nil {
		return err
	}
	for _, row := range rows {
		if row.ParamVersion != snapshot.ParamVersion || row.AlgorithmVersion != snapshot.AlgorithmVersion {
			_ = s.invalidate(snapshotID, "persisted layer version mismatch")
			return core.VersionError(snapshotID,
				snapshot.ParamVersion+"|"+snapshot.AlgorithmVersion,
				row.ParamVersion+"|"+row.AlgorithmVersion)
		}
		if row.State == Processing {
			if err := s.transitionUnlocked(snapshotID, row.BlockIndex, row.Layer, Failed); err != nil {
				return err
			}
			continue
		}
		if row.State != Committed {
			continue
		}
		actual := core.BytesHash(row.Content)
		if row.ContentHash == "" || actual != row.ContentHash {
			_ = s.invalidate(snapshotID, "persisted layer content hash mismatch")
			return core.VersionError(snapshotID, row.ContentHash, actual)
		}
	}
	return nil
}

func (s *Store) transitionUnlocked(snapshotID string, blockIndex int, layer string, to State) error {
	var from State
	if err := s.db.QueryRow(`SELECT state FROM blocks WHERE snapshot_id=? AND block_index=? AND layer=?`,
		snapshotID, blockIndex, layer).Scan(&from); err != nil {
		return err
	}
	if err := ValidateTransition(strconv.Itoa(blockIndex), layer, from, to); err != nil {
		return err
	}
	res, err := s.db.Exec(`UPDATE blocks SET state=?,updated_ns=? WHERE snapshot_id=? AND block_index=? AND layer=?`,
		string(to), time.Now().UnixNano(), snapshotID, blockIndex, layer)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n != 1 {
		return fmt.Errorf("state transition affected %d rows", n)
	}
	return nil
}

func (s *Store) SetSnapshotResult(snapshotID string, regionTable []core.RegionRecord, maskHash, statsHash string) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	body, err := json.Marshal(regionTable)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(`UPDATE snapshots SET region_table_json=?,mask_hash=?,stats_hash=?,updated_ns=?
WHERE snapshot_id=?`, string(body), maskHash, statsHash, time.Now().UnixNano(), snapshotID)
	return err
}

func (s *Store) FinalizeSnapshot(snapshotID string) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	rows, err := s.LoadRows(snapshotID)
	if err != nil {
		return err
	}
	if len(rows) == 0 {
		return core.StateError("snapshot", "all-layers", "empty", string(Committed))
	}
	for _, row := range rows {
		if row.State != Committed || row.ContentHash == "" {
			return core.StateError(strconv.Itoa(row.BlockIndex), row.Layer, string(row.State), string(Committed))
		}
	}
	_, err = s.db.Exec(`UPDATE snapshots SET status='complete',updated_ns=? WHERE snapshot_id=?`,
		time.Now().UnixNano(), snapshotID)
	return err
}

func (s *Store) SnapshotProgress(snapshotID string) (committed, total int, err error) {
	rows, err := s.LoadRows(snapshotID)
	if err != nil {
		return 0, 0, err
	}
	for _, row := range rows {
		if row.State == Committed {
			committed++
		}
	}
	return committed, len(rows), nil
}

func ExpectedOrder(rows []Row) []string {
	order := make([]string, 0, len(rows))
	sorted := append([]Row(nil), rows...)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].BlockIndex != sorted[j].BlockIndex {
			return sorted[i].BlockIndex < sorted[j].BlockIndex
		}
		return layerRank(sorted[i].Layer) < layerRank(sorted[j].Layer)
	})
	for _, row := range sorted {
		order = append(order, strconv.Itoa(row.BlockIndex)+":"+row.Layer)
	}
	return order
}

func layerRank(layer string) int {
	for i, item := range layers.Order {
		if string(item) == layer {
			return i
		}
	}
	return 99
}
