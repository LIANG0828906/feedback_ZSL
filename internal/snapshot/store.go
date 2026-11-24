// Package snapshot persists segment checkpoints in SQLite. All writes share one
// serialized writer, and a segment's frames and metadata commit atomically.
package snapshot

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"

	_ "modernc.org/sqlite"

	"hdrproof/internal/apperr"
	"hdrproof/internal/model"
)

const schemaSQL = `
CREATE TABLE IF NOT EXISTS snapshot_meta (
	key TEXT PRIMARY KEY,
	value TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS routes (
	route_id TEXT PRIMARY KEY,
	route_order INTEGER NOT NULL,
	route_json BLOB NOT NULL,
	truth_fingerprint TEXT NOT NULL,
	param_version TEXT NOT NULL,
	algorithm_version TEXT NOT NULL,
	frame_count INTEGER NOT NULL,
	segment_count INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS segment_plans (
	route_id TEXT NOT NULL,
	segment_index INTEGER NOT NULL,
	frame_start INTEGER NOT NULL,
	frame_end INTEGER NOT NULL,
	commit_order INTEGER NOT NULL,
	PRIMARY KEY (route_id, segment_index),
	UNIQUE (commit_order)
);
CREATE TABLE IF NOT EXISTS segments (
	route_id TEXT NOT NULL,
	segment_index INTEGER NOT NULL,
	commit_order INTEGER NOT NULL,
	source_hash TEXT NOT NULL,
	output_hash TEXT NOT NULL,
	proof_hash TEXT NOT NULL,
	content_hash TEXT NOT NULL,
	param_version TEXT NOT NULL,
	algorithm_version TEXT NOT NULL,
	truth_fingerprint TEXT NOT NULL,
	frame_count INTEGER NOT NULL,
	proof_summary BLOB NOT NULL,
	PRIMARY KEY (route_id, segment_index),
	UNIQUE (commit_order)
);
CREATE TABLE IF NOT EXISTS frames (
	route_id TEXT NOT NULL,
	frame_index INTEGER NOT NULL,
	segment_index INTEGER NOT NULL,
	timestamp_ms INTEGER NOT NULL,
	width INTEGER NOT NULL,
	height INTEGER NOT NULL,
	channels INTEGER NOT NULL,
	pixels BLOB NOT NULL,
	pixel_hash TEXT NOT NULL,
	proof_json BLOB NOT NULL,
	record_hash TEXT NOT NULL,
	param_version TEXT NOT NULL,
	algorithm_version TEXT NOT NULL,
	truth_fingerprint TEXT NOT NULL,
	PRIMARY KEY (route_id, frame_index)
);
CREATE INDEX IF NOT EXISTS frames_segment_idx ON frames(route_id, segment_index, frame_index);
`

type Meta struct {
	SchemaVersion    string `json:"schema_version"`
	AlgorithmVersion string `json:"algorithm_version"`
	ParamVersion     string `json:"param_version"`
	TruthFingerprint string `json:"truth_fingerprint"`
}

type SegmentPlan struct {
	RouteID     string
	Segment     int
	FrameStart  int
	FrameEnd    int
	CommitOrder int64
}

type RoutePlan struct {
	Route    model.Route
	Order    int
	Segments []SegmentPlan
}

type SegmentKey struct {
	RouteID string
	Segment int
}

type FrameKey struct {
	RouteID string
	Frame   int
}

type StoredFrame struct {
	RouteID          string
	FrameIndex       int
	Segment          int
	TimestampMS      int64
	Width            int
	Height           int
	Channels         int
	Pixels           []byte
	PixelHash        string
	ProofJSON        []byte
	RecordHash       string
	ParamVersion     string
	AlgorithmVersion string
	TruthFingerprint string
}

type StoredSegment struct {
	RouteID          string
	Segment          int
	CommitOrder      int64
	SourceHash       string
	OutputHash       string
	ProofHash        string
	ContentHash      string
	ParamVersion     string
	AlgorithmVersion string
	TruthFingerprint string
	FrameCount       int
	ProofSummary     []byte
}

type Loaded struct {
	Segments map[SegmentKey]StoredSegment
	Frames   map[FrameKey]StoredFrame
	Reused   bool
	Reason   string
}

type Store struct {
	path string
	db   *sql.DB
	mu   sync.Mutex
	meta Meta
}

type Commit struct {
	Plan             SegmentPlan
	Frames           []model.FrameResult
	ParamVersion     string
	AlgorithmVersion string
	TruthFingerprint string
}

func BuildPlans(routes []model.Route) []RoutePlan {
	plans := make([]RoutePlan, 0, len(routes))
	for order, route := range routes {
		rp := RoutePlan{Route: route, Order: order}
		count := model.SegmentCount(route)
		for seg := 0; seg < count; seg++ {
			indices := model.FrameIndicesForSegment(route, seg)
			if len(indices) == 0 {
				continue
			}
			rp.Segments = append(rp.Segments, SegmentPlan{
				RouteID: route.ID, Segment: seg,
				FrameStart: indices[0], FrameEnd: indices[len(indices)-1],
				CommitOrder: int64(order)*1_000_000 + int64(seg),
			})
		}
		plans = append(plans, rp)
	}
	return plans
}

func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
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
			return nil, err
		}
	}
	if _, err := db.Exec(schemaSQL); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{path: path, db: db}, nil
}

func OpenReadOnly(path string) (*Store, error) {
	dsn := "file:" + filepath.ToSlash(path) + "?mode=ro"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec("PRAGMA query_only=ON"); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{path: path, db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }
func (s *Store) Path() string { return s.path }

// Initialize validates an existing snapshot as a whole. Version, route, plan,
// frame, pixel and record mismatches invalidate the complete snapshot.
func (s *Store) Initialize(ctx context.Context, meta Meta, plans []RoutePlan, forceFull bool) (*Loaded, error) {
	return s.InitializeWithVersions(ctx, meta, plans, forceFull, nil)
}

// InitializeWithVersions verifies every segment's parameter version against an
// expected plan. A mismatch invalidates the whole snapshot.
func (s *Store) InitializeWithVersions(ctx context.Context, meta Meta, plans []RoutePlan, forceFull bool, expectedSegmentParams map[SegmentKey]string) (*Loaded, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	loaded, reason, err := s.inspectLocked(ctx, meta, plans, expectedSegmentParams)
	if err != nil {
		return nil, err
	}
	if forceFull {
		if err := s.resetLocked(ctx, meta, plans); err != nil {
			return nil, err
		}
		s.meta = meta
		return &Loaded{Segments: map[SegmentKey]StoredSegment{}, Frames: map[FrameKey]StoredFrame{}, Reused: false, Reason: "full_recompute"}, nil
	}
	if loaded != nil {
		s.meta = meta
		return loaded, nil
	}
	if err := s.resetLocked(ctx, meta, plans); err != nil {
		return nil, err
	}
	s.meta = meta
	return &Loaded{Segments: map[SegmentKey]StoredSegment{}, Frames: map[FrameKey]StoredFrame{}, Reused: false, Reason: reason}, nil
}

func (s *Store) inspectLocked(ctx context.Context, expected Meta, plans []RoutePlan, expectedSegmentParams map[SegmentKey]string) (*Loaded, string, error) {
	meta, err := s.readMetaLocked(ctx)
	if err != nil {
		return nil, "", err
	}
	if len(meta) == 0 {
		return nil, "empty_database", nil
	}
	if meta["schema_version"] != expected.SchemaVersion ||
		meta["algorithm_version"] != expected.AlgorithmVersion ||
		meta["param_version"] != expected.ParamVersion ||
		meta["truth_fingerprint"] != expected.TruthFingerprint {
		return nil, "version_mismatch", nil
	}
	if err := s.validateRoutesLocked(ctx, expected, plans); err != nil {
		return nil, "content_hash_mismatch", nil
	}
	segments, err := s.loadSegmentsLocked(ctx)
	if err != nil {
		return nil, "", err
	}
	frames, err := s.loadFramesLocked(ctx)
	if err != nil {
		return nil, "", err
	}
	planByKey := make(map[SegmentKey]SegmentPlan)
	for _, rp := range plans {
		for _, sp := range rp.Segments {
			planByKey[SegmentKey{sp.RouteID, sp.Segment}] = sp
		}
	}
	if len(segments) > len(planByKey) {
		return nil, "content_hash_mismatch", nil
	}
	expectedFrames := 0
	for key, seg := range segments {
		plan, ok := planByKey[key]
		if !ok || seg.CommitOrder != plan.CommitOrder {
			return nil, "content_hash_mismatch", nil
		}
		if expectedSegmentParams != nil {
			wantParam, ok := expectedSegmentParams[key]
			if !ok || seg.ParamVersion != wantParam ||
				seg.AlgorithmVersion != expected.AlgorithmVersion ||
				seg.TruthFingerprint != expected.TruthFingerprint {
				return nil, "version_mismatch", nil
			}
		}
		framesForSegment := make([]StoredFrame, 0, seg.FrameCount)
		for _, fr := range frames {
			if fr.RouteID == key.RouteID && fr.Segment == key.Segment {
				framesForSegment = append(framesForSegment, fr)
			}
		}
		sort.Slice(framesForSegment, func(i, j int) bool { return framesForSegment[i].FrameIndex < framesForSegment[j].FrameIndex })
		expectedIndices := frameIndices(plan.FrameStart, plan.FrameEnd)
		if len(framesForSegment) != len(expectedIndices) || len(framesForSegment) != seg.FrameCount {
			return nil, "content_hash_mismatch", nil
		}
		results := make([]model.FrameResult, 0, len(framesForSegment))
		for i, fr := range framesForSegment {
			if fr.FrameIndex != expectedIndices[i] || model.SHA256Hex(fr.Pixels) != fr.PixelHash {
				return nil, "content_hash_mismatch", nil
			}
			if fr.ParamVersion != seg.ParamVersion || fr.AlgorithmVersion != seg.AlgorithmVersion ||
				fr.TruthFingerprint != seg.TruthFingerprint {
				return nil, "version_mismatch", nil
			}
			var proof model.Proof
			if err := json.Unmarshal(fr.ProofJSON, &proof); err != nil {
				return nil, "content_hash_mismatch", nil
			}
			if proof.RecordSHA256 != fr.RecordHash || model.ComputeRecordHash(proof) != proof.RecordSHA256 {
				return nil, "content_hash_mismatch", nil
			}
			results = append(results, model.FrameResult{
				RouteID: fr.RouteID, Frame: fr.FrameIndex, Segment: fr.Segment,
				Pixels: fr.Pixels, Proof: proof, ProofJSON: fr.ProofJSON,
			})
			expectedFrames++
		}
		computed := BuildSegmentCommit(plan, results, seg.ParamVersion, seg.AlgorithmVersion, seg.TruthFingerprint)
		if computed.SourceHash != seg.SourceHash || computed.OutputHash != seg.OutputHash ||
			computed.ProofHash != seg.ProofHash || computed.ContentHash != seg.ContentHash ||
			string(computed.ProofSummary) != string(seg.ProofSummary) {
			return nil, "content_hash_mismatch", nil
		}
	}
	if expectedFrames != len(frames) {
		return nil, "content_hash_mismatch", nil
	}
	return &Loaded{Segments: segments, Frames: frames, Reused: len(segments) > 0,
		Reason: "version_matched_snapshot_reused"}, "version_matched_snapshot_reused", nil
}

func frameIndices(start, end int) []int {
	out := make([]int, 0, end-start+1)
	for i := start; i <= end; i++ {
		out = append(out, i)
	}
	return out
}

func (s *Store) readMetaLocked(ctx context.Context) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT key, value FROM snapshot_meta`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]string)
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, rows.Err()
}

func (s *Store) validateRoutesLocked(ctx context.Context, expected Meta, plans []RoutePlan) error {
	rows, err := s.db.QueryContext(ctx, `SELECT route_id, route_order, route_json, truth_fingerprint, param_version, algorithm_version, frame_count, segment_count FROM routes ORDER BY route_order`)
	if err != nil {
		return err
	}
	defer rows.Close()
	byID := make(map[string]RoutePlan, len(plans))
	for _, rp := range plans {
		byID[rp.Route.ID] = rp
	}
	count := 0
	for rows.Next() {
		var id, truthFP, pv, av string
		var order, frameCount, segmentCount int
		var routeJSON []byte
		if err := rows.Scan(&id, &order, &routeJSON, &truthFP, &pv, &av, &frameCount, &segmentCount); err != nil {
			return err
		}
		rp, ok := byID[id]
		if !ok || order != rp.Order || truthFP != expected.TruthFingerprint ||
			pv != expected.ParamVersion || av != expected.AlgorithmVersion ||
			frameCount != rp.Route.FrameCount || segmentCount != len(rp.Segments) {
			return fmt.Errorf("route snapshot mismatch")
		}
		var storedRoute model.Route
		if err := json.Unmarshal(routeJSON, &storedRoute); err != nil || model.HashJSON(storedRoute) != model.HashJSON(rp.Route) {
			return fmt.Errorf("route declaration mismatch")
		}
		count++
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if count != len(plans) {
		return fmt.Errorf("route count mismatch")
	}
	planRows, err := s.db.QueryContext(ctx, `SELECT route_id, segment_index, frame_start, frame_end, commit_order FROM segment_plans ORDER BY route_id, segment_index`)
	if err != nil {
		return err
	}
	defer planRows.Close()
	expectedPlans := make(map[SegmentKey]SegmentPlan)
	for _, rp := range plans {
		for _, sp := range rp.Segments {
			expectedPlans[SegmentKey{sp.RouteID, sp.Segment}] = sp
		}
	}
	seen := 0
	for planRows.Next() {
		var key SegmentKey
		var plan SegmentPlan
		if err := planRows.Scan(&plan.RouteID, &plan.Segment, &plan.FrameStart, &plan.FrameEnd, &plan.CommitOrder); err != nil {
			return err
		}
		key = SegmentKey{plan.RouteID, plan.Segment}
		want, ok := expectedPlans[key]
		if !ok || want != plan {
			return fmt.Errorf("segment plan mismatch")
		}
		seen++
	}
	if err := planRows.Err(); err != nil {
		return err
	}
	if seen != len(expectedPlans) {
		return fmt.Errorf("segment plan count mismatch")
	}
	return nil
}

func (s *Store) loadSegmentsLocked(ctx context.Context) (map[SegmentKey]StoredSegment, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT route_id, segment_index, commit_order, source_hash, output_hash, proof_hash, content_hash, param_version, algorithm_version, truth_fingerprint, frame_count, proof_summary FROM segments`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[SegmentKey]StoredSegment)
	for rows.Next() {
		var seg StoredSegment
		if err := rows.Scan(&seg.RouteID, &seg.Segment, &seg.CommitOrder, &seg.SourceHash,
			&seg.OutputHash, &seg.ProofHash, &seg.ContentHash, &seg.ParamVersion,
			&seg.AlgorithmVersion, &seg.TruthFingerprint, &seg.FrameCount, &seg.ProofSummary); err != nil {
			return nil, err
		}
		out[SegmentKey{seg.RouteID, seg.Segment}] = seg
	}
	return out, rows.Err()
}

func (s *Store) loadFramesLocked(ctx context.Context) (map[FrameKey]StoredFrame, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT route_id, frame_index, segment_index, timestamp_ms, width, height, channels, pixels, pixel_hash, proof_json, record_hash, param_version, algorithm_version, truth_fingerprint FROM frames`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[FrameKey]StoredFrame)
	for rows.Next() {
		var fr StoredFrame
		if err := rows.Scan(&fr.RouteID, &fr.FrameIndex, &fr.Segment, &fr.TimestampMS,
			&fr.Width, &fr.Height, &fr.Channels, &fr.Pixels, &fr.PixelHash, &fr.ProofJSON,
			&fr.RecordHash, &fr.ParamVersion, &fr.AlgorithmVersion, &fr.TruthFingerprint); err != nil {
			return nil, err
		}
		out[FrameKey{fr.RouteID, fr.FrameIndex}] = fr
	}
	return out, rows.Err()
}

func (s *Store) resetLocked(ctx context.Context, meta Meta, plans []RoutePlan) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, table := range []string{"frames", "segments", "segment_plans", "routes", "snapshot_meta"} {
		if _, err := tx.ExecContext(ctx, "DELETE FROM "+table); err != nil {
			return err
		}
	}
	metaRows := []struct{ k, v string }{
		{"schema_version", meta.SchemaVersion},
		{"algorithm_version", meta.AlgorithmVersion},
		{"param_version", meta.ParamVersion},
		{"truth_fingerprint", meta.TruthFingerprint},
	}
	for _, row := range metaRows {
		if _, err := tx.ExecContext(ctx, `INSERT INTO snapshot_meta(key, value) VALUES(?, ?)`, row.k, row.v); err != nil {
			return err
		}
	}
	for _, rp := range plans {
		routeJSON, _ := json.Marshal(rp.Route)
		if _, err := tx.ExecContext(ctx, `INSERT INTO routes(route_id, route_order, route_json, truth_fingerprint, param_version, algorithm_version, frame_count, segment_count) VALUES(?,?,?,?,?,?,?,?)`,
			rp.Route.ID, rp.Order, routeJSON, meta.TruthFingerprint, meta.ParamVersion,
			meta.AlgorithmVersion, rp.Route.FrameCount, len(rp.Segments)); err != nil {
			return err
		}
		for _, sp := range rp.Segments {
			if _, err := tx.ExecContext(ctx, `INSERT INTO segment_plans(route_id, segment_index, frame_start, frame_end, commit_order) VALUES(?,?,?,?,?)`,
				sp.RouteID, sp.Segment, sp.FrameStart, sp.FrameEnd, sp.CommitOrder); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

func (s *Store) CommitSegment(ctx context.Context, c Commit) (StoredSegment, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(c.Frames) == 0 {
		return StoredSegment{}, apperr.New(apperr.CodeSeg, c.Plan.RouteID, c.Plan.Segment, -1,
			"segment_frames", "at least one frame", "empty segment commit", nil)
	}
	sort.Slice(c.Frames, func(i, j int) bool { return c.Frames[i].Frame < c.Frames[j].Frame })
	computed := BuildSegmentCommit(c.Plan, c.Frames, c.ParamVersion, c.AlgorithmVersion, c.TruthFingerprint)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return StoredSegment{}, err
	}
	defer tx.Rollback()
	var existing string
	err = tx.QueryRowContext(ctx, `SELECT content_hash FROM segments WHERE route_id=? AND segment_index=?`,
		c.Plan.RouteID, c.Plan.Segment).Scan(&existing)
	if err != nil && err != sql.ErrNoRows {
		return StoredSegment{}, err
	}
	if err == nil {
		if existing == computed.ContentHash {
			return StoredSegment{
				RouteID: c.Plan.RouteID, Segment: c.Plan.Segment, CommitOrder: c.Plan.CommitOrder,
				SourceHash: computed.SourceHash, OutputHash: computed.OutputHash,
				ProofHash: computed.ProofHash, ContentHash: computed.ContentHash,
				ParamVersion: c.ParamVersion, AlgorithmVersion: c.AlgorithmVersion,
				TruthFingerprint: c.TruthFingerprint, FrameCount: len(c.Frames),
				ProofSummary: computed.ProofSummary,
			}, nil
		}
		return StoredSegment{}, apperr.New(apperr.CodeState, c.Plan.RouteID, c.Plan.Segment, -1,
			"segment_state", "committed segment is immutable", "attempted to replace committed segment", nil)
	}
	for _, frame := range c.Frames {
		if frame.Proof.RouteID != c.Plan.RouteID || frame.Proof.Segment != c.Plan.Segment {
			return StoredSegment{}, apperr.New(apperr.CodeSeg, c.Plan.RouteID, c.Plan.Segment, frame.Frame,
				"proof_identity", "matching route and segment", "proof identity mismatch", nil)
		}
		if model.ComputeRecordHash(frame.Proof) != frame.Proof.RecordSHA256 {
			return StoredSegment{}, apperr.New(apperr.CodeSeg, c.Plan.RouteID, c.Plan.Segment, frame.Frame,
				"record_sha256", "recomputed proof hash", "proof hash mismatch", nil)
		}
		recordHash := frame.Proof.RecordSHA256
		pixelHash := model.SHA256Hex(frame.Pixels)
		if _, err := tx.ExecContext(ctx, `INSERT INTO frames(route_id, frame_index, segment_index, timestamp_ms, width, height, channels, pixels, pixel_hash, proof_json, record_hash, param_version, algorithm_version, truth_fingerprint) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			frame.RouteID, frame.Frame, frame.Segment, frame.Proof.TimestampMS,
			frame.Proof.OutputStats.Width, frame.Proof.OutputStats.Height, frame.Proof.OutputStats.Channels,
			frame.Pixels, pixelHash, frame.ProofJSON, recordHash,
			c.ParamVersion, c.AlgorithmVersion, c.TruthFingerprint); err != nil {
			return StoredSegment{}, err
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO segments(route_id, segment_index, commit_order, source_hash, output_hash, proof_hash, content_hash, param_version, algorithm_version, truth_fingerprint, frame_count, proof_summary) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`,
		c.Plan.RouteID, c.Plan.Segment, c.Plan.CommitOrder, computed.SourceHash,
		computed.OutputHash, computed.ProofHash, computed.ContentHash, c.ParamVersion,
		c.AlgorithmVersion, c.TruthFingerprint, len(c.Frames), computed.ProofSummary); err != nil {
		return StoredSegment{}, err
	}
	if err := tx.Commit(); err != nil {
		return StoredSegment{}, err
	}
	return StoredSegment{
		RouteID: c.Plan.RouteID, Segment: c.Plan.Segment, CommitOrder: c.Plan.CommitOrder,
		SourceHash: computed.SourceHash, OutputHash: computed.OutputHash,
		ProofHash: computed.ProofHash, ContentHash: computed.ContentHash,
		ParamVersion: c.ParamVersion, AlgorithmVersion: c.AlgorithmVersion,
		TruthFingerprint: c.TruthFingerprint, FrameCount: len(c.Frames),
		ProofSummary: computed.ProofSummary,
	}, nil
}

type CommitHashes struct {
	SourceHash   string
	OutputHash   string
	ProofHash    string
	ContentHash  string
	ProofSummary []byte
}

func BuildSegmentCommit(plan SegmentPlan, frames []model.FrameResult, paramVersion, algorithmVersion, truthFingerprint string) CommitHashes {
	source := newStringHasher()
	output := newStringHasher()
	proof := newStringHasher()
	for _, frame := range frames {
		source.Add(frame.Proof.Input.SourceFrameSHA256)
		output.Add(model.SHA256Hex(frame.Pixels))
		proof.Add(frame.Proof.RecordSHA256)
	}
	summary := struct {
		RecordVersion    string `json:"record_version"`
		AlgorithmVersion string `json:"algorithm_version"`
		ParamVersion     string `json:"param_version"`
		RouteID          string `json:"route_id"`
		Segment          int    `json:"segment"`
		FrameCount       int    `json:"frame_count"`
		SourceSHA256     string `json:"source_sha256"`
		OutputSHA256     string `json:"output_sha256"`
		ProofSHA256      string `json:"proof_sha256"`
	}{
		RecordVersion: model.RecordVersion, AlgorithmVersion: algorithmVersion,
		ParamVersion: paramVersion, RouteID: plan.RouteID, Segment: plan.Segment,
		FrameCount: len(frames), SourceSHA256: source.Sum(), OutputSHA256: output.Sum(),
		ProofSHA256: proof.Sum(),
	}
	summaryJSON, _ := json.Marshal(summary)
	contentInput := struct {
		Plan             SegmentPlan `json:"plan"`
		ParamVersion     string      `json:"param_version"`
		AlgorithmVersion string      `json:"algorithm_version"`
		TruthFingerprint string      `json:"truth_fingerprint"`
		Summary          interface{} `json:"summary"`
	}{
		Plan: plan, ParamVersion: paramVersion, AlgorithmVersion: algorithmVersion,
		TruthFingerprint: truthFingerprint,
		Summary: struct {
			Source string `json:"source"`
			Output string `json:"output"`
			Proof  string `json:"proof"`
		}{source.Sum(), output.Sum(), proof.Sum()},
	}
	return CommitHashes{
		SourceHash: source.Sum(), OutputHash: output.Sum(), ProofHash: proof.Sum(),
		ContentHash: model.HashJSON(contentInput), ProofSummary: summaryJSON,
	}
}

type stringHasher struct {
	parts []string
}

func newStringHasher() *stringHasher { return &stringHasher{} }
func (h *stringHasher) Add(s string) { h.parts = append(h.parts, s) }
func (h *stringHasher) Sum() string {
	var data []byte
	for _, p := range h.parts {
		data = append(data, p...)
	}
	return model.SHA256Hex(data)
}

// ReadAll loads a snapshot for comparison or audit. It does not validate or
// mutate it.
func (s *Store) ReadAll(ctx context.Context) (map[SegmentKey]StoredSegment, map[FrameKey]StoredFrame, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	segments, err := s.loadSegmentsLocked(ctx)
	if err != nil {
		return nil, nil, err
	}
	frames, err := s.loadFramesLocked(ctx)
	if err != nil {
		return nil, nil, err
	}
	return segments, frames, nil
}

func (s *Store) FrameCounts(ctx context.Context) (map[FrameKey]int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.QueryContext(ctx, `SELECT route_id, frame_index, COUNT(*) FROM frames GROUP BY route_id, frame_index`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[FrameKey]int)
	for rows.Next() {
		var key FrameKey
		var count int
		if err := rows.Scan(&key.RouteID, &key.Frame, &count); err != nil {
			return nil, err
		}
		out[key] = count
	}
	return out, rows.Err()
}

func (s *Store) Meta(ctx context.Context) (map[string]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.readMetaLocked(ctx)
}

func (s *Store) RouteJSON(ctx context.Context) ([]model.Route, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.QueryContext(ctx, `SELECT route_json FROM routes ORDER BY route_order`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var routes []model.Route
	for rows.Next() {
		var b []byte
		if err := rows.Scan(&b); err != nil {
			return nil, err
		}
		var r model.Route
		if err := json.Unmarshal(b, &r); err != nil {
			return nil, err
		}
		routes = append(routes, r)
	}
	return routes, rows.Err()
}
