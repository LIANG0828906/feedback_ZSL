package fog

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

const checkpointMagic = "vfog-checkpoint-v1"

type PixelValue struct {
	X             int     `json:"x"`
	Y             int     `json:"y"`
	Radiance      float64 `json:"radiance"`
	Transmittance float64 `json:"transmittance"`
	RayDigest     string  `json:"ray_digest"`
	CacheKey      string  `json:"cache_key"`
	Missing       bool    `json:"missing,omitempty"`
}

type TileResult struct {
	ID               string            `json:"id"`
	X                int               `json:"x"`
	Y                int               `json:"y"`
	W                int               `json:"w"`
	H                int               `json:"h"`
	Pixels           []PixelValue      `json:"pixels"`
	Rays             []RayResult       `json:"rays"`
	SegmentDigest    string            `json:"segment_digest"`
	CacheKey         string            `json:"cache_key"`
	ContentHash      string            `json:"content_hash"`
	MediaHashes      map[string]string `json:"media_hashes"`
	DependencyHashes map[string]string `json:"dependency_hashes"`
	CameraVersion    string            `json:"camera_version"`
	RecipeVersion    string            `json:"recipe_version"`
	AlgorithmVersion string            `json:"algorithm_version"`
	StructureVersion string            `json:"structure_version"`
	Profile          string            `json:"profile"`
	Steps            int               `json:"steps"`
	ShadowSamples    int               `json:"shadow_samples"`
	RayBufferPeak    int               `json:"ray_buffer_peak,omitempty"`
	QueueEvents      []QueueEvent      `json:"queue_events,omitempty"`
	CommitSeq        int64             `json:"commit_seq,omitempty"`
}

func (t *TileResult) Finalize() {
	sort.Slice(t.Pixels, func(i, j int) bool {
		if t.Pixels[i].Y != t.Pixels[j].Y {
			return t.Pixels[i].Y < t.Pixels[j].Y
		}
		return t.Pixels[i].X < t.Pixels[j].X
	})
	sort.Slice(t.Rays, func(i, j int) bool {
		if t.Rays[i].Y != t.Rays[j].Y {
			return t.Rays[i].Y < t.Rays[j].Y
		}
		return t.Rays[i].X < t.Rays[j].X
	})
	if t.MediaHashes == nil {
		t.MediaHashes = map[string]string{}
	}
	if t.DependencyHashes == nil {
		t.DependencyHashes = map[string]string{}
	}
	t.SegmentDigest = hashStrings(tileRayDigests(t.Rays)...)
	t.CacheKey = tileCacheKey(t)
	t.ContentHash = tileContentHash(t)
}

func tileRayDigests(rays []RayResult) []string {
	out := make([]string, 0, len(rays)+2)
	for _, r := range rays {
		out = append(out, r.RayID, r.Digest, r.CacheKey)
	}
	return out
}

func tileCacheKey(t *TileResult) string {
	ids := make([]string, 0, len(t.MediaHashes))
	for id := range t.MediaHashes {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	parts := []string{
		"tile", t.ID, fmt.Sprintf("%d:%d:%d:%d", t.X, t.Y, t.W, t.H),
		t.CameraVersion, t.RecipeVersion, t.AlgorithmVersion, t.Profile,
		t.StructureVersion,
	}
	for _, id := range ids {
		parts = append(parts, id, t.MediaHashes[id])
	}
	depIDs := make([]string, 0, len(t.DependencyHashes))
	for id := range t.DependencyHashes {
		depIDs = append(depIDs, id)
	}
	sort.Strings(depIDs)
	for _, id := range depIDs {
		parts = append(parts, id, t.DependencyHashes[id])
	}
	for _, r := range t.Rays {
		parts = append(parts, r.RayID, r.CacheKey)
	}
	return hashStrings(parts...)
}

func tileContentHash(t *TileResult) string {
	type persisted struct {
		ID               string            `json:"id"`
		XYWH             [4]int            `json:"xywh"`
		Pixels           []PixelValue      `json:"pixels"`
		Rays             []RayResult       `json:"rays"`
		MediaHashes      map[string]string `json:"media_hashes"`
		DependencyHashes map[string]string `json:"dependency_hashes"`
		CameraVersion    string            `json:"camera_version"`
		RecipeVersion    string            `json:"recipe_version"`
		AlgorithmVersion string            `json:"algorithm_version"`
		StructureVersion string            `json:"structure_version"`
		Profile          string            `json:"profile"`
		Steps            int               `json:"steps"`
		ShadowSamples    int               `json:"shadow_samples"`
		RayBufferPeak    int               `json:"ray_buffer_peak,omitempty"`
	}
	p := persisted{
		ID: t.ID, XYWH: [4]int{t.X, t.Y, t.W, t.H}, Pixels: t.Pixels, Rays: t.Rays,
		MediaHashes: t.MediaHashes, CameraVersion: t.CameraVersion,
		DependencyHashes: t.DependencyHashes,
		RecipeVersion:    t.RecipeVersion, AlgorithmVersion: t.AlgorithmVersion,
		StructureVersion: t.StructureVersion,
		Profile:          t.Profile, Steps: t.Steps, ShadowSamples: t.ShadowSamples,
		RayBufferPeak: t.RayBufferPeak,
	}
	b, _ := json.Marshal(p)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

type journalBody struct {
	Magic    string      `json:"magic"`
	Version  int         `json:"version"`
	Txn      int64       `json:"txn"`
	Kind     string      `json:"kind"`
	PrevHash string      `json:"prev_hash"`
	Tile     *TileResult `json:"tile,omitempty"`
	Invalid  []string    `json:"invalidate,omitempty"`
	Reason   string      `json:"reason,omitempty"`
}

type journalEntry struct {
	journalBody
	Checksum string `json:"checksum"`
}

func (b journalBody) checksum() string {
	raw, _ := json.Marshal(b)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

type CheckpointDiagnostics struct {
	Path          string   `json:"path"`
	Entries       int64    `json:"entries"`
	Tiles         int      `json:"tiles"`
	CorruptLines  int      `json:"corrupt_lines"`
	ChainBreaks   int      `json:"chain_breaks"`
	TruncatedTail bool     `json:"truncated_tail"`
	LastError     string   `json:"last_error,omitempty"`
	CommitOrder   []string `json:"commit_order"`
}

type CheckpointStore struct {
	mu     sync.Mutex
	path   string
	file   *os.File
	nextTx int64
	prev   string
	tiles  map[string]TileResult
	order  []string
	diag   CheckpointDiagnostics
}

func OpenCheckpointStore(path string) (*CheckpointStore, error) {
	if path == "" {
		return nil, errors.New("empty checkpoint path")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	s := &CheckpointStore{
		path:  path,
		tiles: map[string]TileResult{},
		diag:  CheckpointDiagnostics{Path: path},
	}
	if err := s.replay(); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	s.file = f
	return s, nil
}

func (s *CheckpointStore) replay() error {
	f, err := os.Open(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer f.Close()
	br := bufio.NewReaderSize(f, 64*1024)
	var prev string
	var validEnd int64
	for {
		line, readErr := br.ReadBytes('\n')
		if len(line) == 0 && readErr == io.EOF {
			break
		}
		if readErr == io.EOF {
			// A process can die between write and newline. The tail is not a
			// committed transaction and must not be replayed.
			if len(line) > 0 {
				s.diag.TruncatedTail = true
				if err := os.Truncate(s.path, validEnd); err != nil {
					return err
				}
			}
			break
		}
		if readErr != nil {
			return readErr
		}
		validEnd += int64(len(line))
		line = line[:len(line)-1]
		if len(line) == 0 {
			continue
		}
		var e journalEntry
		if err := json.Unmarshal(line, &e); err != nil {
			s.diag.CorruptLines++
			s.diag.LastError = "decode: " + err.Error()
			continue
		}
		want := e.journalBody.checksum()
		if e.Magic != checkpointMagic || e.Checksum != want {
			s.diag.CorruptLines++
			s.diag.LastError = fmt.Sprintf("checksum mismatch txn=%d", e.Txn)
			if e.Tile != nil && e.Tile.ID != "" {
				delete(s.tiles, e.Tile.ID)
			}
			continue
		}
		if prev != "" && e.PrevHash != prev {
			s.diag.ChainBreaks++
			s.diag.LastError = fmt.Sprintf("chain break txn=%d", e.Txn)
		}
		prev = e.Checksum
		s.diag.Entries++
		if e.Txn >= s.nextTx {
			s.nextTx = e.Txn + 1
		}
		switch e.Kind {
		case "tile":
			if e.Tile == nil {
				s.diag.CorruptLines++
				continue
			}
			t := *e.Tile
			if t.ContentHash != tileContentHash(&t) {
				s.diag.CorruptLines++
				s.diag.LastError = "tile content hash mismatch: " + t.ID
				delete(s.tiles, t.ID)
				continue
			}
			t.CommitSeq = e.Txn
			s.tiles[t.ID] = t
			s.rememberOrder(t.ID)
		case "invalidate":
			for _, id := range e.Invalid {
				delete(s.tiles, id)
			}
		default:
			s.diag.CorruptLines++
			s.diag.LastError = "unknown transaction kind: " + e.Kind
		}
	}
	s.prev = prev
	s.refreshOrder()
	return nil
}

func (s *CheckpointStore) rememberOrder(id string) {
	for _, existing := range s.order {
		if existing == id {
			return
		}
	}
	s.order = append(s.order, id)
}

func (s *CheckpointStore) refreshOrder() {
	ids := make([]string, 0, len(s.tiles))
	for id := range s.tiles {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		a, b := s.tiles[ids[i]], s.tiles[ids[j]]
		if a.CommitSeq != b.CommitSeq {
			return a.CommitSeq < b.CommitSeq
		}
		return ids[i] < ids[j]
	})
	s.order = ids
	s.diag.Tiles = len(ids)
	s.diag.CommitOrder = append([]string(nil), ids...)
}

func (s *CheckpointStore) commit(kind string, tile *TileResult, invalid []string, reason string) (int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.file == nil {
		return 0, errors.New("checkpoint store closed")
	}
	txn := s.nextTx
	if txn == 0 {
		txn = 1
	}
	body := journalBody{
		Magic: checkpointMagic, Version: 1, Txn: txn, Kind: kind,
		PrevHash: s.prev, Tile: tile, Invalid: invalid, Reason: reason,
	}
	e := journalEntry{journalBody: body, Checksum: body.checksum()}
	raw, err := json.Marshal(e)
	if err != nil {
		return 0, err
	}
	raw = append(raw, '\n')
	n, err := s.file.Write(raw)
	if err != nil {
		return 0, err
	}
	if n != len(raw) {
		return 0, io.ErrShortWrite
	}
	if err := s.file.Sync(); err != nil {
		return 0, err
	}
	s.nextTx = txn + 1
	s.prev = e.Checksum
	s.diag.Entries++
	switch kind {
	case "tile":
		if tile != nil {
			t := *tile
			t.CommitSeq = txn
			s.tiles[t.ID] = t
			s.rememberOrder(t.ID)
		}
	case "invalidate":
		for _, id := range invalid {
			delete(s.tiles, id)
		}
	}
	s.refreshOrder()
	return txn, nil
}

func (s *CheckpointStore) CommitTile(t TileResult) (int64, error) {
	if t.ID == "" {
		return 0, errors.New("tile id is required")
	}
	t.Finalize()
	return s.commit("tile", &t, nil, "")
}

func (s *CheckpointStore) CommitTilesStable(tiles []TileResult) (map[string]int64, error) {
	cp := append([]TileResult(nil), tiles...)
	sort.Slice(cp, func(i, j int) bool { return cp[i].ID < cp[j].ID })
	out := map[string]int64{}
	for _, t := range cp {
		seq, err := s.CommitTile(t)
		if err != nil {
			return out, err
		}
		out[t.ID] = seq
	}
	return out, nil
}

func (s *CheckpointStore) InvalidateTiles(ids []string, reason string) (int64, error) {
	cp := append([]string(nil), ids...)
	sort.Strings(cp)
	return s.commit("invalidate", nil, cp, reason)
}

func (s *CheckpointStore) Tile(id string) (TileResult, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t, ok := s.tiles[id]
	return t, ok
}

func (s *CheckpointStore) LoadFor(scene *Scene, profile string) (map[string]TileResult, []*Fault, CheckpointDiagnostics) {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]TileResult{}
	var faults []*Fault
	alg := scene.Recipe.AlgorithmVersion
	recipeHash := scene.RecipeHash()
	camHash := scene.CameraHash()
	for id, t := range s.tiles {
		f := validateLoadedTile(scene, profile, alg, recipeHash, camHash, t)
		if f != nil {
			faults = append(faults, f)
			delete(s.tiles, id)
			continue
		}
		out[id] = t
	}
	s.refreshOrder()
	diag := s.diag
	diag.CommitOrder = append([]string(nil), s.order...)
	return out, faults, diag
}

func validateLoadedTile(scene *Scene, profile, alg, recipeHash, camHash string, t TileResult) *Fault {
	if t.ContentHash != tileContentHash(&t) {
		return &Fault{Code: ECache, TileID: t.ID, Path: "checkpoint.content_hash", Actual: t.ContentHash, Expected: "recomputed", Message: "content hash mismatch; tile invalidated"}
	}
	structureHash := scene.StructureHash()
	if t.Profile != profile || t.AlgorithmVersion != alg || t.RecipeVersion != recipeHash || t.CameraVersion != camHash || t.StructureVersion != structureHash {
		return &Fault{Code: ECache, TileID: t.ID, Path: "checkpoint.version", Actual: []string{t.Profile, t.AlgorithmVersion, t.RecipeVersion, t.CameraVersion, t.StructureVersion}, Expected: []string{profile, alg, recipeHash, camHash, structureHash}, Message: "version mismatch; tile invalidated"}
	}
	for id, oldHash := range t.MediaHashes {
		if _, m := scene.MediaByID(id); m == nil {
			return &Fault{Code: ECache, TileID: t.ID, MediaID: id, Path: "checkpoint.media_hashes", Actual: "missing", Expected: oldHash, Message: "media dependency absent; tile invalidated"}
		}
		if cur := MediaDependencyHash(id, t.DependencyHashes); cur != oldHash {
			return &Fault{Code: ECache, TileID: t.ID, MediaID: id, Path: "checkpoint.media_hashes." + id, Actual: cur, Expected: oldHash, Message: "media dependency subset hash mismatch; tile invalidated"}
		}
	}
	for source, oldHash := range t.DependencyHashes {
		cur, ok := SourceContentHash(scene, source)
		if !ok || cur != oldHash {
			mediaID := SourceMediaID(source)
			return &Fault{Code: ECache, TileID: t.ID, MediaID: mediaID, Path: "checkpoint.dependency_hashes." + source, Actual: cur, Expected: oldHash, Message: "dependency content hash mismatch; tile invalidated"}
		}
	}
	if want := tileCacheKey(&t); want != t.CacheKey {
		return &Fault{Code: ECache, TileID: t.ID, Path: "checkpoint.cache_key", Actual: t.CacheKey, Expected: want, Message: "cache key mismatch; tile invalidated"}
	}
	return nil
}

func (s *CheckpointStore) CommitSequence() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]string, 0, len(s.order))
	for _, id := range s.order {
		if _, ok := s.tiles[id]; ok {
			out = append(out, id)
		}
	}
	return out
}

func (s *CheckpointStore) AllTiles() []TileResult {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]TileResult, 0, len(s.order))
	for _, id := range s.order {
		if t, ok := s.tiles[id]; ok {
			out = append(out, t)
		}
	}
	return out
}

func (s *CheckpointStore) Diagnostics() CheckpointDiagnostics {
	s.mu.Lock()
	defer s.mu.Unlock()
	d := s.diag
	d.Tiles = len(s.tiles)
	d.CommitOrder = append([]string(nil), s.order...)
	return d
}

func (s *CheckpointStore) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.file == nil {
		return nil
	}
	err := s.file.Sync()
	if closeErr := s.file.Close(); err == nil {
		err = closeErr
	}
	s.file = nil
	return err
}

// CorruptTileForTest is intentionally exported through testing helpers rather
// than production APIs. It simulates an on-disk content hash mismatch.
func CorruptTileForTest(t *TileResult) { t.ContentHash = "corrupted-" + t.ContentHash }
