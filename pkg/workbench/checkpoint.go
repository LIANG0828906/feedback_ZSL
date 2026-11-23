package workbench

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/gob"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"sync"
)

type checkpointRecord struct {
	Magic            string          `json:"magic"`
	SceneID          string          `json:"sceneId"`
	Seed             int64           `json:"seed"`
	BandMetaVersion  string          `json:"bandMetaVersion"`
	AlgorithmVersion string          `json:"algorithmVersion"`
	Segment          SegmentSnapshot `json:"segment"`
}

type checkpointEntry struct {
	Index            int    `json:"index"`
	File             string `json:"file"`
	Hash             string `json:"hash"`
	ParamVersion     string `json:"paramVersion"`
	BandMetaVersion  string `json:"bandMetaVersion"`
	AlgorithmVersion string `json:"algorithmVersion"`
	Width            int    `json:"width"`
	Height           int    `json:"height"`
}

type checkpointManifest struct {
	Magic            string            `json:"magic"`
	SceneID          string            `json:"sceneId"`
	Seed             int64             `json:"seed"`
	BandMetaVersion  string            `json:"bandMetaVersion"`
	AlgorithmVersion string            `json:"algorithmVersion"`
	Entries          []checkpointEntry `json:"entries"`
}

type CheckpointReport struct {
	StorePath   string `json:"storePath"`
	Loaded      int    `json:"loadedSegments"`
	Invalidated bool   `json:"invalidated"`
	Reason      string `json:"reason,omitempty"`
	Manifest    string `json:"manifest,omitempty"`
}

type CheckpointStore struct {
	root string
	mu   sync.Mutex
}

func NewCheckpointStore(root string) (*CheckpointStore, error) {
	if root == "" {
		return nil, fieldErr(CodeIO, "checkpointRoot", root, "non-empty writable path", "checkpoint directory is missing")
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, wbErr(CodeIO, "create checkpoint directory: %v", err)
	}
	return &CheckpointStore{root: root}, nil
}

func (s *CheckpointStore) sceneDir(sceneID string) string {
	safe := make([]rune, 0, len(sceneID))
	for _, r := range sceneID {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			safe = append(safe, r)
		default:
			safe = append(safe, '_')
		}
	}
	return filepath.Join(s.root, string(safe))
}

func segmentHash(seg *SegmentSnapshot) string {
	h := sha256.New()
	writeInt := func(v int64) {
		var buf [8]byte
		binary.LittleEndian.PutUint64(buf[:], uint64(v))
		_, _ = h.Write(buf[:])
	}
	writeString := func(v string) {
		writeInt(int64(len(v)))
		_, _ = h.Write([]byte(v))
	}
	writeFloat := func(v float64) {
		var buf [8]byte
		binary.LittleEndian.PutUint64(buf[:], math.Float64bits(v))
		_, _ = h.Write(buf[:])
	}
	writeString("segment-v1")
	writeInt(int64(seg.Index))
	writeInt(int64(seg.X))
	writeInt(int64(seg.Y))
	writeInt(int64(seg.Width))
	writeInt(int64(seg.Height))
	writeString(seg.BandMetaVersion)
	writeString(seg.AlgorithmVersion)
	writeString(seg.Params.Version)
	writeString(seg.Path)
	writeInt(int64(seg.DownsampleFactor))
	writeString(seg.Params.GamutIntent)
	writeInt(int64(seg.Params.InterpolationOrder))
	writeFloat(seg.Params.WeightBias)
	writeFloat(seg.Params.WeightSharpness)
	writeFloat(seg.Params.DetailStrength)
	writeFloat(seg.Params.MaxRegistrationOffset)
	writeFloat(seg.Params.DisplayExposure)
	writeFloat(seg.Peak)
	writeFloat(seg.Mean)
	writeInt(int64(seg.OutOfGamut))
	for _, sample := range seg.OutOfGamutSample {
		writeInt(int64(sample.X))
		writeInt(int64(sample.Y))
	}
	for _, v := range seg.Fused {
		writeFloat(v)
	}
	for _, v := range seg.Chroma {
		writeFloat(v)
	}
	for _, v := range seg.Display {
		_, _ = h.Write([]byte{v})
	}
	for _, v := range seg.GamutMask {
		_, _ = h.Write([]byte{v})
	}
	return hex.EncodeToString(h.Sum(nil))
}

func (s *CheckpointStore) Save(scene *Scene, seg *SegmentSnapshot) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if scene == nil || seg == nil {
		return wbErr(CodeIO, "cannot checkpoint nil scene or segment")
	}
	if seg.ContentHash == "" {
		seg.ContentHash = segmentHash(seg)
	}
	dir := s.sceneDir(scene.ID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return wbErr(CodeIO, "create snapshot directory: %v", err)
	}
	record := checkpointRecord{
		Magic:            "spectral-workbench-checkpoint-v1",
		SceneID:          scene.ID,
		Seed:             scene.Seed,
		BandMetaVersion:  scene.Meta.BandMetadataVersion,
		AlgorithmVersion: AlgorithmVersion,
		Segment:          *seg,
	}
	var buf bytes.Buffer
	if err := gob.NewEncoder(&buf).Encode(record); err != nil {
		return wbErr(CodeIO, "encode checkpoint: %v", err)
	}
	name := fmt.Sprintf("segment-%06d.gob", seg.Index)
	path := filepath.Join(dir, name)
	if err := atomicWrite(path, buf.Bytes()); err != nil {
		return wbErr(CodeIO, "write checkpoint segment: %v", err)
	}
	manifestPath := filepath.Join(dir, "manifest.json")
	manifest := checkpointManifest{
		Magic:            "spectral-workbench-manifest-v1",
		SceneID:          scene.ID,
		Seed:             scene.Seed,
		BandMetaVersion:  scene.Meta.BandMetadataVersion,
		AlgorithmVersion: AlgorithmVersion,
	}
	if existing, err := readManifest(manifestPath); err == nil && existing.SceneID == scene.ID {
		manifest.Entries = existing.Entries
	}
	entry := checkpointEntry{
		Index:            seg.Index,
		File:             name,
		Hash:             seg.ContentHash,
		ParamVersion:     seg.Params.Version,
		BandMetaVersion:  seg.BandMetaVersion,
		AlgorithmVersion: seg.AlgorithmVersion,
		Width:            seg.Width,
		Height:           seg.Height,
	}
	replaced := false
	for i := range manifest.Entries {
		if manifest.Entries[i].Index == entry.Index {
			manifest.Entries[i] = entry
			replaced = true
			break
		}
	}
	if !replaced {
		manifest.Entries = append(manifest.Entries, entry)
	}
	sort.Slice(manifest.Entries, func(i, j int) bool {
		return manifest.Entries[i].Index < manifest.Entries[j].Index
	})
	manifestBytes, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return wbErr(CodeIO, "encode checkpoint manifest: %v", err)
	}
	if err := atomicWrite(manifestPath, manifestBytes); err != nil {
		return wbErr(CodeIO, "write checkpoint manifest: %v", err)
	}
	return nil
}

func (s *CheckpointStore) Load(scene *Scene) (map[int]*SegmentSnapshot, CheckpointReport) {
	s.mu.Lock()
	defer s.mu.Unlock()
	report := CheckpointReport{}
	out := make(map[int]*SegmentSnapshot)
	if scene == nil {
		report.Invalidated = true
		report.Reason = "nil scene"
		return out, report
	}
	dir := s.sceneDir(scene.ID)
	report.StorePath = dir
	manifestPath := filepath.Join(dir, "manifest.json")
	report.Manifest = manifestPath
	manifest, err := readManifest(manifestPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return out, report
		}
		report.Invalidated = true
		report.Reason = "manifest unreadable: " + err.Error()
		_ = os.RemoveAll(dir)
		return out, report
	}
	invalid := func(reason string) (map[int]*SegmentSnapshot, CheckpointReport) {
		report.Invalidated = true
		report.Reason = reason
		_ = os.RemoveAll(dir)
		return map[int]*SegmentSnapshot{}, report
	}
	if manifest.Magic != "spectral-workbench-manifest-v1" ||
		manifest.SceneID != scene.ID ||
		manifest.Seed != scene.Seed ||
		manifest.BandMetaVersion != scene.Meta.BandMetadataVersion ||
		manifest.AlgorithmVersion != AlgorithmVersion {
		return invalid("checkpoint manifest version or scene identity mismatch")
	}
	for _, entry := range manifest.Entries {
		if entry.BandMetaVersion != scene.Meta.BandMetadataVersion ||
			entry.AlgorithmVersion != AlgorithmVersion {
			return invalid(fmt.Sprintf("segment %d version mismatch", entry.Index))
		}
		path := filepath.Join(dir, entry.File)
		f, err := os.Open(path)
		if err != nil {
			return invalid(fmt.Sprintf("segment %d cannot open: %v", entry.Index, err))
		}
		var record checkpointRecord
		err = gob.NewDecoder(io.LimitReader(f, 512*1024*1024)).Decode(&record)
		closeErr := f.Close()
		if err != nil {
			return invalid(fmt.Sprintf("segment %d decode failed: %v", entry.Index, err))
		}
		if closeErr != nil {
			return invalid(fmt.Sprintf("segment %d close failed: %v", entry.Index, closeErr))
		}
		if record.Magic != "spectral-workbench-checkpoint-v1" ||
			record.SceneID != scene.ID ||
			record.Seed != scene.Seed ||
			record.BandMetaVersion != entry.BandMetaVersion ||
			record.AlgorithmVersion != entry.AlgorithmVersion ||
			record.Segment.Params.Version != entry.ParamVersion ||
			record.Segment.ContentHash != entry.Hash {
			return invalid(fmt.Sprintf("segment %d header or parameter version mismatch", entry.Index))
		}
		seg := record.Segment
		if seg.Index != entry.Index || seg.Width != entry.Width || seg.Height != entry.Height ||
			len(seg.Fused) != seg.Width*seg.Height ||
			len(seg.Chroma) != seg.Width*seg.Height*3 ||
			len(seg.Display) != seg.Width*seg.Height*3 ||
			len(seg.GamutMask) != seg.Width*seg.Height {
			return invalid(fmt.Sprintf("segment %d buffer shape mismatch", entry.Index))
		}
		if segmentHash(&seg) != entry.Hash {
			return invalid(fmt.Sprintf("segment %d content hash mismatch", entry.Index))
		}
		copySeg := seg
		out[entry.Index] = &copySeg
		report.Loaded++
	}
	return out, report
}

func readManifest(path string) (checkpointManifest, error) {
	var manifest checkpointManifest
	data, err := os.ReadFile(path)
	if err != nil {
		return manifest, err
	}
	if err := json.Unmarshal(data, &manifest); err != nil {
		return manifest, err
	}
	return manifest, nil
}

func atomicWrite(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".checkpoint-*")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	cleanup := func() {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
	}
	if _, err := tmp.Write(data); err != nil {
		cleanup()
		return err
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	if err := os.Rename(tmpPath, path); err == nil {
		return nil
	}
	// Windows does not replace an existing destination with Rename. Move the
	// old manifest aside under the same store lock, then retry. Segment files
	// have unique names and therefore use the atomic path above.
	backup := path + ".bak"
	_ = os.Remove(backup)
	if err := os.Rename(path, backup); err != nil && !errors.Is(err, os.ErrNotExist) {
		_ = os.Remove(tmpPath)
		return err
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Rename(backup, path)
		_ = os.Remove(tmpPath)
		return err
	}
	_ = os.Remove(backup)
	return nil
}
