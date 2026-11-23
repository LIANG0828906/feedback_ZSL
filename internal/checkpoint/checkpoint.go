// Package checkpoint persists per-stroke-segment results with strict version
// gating. A snapshot is used only if glyph hash, parameter version and
// algorithm version all match; otherwise the whole snapshot is discarded.
package checkpoint

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"glyphbrush/internal/raster"
	"glyphbrush/internal/stroke"
)

const SchemaVersion = "glyphbrush.checkpoint/v1"

var (
	ErrNoSnapshot      = errors.New("checkpoint: no snapshot")
	ErrVersionMismatch = errors.New("checkpoint: version mismatch")
	ErrCorrupt         = errors.New("checkpoint: corrupt snapshot")
)

type Meta struct {
	SchemaVersion    string `json:"schemaVersion"`
	GlyphID          string `json:"glyphId"`
	GlyphHash        string `json:"glyphHash"`
	ParamVersion     string `json:"paramVersion"`
	AlgorithmVersion string `json:"algorithmVersion"`
	SegmentCount     int    `json:"segmentCount"`
}

type Record struct {
	SchemaVersion    string                 `json:"schemaVersion"`
	GlyphID          string                 `json:"glyphId"`
	SegmentID        int                    `json:"segmentId"`
	Ordinal          int                    `json:"ordinal"`
	ContourIndex     int                    `json:"contourIndex"`
	CurveIndex       int                    `json:"curveIndex"`
	ArcStart         float64                `json:"arcStart"`
	ArcEnd           float64                `json:"arcEnd"`
	GlyphHash        string                 `json:"glyphHash"`
	ParamVersion     string                 `json:"paramVersion"`
	AlgorithmVersion string                 `json:"algorithmVersion"`
	RecordHash       string                 `json:"recordHash"`
	Band             stroke.Band            `json:"band"`
	Mask             raster.Mask            `json:"mask"`
	Densities        []raster.DensitySample `json:"densities"`
}

type manifest struct {
	Meta
	Committed     int               `json:"committed"`
	SegmentHashes map[string]string `json:"segmentHashes"`
}

type Store struct {
	Root string
	mu   *sync.Mutex
}

var rootLocks sync.Map

func New(root string) *Store {
	key := root
	if abs, err := filepath.Abs(root); err == nil {
		key = abs
	}
	lock, _ := rootLocks.LoadOrStore(key, &sync.Mutex{})
	return &Store{Root: root, mu: lock.(*sync.Mutex)}
}

func (s *Store) glyphDir(glyphID string) string {
	safe := sanitize(glyphID)
	return filepath.Join(s.Root, safe)
}

func sanitize(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	if b.Len() == 0 {
		return "_glyph"
	}
	return b.String()
}

func (s *Store) manifestPath(glyphID string) string {
	return filepath.Join(s.glyphDir(glyphID), "manifest.json")
}

func (s *Store) segmentPath(glyphID string, ordinal int) string {
	return filepath.Join(s.glyphDir(glyphID), "segments", fmt.Sprintf("%06d.json", ordinal))
}

// Load returns the snapshot in stable ordinal order. Any mismatch or
// corruption removes the complete snapshot before returning.
func (s *Store) Load(meta Meta) ([]Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loadLocked(meta)
}

func (s *Store) loadLocked(meta Meta) ([]Record, error) {
	raw, err := os.ReadFile(s.manifestPath(meta.GlyphID))
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNoSnapshot
	}
	if err != nil {
		return nil, err
	}
	var m manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		s.discard(meta.GlyphID)
		return nil, fmt.Errorf("%w: manifest json: %v", ErrCorrupt, err)
	}
	if m.SchemaVersion != SchemaVersion || m.GlyphID != meta.GlyphID ||
		m.GlyphHash != meta.GlyphHash || m.ParamVersion != meta.ParamVersion ||
		m.AlgorithmVersion != meta.AlgorithmVersion || m.SegmentCount != meta.SegmentCount {
		s.discard(meta.GlyphID)
		return nil, fmt.Errorf("%w: glyph hash, parameter version, algorithm version or segment count differs", ErrVersionMismatch)
	}
	if m.Committed != meta.SegmentCount {
		s.discard(meta.GlyphID)
		return nil, fmt.Errorf("%w: snapshot has %d/%d segments", ErrVersionMismatch, m.Committed, meta.SegmentCount)
	}
	records := make([]Record, 0, meta.SegmentCount)
	for i := 0; i < meta.SegmentCount; i++ {
		segRaw, err := os.ReadFile(s.segmentPath(meta.GlyphID, i))
		if err != nil {
			s.discard(meta.GlyphID)
			return nil, fmt.Errorf("%w: segment %d: %v", ErrCorrupt, i, err)
		}
		var rec Record
		if err := json.Unmarshal(segRaw, &rec); err != nil {
			s.discard(meta.GlyphID)
			return nil, fmt.Errorf("%w: segment %d json: %v", ErrCorrupt, i, err)
		}
		if rec.Ordinal != i || rec.GlyphID != meta.GlyphID || rec.GlyphHash != meta.GlyphHash ||
			rec.ParamVersion != meta.ParamVersion || rec.AlgorithmVersion != meta.AlgorithmVersion ||
			rec.RecordHash != HashRecord(rec) {
			s.discard(meta.GlyphID)
			return nil, fmt.Errorf("%w: segment %d metadata mismatch", ErrVersionMismatch, i)
		}
		expectedHash := m.SegmentHashes[fmt.Sprint(i)]
		if expectedHash == "" || expectedHash != rec.RecordHash {
			s.discard(meta.GlyphID)
			return nil, fmt.Errorf("%w: segment %d hash mismatch", ErrCorrupt, i)
		}
		records = append(records, rec)
	}
	sort.SliceStable(records, func(i, j int) bool { return records[i].Ordinal < records[j].Ordinal })
	return records, nil
}

// Reset discards any existing snapshot and writes an empty manifest.
func (s *Store) Reset(meta Meta) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.resetLocked(meta)
}

func (s *Store) resetLocked(meta Meta) error {
	s.discard(meta.GlyphID)
	if err := os.MkdirAll(filepath.Join(s.glyphDir(meta.GlyphID), "segments"), 0o755); err != nil {
		return err
	}
	m := manifest{Meta: meta, Committed: 0, SegmentHashes: map[string]string{}}
	return s.writeManifest(meta.GlyphID, m)
}

// Commit writes one segment atomically and then advances the manifest. It is
// serialized by the store mutex and requires contiguous ordinals.
func (s *Store) Commit(meta Meta, rec Record, ordinal int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.commitLocked(meta, rec, ordinal)
}

func (s *Store) commitLocked(meta Meta, rec Record, ordinal int) error {
	if ordinal < 0 || ordinal >= meta.SegmentCount {
		return fmt.Errorf("checkpoint: ordinal %d outside [0,%d)", ordinal, meta.SegmentCount)
	}
	rawManifest, err := os.ReadFile(s.manifestPath(meta.GlyphID))
	if err != nil {
		return err
	}
	var m manifest
	if err := json.Unmarshal(rawManifest, &m); err != nil {
		s.discard(meta.GlyphID)
		return fmt.Errorf("%w: %v", ErrCorrupt, err)
	}
	if m.GlyphHash != meta.GlyphHash || m.ParamVersion != meta.ParamVersion ||
		m.AlgorithmVersion != meta.AlgorithmVersion || m.SegmentCount != meta.SegmentCount {
		s.discard(meta.GlyphID)
		return ErrVersionMismatch
	}
	if ordinal != m.Committed {
		return fmt.Errorf("checkpoint: expected ordinal %d, got %d", m.Committed, ordinal)
	}
	rec.Ordinal = ordinal
	rec.GlyphID = meta.GlyphID
	rec.GlyphHash = meta.GlyphHash
	rec.ParamVersion = meta.ParamVersion
	rec.AlgorithmVersion = meta.AlgorithmVersion
	rec.SchemaVersion = SchemaVersion
	rec.RecordHash = HashRecord(rec)
	raw, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	if err := atomicWrite(s.segmentPath(meta.GlyphID, ordinal), raw); err != nil {
		return err
	}
	m.Committed = ordinal + 1
	if m.SegmentHashes == nil {
		m.SegmentHashes = map[string]string{}
	}
	m.SegmentHashes[fmt.Sprint(ordinal)] = rec.RecordHash
	return s.writeManifest(meta.GlyphID, m)
}

// CommitAll resets the snapshot then commits every record in ordinal order.
func (s *Store) CommitAll(meta Meta, records []Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(records) != meta.SegmentCount {
		return fmt.Errorf("checkpoint: got %d records for %d segments", len(records), meta.SegmentCount)
	}
	if err := s.resetLocked(meta); err != nil {
		return err
	}
	for i, rec := range records {
		if err := s.commitLocked(meta, rec, i); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) writeManifest(glyphID string, m manifest) error {
	raw, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return atomicWrite(s.manifestPath(glyphID), raw)
}

func (s *Store) discard(glyphID string) {
	_ = os.RemoveAll(s.glyphDir(glyphID))
}

// Invalidate removes the complete snapshot for a glyph.
func (s *Store) Invalidate(glyphID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.discard(glyphID)
}

func atomicWrite(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	ok := false
	defer func() {
		_ = f.Close()
		if !ok {
			_ = os.Remove(tmp)
		}
	}()
	if _, err := f.Write(data); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	ok = true
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

// HashRecord hashes every record field except RecordHash itself.
func HashRecord(rec Record) string {
	cp := rec
	cp.RecordHash = ""
	raw, _ := json.Marshal(cp)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// ValidateRecordGeometry rechecks the persisted band and mask dimensions.
func ValidateRecordGeometry(rec Record, width, height int) error {
	if len(rec.Band.Polygon) < 3 {
		return fmt.Errorf("%w: segment %d band is degenerate", ErrCorrupt, rec.SegmentID)
	}
	if rec.Mask.Width != width || rec.Mask.Height != height || len(rec.Mask.Data) != width*height {
		return fmt.Errorf("%w: segment %d mask dimensions differ", ErrVersionMismatch, rec.SegmentID)
	}
	for _, p := range rec.Band.Polygon {
		if !rec.Band.Bounds.Contains(p) {
			return fmt.Errorf("%w: segment %d band bounds are inconsistent", ErrCorrupt, rec.SegmentID)
		}
	}
	return nil
}
