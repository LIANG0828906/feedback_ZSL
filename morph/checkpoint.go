package morph

import (
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

type PathCheckpoint struct {
	PathID             string         `json:"path_id"`
	Points             []Vec2         `json:"points"`
	GeometryBBox       BBox           `json:"geometry_bbox"`
	StrokeBBox         BBox           `json:"stroke_bbox"`
	ContourArea        float64        `json:"contour_area"`
	PolylineLength     float64        `json:"polyline_length"`
	SegmentCount       int            `json:"segment_count"`
	InterpolationCount int            `json:"interpolation_count"`
	Stats              PathFrameStats `json:"stats"`
}

type SampleCheckpoint struct {
	Format         string           `json:"format"`
	SceneID        string           `json:"scene_id"`
	SceneHash      string           `json:"scene_hash"`
	Algorithm      string           `json:"algorithm"`
	Quality        Quality          `json:"quality"`
	QualityVersion string           `json:"quality_version"`
	ConfigVersion  string           `json:"config_version"`
	BlendVersion   string           `json:"blend_version"`
	Index          int              `json:"index"`
	Time           float64          `json:"time"`
	Width          int              `json:"width"`
	Height         int              `json:"height"`
	Frame          FrameGeometry    `json:"frame"`
	Paths          []PathCheckpoint `json:"paths"`
	PathStats      []PathFrameStats `json:"path_stats"`
	PixelHash      string           `json:"pixel_hash"`
	Gray           []uint8          `json:"gray"`
	ContentHash    string           `json:"content_hash"`
}

type ManifestEntry struct {
	Index       int     `json:"index"`
	Time        float64 `json:"time"`
	ContentHash string  `json:"content_hash"`
	PixelHash   string  `json:"pixel_hash"`
}

type CheckpointManifest struct {
	Format       string          `json:"format"`
	SceneID      string          `json:"scene_id"`
	SceneHash    string          `json:"scene_hash"`
	Algorithm    string          `json:"algorithm"`
	StyleVersion string          `json:"style_version"`
	Samples      []ManifestEntry `json:"samples"`
}

type CheckpointStore struct {
	mu           sync.Mutex
	dir          string
	sceneID      string
	sceneHash    string
	styleVersion string
	algorithm    string
	entries      []ManifestEntry
}

func SceneHash(scene *Scene) string {
	data, _ := json.Marshal(scene)
	return HashBytes(data)
}

func OpenCheckpoint(dir string, scene *Scene) (*CheckpointStore, error) {
	if dir == "" {
		return nil, withField(newError(CodeParam, "checkpoint directory is required"), "checkpoint")
	}
	st := &CheckpointStore{
		dir: dir, sceneID: scene.ID, sceneHash: SceneHash(scene),
		styleVersion: SceneStyleVersion(scene), algorithm: AlgorithmVersion,
	}
	if err := os.MkdirAll(filepath.Join(dir, "samples"), 0o755); err != nil {
		e := newError(CodeDecode, "cannot create checkpoint directory")
		e.Cause = err
		return nil, e
	}
	manifestPath := filepath.Join(dir, "manifest.json")
	data, err := os.ReadFile(manifestPath)
	if err != nil {
		if !os.IsNotExist(err) {
			e := newError(CodeDecode, "cannot read checkpoint manifest")
			e.Cause = err
			return nil, e
		}
		if err := st.rebuildManifest(); err != nil {
			return nil, err
		}
		if err := st.persistManifestLocked(); err != nil {
			return nil, err
		}
		return st, nil
	}
	var m CheckpointManifest
	if err := json.Unmarshal(data, &m); err != nil {
		e := newError(CodeDecode, "corrupt checkpoint manifest")
		e.Cause = err
		return nil, e
	}
	if m.Format != CheckpointFormat || m.SceneID != scene.ID || m.SceneHash != st.sceneHash ||
		m.Algorithm != AlgorithmVersion || m.StyleVersion != st.styleVersion {
		e := withField(newError(CodeDiverge, "checkpoint version is inconsistent with the scene"), "checkpoint")
		e.Measured = m.SceneHash + "/" + m.Algorithm
		e.Expected = st.sceneHash + "/" + AlgorithmVersion
		return nil, e
	}
	for _, en := range m.Samples {
		if en.Index != len(st.entries) {
			e := withField(newError(CodeDiverge, "checkpoint manifest has a non-contiguous prefix"), "checkpoint.samples")
			e.Sample = en.Index
			return nil, e
		}
		st.entries = append(st.entries, en)
	}
	return st, nil
}

func (st *CheckpointStore) rebuildManifest() error {
	files, err := os.ReadDir(filepath.Join(st.dir, "samples"))
	if err != nil {
		e := newError(CodeDecode, "cannot scan checkpoint samples")
		e.Cause = err
		return e
	}
	var names []string
	for _, f := range files {
		if !f.IsDir() && (filepath.Ext(f.Name()) == ".json" || filepath.Ext(f.Name()) == ".gz") {
			names = append(names, f.Name())
		}
	}
	sort.Strings(names)
	for i, name := range names {
		cp, err := readCheckpointFile(filepath.Join(st.dir, "samples", name))
		if err != nil {
			e := newError(CodeDecode, "corrupt checkpoint sample")
			e.Cause = err
			return e
		}
		if cp.Index != i {
			break
		}
		if err := validateCheckpoint(st, cp); err != nil {
			return err
		}
		st.entries = append(st.entries, ManifestEntry{
			Index: cp.Index, Time: cp.Time, ContentHash: cp.ContentHash, PixelHash: cp.PixelHash,
		})
	}
	return nil
}

func validateCheckpoint(st *CheckpointStore, cp *SampleCheckpoint) error {
	if cp.Format != CheckpointFormat {
		return withField(newError(CodeDecode, "unsupported checkpoint format"), "checkpoint.format")
	}
	if cp.SceneHash != st.sceneHash || cp.SceneID != st.sceneID {
		return withField(newError(CodeDiverge, "checkpoint belongs to a different scene"), "checkpoint.scene_hash")
	}
	if cp.Algorithm != st.algorithm {
		return withField(newError(CodeDiverge, "checkpoint algorithm version differs"), "checkpoint.algorithm")
	}
	if cp.BlendVersion != st.styleVersion {
		return withField(newError(CodeDiverge, "checkpoint blend/style version differs"), "checkpoint.blend_version")
	}
	if cp.ContentHash != checkpointContentHash(cp) {
		return withField(newError(CodeDecode, "checkpoint content hash mismatch"), "checkpoint.content_hash")
	}
	if cp.PixelHash != HashBytes(cp.Gray) {
		return withField(newError(CodeDecode, "checkpoint pixel hash mismatch"), "checkpoint.pixel_hash")
	}
	if len(cp.Gray) != cp.Width*cp.Height {
		return withField(newError(CodeDecode, "checkpoint bitmap length is inconsistent"), "checkpoint.gray")
	}
	return nil
}

func checkpointContentHash(cp *SampleCheckpoint) string {
	type payload struct {
		Format         string
		SceneID        string
		SceneHash      string
		Algorithm      string
		Quality        Quality
		QualityVersion string
		ConfigVersion  string
		BlendVersion   string
		Index          int
		Time           float64
		Width          int
		Height         int
		Frame          FrameGeometry
		Paths          []PathCheckpoint
		PathStats      []PathFrameStats
		PixelHash      string
		Gray           []uint8
	}
	p := payload{
		cp.Format, cp.SceneID, cp.SceneHash, cp.Algorithm, cp.Quality, cp.QualityVersion,
		cp.ConfigVersion, cp.BlendVersion, cp.Index, cp.Time, cp.Width, cp.Height,
		cp.Frame, cp.Paths, cp.PathStats, cp.PixelHash, cp.Gray,
	}
	data, _ := json.Marshal(p)
	return HashBytes(data)
}

func (st *CheckpointStore) CommitSample(res *SampleResult) error {
	st.mu.Lock()
	defer st.mu.Unlock()
	if res.Index != len(st.entries) {
		e := withField(newError(CodeState, "checkpoint commits must be contiguous and ordered"), "checkpoint.sample")
		e.Sample = res.Index
		return e
	}
	cp := &SampleCheckpoint{
		Format: CheckpointFormat, SceneID: st.sceneID, SceneHash: st.sceneHash,
		Algorithm: AlgorithmVersion, Quality: res.Quality, QualityVersion: res.Quality.Version(),
		ConfigVersion: res.ConfigVersion,
		BlendVersion:  st.styleVersion, Index: res.Index, Time: res.Time,
		Width: res.Width, Height: res.Height, Frame: res.Frame,
		PathStats: res.PathStats, PixelHash: HashBytes(res.Gray), Gray: append([]uint8(nil), res.Gray...),
	}
	for _, flat := range res.Flattened {
		stats := findStats(res.PathStats, flat.PathID)
		cp.Paths = append(cp.Paths, PathCheckpoint{
			PathID: flat.PathID, Points: append([]Vec2(nil), flat.Points...),
			GeometryBBox: flat.GeometryBBox, StrokeBBox: flat.StrokeBBox,
			ContourArea: flat.ContourArea, PolylineLength: flat.PolylineLength,
			SegmentCount: flat.SegmentCount, InterpolationCount: flat.InterpolationCount,
			Stats: stats,
		})
	}
	cp.ContentHash = checkpointContentHash(cp)
	if err := st.writeCheckpointLocked(cp); err != nil {
		return err
	}
	st.entries = append(st.entries, ManifestEntry{
		Index: cp.Index, Time: cp.Time, ContentHash: cp.ContentHash, PixelHash: cp.PixelHash,
	})
	return st.persistManifestLocked()
}

func findStats(stats []PathFrameStats, id string) PathFrameStats {
	for _, s := range stats {
		if s.PathID == id {
			return s
		}
	}
	return PathFrameStats{PathID: id}
}

func (st *CheckpointStore) writeCheckpointLocked(cp *SampleCheckpoint) error {
	path := filepath.Join(st.dir, "samples", fmt.Sprintf("%08d.json.gz", cp.Index))
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	zw := gzip.NewWriter(f)
	enc := json.NewEncoder(zw)
	if err := enc.Encode(cp); err != nil {
		zw.Close()
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := zw.Close(); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return syncDir(filepath.Join(st.dir, "samples"))
}

func (st *CheckpointStore) persistManifestLocked() error {
	m := CheckpointManifest{
		Format: CheckpointFormat, SceneID: st.sceneID, SceneHash: st.sceneHash,
		Algorithm: st.algorithm, StyleVersion: st.styleVersion,
		Samples: append([]ManifestEntry(nil), st.entries...),
	}
	if err := WriteJSONAtomic(filepath.Join(st.dir, "manifest.json"), m); err != nil {
		return err
	}
	return syncDir(st.dir)
}

func syncDir(dir string) error {
	f, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

func (st *CheckpointStore) LoadCommitted() ([]*SampleResult, error) {
	st.mu.Lock()
	defer st.mu.Unlock()
	out := make([]*SampleResult, 0, len(st.entries))
	for _, en := range st.entries {
		cp, err := st.readCheckpointLocked(en.Index)
		if err != nil {
			return nil, err
		}
		res, err := cp.toResult()
		if err != nil {
			return nil, err
		}
		res.FromCheckpoint = true
		out = append(out, res)
	}
	return out, nil
}

func (st *CheckpointStore) readCheckpointLocked(index int) (*SampleCheckpoint, error) {
	path := filepath.Join(st.dir, "samples", fmt.Sprintf("%08d.json.gz", index))
	cp, err := readCheckpointFile(path)
	if os.IsNotExist(err) {
		path = filepath.Join(st.dir, "samples", fmt.Sprintf("%08d.json", index))
		cp, err = readCheckpointFile(path)
	}
	if err != nil {
		e := newError(CodeDecode, "cannot read checkpoint sample")
		e.Sample = index
		e.Cause = err
		return nil, e
	}
	if err := validateCheckpoint(st, cp); err != nil {
		if e, ok := AsError(err); ok {
			e.Sample = index
		}
		return nil, err
	}
	return cp, nil
}

func readCheckpointFile(path string) (*SampleCheckpoint, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var reader io.Reader = f
	if filepath.Ext(path) == ".gz" {
		zr, err := gzip.NewReader(f)
		if err != nil {
			return nil, err
		}
		defer zr.Close()
		reader = zr
	}
	var cp SampleCheckpoint
	if err := json.NewDecoder(reader).Decode(&cp); err != nil {
		return nil, err
	}
	return &cp, nil
}

func (cp *SampleCheckpoint) toResult() (*SampleResult, error) {
	if len(cp.Gray) != cp.Width*cp.Height {
		return nil, withField(newError(CodeDecode, "checkpoint bitmap is corrupt"), "checkpoint.gray")
	}
	res := &SampleResult{
		Index: cp.Index, Time: cp.Time, Width: cp.Width, Height: cp.Height,
		Frame: cp.Frame, Quality: cp.Quality, Gray: append([]uint8(nil), cp.Gray...),
		PathStats: append([]PathFrameStats(nil), cp.PathStats...), PixelHash: cp.PixelHash,
		ContentHash: cp.ContentHash, Mode: cp.Quality.Mode, Algorithm: cp.Algorithm,
		ConfigVersion: cp.ConfigVersion, StyleVersion: cp.BlendVersion,
	}
	for _, pc := range cp.Paths {
		res.Flattened = append(res.Flattened, &FlattenedPath{
			PathID: pc.PathID, Time: cp.Time, Points: append([]Vec2(nil), pc.Points...),
			GeometryBBox: pc.GeometryBBox, StrokeBBox: pc.StrokeBBox,
			ContourArea: pc.ContourArea, PolylineLength: pc.PolylineLength,
			SegmentCount: pc.SegmentCount, InterpolationCount: pc.InterpolationCount,
		})
	}
	return res, nil
}

func (st *CheckpointStore) CommittedCount() int {
	st.mu.Lock()
	defer st.mu.Unlock()
	return len(st.entries)
}

func (st *CheckpointStore) Manifest() CheckpointManifest {
	st.mu.Lock()
	defer st.mu.Unlock()
	return CheckpointManifest{
		Format: CheckpointFormat, SceneID: st.sceneID, SceneHash: st.sceneHash,
		Algorithm: st.algorithm, StyleVersion: st.styleVersion,
		Samples: append([]ManifestEntry(nil), st.entries...),
	}
}
