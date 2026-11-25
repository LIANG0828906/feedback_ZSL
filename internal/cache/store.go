package cache

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	_ "modernc.org/sqlite"

	"uvenhance/internal/apperr"
	"uvenhance/internal/yuv"
)

type SnapshotMeta struct {
	ParamVersion     int
	AlignmentVersion int
	Alignment        string
	Upsample         string
	InputHashes      map[string]string
	Complete         bool
}

type Snapshot struct {
	ID               int64
	UID              string
	ParamVersion     int
	AlignmentVersion int
	Alignment        string
	Upsample         string
	InputHashes      map[string]string
	Complete         bool
	ManifestDigest   string
	Stages           map[string]*Output
}

type LoadResult struct {
	Snapshot       *Snapshot
	RejectedReason string
}

type Store struct {
	db      *sql.DB
	writeMu sync.Mutex
}

func Open(path string) (*Store, error) {
	if path != ":memory:" {
		dir := filepath.Dir(path)
		if dir != "." {
			if err := os.MkdirAll(dir, 0o755); err != nil {
				return nil, apperr.IO("create sqlite cache directory", err)
			}
		}
	}
	dsn := "file:" + filepath.ToSlash(path) + "?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, apperr.IO("open sqlite cache", err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`PRAGMA foreign_keys=ON`); err != nil {
		_ = db.Close()
		return nil, apperr.IO("enable sqlite foreign keys", err)
	}
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate() error {
	const schema = `
CREATE TABLE IF NOT EXISTS snapshots (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	uid TEXT NOT NULL UNIQUE,
	param_version INTEGER NOT NULL,
	alignment_version INTEGER NOT NULL,
	alignment TEXT NOT NULL,
	upsample TEXT NOT NULL,
	input_y_hash TEXT NOT NULL,
	input_u_hash TEXT NOT NULL,
	input_v_hash TEXT NOT NULL,
	complete INTEGER NOT NULL,
	manifest_digest TEXT NOT NULL,
	stage_count INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS stages (
	snapshot_id INTEGER NOT NULL REFERENCES snapshots(id) ON DELETE CASCADE,
	stage_id TEXT NOT NULL,
	plane TEXT NOT NULL,
	kind TEXT NOT NULL,
	cache_key TEXT NOT NULL,
	input_hash TEXT NOT NULL,
	upstream_key TEXT NOT NULL,
	param_digest TEXT NOT NULL,
	output_digest TEXT NOT NULL,
	width INTEGER NOT NULL,
	height INTEGER NOT NULL,
	bit_depth INTEGER NOT NULL,
	quant_min INTEGER NOT NULL,
	quant_max INTEGER NOT NULL,
	alignment TEXT NOT NULL,
	commit_seq INTEGER NOT NULL,
	payload BLOB NOT NULL,
	PRIMARY KEY(snapshot_id, stage_id)
);
`
	if _, err := s.db.Exec(schema); err != nil {
		return apperr.IO("migrate sqlite cache", err)
	}
	return nil
}

func (s *Store) SaveSnapshot(ctx context.Context, meta SnapshotMeta, stages map[string]*Output) (int64, string, error) {
	if err := validateSnapshotInputs(meta, stages); err != nil {
		return 0, "", err
	}
	ordered := orderedOutputs(stages)
	manifest, err := manifestDigest(meta, ordered)
	if err != nil {
		return 0, "", err
	}

	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, "", apperr.IO("begin snapshot transaction", err)
	}
	defer func() { _ = tx.Rollback() }()

	res, err := tx.ExecContext(ctx, `
INSERT INTO snapshots
(uid,param_version,alignment_version,alignment,upsample,input_y_hash,input_u_hash,input_v_hash,complete,manifest_digest,stage_count)
VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		"pending", meta.ParamVersion, meta.AlignmentVersion, meta.Alignment, meta.Upsample,
		meta.InputHashes["Y"], meta.InputHashes["U"], meta.InputHashes["V"],
		boolInt(meta.Complete), manifest, len(ordered))
	if err != nil {
		return 0, "", apperr.IO("insert snapshot", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, "", apperr.IO("read snapshot id", err)
	}
	uid := fmt.Sprintf("snap-%020d", id)
	if _, err := tx.ExecContext(ctx, `UPDATE snapshots SET uid=? WHERE id=?`, uid, id); err != nil {
		return 0, "", apperr.IO("set snapshot uid", err)
	}
	for i, out := range ordered {
		payload, err := encodePayload(out)
		if err != nil {
			return 0, "", err
		}
		if _, err := tx.ExecContext(ctx, `
INSERT INTO stages
(snapshot_id,stage_id,plane,kind,cache_key,input_hash,upstream_key,param_digest,output_digest,
 width,height,bit_depth,quant_min,quant_max,alignment,commit_seq,payload)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			id, out.StageID, out.Plane, out.Kind, out.Key, out.InputHash, out.UpstreamKey,
			out.ParamDigest, out.OutputHash, out.Width, out.Height, out.BitDepth,
			out.QuantMin, out.QuantMax, out.Alignment, i, payload); err != nil {
			return 0, "", apperr.IO("insert stage snapshot", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM snapshots WHERE id<>?`, id); err != nil {
		return 0, "", apperr.IO("prune older snapshots", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, "", apperr.IO("commit snapshot transaction", err)
	}
	return id, uid, nil
}

func (s *Store) LoadLatest(ctx context.Context) (LoadResult, error) {
	var snap Snapshot
	var id int64
	var complete int
	var stageCount int
	var yHash, uHash, vHash string
	err := s.db.QueryRowContext(ctx, `
SELECT id,uid,param_version,alignment_version,alignment,upsample,input_y_hash,input_u_hash,input_v_hash,
       complete,manifest_digest,stage_count
FROM snapshots ORDER BY id DESC LIMIT 1`).Scan(
		&id, &snap.UID, &snap.ParamVersion, &snap.AlignmentVersion, &snap.Alignment, &snap.Upsample,
		&yHash, &uHash, &vHash, &complete, &snap.ManifestDigest, &stageCount,
	)
	if err == sql.ErrNoRows {
		return LoadResult{}, nil
	}
	if err != nil {
		return LoadResult{}, apperr.IO("load latest snapshot", err)
	}
	snap.ID = id
	snap.Complete = complete != 0
	snap.InputHashes = map[string]string{"Y": yHash, "U": uHash, "V": vHash}
	snap.Stages = make(map[string]*Output)

	rows, err := s.db.QueryContext(ctx, `
SELECT stage_id,plane,kind,cache_key,input_hash,upstream_key,param_digest,output_digest,
       width,height,bit_depth,quant_min,quant_max,alignment,commit_seq,payload
FROM stages WHERE snapshot_id=? ORDER BY commit_seq`, id)
	if err != nil {
		return LoadResult{}, apperr.IO("load snapshot stages", err)
	}
	rejectReason := ""
	for rows.Next() {
		out := &Output{}
		var payload []byte
		if err := rows.Scan(&out.StageID, &out.Plane, &out.Kind, &out.Key, &out.InputHash,
			&out.UpstreamKey, &out.ParamDigest, &out.OutputHash, &out.Width, &out.Height,
			&out.BitDepth, &out.QuantMin, &out.QuantMax, &out.Alignment, &out.CommittedSeq, &payload); err != nil {
			_ = rows.Close()
			return LoadResult{}, apperr.IO("scan snapshot stage", err)
		}
		if err := decodePayload(out, payload); err != nil {
			rejectReason = fmt.Sprintf("stage %s payload invalid: %v", out.StageID, err)
			break
		}
		snap.Stages[out.StageID] = out
	}
	if err := rows.Err(); err != nil {
		_ = rows.Close()
		return LoadResult{}, apperr.IO("iterate snapshot stages", err)
	}
	if err := rows.Close(); err != nil {
		return LoadResult{}, apperr.IO("close snapshot stage rows", err)
	}
	if rejectReason != "" {
		return s.rejectAndReport(ctx, id, rejectReason)
	}
	if len(snap.Stages) != stageCount {
		return s.rejectAndReport(ctx, id, fmt.Sprintf("snapshot stage count mismatch: declared=%d loaded=%d", stageCount, len(snap.Stages)))
	}
	if snap.Complete && len(snap.Stages) != len(Stages) {
		return s.rejectAndReport(ctx, id, "complete snapshot is missing stages")
	}
	recomputedManifest, err := manifestDigestFromSnapshot(&snap)
	if err != nil {
		return LoadResult{}, apperr.IO("recompute snapshot manifest", err)
	}
	if recomputedManifest != snap.ManifestDigest {
		return s.rejectAndReport(ctx, id, "snapshot manifest digest mismatch")
	}
	return LoadResult{Snapshot: &snap}, nil
}

func (s *Store) rejectAndReport(ctx context.Context, id int64, reason string) (LoadResult, error) {
	if err := s.rejectLocked(ctx, id); err != nil {
		return LoadResult{}, err
	}
	return LoadResult{RejectedReason: reason}, nil
}

func (s *Store) rejectLocked(ctx context.Context, id int64) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if _, err := s.db.ExecContext(ctx, `DELETE FROM snapshots WHERE id=?`, id); err != nil {
		return apperr.IO("reject invalid snapshot", err)
	}
	return nil
}

func (s *Store) Reject(ctx context.Context, id int64) error {
	return s.rejectLocked(ctx, id)
}

func (s *Snapshot) RejectReason(paramVersion, alignmentVersion int, alignment, upsample string, inputHashes map[string]string) string {
	if paramVersion != s.ParamVersion {
		return fmt.Sprintf("parameter version mismatch: snapshot=%d current=%d", s.ParamVersion, paramVersion)
	}
	if alignmentVersion != s.AlignmentVersion {
		return fmt.Sprintf("alignment version mismatch: snapshot=%d current=%d", s.AlignmentVersion, alignmentVersion)
	}
	if alignment != s.Alignment {
		return fmt.Sprintf("alignment mismatch: snapshot=%s current=%s", s.Alignment, alignment)
	}
	if upsample != s.Upsample {
		return fmt.Sprintf("upsample mismatch: snapshot=%s current=%s", s.Upsample, upsample)
	}
	for _, name := range []string{"Y", "U", "V"} {
		if inputHashes[name] != s.InputHashes[name] {
			return fmt.Sprintf("%s input content hash mismatch: snapshot=%s current=%s", name, s.InputHashes[name], inputHashes[name])
		}
	}
	return ""
}

func validateSnapshotInputs(meta SnapshotMeta, stages map[string]*Output) error {
	if meta.ParamVersion <= 0 {
		return apperr.New(apperr.CodeCache, "parameter version must be positive")
	}
	if meta.AlignmentVersion <= 0 {
		return apperr.New(apperr.CodeCache, "alignment version must be positive")
	}
	if meta.InputHashes["Y"] == "" || meta.InputHashes["U"] == "" || meta.InputHashes["V"] == "" {
		return apperr.New(apperr.CodeCache, "all three input hashes are required")
	}
	for id, out := range stages {
		if id != out.StageID {
			return apperr.New(apperr.CodeCache, "stage key/id mismatch: "+id)
		}
		spec, ok := StageByID(id)
		if !ok {
			return apperr.New(apperr.CodeCache, "unknown stage in snapshot: "+id)
		}
		if out.Plane != spec.Plane || out.Kind != spec.Kind {
			return apperr.New(apperr.CodeCache, "stage metadata mismatch: "+id)
		}
		if err := out.ValidatePayload(); err != nil {
			return apperr.New(apperr.CodeCache, err.Error())
		}
	}
	return nil
}

func orderedOutputs(stages map[string]*Output) []*Output {
	out := make([]*Output, 0, len(stages))
	for _, stage := range Stages {
		if v, ok := stages[stage.ID]; ok {
			out = append(out, v)
		}
	}
	return out
}

func manifestDigest(meta SnapshotMeta, outputs []*Output) (string, error) {
	type row struct {
		StageID      string `json:"stage_id"`
		Plane        string `json:"plane"`
		Kind         string `json:"kind"`
		Key          string `json:"cache_key"`
		InputHash    string `json:"input_hash"`
		UpstreamKey  string `json:"upstream_key"`
		ParamDigest  string `json:"param_digest"`
		OutputDigest string `json:"output_digest"`
		Width        int    `json:"width"`
		Height       int    `json:"height"`
		BitDepth     int    `json:"bit_depth"`
		QuantMin     uint16 `json:"quant_min"`
		QuantMax     uint16 `json:"quant_max"`
		Alignment    string `json:"alignment"`
	}
	rows := make([]row, 0, len(outputs))
	for _, o := range outputs {
		rows = append(rows, row{
			StageID: o.StageID, Plane: o.Plane, Kind: o.Kind, Key: o.Key,
			InputHash: o.InputHash, UpstreamKey: o.UpstreamKey, ParamDigest: o.ParamDigest,
			OutputDigest: o.OutputHash, Width: o.Width, Height: o.Height,
			BitDepth: o.BitDepth, QuantMin: o.QuantMin, QuantMax: o.QuantMax,
			Alignment: o.Alignment,
		})
	}
	envelope := struct {
		Version          string            `json:"version"`
		PipelineVersion  int               `json:"pipeline_version"`
		ParamVersion     int               `json:"parameter_version"`
		AlignmentVersion int               `json:"alignment_version"`
		Alignment        string            `json:"alignment"`
		Upsample         string            `json:"upsample"`
		InputHashes      map[string]string `json:"input_hashes"`
		Complete         bool              `json:"complete"`
		Stages           []row             `json:"stages"`
	}{
		Version: "uv-snapshot-v2", PipelineVersion: PipelineVersion, ParamVersion: meta.ParamVersion,
		AlignmentVersion: meta.AlignmentVersion, Alignment: meta.Alignment,
		Upsample: meta.Upsample, InputHashes: meta.InputHashes,
		Complete: meta.Complete, Stages: rows,
	}
	raw, err := json.Marshal(envelope)
	if err != nil {
		return "", apperr.IO("encode snapshot manifest", err)
	}
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

func manifestDigestFromSnapshot(s *Snapshot) (string, error) {
	meta := SnapshotMeta{
		ParamVersion: s.ParamVersion, AlignmentVersion: s.AlignmentVersion,
		Alignment: s.Alignment, Upsample: s.Upsample, InputHashes: s.InputHashes,
		Complete: s.Complete,
	}
	return manifestDigest(meta, orderedOutputs(s.Stages))
}

func encodePayload(o *Output) ([]byte, error) {
	switch o.Kind {
	case KindPlane:
		if o.PlaneData == nil {
			return nil, apperr.New(apperr.CodeCache, "missing plane payload for "+o.StageID)
		}
		raw := make([]byte, len(o.PlaneData.Data)*2)
		for i, v := range o.PlaneData.Data {
			binary.LittleEndian.PutUint16(raw[i*2:i*2+2], v)
		}
		return raw, nil
	case KindImage:
		if o.ImageData == nil {
			return nil, apperr.New(apperr.CodeCache, "missing image payload for "+o.StageID)
		}
		raw := make([]byte, len(o.ImageData.Pixels)*6)
		for i, p := range o.ImageData.Pixels {
			binary.LittleEndian.PutUint16(raw[i*6:i*6+2], p.Y)
			binary.LittleEndian.PutUint16(raw[i*6+2:i*6+4], p.U)
			binary.LittleEndian.PutUint16(raw[i*6+4:i*6+6], p.V)
		}
		return raw, nil
	default:
		return nil, apperr.New(apperr.CodeCache, "unknown stage kind "+o.Kind)
	}
}

func decodePayload(o *Output, raw []byte) error {
	spec, ok := StageByID(o.StageID)
	if !ok {
		return fmt.Errorf("unknown stage %q", o.StageID)
	}
	if o.Plane != spec.Plane || o.Kind != spec.Kind {
		return fmt.Errorf("stage metadata does not match graph")
	}
	switch o.Kind {
	case KindPlane:
		if len(raw) != o.Width*o.Height*2 {
			return fmt.Errorf("plane payload length=%d expected=%d", len(raw), o.Width*o.Height*2)
		}
		data := make([]uint16, o.Width*o.Height)
		for i := range data {
			data[i] = binary.LittleEndian.Uint16(raw[i*2 : i*2+2])
		}
		o.PlaneData = &yuv.Plane{
			Name: o.Plane, Width: o.Width, Height: o.Height, BitDepth: o.BitDepth,
			QuantMin: o.QuantMin, QuantMax: o.QuantMax, Data: data,
		}
	case KindImage:
		if len(raw) != o.Width*o.Height*6 {
			return fmt.Errorf("image payload length=%d expected=%d", len(raw), o.Width*o.Height*6)
		}
		pixels := make([]yuv.Pixel, o.Width*o.Height)
		for i := range pixels {
			pixels[i] = yuv.Pixel{
				Y: binary.LittleEndian.Uint16(raw[i*6 : i*6+2]),
				U: binary.LittleEndian.Uint16(raw[i*6+2 : i*6+4]),
				V: binary.LittleEndian.Uint16(raw[i*6+4 : i*6+6]),
			}
		}
		o.ImageData = &yuv.Image{
			Width: o.Width, Height: o.Height, BitDepth: o.BitDepth,
			QuantMin: o.QuantMin, QuantMax: o.QuantMax, Pixels: pixels,
		}
	default:
		return fmt.Errorf("unknown kind %q", o.Kind)
	}
	return o.ValidatePayload()
}

func boolInt(v bool) int {
	if v {
		return 1
	}
	return 0
}

func (s *Store) SnapshotCount(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM snapshots`).Scan(&n)
	return n, err
}

func (s *Store) DumpStageKeys(ctx context.Context) (string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT stage_id,cache_key FROM stages ORDER BY stage_id`)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var parts []string
	for rows.Next() {
		var id, key string
		if err := rows.Scan(&id, &key); err != nil {
			return "", err
		}
		parts = append(parts, id+"="+key)
	}
	sort.Strings(parts)
	return strings.Join(parts, "\n"), rows.Err()
}
