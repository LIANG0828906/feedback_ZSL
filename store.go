package terrainbake

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

const checkpointFormatVersion = 1

// BlockCheckpoint is the durable identity of one committed block.
type BlockCheckpoint struct {
	BlockID          string    `json:"block_id"`
	Width            int       `json:"width"`
	Height           int       `json:"height"`
	NormalDigest     string    `json:"normal_digest"`
	SlopeDigest      string    `json:"slope_digest"`
	DataDigest       string    `json:"data_digest"`
	ContentHash      string    `json:"content_hash"`
	ParamsVersion    string    `json:"params_version"`
	AlgorithmVersion string    `json:"algorithm_version"`
	Seam             SeamStats `json:"seam"`
}

// Manifest is the serial checkpoint snapshot. Every mutation is written with
// an atomic rename under Store's mutex.
type Manifest struct {
	FormatVersion     int                        `json:"format_version"`
	AlgorithmVersion  string                     `json:"algorithm_version"`
	ParamsVersion     string                     `json:"params_version"`
	TargetContentHash string                     `json:"target_content_hash"`
	Complete          bool                       `json:"complete"`
	Blocks            map[string]BlockCheckpoint `json:"blocks"`
	Pending           []string                   `json:"pending"`
	EpochDirty        []string                   `json:"epoch_dirty"`
	CommitOrder       []string                   `json:"commit_order"`
	Invalidation      []InvalidationItem         `json:"invalidation,omitempty"`
	Failures          map[string]string          `json:"failures,omitempty"`
}

// Store owns a local directory snapshot. All shared snapshot writes are
// serialized through mu.
type Store struct {
	dir      string
	mu       sync.Mutex
	manifest Manifest
	loaded   bool
}

func OpenStore(dir string) (*Store, error) {
	if dir == "" {
		return nil, &Error{Code: CodeParam, Object: "store", Field: "dir", Message: "store directory must not be empty"}
	}
	s := &Store{dir: dir}
	if err := os.MkdirAll(filepath.Join(dir, "blocks"), 0o755); err != nil {
		return nil, &Error{Code: CodeIO, Object: "store", Field: "dir", Message: err.Error()}
	}
	if err := os.MkdirAll(filepath.Join(dir, "tmp"), 0o755); err != nil {
		return nil, &Error{Code: CodeIO, Object: "store", Field: "tmp", Message: err.Error()}
	}
	data, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			s.manifest = emptyManifest()
			return s, nil
		}
		return nil, &Error{Code: CodeIO, Object: "checkpoint", Field: "manifest", Message: err.Error()}
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		// A torn/incompatible manifest is treated as a fully invalid snapshot,
		// never as a reason to mix partial blocks.
		s.manifest = emptyManifest()
		return s, nil
	}
	if m.FormatVersion != checkpointFormatVersion || m.Blocks == nil {
		s.manifest = emptyManifest()
		return s, nil
	}
	s.manifest = m
	s.loaded = true
	return s, nil
}

func emptyManifest() Manifest {
	return Manifest{
		FormatVersion: checkpointFormatVersion,
		Blocks:        make(map[string]BlockCheckpoint),
		Failures:      make(map[string]string),
	}
}

func (s *Store) Dir() string { return s.dir }

func (s *Store) Manifest() Manifest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return cloneManifest(s.manifest)
}

func (s *Store) HasSnapshot() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.manifest.Blocks) != 0 || s.manifest.TargetContentHash != ""
}

func cloneManifest(m Manifest) Manifest {
	out := m
	out.Blocks = make(map[string]BlockCheckpoint, len(m.Blocks))
	for k, v := range m.Blocks {
		out.Blocks[k] = v
	}
	out.Pending = append([]string(nil), m.Pending...)
	out.EpochDirty = append([]string(nil), m.EpochDirty...)
	out.CommitOrder = append([]string(nil), m.CommitOrder...)
	out.Invalidation = append([]InvalidationItem(nil), m.Invalidation...)
	out.Failures = make(map[string]string, len(m.Failures))
	for k, v := range m.Failures {
		out.Failures[k] = v
	}
	return out
}

// Reset atomically abandons the entire snapshot. It is used for global hash or
// version mismatch and guarantees no partial old/new block mixture survives.
func (s *Store) Reset() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.RemoveAll(filepath.Join(s.dir, "blocks")); err != nil {
		return &Error{Code: CodeIO, Object: "checkpoint", Field: "blocks", Message: err.Error()}
	}
	if err := os.RemoveAll(filepath.Join(s.dir, "tmp")); err != nil {
		return &Error{Code: CodeIO, Object: "checkpoint", Field: "tmp", Message: err.Error()}
	}
	if err := os.Remove(filepath.Join(s.dir, "manifest.json")); err != nil && !errors.Is(err, os.ErrNotExist) {
		return &Error{Code: CodeIO, Object: "checkpoint", Field: "manifest", Message: err.Error()}
	}
	if err := os.MkdirAll(filepath.Join(s.dir, "blocks"), 0o755); err != nil {
		return &Error{Code: CodeIO, Object: "checkpoint", Field: "blocks", Message: err.Error()}
	}
	if err := os.MkdirAll(filepath.Join(s.dir, "tmp"), 0o755); err != nil {
		return &Error{Code: CodeIO, Object: "checkpoint", Field: "tmp", Message: err.Error()}
	}
	s.manifest = emptyManifest()
	s.loaded = false
	return nil
}

// WriteManifest persists a manifest without changing block files.
func (s *Store) WriteManifest(m Manifest) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.writeManifestLocked(m)
}

func (s *Store) writeManifestLocked(m Manifest) error {
	m.FormatVersion = checkpointFormatVersion
	if m.Blocks == nil {
		m.Blocks = make(map[string]BlockCheckpoint)
	}
	data, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return &Error{Code: CodeIO, Object: "checkpoint", Field: "manifest", Message: err.Error()}
	}
	if err := writeFileAtomic(filepath.Join(s.dir, "manifest.json"), data, 0o644); err != nil {
		return &Error{Code: CodeIO, Object: "checkpoint", Field: "manifest", Message: err.Error()}
	}
	s.manifest = cloneManifest(m)
	s.loaded = true
	return nil
}

// CommitBlock writes PNG/data files and the updated manifest under one lock.
// A crash can leave orphan files, but never a manifest that references a
// partially written block.
func (s *Store) CommitBlock(o *BlockOutput, cp BlockCheckpoint, m Manifest) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	dir := s.blockDir(o.BlockID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return &Error{Code: CodeIO, Object: "checkpoint", Field: "block_dir", BlockID: o.BlockID, Message: err.Error()}
	}
	if err := writeFileAtomic(filepath.Join(dir, "normal.png"), o.NormalPNG, 0o644); err != nil {
		return &Error{Code: CodeIO, Object: "checkpoint", Field: "normal.png", BlockID: o.BlockID, Message: err.Error()}
	}
	if err := writeFileAtomic(filepath.Join(dir, "slope.png"), o.SlopePNG, 0o644); err != nil {
		return &Error{Code: CodeIO, Object: "checkpoint", Field: "slope.png", BlockID: o.BlockID, Message: err.Error()}
	}
	data, err := encodeDataBinary(o.Width, o.Height, o.Normals, o.SlopeDeg)
	if err != nil {
		return err
	}
	if err := writeFileAtomic(filepath.Join(dir, "data.bin"), data, 0o644); err != nil {
		return &Error{Code: CodeIO, Object: "checkpoint", Field: "data.bin", BlockID: o.BlockID, Message: err.Error()}
	}
	if m.Blocks == nil {
		m.Blocks = make(map[string]BlockCheckpoint)
	}
	m.Blocks[o.BlockID] = cp
	return s.writeManifestLocked(m)
}

// LoadBlock reads and verifies a committed block by content hash.
func (s *Store) LoadBlock(cp BlockCheckpoint) (*BlockOutput, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	dir := s.blockDir(cp.BlockID)
	normalPNG, err := os.ReadFile(filepath.Join(dir, "normal.png"))
	if err != nil {
		return nil, &Error{Code: CodeIO, Object: "checkpoint", Field: "normal.png", BlockID: cp.BlockID, Message: err.Error()}
	}
	slopePNG, err := os.ReadFile(filepath.Join(dir, "slope.png"))
	if err != nil {
		return nil, &Error{Code: CodeIO, Object: "checkpoint", Field: "slope.png", BlockID: cp.BlockID, Message: err.Error()}
	}
	data, err := os.ReadFile(filepath.Join(dir, "data.bin"))
	if err != nil {
		return nil, &Error{Code: CodeIO, Object: "checkpoint", Field: "data.bin", BlockID: cp.BlockID, Message: err.Error()}
	}
	if digestBytes(normalPNG) != cp.NormalDigest || digestBytes(slopePNG) != cp.SlopeDigest || digestBytes(data) != cp.DataDigest {
		return nil, &Error{Code: CodeIO, Object: "checkpoint", Field: "digest", BlockID: cp.BlockID, Message: "cached block digest mismatch"}
	}
	w, h, normals, slopes, err := decodeDataBinary(data)
	if err != nil {
		return nil, err
	}
	if w != cp.Width || h != cp.Height {
		return nil, &Error{Code: CodeIO, Object: "checkpoint", Field: "dimensions", BlockID: cp.BlockID, Message: "cached dimensions mismatch"}
	}
	return &BlockOutput{
		BlockID: cp.BlockID, Width: w, Height: h,
		Normals: normals, SlopeDeg: slopes,
		NormalPNG: normalPNG, SlopePNG: slopePNG,
		NormalDigest: cp.NormalDigest, SlopeDigest: cp.SlopeDigest, DataDigest: cp.DataDigest,
		ContentHash: cp.ContentHash, Seam: cp.Seam,
	}, nil
}

func (s *Store) blockDir(id string) string {
	sum := sha256.Sum256([]byte(id))
	return filepath.Join(s.dir, "blocks", hex.EncodeToString(sum[:12]))
}

// TempRawPath returns a unique raw-bake spool path. The caller owns it.
func (s *Store) TempRawPath(blockID string, generation int) string {
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s/%d", blockID, generation)))
	return filepath.Join(s.dir, "tmp", fmt.Sprintf("raw-%s.bin", hex.EncodeToString(sum[:12])))
}

// CleanTemps removes raw spool files from previous runs.
func (s *Store) CleanTemps() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	entries, err := os.ReadDir(filepath.Join(s.dir, "tmp"))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return &Error{Code: CodeIO, Object: "checkpoint", Field: "tmp", Message: err.Error()}
	}
	for _, e := range entries {
		if err := os.RemoveAll(filepath.Join(s.dir, "tmp", e.Name())); err != nil {
			return &Error{Code: CodeIO, Object: "checkpoint", Field: "tmp", Message: err.Error()}
		}
	}
	return nil
}

func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	name := f.Name()
	ok := false
	defer func() {
		_ = f.Close()
		if !ok {
			_ = os.Remove(name)
		}
	}()
	if err := f.Chmod(perm); err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(name, path); err != nil {
		// Some Windows filesystems do not replace an existing file during
		// rename. Retry after removing the old destination.
		if removeErr := os.Remove(path); removeErr == nil {
			err = os.Rename(name, path)
		}
		if err != nil {
			return err
		}
	}
	ok = true
	return nil
}

func sortedStrings(in []string) []string {
	out := append([]string(nil), in...)
	sort.Strings(out)
	return out
}
