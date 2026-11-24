package spectralrender

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

type SnapshotMeta struct {
	SchemaVersion    int          `json:"schema_version"`
	AlgorithmVersion string       `json:"algorithm_version"`
	SceneHash        string       `json:"scene_hash"`
	SamplingVersion  string       `json:"sampling_version"`
	ParamVersion     uint64       `json:"param_version"`
	ParamsDigest     string       `json:"params_digest"`
	MaterialCacheKey string       `json:"material_cache_key"`
	Params           RenderParams `json:"params"`
	Width            int          `json:"width"`
	Height           int          `json:"height"`
	TileSize         int          `json:"tile_size"`
	TilesX           int          `json:"tiles_x"`
	TilesY           int          `json:"tiles_y"`
}

type TileManifestEntry struct {
	Index  int    `json:"index"`
	X      int    `json:"x"`
	Y      int    `json:"y"`
	Width  int    `json:"width"`
	Height int    `json:"height"`
	Digest string `json:"digest"`
	File   string `json:"file"`
}

type SnapshotManifest struct {
	SchemaVersion int                 `json:"schema_version"`
	SnapshotID    string              `json:"snapshot_id"`
	Meta          SnapshotMeta        `json:"meta"`
	Tiles         []TileManifestEntry `json:"tiles"`
	Complete      bool                `json:"complete"`
	ImageFile     string              `json:"image_file,omitempty"`
}

type SnapshotInvalidation struct {
	Reason                  string `json:"reason"`
	PreviousSnapshotID      string `json:"previous_snapshot_id,omitempty"`
	PreviousParamVersion    uint64 `json:"previous_param_version,omitempty"`
	NewParamVersion         uint64 `json:"new_param_version"`
	PreviousSamplingVersion string `json:"previous_sampling_version,omitempty"`
	NewSamplingVersion      string `json:"new_sampling_version"`
	PreviousMaterialKey     string `json:"previous_material_cache_key,omitempty"`
	NewMaterialKey          string `json:"new_material_cache_key"`
}

type ResumeState struct {
	Tiles        map[int]*TileResult
	Invalidation *SnapshotInvalidation
	SnapshotID   string
}

type SnapshotData struct {
	Manifest SnapshotManifest
	Tiles    map[int]*TileResult
	Image    *Frame
}

type currentPointer struct {
	SchemaVersion int    `json:"schema_version"`
	Snapshot      string `json:"snapshot"`
}

type CheckpointStore struct {
	mu         sync.Mutex
	root       string
	meta       SnapshotMeta
	snapshotID string
	dir        string
	manifest   SnapshotManifest
	begun      bool
}

func OpenCheckpointStore(root string) (*CheckpointStore, error) {
	if root == "" {
		return nil, &Error{Code: ErrCheckpoint, Field: "checkpoint_dir", Message: "must not be empty"}
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, &Error{Code: ErrCheckpoint, Field: "checkpoint_dir", Message: "cannot create checkpoint directory", Err: err}
	}
	return &CheckpointStore{root: root}, nil
}

func (s *CheckpointStore) Root() string        { return s.root }
func (s *CheckpointStore) SnapshotID() string  { return s.snapshotID }
func (s *CheckpointStore) SnapshotDir() string { return s.dir }

func paramsDigest(p RenderParams) string {
	b, err := json.Marshal(p)
	if err != nil {
		return ""
	}
	return hashBytes(b)
}

func snapshotIDFor(meta SnapshotMeta) string {
	return "snapshot-" + hashString(
		meta.AlgorithmVersion,
		meta.SceneHash,
		meta.SamplingVersion,
		fmt.Sprint(meta.ParamVersion),
		meta.ParamsDigest,
		meta.MaterialCacheKey,
		fmt.Sprintf("%dx%d/%d", meta.Width, meta.Height, meta.TileSize),
	)[:16]
}

func (s *CheckpointStore) Begin(meta SnapshotMeta) (ResumeState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	meta.ParamsDigest = paramsDigest(meta.Params)
	id := snapshotIDFor(meta)

	var invalidation *SnapshotInvalidation
	ptr, ptrErr := s.readCurrent()
	if ptrErr == nil && ptr.Snapshot != "" {
		oldManifest, oldErr := readManifest(filepath.Join(s.root, ptr.Snapshot, "manifest.json"))
		if oldErr != nil {
			invalidation = &SnapshotInvalidation{
				Reason:             "previous snapshot manifest is unreadable: " + oldErr.Error(),
				PreviousSnapshotID: ptr.Snapshot,
				NewParamVersion:    meta.ParamVersion,
				NewSamplingVersion: meta.SamplingVersion,
				NewMaterialKey:     meta.MaterialCacheKey,
			}
		} else if reason := identityMismatch(oldManifest.Meta, meta); reason != "" {
			invalidation = &SnapshotInvalidation{
				Reason:                  reason,
				PreviousSnapshotID:      ptr.Snapshot,
				PreviousParamVersion:    oldManifest.Meta.ParamVersion,
				NewParamVersion:         meta.ParamVersion,
				PreviousSamplingVersion: oldManifest.Meta.SamplingVersion,
				NewSamplingVersion:      meta.SamplingVersion,
				PreviousMaterialKey:     oldManifest.Meta.MaterialCacheKey,
				NewMaterialKey:          meta.MaterialCacheKey,
			}
		} else {
			tiles, loadErr := loadSnapshotTiles(filepath.Join(s.root, ptr.Snapshot), oldManifest)
			if loadErr != nil {
				invalidation = &SnapshotInvalidation{
					Reason:                  "previous snapshot tile validation failed: " + loadErr.Error(),
					PreviousSnapshotID:      ptr.Snapshot,
					PreviousParamVersion:    oldManifest.Meta.ParamVersion,
					NewParamVersion:         meta.ParamVersion,
					PreviousSamplingVersion: oldManifest.Meta.SamplingVersion,
					NewSamplingVersion:      meta.SamplingVersion,
					PreviousMaterialKey:     oldManifest.Meta.MaterialCacheKey,
					NewMaterialKey:          meta.MaterialCacheKey,
				}
			} else {
				s.meta = meta
				s.snapshotID = ptr.Snapshot
				s.dir = filepath.Join(s.root, ptr.Snapshot)
				s.manifest = *oldManifest
				s.begun = true
				return ResumeState{Tiles: tiles, SnapshotID: ptr.Snapshot}, nil
			}
		}
	}

	// Start a fresh snapshot. Existing directories are never overwritten: if a
	// stale directory has the same identity (for example because one tile was
	// manually corrupted), a generation suffix is used.
	dirName := id
	for n := 1; ; n++ {
		dir := filepath.Join(s.root, dirName)
		if _, err := os.Stat(dir); os.IsNotExist(err) {
			break
		}
		dirName = fmt.Sprintf("%s-r%d", id, n)
	}
	dir := filepath.Join(s.root, dirName)
	if err := os.MkdirAll(filepath.Join(dir, "tiles"), 0o755); err != nil {
		return ResumeState{}, &Error{Code: ErrCheckpoint, Message: "cannot create snapshot directory", Err: err}
	}
	manifest := SnapshotManifest{
		SchemaVersion: 1,
		SnapshotID:    dirName,
		Meta:          meta,
		Tiles:         []TileManifestEntry{},
		Complete:      false,
	}
	if err := writeManifest(filepath.Join(dir, "manifest.json"), manifest); err != nil {
		return ResumeState{}, err
	}
	ptr = currentPointer{SchemaVersion: 1, Snapshot: dirName}
	if err := writeJSONAtomic(filepath.Join(s.root, "current.json"), ptr); err != nil {
		return ResumeState{}, err
	}
	s.meta = meta
	s.snapshotID = dirName
	s.dir = dir
	s.manifest = manifest
	s.begun = true
	return ResumeState{Tiles: map[int]*TileResult{}, Invalidation: invalidation, SnapshotID: dirName}, nil
}

func identityMismatch(old, want SnapshotMeta) string {
	switch {
	case old.SchemaVersion != want.SchemaVersion:
		return "snapshot schema version mismatch"
	case old.AlgorithmVersion != want.AlgorithmVersion:
		return "renderer algorithm version mismatch"
	case old.SceneHash != want.SceneHash:
		return "scene content hash mismatch"
	case old.SamplingVersion != want.SamplingVersion:
		return "spectral sampling version mismatch"
	case old.ParamVersion != want.ParamVersion:
		return fmt.Sprintf("parameter version mismatch (checkpoint %d, request %d)", old.ParamVersion, want.ParamVersion)
	case old.ParamsDigest != want.ParamsDigest:
		return "parameter snapshot digest mismatch"
	case old.MaterialCacheKey != want.MaterialCacheKey:
		return "material cache key mismatch"
	case old.Width != want.Width || old.Height != want.Height || old.TileSize != want.TileSize ||
		old.TilesX != want.TilesX || old.TilesY != want.TilesY:
		return "render geometry/tile layout mismatch"
	default:
		return ""
	}
}

func (s *CheckpointStore) Commit(tile *TileResult) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.begun {
		return &Error{Code: ErrCheckpoint, Message: "checkpoint store has not been begun"}
	}
	if err := validateTileResult(tile); err != nil {
		return err
	}
	if tile.SceneHash != s.meta.SceneHash || tile.SamplingVersion != s.meta.SamplingVersion ||
		tile.ParamVersion != s.meta.ParamVersion || tile.MaterialCacheKey != s.meta.MaterialCacheKey {
		return verError(tile.ParamVersion, s.meta.ParamVersion, "tile.param_version",
			"tile metadata does not match the active snapshot")
	}
	for _, entry := range s.manifest.Tiles {
		if entry.Index != tile.Index {
			continue
		}
		if entry.Digest == tile.Digest {
			return nil
		}
		return &Error{Code: ErrCheckpoint, Tile: tile.Index,
			Message: "refusing to overwrite an already committed tile with different content"}
	}
	fileName := fmt.Sprintf("tiles/tile-%06d.json", tile.Index)
	if err := writeJSONAtomic(filepath.Join(s.dir, filepath.FromSlash(fileName)), tile); err != nil {
		return err
	}
	s.manifest.Tiles = append(s.manifest.Tiles, TileManifestEntry{
		Index: tile.Index, X: tile.X, Y: tile.Y, Width: tile.Width, Height: tile.Height,
		Digest: tile.Digest, File: fileName,
	})
	sort.Slice(s.manifest.Tiles, func(i, j int) bool { return s.manifest.Tiles[i].Index < s.manifest.Tiles[j].Index })
	if err := writeManifest(filepath.Join(s.dir, "manifest.json"), s.manifest); err != nil {
		return err
	}
	return nil
}

func (s *CheckpointStore) WriteFinalImage(frame *Frame) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.begun {
		return &Error{Code: ErrCheckpoint, Message: "checkpoint store has not been begun"}
	}
	expected := s.meta.TilesX * s.meta.TilesY
	if len(s.manifest.Tiles) != expected {
		return &Error{Code: ErrState, Message: fmt.Sprintf("refusing final image: %d/%d tiles committed", len(s.manifest.Tiles), expected)}
	}
	if frame.ParamVersion != s.meta.ParamVersion || frame.SamplingVersion != s.meta.SamplingVersion ||
		frame.MaterialCacheKey != s.meta.MaterialCacheKey || frame.SceneHash != s.meta.SceneHash {
		return verError(frame.ParamVersion, s.meta.ParamVersion, "frame.param_version",
			"final image metadata does not match the active snapshot")
	}
	if err := writeJSONAtomic(filepath.Join(s.dir, "image.json"), frame); err != nil {
		return err
	}
	s.manifest.Complete = true
	s.manifest.ImageFile = "image.json"
	if err := writeManifest(filepath.Join(s.dir, "manifest.json"), s.manifest); err != nil {
		return err
	}
	return nil
}

func (s *CheckpointStore) readCurrent() (currentPointer, error) {
	var ptr currentPointer
	b, err := os.ReadFile(filepath.Join(s.root, "current.json"))
	if err != nil {
		if os.IsNotExist(err) {
			return ptr, nil
		}
		return ptr, &Error{Code: ErrCheckpoint, Message: "cannot read checkpoint pointer", Err: err}
	}
	if err := json.Unmarshal(b, &ptr); err != nil {
		return ptr, &Error{Code: ErrDecode, Path: "current.json", Message: "invalid checkpoint pointer", Err: err}
	}
	return ptr, nil
}

func LoadSnapshot(root string) (*SnapshotData, error) {
	store, err := OpenCheckpointStore(root)
	if err != nil {
		return nil, err
	}
	ptr, err := store.readCurrent()
	if err != nil {
		return nil, err
	}
	if ptr.Snapshot == "" {
		return nil, &Error{Code: ErrState, Message: "no checkpoint snapshot is available"}
	}
	dir := filepath.Join(root, ptr.Snapshot)
	manifest, err := readManifest(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return nil, err
	}
	tiles, err := loadSnapshotTiles(dir, manifest)
	if err != nil {
		return nil, err
	}
	data := &SnapshotData{Manifest: *manifest, Tiles: tiles}
	if manifest.Complete && manifest.ImageFile != "" {
		var frame Frame
		if err := readJSON(filepath.Join(dir, filepath.FromSlash(manifest.ImageFile)), &frame); err != nil {
			return nil, err
		}
		data.Image = &frame
	}
	return data, nil
}

// loadSnapshotForVerify reads a snapshot without rejecting a tile whose
// manifest digest no longer matches. Resume remains strict, while the verifier
// can report the first differing pixel and field instead of failing at load.
func loadSnapshotForVerify(root string) (*SnapshotData, error) {
	store, err := OpenCheckpointStore(root)
	if err != nil {
		return nil, err
	}
	ptr, err := store.readCurrent()
	if err != nil {
		return nil, err
	}
	if ptr.Snapshot == "" {
		return nil, &Error{Code: ErrState, Message: "no checkpoint snapshot is available"}
	}
	dir := filepath.Join(root, ptr.Snapshot)
	manifest, err := readManifest(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return nil, err
	}
	tiles := make(map[int]*TileResult, len(manifest.Tiles))
	for _, entry := range manifest.Tiles {
		if _, exists := tiles[entry.Index]; exists {
			return nil, &Error{Code: ErrCheckpoint, Tile: entry.Index, Message: "duplicate tile index in manifest"}
		}
		var tile TileResult
		if err := readJSON(filepath.Join(dir, filepath.FromSlash(entry.File)), &tile); err != nil {
			return nil, err
		}
		if tile.SceneHash != manifest.Meta.SceneHash ||
			tile.SamplingVersion != manifest.Meta.SamplingVersion ||
			tile.ParamVersion != manifest.Meta.ParamVersion ||
			tile.MaterialCacheKey != manifest.Meta.MaterialCacheKey {
			return nil, &Error{Code: ErrVer, Tile: entry.Index,
				Versions: []uint64{tile.ParamVersion, manifest.Meta.ParamVersion},
				Message:  "tile version does not match snapshot"}
		}
		tiles[entry.Index] = &tile
	}
	data := &SnapshotData{Manifest: *manifest, Tiles: tiles}
	if manifest.Complete && manifest.ImageFile != "" {
		var frame Frame
		if err := readJSON(filepath.Join(dir, filepath.FromSlash(manifest.ImageFile)), &frame); err != nil {
			return nil, err
		}
		data.Image = &frame
	}
	return data, nil
}

func loadSnapshotTiles(dir string, manifest *SnapshotManifest) (map[int]*TileResult, error) {
	tiles := make(map[int]*TileResult, len(manifest.Tiles))
	for _, entry := range manifest.Tiles {
		if _, exists := tiles[entry.Index]; exists {
			return nil, &Error{Code: ErrCheckpoint, Tile: entry.Index, Message: "duplicate tile index in manifest"}
		}
		var tile TileResult
		if err := readJSON(filepath.Join(dir, filepath.FromSlash(entry.File)), &tile); err != nil {
			return nil, err
		}
		if tile.Index != entry.Index || tile.Digest != entry.Digest {
			return nil, &Error{Code: ErrCheckpoint, Tile: entry.Index, Message: "tile manifest/content mismatch"}
		}
		if tile.X != entry.X || tile.Y != entry.Y || tile.Width != entry.Width || tile.Height != entry.Height {
			return nil, &Error{Code: ErrCheckpoint, Tile: entry.Index, Message: "tile geometry does not match manifest"}
		}
		if tile.SceneHash != manifest.Meta.SceneHash ||
			tile.SamplingVersion != manifest.Meta.SamplingVersion ||
			tile.ParamVersion != manifest.Meta.ParamVersion ||
			tile.MaterialCacheKey != manifest.Meta.MaterialCacheKey {
			return nil, &Error{Code: ErrVer, Tile: entry.Index,
				Versions: []uint64{tile.ParamVersion, manifest.Meta.ParamVersion},
				Message:  "tile version does not match snapshot"}
		}
		if err := validateTileResult(&tile); err != nil {
			return nil, err
		}
		tiles[entry.Index] = &tile
	}
	return tiles, nil
}

func readManifest(path string) (*SnapshotManifest, error) {
	var manifest SnapshotManifest
	if err := readJSON(path, &manifest); err != nil {
		return nil, err
	}
	if manifest.SchemaVersion != 1 {
		return nil, &Error{Code: ErrCheckpoint, Path: path, Message: "unsupported checkpoint schema"}
	}
	return &manifest, nil
}

func readJSON(path string, dst any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &Error{Code: ErrDecode, Path: path, Message: "checkpoint file is missing", Err: err}
		}
		return &Error{Code: ErrDecode, Path: path, Message: "cannot read checkpoint file", Err: err}
	}
	if err := json.Unmarshal(b, dst); err != nil {
		return &Error{Code: ErrDecode, Path: path, Message: "invalid checkpoint JSON", Err: err}
	}
	return nil
}

func writeManifest(path string, manifest SnapshotManifest) error {
	return writeJSONAtomic(path, manifest)
}

func writeJSONAtomic(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return &Error{Code: ErrCheckpoint, Path: path, Message: "cannot encode checkpoint JSON", Err: err}
	}
	b = append(b, '\n')
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return &Error{Code: ErrCheckpoint, Path: path, Message: "cannot create checkpoint parent directory", Err: err}
	}
	tmp, err := os.CreateTemp(dir, ".tmp-checkpoint-*")
	if err != nil {
		return &Error{Code: ErrCheckpoint, Path: path, Message: "cannot create checkpoint temp file", Err: err}
	}
	tmpName := tmp.Name()
	ok := false
	defer func() {
		tmp.Close()
		if !ok {
			os.Remove(tmpName)
		}
	}()
	if _, err := tmp.Write(b); err != nil {
		return &Error{Code: ErrCheckpoint, Path: path, Message: "cannot write checkpoint temp file", Err: err}
	}
	if err := tmp.Sync(); err != nil {
		return &Error{Code: ErrCheckpoint, Path: path, Message: "cannot sync checkpoint temp file", Err: err}
	}
	if err := tmp.Chmod(0o644); err != nil {
		return &Error{Code: ErrCheckpoint, Path: path, Message: "cannot set checkpoint permissions", Err: err}
	}
	if err := tmp.Close(); err != nil {
		return &Error{Code: ErrCheckpoint, Path: path, Message: "cannot close checkpoint temp file", Err: err}
	}
	if err := os.Rename(tmpName, path); err != nil {
		return &Error{Code: ErrCheckpoint, Path: path, Message: "cannot atomically replace checkpoint file", Err: err}
	}
	ok = true
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}
