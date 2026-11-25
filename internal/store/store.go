// Package store owns atomic, serialized checkpoint persistence. Tile files are
// immutable and written as a contiguous prefix; the commit log is append-only.
package store

import (
	"bufio"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"

	"spectralforge/internal/domain"
	"spectralforge/internal/errs"
)

type Manifest struct {
	SchemaVersion   int                 `json:"schema_version"`
	ContentHash     string              `json:"content_hash"`
	SceneHash       string              `json:"scene_hash"`
	SpectralHash    string              `json:"spectral_hash"`
	SamplingVersion string              `json:"sampling_version"`
	ParamVersion    string              `json:"param_version"`
	Width           int                 `json:"width"`
	Height          int                 `json:"height"`
	TileSize        int                 `json:"tile_size"`
	TileCount       int                 `json:"tile_count"`
	MaterialKeys    map[string]string   `json:"material_keys"`
	Config          domain.RenderConfig `json:"config"`
}

type TileSnapshot struct {
	SchemaVersion   int               `json:"schema_version"`
	CommitSeq       int               `json:"commit_seq"`
	TileIndex       int               `json:"tile_index"`
	TileX           int               `json:"tile_x"`
	TileY           int               `json:"tile_y"`
	TileW           int               `json:"tile_w"`
	TileH           int               `json:"tile_h"`
	ContentHash     string            `json:"content_hash"`
	SceneHash       string            `json:"scene_hash"`
	SamplingVersion string            `json:"sampling_version"`
	ParamVersion    string            `json:"param_version"`
	MaterialKeys    map[string]string `json:"material_keys"`
	LinearRGB       []float64         `json:"linear_rgb"`
	ObjectIDs       []uint32          `json:"object_ids"`
	PeakRadiance    []float64         `json:"peak_radiance"`
	Digest          string            `json:"digest"`
}

type CommitRecord struct {
	Seq      int    `json:"seq"`
	Tile     int    `json:"tile"`
	Digest   string `json:"digest"`
	ParamVer string `json:"param_version"`
}

type Failure struct {
	Tile    int    `json:"tile"`
	Attempt int    `json:"attempt"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

type WaitStats struct {
	Total    int                    `json:"total"`
	ByReason map[string]int         `json:"by_reason"`
	ByTile   map[int]map[string]int `json:"by_tile"`
}

type RunStats struct {
	Mode               string      `json:"mode"`
	ContentHash        string      `json:"content_hash"`
	SamplingVersion    string      `json:"sampling_version"`
	ParamVersion       string      `json:"param_version"`
	RenderSeconds      float64     `json:"render_seconds"`
	TilesRendered      int         `json:"tiles_rendered"`
	TilesReused        int         `json:"tiles_reused"`
	CommitSequence     []int       `json:"commit_sequence"`
	WaitStats          WaitStats   `json:"wait_stats"`
	TileAttempts       map[int]int `json:"tile_attempts"`
	Failures           []Failure   `json:"failures"`
	Invalidated        bool        `json:"invalidated"`
	InvalidationReason string      `json:"invalidation_reason,omitempty"`
}

type PrepareResult struct {
	Tiles       map[int]TileSnapshot
	Invalidated bool
	Reason      string
	Reused      int
	NextSeq     int
}

type Store struct {
	Root     string
	Manifest Manifest
	mu       sync.Mutex
}

func NewManifest(b *domain.SampleBundle, config domain.RenderConfig, materialKeys map[string]string, tileCount int) Manifest {
	return Manifest{
		SchemaVersion:   1,
		ContentHash:     b.ContentHash,
		SceneHash:       b.SceneHash,
		SpectralHash:    b.SpectralHash,
		SamplingVersion: domain.SamplingVersion(b.Spectral, config),
		ParamVersion:    domain.ConfigVersion(config),
		Width:           b.Scene.Width,
		Height:          b.Scene.Height,
		TileSize:        config.TileSize,
		TileCount:       tileCount,
		MaterialKeys:    materialKeys,
		Config:          config,
	}
}

func Prepare(root string, manifest Manifest, resume bool) (*Store, PrepareResult, error) {
	result := PrepareResult{Tiles: make(map[int]TileSnapshot)}
	if !resume {
		if err := os.RemoveAll(root); err != nil {
			return nil, result, errs.IO("checkpoint_root", err)
		}
	} else {
		existing, reason, err := loadManifest(filepath.Join(root, "manifest.json"))
		if err != nil || reason != "" || !manifestsEqual(existing, manifest) {
			if reason == "" {
				reason = "manifest content/sampling/parameter version mismatch"
			}
			result.Invalidated = true
			result.Reason = reason
			if err := os.RemoveAll(root); err != nil {
				return nil, result, errs.IO("checkpoint_root", err)
			}
		} else {
			tiles, next, reason, err := loadTiles(root, manifest)
			if err != nil {
				return nil, result, err
			}
			if reason != "" {
				result.Invalidated = true
				result.Reason = reason
				if err := os.RemoveAll(root); err != nil {
					return nil, result, errs.IO("checkpoint_root", err)
				}
			} else {
				result.Tiles = tiles
				result.Reused = len(tiles)
				result.NextSeq = next
			}
		}
	}
	if err := os.MkdirAll(filepath.Join(root, "tiles"), 0o755); err != nil {
		return nil, result, errs.IO("checkpoint_root", err)
	}
	if result.Reused == 0 {
		data, err := json.MarshalIndent(manifest, "", "  ")
		if err != nil {
			return nil, result, errs.Decode("manifest", "cannot encode: %v", err)
		}
		if err := WriteFileAtomic(filepath.Join(root, "manifest.json"), append(data, '\n')); err != nil {
			return nil, result, err
		}
	}
	return &Store{Root: root, Manifest: manifest}, result, nil
}

func loadManifest(path string) (Manifest, string, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return Manifest{}, "checkpoint manifest is missing", nil
	}
	if err != nil {
		return Manifest{}, "", errs.IO("manifest", err)
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return Manifest{}, "checkpoint manifest is corrupt", nil
	}
	if m.SchemaVersion != 1 {
		return Manifest{}, fmt.Sprintf("unsupported checkpoint schema %d", m.SchemaVersion), nil
	}
	return m, "", nil
}

func loadTiles(root string, manifest Manifest) (map[int]TileSnapshot, int, string, error) {
	tiles := make(map[int]TileSnapshot)
	for i := 0; i < manifest.TileCount; i++ {
		path := tilePath(root, i)
		data, err := os.ReadFile(path)
		if os.IsNotExist(err) {
			for j := i + 1; j < manifest.TileCount; j++ {
				if _, statErr := os.Stat(tilePath(root, j)); statErr == nil {
					return nil, 0, "tile checkpoint sequence has a gap", nil
				}
			}
			return tiles, i, "", nil
		}
		if err != nil {
			return nil, 0, "", errs.IO("tile_checkpoint", err)
		}
		var tile TileSnapshot
		if err := json.Unmarshal(data, &tile); err != nil {
			return nil, 0, fmt.Sprintf("tile %d checkpoint is corrupt", i), nil
		}
		if reason := validateLoadedTile(tile, i, manifest); reason != "" {
			return nil, 0, reason, nil
		}
		tiles[i] = tile
	}
	return tiles, len(tiles), "", nil
}

func validateLoadedTile(t TileSnapshot, index int, m Manifest) string {
	switch {
	case t.SchemaVersion != 1:
		return fmt.Sprintf("tile %d checkpoint schema mismatch", index)
	case t.TileIndex != index || t.CommitSeq != index:
		return fmt.Sprintf("tile %d checkpoint order mismatch", index)
	case t.ContentHash != m.ContentHash:
		return fmt.Sprintf("tile %d content hash mismatch", index)
	case t.SceneHash != m.SceneHash:
		return fmt.Sprintf("tile %d scene hash mismatch", index)
	case t.SamplingVersion != m.SamplingVersion:
		return fmt.Sprintf("tile %d sampling version mismatch", index)
	case t.ParamVersion != m.ParamVersion:
		return fmt.Sprintf("tile %d parameter version mismatch", index)
	case len(t.LinearRGB) != t.TileW*t.TileH*3:
		return fmt.Sprintf("tile %d pixel buffer length mismatch", index)
	case len(t.ObjectIDs) != t.TileW*t.TileH:
		return fmt.Sprintf("tile %d object buffer length mismatch", index)
	case len(t.PeakRadiance) != t.TileW*t.TileH:
		return fmt.Sprintf("tile %d metric buffer length mismatch", index)
	case t.Digest != TileDigest(t.TileW, t.TileH, t.LinearRGB):
		return fmt.Sprintf("tile %d digest mismatch", index)
	case !mapsEqual(t.MaterialKeys, m.MaterialKeys):
		return fmt.Sprintf("tile %d material cache key mismatch", index)
	}
	return ""
}

func (s *Store) CommitTile(tile TileSnapshot) error {
	if tile.TileIndex < 0 || tile.TileIndex >= s.Manifest.TileCount {
		return errs.Para("tile_index", fmt.Sprintf("[0,%d]", s.Manifest.TileCount-1), "tile index out of range")
	}
	if tile.ParamVersion != s.Manifest.ParamVersion {
		return errs.Version(s.Manifest.ParamVersion, tile.ParamVersion)
	}
	if tile.SamplingVersion != s.Manifest.SamplingVersion {
		return errs.Version(s.Manifest.SamplingVersion, tile.SamplingVersion)
	}
	if tile.ContentHash != s.Manifest.ContentHash {
		return errs.Version(s.Manifest.ContentHash, tile.ContentHash)
	}
	if tile.SceneHash != s.Manifest.SceneHash {
		return errs.Version(s.Manifest.SceneHash, tile.SceneHash)
	}
	if !mapsEqual(tile.MaterialKeys, s.Manifest.MaterialKeys) {
		return errs.QMix("tile.material_keys", "tile %d material cache keys do not match quantization-bound manifest keys", tile.TileIndex)
	}
	if tile.Digest != TileDigest(tile.TileW, tile.TileH, tile.LinearRGB) {
		return errs.New(errs.CodeVerify, "tile %d digest does not match its pixel buffer", tile.TileIndex)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	if tile.CommitSeq != tile.TileIndex {
		return errs.New(errs.CodeVerify, "commit sequence %d does not match tile %d", tile.CommitSeq, tile.TileIndex)
	}
	if _, err := os.Stat(tilePath(s.Root, tile.TileIndex)); err == nil {
		return errs.New(errs.CodeVerify, "tile %d was already committed", tile.TileIndex)
	}
	data, err := json.Marshal(tile)
	if err != nil {
		return errs.Decode("tile_checkpoint", "cannot encode tile: %v", err)
	}
	if err := WriteFileAtomic(tilePath(s.Root, tile.TileIndex), data); err != nil {
		return err
	}
	record := CommitRecord{Seq: tile.CommitSeq, Tile: tile.TileIndex, Digest: tile.Digest, ParamVer: tile.ParamVersion}
	line, _ := json.Marshal(record)
	f, err := os.OpenFile(filepath.Join(s.Root, "commits.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return errs.IO("commits.log", err)
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		_ = f.Close()
		return errs.IO("commits.log", err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return errs.IO("commits.log", err)
	}
	if err := f.Close(); err != nil {
		return errs.IO("commits.log", err)
	}
	return nil
}

func (s *Store) WriteStats(stats RunStats) error {
	data, err := json.MarshalIndent(stats, "", "  ")
	if err != nil {
		return errs.Decode("run_stats", "cannot encode: %v", err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return WriteFileAtomic(filepath.Join(s.Root, "run-stats.json"), append(data, '\n'))
}

func (s *Store) ReadStats() (RunStats, error) {
	return ReadStats(s.Root)
}

func ReadStats(root string) (RunStats, error) {
	data, err := os.ReadFile(filepath.Join(root, "run-stats.json"))
	if err != nil {
		return RunStats{}, errs.IO("run-stats.json", err)
	}
	var stats RunStats
	if err := json.Unmarshal(data, &stats); err != nil {
		return stats, errs.Decode("run-stats.json", "invalid JSON: %v", err)
	}
	return stats, nil
}

func ReadManifest(root string) (Manifest, error) {
	m, reason, err := loadManifest(filepath.Join(root, "manifest.json"))
	if err != nil {
		return m, err
	}
	if reason != "" {
		return m, errs.Decode("manifest", "%s", reason)
	}
	return m, nil
}

func ReadTiles(root string, manifest Manifest) ([]TileSnapshot, error) {
	out := make([]TileSnapshot, 0, manifest.TileCount)
	for i := 0; i < manifest.TileCount; i++ {
		data, err := os.ReadFile(tilePath(root, i))
		if err != nil {
			return nil, errs.Decode("tile_checkpoint", "cannot read tile %d: %v", i, err)
		}
		var tile TileSnapshot
		if err := json.Unmarshal(data, &tile); err != nil {
			return nil, errs.Decode("tile_checkpoint", "tile %d is corrupt: %v", i, err)
		}
		if reason := validateLoadedTile(tile, i, manifest); reason != "" {
			return nil, errs.Decode("tile_checkpoint", "%s", reason)
		}
		out = append(out, tile)
	}
	return out, nil
}

func ReadCommits(root string) ([]CommitRecord, error) {
	f, err := os.Open(filepath.Join(root, "commits.log"))
	if err != nil {
		return nil, errs.IO("commits.log", err)
	}
	defer f.Close()
	var out []CommitRecord
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		var rec CommitRecord
		if err := json.Unmarshal(scanner.Bytes(), &rec); err != nil {
			return nil, errs.Decode("commits.log", "invalid commit record: %v", err)
		}
		out = append(out, rec)
	}
	if err := scanner.Err(); err != nil {
		return nil, errs.IO("commits.log", err)
	}
	return out, nil
}

func WriteFileAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return errs.IO(filepath.Base(path), err)
	}
	tmp := fmt.Sprintf("%s.tmp.%d.%d", path, os.Getpid(), atomic.AddUint64(&atomicWriteCounter, 1))
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return errs.IO(filepath.Base(path), err)
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return errs.IO(filepath.Base(path), err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmp)
		return errs.IO(filepath.Base(path), err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(tmp)
		return errs.IO(filepath.Base(path), err)
	}
	if err := os.Rename(tmp, path); err != nil {
		// Windows cannot rename over an existing file. These writes are either
		// first-time tile writes or final small metadata writes; the fallback
		// keeps a complete temp file until the replacement succeeds.
		if removeErr := os.Remove(path); removeErr == nil {
			if retryErr := os.Rename(tmp, path); retryErr == nil {
				return nil
			}
		}
		_ = os.Remove(tmp)
		return errs.IO(filepath.Base(path), err)
	}
	return nil
}

func WriteJSONAtomic(path string, v any) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return errs.Decode(filepath.Base(path), "cannot encode JSON: %v", err)
	}
	return WriteFileAtomic(path, append(data, '\n'))
}

func TileDigest(w, h int, pixels []float64) string {
	hash := sha256.New()
	var header [16]byte
	binary.LittleEndian.PutUint64(header[0:8], uint64(w))
	binary.LittleEndian.PutUint64(header[8:16], uint64(h))
	_, _ = hash.Write(header[:])
	var buf [8]byte
	for _, v := range pixels {
		binary.LittleEndian.PutUint64(buf[:], math.Float64bits(v))
		_, _ = hash.Write(buf[:])
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func tilePath(root string, index int) string {
	return filepath.Join(root, "tiles", fmt.Sprintf("%06d.json", index))
}

func manifestsEqual(a, b Manifest) bool {
	if a.ContentHash != b.ContentHash ||
		a.SceneHash != b.SceneHash ||
		a.SpectralHash != b.SpectralHash ||
		a.SamplingVersion != b.SamplingVersion ||
		a.ParamVersion != b.ParamVersion ||
		a.Width != b.Width ||
		a.Height != b.Height ||
		a.TileSize != b.TileSize ||
		a.TileCount != b.TileCount ||
		!mapsEqual(a.MaterialKeys, b.MaterialKeys) {
		return false
	}
	return a.ConfigVersion() == b.ConfigVersion()
}

func (m Manifest) ConfigVersion() string {
	return domain.ConfigVersion(m.Config)
}

func mapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	keys := make([]string, 0, len(a))
	for k := range a {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if b[k] != a[k] {
			return false
		}
	}
	return true
}

var atomicWriteCounter uint64
