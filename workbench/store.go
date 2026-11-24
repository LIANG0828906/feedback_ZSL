package workbench

import (
	"database/sql"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	_ "modernc.org/sqlite"

	"yuvench/errs"
	"yuvench/yuv"
)

type Store struct {
	db      *sql.DB
	path    string
	writeMu sync.Mutex
}

type LoadedSnapshot struct {
	Exists     bool
	Meta       SnapshotMeta
	Stages     map[string]*CacheEntry
	HardReason string
}

func OpenStore(path string) (*Store, error) {
	if path == "" {
		return nil, errs.New(errs.CodeIO, "snapshot path is empty")
	}
	if dir := filepath.Dir(path); dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, &errs.Error{Code: errs.CodeIO, Message: "create snapshot directory", Cause: err.Error()}
		}
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, &errs.Error{Code: errs.CodeIO, Message: "open sqlite snapshot", Cause: err.Error()}
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	for _, pragma := range []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA synchronous=FULL",
		"PRAGMA busy_timeout=5000",
		"PRAGMA foreign_keys=ON",
	} {
		if _, err := db.Exec(pragma); err != nil {
			db.Close()
			return nil, &errs.Error{Code: errs.CodeIO, Message: "configure sqlite snapshot", Cause: err.Error()}
		}
	}
	store := &Store{db: db, path: path}
	if err := store.init(); err != nil {
		db.Close()
		return nil, err
	}
	return store, nil
}

func (s *Store) init() error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	statements := []string{
		`CREATE TABLE IF NOT EXISTS run_meta (
			id INTEGER PRIMARY KEY CHECK (id = 1),
			run_id TEXT NOT NULL,
			schema_version TEXT NOT NULL,
			algorithm_version TEXT NOT NULL,
			param_version TEXT NOT NULL,
			chroma_version TEXT NOT NULL,
			input_hash TEXT NOT NULL,
			manifest_hash TEXT NOT NULL,
			complete INTEGER NOT NULL DEFAULT 0
		)`,
		`CREATE TABLE IF NOT EXISTS stages (
			run_id TEXT NOT NULL,
			seq INTEGER NOT NULL,
			node_id TEXT NOT NULL,
			plane TEXT NOT NULL,
			stage TEXT NOT NULL,
			kind TEXT NOT NULL,
			cache_key TEXT NOT NULL,
			input_hash TEXT NOT NULL,
			params_hash TEXT NOT NULL,
			param_version TEXT NOT NULL,
			chroma_version TEXT NOT NULL,
			upstream_json TEXT NOT NULL,
			params_json BLOB NOT NULL,
			summary TEXT NOT NULL,
			width INTEGER NOT NULL,
			height INTEGER NOT NULL,
			bit_depth INTEGER NOT NULL,
			min_value INTEGER NOT NULL,
			max_value INTEGER NOT NULL,
			meta_json TEXT NOT NULL,
			data BLOB NOT NULL,
			PRIMARY KEY (run_id, node_id)
		)`,
		`CREATE INDEX IF NOT EXISTS stages_run_seq ON stages(run_id, seq)`,
	}
	for _, statement := range statements {
		if _, err := s.db.Exec(statement); err != nil {
			return &errs.Error{Code: errs.CodeIO, Message: "initialize sqlite schema", Cause: err.Error()}
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

func (s *Store) Path() string {
	return s.path
}

func (s *Store) Load(expected SnapshotMeta) (*LoadedSnapshot, error) {
	loaded := &LoadedSnapshot{Stages: make(map[string]*CacheEntry)}
	row := s.db.QueryRow(`SELECT run_id, schema_version, algorithm_version, param_version, chroma_version, input_hash, manifest_hash, complete FROM run_meta WHERE id=1`)
	var meta SnapshotMeta
	var complete int
	err := row.Scan(&meta.RunID, &meta.SchemaVersion, &meta.AlgorithmVersion, &meta.ParamVersion, &meta.ChromaVersion, &meta.InputHash, &meta.ManifestHash, &complete)
	if err == sql.ErrNoRows {
		return loaded, nil
	}
	if err != nil {
		return nil, &errs.Error{Code: errs.CodeSnapshot, Message: "read snapshot metadata", Cause: err.Error()}
	}
	meta.Complete = complete != 0
	loaded.Exists = true
	loaded.Meta = meta
	if meta.SchemaVersion != expected.SchemaVersion {
		loaded.HardReason = "snapshot-version-mismatch:schemaVersion"
		return loaded, nil
	}
	if meta.AlgorithmVersion != expected.AlgorithmVersion {
		loaded.HardReason = "snapshot-version-mismatch:algorithmVersion"
		return loaded, nil
	}
	if meta.ParamVersion != expected.ParamVersion {
		loaded.HardReason = "snapshot-version-mismatch:paramVersion"
		return loaded, nil
	}
	if meta.ChromaVersion != expected.ChromaVersion {
		loaded.HardReason = "snapshot-version-mismatch:chromaVersion"
		return loaded, nil
	}
	if meta.InputHash != expected.InputHash {
		loaded.HardReason = "snapshot-version-mismatch:inputHash"
		return loaded, nil
	}
	if meta.ManifestHash != expected.ManifestHash {
		loaded.HardReason = "snapshot-version-mismatch:manifestHash"
		return loaded, nil
	}
	if meta.RunID != ComputeRunID(meta) {
		loaded.HardReason = "snapshot-version-mismatch:runId"
		return loaded, nil
	}
	rows, err := s.db.Query(`SELECT node_id, cache_key, input_hash, params_hash, param_version, chroma_version, upstream_json, summary,
		width, height, bit_depth, min_value, max_value, meta_json, kind, data, plane, stage, seq
		FROM stages WHERE run_id=? ORDER BY seq`, meta.RunID)
	if err != nil {
		return nil, &errs.Error{Code: errs.CodeSnapshot, Message: "read stage snapshots", Cause: err.Error()}
	}
	defer rows.Close()
	for rows.Next() {
		var entry CacheEntry
		var upstreamJSON, metaJSON, kind, data, plane, stage string
		var seq int
		var width, height, bitDepth, minValue, maxValue int
		if err := rows.Scan(&entry.NodeID, &entry.CacheKey, &entry.InputHash, &entry.ParamsHash, &entry.ParamVersion, &entry.ChromaVersion, &upstreamJSON, &entry.Summary,
			&width, &height, &bitDepth, &minValue, &maxValue, &metaJSON, &kind, &data, &plane, &stage, &seq); err != nil {
			return nil, &errs.Error{Code: errs.CodeSnapshot, Message: "scan stage snapshot", Cause: err.Error()}
		}
		if err := json.Unmarshal([]byte(upstreamJSON), &entry.UpstreamKeys); err != nil {
			loaded.HardReason = "snapshot-content-mismatch:" + entry.NodeID
			loaded.Stages = make(map[string]*CacheEntry)
			return loaded, nil
		}
		if entry.ParamVersion != meta.ParamVersion {
			loaded.HardReason = "snapshot-content-mismatch:" + entry.NodeID
			loaded.Stages = make(map[string]*CacheEntry)
			return loaded, nil
		}
		output, err := decodeOutput(kind, plane, width, height, bitDepth, minValue, maxValue, metaJSON, []byte(data))
		if err != nil {
			loaded.HardReason = "snapshot-content-mismatch:" + entry.NodeID
			loaded.Stages = make(map[string]*CacheEntry)
			return loaded, nil
		}
		if output.Summary != entry.Summary {
			loaded.HardReason = "snapshot-content-mismatch:" + entry.NodeID
			loaded.Stages = make(map[string]*CacheEntry)
			return loaded, nil
		}
		entry.Output = output
		loaded.Stages[entry.NodeID] = &entry
	}
	if err := rows.Err(); err != nil {
		return nil, &errs.Error{Code: errs.CodeSnapshot, Message: "iterate stage snapshots", Cause: err.Error()}
	}
	return loaded, nil
}

func (s *Store) BeginSnapshot(meta SnapshotMeta) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return &errs.Error{Code: errs.CodeSnapshot, Message: "begin snapshot transaction", Cause: err.Error()}
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM stages`); err != nil {
		return &errs.Error{Code: errs.CodeSnapshot, Message: "clear old stage snapshot", Cause: err.Error()}
	}
	if _, err := tx.Exec(`INSERT INTO run_meta(id, run_id, schema_version, algorithm_version, param_version, chroma_version, input_hash, manifest_hash, complete)
		VALUES(1,?,?,?,?,?,?,?,0)
		ON CONFLICT(id) DO UPDATE SET run_id=excluded.run_id, schema_version=excluded.schema_version,
		algorithm_version=excluded.algorithm_version, param_version=excluded.param_version,
		chroma_version=excluded.chroma_version, input_hash=excluded.input_hash,
		manifest_hash=excluded.manifest_hash, complete=0`,
		meta.RunID, meta.SchemaVersion, meta.AlgorithmVersion, meta.ParamVersion, meta.ChromaVersion, meta.InputHash, meta.ManifestHash); err != nil {
		return &errs.Error{Code: errs.CodeSnapshot, Message: "write snapshot metadata", Cause: err.Error()}
	}
	if err := tx.Commit(); err != nil {
		return &errs.Error{Code: errs.CodeSnapshot, Message: "commit snapshot metadata", Cause: err.Error()}
	}
	return nil
}

func (s *Store) ResetStages(meta SnapshotMeta) error {
	return s.BeginSnapshot(meta)
}

func (s *Store) SetComplete(meta SnapshotMeta, complete bool) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	value := 0
	if complete {
		value = 1
	}
	result, err := s.db.Exec(`UPDATE run_meta SET complete=? WHERE id=1 AND run_id=?`, value, meta.RunID)
	if err != nil {
		return &errs.Error{Code: errs.CodeSnapshot, Message: "update snapshot completion", Cause: err.Error()}
	}
	affected, _ := result.RowsAffected()
	if affected != 1 {
		return &errs.Error{Code: errs.CodeSnapshot, Message: "snapshot completion lost its metadata row", Node: meta.RunID}
	}
	return nil
}

func (s *Store) CommitStage(meta SnapshotMeta, node *Node, entry *CacheEntry, output *NodeOutput) error {
	if node == nil || entry == nil || output == nil {
		return errs.New(errs.CodeSnapshot, "missing stage snapshot data")
	}
	kind, width, height, bitDepth, minValue, maxValue, metaJSON, payload, err := encodeOutput(node, output)
	if err != nil {
		return err
	}
	upstreamJSON, _ := json.Marshal(entry.UpstreamKeys)
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return &errs.Error{Code: errs.CodeSnapshot, Message: "begin stage commit", Node: node.ID, Cause: err.Error()}
	}
	defer tx.Rollback()
	var runID string
	if err := tx.QueryRow(`SELECT run_id FROM run_meta WHERE id=1`).Scan(&runID); err != nil {
		return &errs.Error{Code: errs.CodeSnapshot, Message: "read active snapshot", Node: node.ID, Cause: err.Error()}
	}
	if runID != meta.RunID {
		return &errs.Error{Code: errs.CodeSnapshot, Message: "stage commit does not match active snapshot", Node: node.ID, Expected: meta.RunID, Actual: runID}
	}
	_, err = tx.Exec(`INSERT INTO stages(run_id, seq, node_id, plane, stage, kind, cache_key, input_hash, params_hash, param_version, chroma_version,
		upstream_json, params_json, summary, width, height, bit_depth, min_value, max_value, meta_json, data)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(run_id, node_id) DO UPDATE SET seq=excluded.seq, plane=excluded.plane, stage=excluded.stage,
		kind=excluded.kind, cache_key=excluded.cache_key, input_hash=excluded.input_hash, params_hash=excluded.params_hash,
		param_version=excluded.param_version, chroma_version=excluded.chroma_version, upstream_json=excluded.upstream_json, params_json=excluded.params_json,
		summary=excluded.summary, width=excluded.width, height=excluded.height, bit_depth=excluded.bit_depth,
		min_value=excluded.min_value, max_value=excluded.max_value, meta_json=excluded.meta_json, data=excluded.data`,
		meta.RunID, node.Seq, node.ID, node.Plane, node.Stage, kind, entry.CacheKey, entry.InputHash, entry.ParamsHash,
		entry.ParamVersion, entry.ChromaVersion, string(upstreamJSON), node.ParamsJSON, output.Summary, width, height, bitDepth, minValue, maxValue, metaJSON, payload)
	if err != nil {
		return &errs.Error{Code: errs.CodeSnapshot, Message: "write stage snapshot", Node: node.ID, Cause: err.Error()}
	}
	if err := tx.Commit(); err != nil {
		return &errs.Error{Code: errs.CodeSnapshot, Message: "commit stage snapshot", Node: node.ID, Cause: err.Error()}
	}
	return nil
}

type compositeMeta struct {
	QuantRange string `json:"quantRange"`
	YMin       uint16 `json:"yMin"`
	YMax       uint16 `json:"yMax"`
	UMin       uint16 `json:"uMin"`
	UMax       uint16 `json:"uMax"`
	VMin       uint16 `json:"vMin"`
	VMax       uint16 `json:"vMax"`
}

func encodeOutput(node *Node, output *NodeOutput) (kind string, width, height, bitDepth, minValue, maxValue int, metaJSON string, payload []byte, err error) {
	if output.Plane != nil {
		plane := output.Plane
		kind = "plane"
		width, height, bitDepth = plane.Width, plane.Height, plane.BitDepth
		minValue, maxValue = int(plane.MinCode), int(plane.MaxCode)
		metaData, _ := json.Marshal(map[string]any{"name": plane.Name})
		return kind, width, height, bitDepth, minValue, maxValue, string(metaData), plane.EncodeRaw(), nil
	}
	if output.Composite != nil {
		composite := output.Composite
		kind = "interleave"
		width, height, bitDepth = composite.Width, composite.Height, composite.BitDepth
		minValue, maxValue = int(composite.YMin), int(composite.YMax)
		metaData, _ := json.Marshal(compositeMeta{
			QuantRange: composite.QuantRange,
			YMin:       composite.YMin,
			YMax:       composite.YMax,
			UMin:       composite.UMin,
			UMax:       composite.UMax,
			VMin:       composite.VMin,
			VMax:       composite.VMax,
		})
		return kind, width, height, bitDepth, minValue, maxValue, string(metaData), composite.EncodeRaw16LE(), nil
	}
	return "", 0, 0, 0, 0, 0, "", nil, errs.New(errs.CodeSnapshot, "stage has no output")
}

func decodeOutput(kind, planeName string, width, height, bitDepth, minValue, maxValue int, metaJSON string, payload []byte) (*NodeOutput, error) {
	if kind == "plane" {
		expected := width * height
		if bitDepth > 8 {
			expected *= 2
		}
		if len(payload) != expected {
			return nil, fmt.Errorf("plane payload length %d, expected %d", len(payload), expected)
		}
		data := make([]uint16, width*height)
		for i := range data {
			if bitDepth <= 8 {
				data[i] = uint16(payload[i])
			} else {
				data[i] = binary.LittleEndian.Uint16(payload[i*2:])
			}
			if data[i] > uint16(maxValue) {
				return nil, fmt.Errorf("plane value %d exceeds stage max %d", data[i], maxValue)
			}
		}
		plane := &yuv.Plane{
			Name:     planeName,
			Width:    width,
			Height:   height,
			Channels: 1,
			BitDepth: bitDepth,
			MinCode:  uint16(minValue),
			MaxCode:  uint16(maxValue),
			Data:     data,
		}
		return &NodeOutput{Plane: plane, Summary: plane.Summary()}, nil
	}
	if kind != "interleave" {
		return nil, fmt.Errorf("unknown stage kind %q", kind)
	}
	var meta compositeMeta
	if err := json.Unmarshal([]byte(metaJSON), &meta); err != nil {
		return nil, err
	}
	if width <= 0 || height <= 0 || bitDepth <= 0 || len(payload) != width*height*3*2 {
		return nil, fmt.Errorf("invalid composite payload")
	}
	composite := &yuv.Composite{
		Width:      width,
		Height:     height,
		BitDepth:   bitDepth,
		QuantRange: meta.QuantRange,
		YMin:       meta.YMin,
		YMax:       meta.YMax,
		UMin:       meta.UMin,
		UMax:       meta.UMax,
		VMin:       meta.VMin,
		VMax:       meta.VMax,
		Y:          make([]uint16, width*height),
		U:          make([]uint16, width*height),
		V:          make([]uint16, width*height),
	}
	offset := 0
	for _, target := range [][]uint16{composite.Y, composite.U, composite.V} {
		for i := range target {
			target[i] = binary.LittleEndian.Uint16(payload[offset:])
			offset += 2
		}
	}
	return &NodeOutput{Composite: composite, Summary: composite.Summary()}, nil
}
