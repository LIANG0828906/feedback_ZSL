// Package checkpoint implements version-safe, atomic per-segment persistence.
package checkpoint

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"sync"

	"strokeforge/internal/checkpoint/pb"
	"strokeforge/internal/model"
	"strokeforge/internal/raster"
)

// RecoveryReport explains what happened on load.
type RecoveryReport struct {
	Directory   string `json:"directory"`
	Records     int    `json:"records"`
	Restored    int    `json:"restored"`
	Invalidated bool   `json:"invalidated"`
	FullRebuild bool   `json:"full_rebuild"`
	Reason      string `json:"reason"`
}

// Store serializes writes through a single mutex and uses temp-file rename
// for atomicity.
type Store struct {
	mu  sync.Mutex
	dir string
}

// Open creates or opens a checkpoint directory.
func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, model.NewError(model.EStage, "cannot create checkpoint dir: "+err.Error())
	}
	return &Store{dir: dir}, nil
}

func (s *Store) manifestPath() string { return filepath.Join(s.dir, "manifest.json") }

func (s *Store) recordPath(glyph string, seg int) string {
	return filepath.Join(s.dir, fmt.Sprintf("%s-%03d.json", glyph, seg))
}

// WriteManifest atomically stores document identity.
func (s *Store) WriteManifest(m pb.Manifest) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return atomicWriteJSON(s.manifestPath(), m)
}

// SaveRecord serializes one committed segment atomically and serially.
func (s *Store) SaveRecord(snap pb.SegmentSnapshot) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	r := pb.Record{
		Algorithm:    snap.Algorithm,
		ContentHash:  snap.ContentHash,
		ContourHash:  snap.ContourHash,
		ParamVersion: snap.ParamVersion,
		GlyphID:      snap.GlyphID,
		SegmentID:    snap.SegmentID,
		ContourID:    snap.ContourID,
		CacheKey:     snap.CacheKey,
		MaskWidth:    snap.MaskWidth,
		MaskHeight:   snap.MaskHeight,
		MaskPix:      snap.MaskPix,
		Densities:    snap.Densities,
		Boundary:     snap.Boundary,
	}
	r.RecordHash = HashRecord(r)
	return atomicWriteJSON(s.recordPath(r.GlyphID, r.SegmentID), r)
}

// HashRecord fingerprints a record's semantic payload.
func HashRecord(r pb.Record) string {
	type wire struct {
		Algorithm    string
		ContentHash  string
		ContourHash  string
		ParamVersion string
		GlyphID      string
		SegmentID    int
		ContourID    string
		CacheKey     string
		MaskPix      []byte
		Densities    []int
	}
	raw, _ := json.Marshal(wire{
		Algorithm: r.Algorithm, ContentHash: r.ContentHash,
		ContourHash:  r.ContourHash,
		ParamVersion: r.ParamVersion, GlyphID: r.GlyphID,
		SegmentID: r.SegmentID, ContourID: r.ContourID,
		CacheKey: r.CacheKey, MaskPix: r.MaskPix, Densities: r.Densities,
	})
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func atomicWriteJSON(path string, v any) error {
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return model.NewError(model.EStage, "checkpoint encode failed: "+err.Error())
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return model.NewError(model.EStage, "checkpoint write failed: "+err.Error())
	}
	if err := os.Rename(tmp, path); err != nil {
		return model.NewError(model.EStage, "checkpoint rename failed: "+err.Error())
	}
	return nil
}

// Load validates the manifest and every record. Any mismatch invalidates the
// entire directory (no partial mixing) and triggers a full rebuild.
func (s *Store) Load(expected pb.Manifest, frames map[string]raster.Frame) ([]pb.Record, *RecoveryReport, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	report := &RecoveryReport{Directory: s.dir}
	manRaw, err := os.ReadFile(s.manifestPath())
	if err != nil {
		return s.invalidate(report, "manifest missing: "+err.Error())
	}
	var got pb.Manifest
	if err := json.Unmarshal(manRaw, &got); err != nil {
		return s.invalidate(report, "manifest unreadable: "+err.Error())
	}
	if !reflect.DeepEqual(got, expected) {
		return s.invalidate(report, fmt.Sprintf(
			"manifest mismatch got={%s %v %s} want={%s %v %s}",
			got.Algorithm, got.GlyphHashes, got.ParamVersion,
			expected.Algorithm, expected.GlyphHashes, expected.ParamVersion))
	}
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, report, model.NewError(model.EStage, "read checkpoint dir: "+err.Error())
	}
	var records []pb.Record
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" || e.Name() == "manifest.json" {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(s.dir, e.Name()))
		if err != nil {
			return s.invalidate(report, "record unreadable: "+e.Name())
		}
		var r pb.Record
		if err := json.Unmarshal(raw, &r); err != nil {
			return s.invalidate(report, "record malformed: "+e.Name())
		}
		if HashRecord(r) != r.RecordHash {
			return s.invalidate(report, fmt.Sprintf("segment %s#%d hash mismatch", r.GlyphID, r.SegmentID))
		}
		expectedHash, hashOK := expected.GlyphHashes[r.GlyphID]
		if !hashOK {
			return s.invalidate(report, fmt.Sprintf("segment %s#%d unknown glyph", r.GlyphID, r.SegmentID))
		}
		if r.Algorithm != expected.Algorithm || r.ContentHash != expectedHash ||
			r.ParamVersion != expected.ParamVersion {
			return s.invalidate(report, fmt.Sprintf("segment %s#%d version mismatch", r.GlyphID, r.SegmentID))
		}
		fr, ok := frames[r.GlyphID]
		if !ok || fr.Width != r.MaskWidth || fr.Height != r.MaskHeight {
			return s.invalidate(report, fmt.Sprintf("segment %s#%d frame mismatch", r.GlyphID, r.SegmentID))
		}
		records = append(records, r)
	}
	sort.Slice(records, func(i, j int) bool {
		if records[i].GlyphID != records[j].GlyphID {
			return records[i].GlyphID < records[j].GlyphID
		}
		return records[i].SegmentID < records[j].SegmentID
	})
	report.Records = len(records)
	report.Restored = len(records)
	return records, report, nil
}

func (s *Store) invalidate(report *RecoveryReport, reason string) ([]pb.Record, *RecoveryReport, error) {
	_ = os.RemoveAll(s.dir)
	_ = os.MkdirAll(s.dir, 0o755)
	report.Invalidated = true
	report.FullRebuild = true
	report.Reason = reason
	return nil, report, nil
}

// Reset removes every checkpoint file.
func (s *Store) Reset() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.RemoveAll(s.dir); err != nil {
		return err
	}
	return os.MkdirAll(s.dir, 0o755)
}

// Tamper flips one mask byte while keeping the stale hash, so Load rejects it.
func (s *Store) Tamper(glyph string, seg int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	path := s.recordPath(glyph, seg)
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var r pb.Record
	if err := json.Unmarshal(raw, &r); err != nil {
		return err
	}
	if len(r.MaskPix) == 0 {
		return fmt.Errorf("empty mask")
	}
	r.MaskPix[0] ^= 0xFF
	return atomicWriteJSON(path, r)
}
