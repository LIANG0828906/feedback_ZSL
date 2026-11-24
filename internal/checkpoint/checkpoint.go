package checkpoint

import (
	"bytes"
	"compress/zlib"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"

	"texlab/internal/apperr"
	"texlab/internal/engine"
	"texlab/internal/model"
)

type LoadStatus struct {
	Files      int    `json:"files"`
	Valid      bool   `json:"valid"`
	Discarded  bool   `json:"discarded"`
	Reason     string `json:"reason,omitempty"`
	CommitSeqs []int  `json:"commit_sequences,omitempty"`
}

type Store struct {
	mu       sync.Mutex
	Dir      string
	PlanFP   string
	SourceFP string
	Revision int
	closed   bool
}

type metadata struct {
	Schema            string `json:"schema"`
	PlanFingerprint   string `json:"plan_fingerprint"`
	SourceFingerprint string `json:"source_fingerprint"`
	ParamVersion      int    `json:"param_version"`
	AlgorithmVersion  string `json:"algorithm_version"`
}

type diskImage struct {
	Width    int    `json:"width"`
	Height   int    `json:"height"`
	Depth    int    `json:"depth"`
	Channels int    `json:"channels"`
	Layout   string `json:"layout"`
	Alpha    string `json:"alpha"`
	PixelZ   string `json:"pixels_zlib_b64"`
}

type diskStage struct {
	EntryID       string       `json:"entry_id"`
	EntryIndex    int          `json:"entry_index"`
	Entry         model.Entry  `json:"entry"`
	Segment       int          `json:"segment"`
	Image         diskImage    `json:"image"`
	CacheKey      string       `json:"cache_key"`
	InputHash     string       `json:"input_hash"`
	RegionHash    string       `json:"region_hash"`
	ContentHash   string       `json:"content_hash"`
	ParamHash     string       `json:"param_hash"`
	CoverageHash  string       `json:"coverage_hash"`
	Coverage      []model.Rect `json:"coverage"`
	UpstreamKey   string       `json:"upstream_key"`
	Reused        bool         `json:"reused"`
	CacheHit      bool         `json:"cache_hit"`
	Invalidated   bool         `json:"invalidated"`
	InvalidReason string       `json:"invalid_reason,omitempty"`
}

type diskCheckpoint struct {
	Schema            string        `json:"schema"`
	PlanFingerprint   string        `json:"plan_fingerprint"`
	SourceFingerprint string        `json:"source_fingerprint"`
	ParamVersion      int           `json:"param_version"`
	AlgorithmVersion  string        `json:"algorithm_version"`
	Segment           model.Segment `json:"segment"`
	Stages            []diskStage   `json:"stages"`
	Final             diskImage     `json:"final"`
	FinalHash         string        `json:"final_hash"`
	CommitSeq         int           `json:"commit_seq"`
	Reused            bool          `json:"reused"`
}

func NewStore(dir string, plan model.Plan, sourceFP string) (*Store, error) {
	if dir == "" {
		return nil, apperr.New(apperr.EPara, "checkpoint directory is required")
	}
	s := &Store{
		Dir: dir, PlanFP: model.PlanFingerprint(plan), SourceFP: sourceFP,
		Revision: plan.Revision,
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, &apperr.Error{Code: apperr.ECheckpoint, Message: "create checkpoint directory", Path: dir, Details: err.Error()}
	}
	if err := s.ensureMetadata(); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Store) ensureMetadata() error {
	want := metadata{
		Schema: "texlab-checkpoint-meta-v1", PlanFingerprint: s.PlanFP,
		SourceFingerprint: s.SourceFP, ParamVersion: s.Revision,
		AlgorithmVersion: model.AlgorithmVersion,
	}
	path := filepath.Join(s.Dir, "metadata.json")
	raw, err := os.ReadFile(path)
	if err == nil {
		var got metadata
		if json.Unmarshal(raw, &got) == nil && got == want {
			return nil
		}
	}
	if err := clearSegments(s.Dir); err != nil {
		return err
	}
	return writeJSONAtomic(path, want)
}

func (s *Store) CommitSegment(c *engine.SegmentCommit) error {
	if c == nil {
		return apperr.New(apperr.ECheckpoint, "nil segment commit")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if c.PlanFingerprint != s.PlanFP || c.SourceFingerprint != s.SourceFP ||
		c.ParamVersion != s.Revision || c.AlgorithmVersion != model.AlgorithmVersion {
		return &apperr.Error{Code: apperr.EVersion, Segment: c.Segment.Index, Field: "checkpoint", Message: "segment commit version does not match checkpoint store"}
	}
	if err := s.ensureMetadataLocked(); err != nil {
		return err
	}
	dc := diskCheckpoint{
		Schema: model.CheckpointSchema, PlanFingerprint: c.PlanFingerprint,
		SourceFingerprint: c.SourceFingerprint, ParamVersion: c.ParamVersion,
		AlgorithmVersion: c.AlgorithmVersion, Segment: c.Segment,
		FinalHash: c.FinalHash, CommitSeq: c.CommitSeq, Reused: c.Reused,
	}
	dc.Final = encodeImage(c.Final)
	for _, st := range c.Stages {
		dc.Stages = append(dc.Stages, diskStage{
			EntryID: st.EntryID, EntryIndex: st.EntryIndex, Entry: st.Entry, Segment: st.Segment,
			Image: encodeImage(st.Image), CacheKey: st.CacheKey, InputHash: st.InputHash,
			RegionHash: st.RegionHash, ContentHash: st.ContentHash, ParamHash: st.ParamHash,
			CoverageHash: st.CoverageHash, Coverage: st.Coverage, UpstreamKey: st.UpstreamKey,
			Reused: st.Reused, CacheHit: st.CacheHit, Invalidated: st.Invalidated,
			InvalidReason: st.InvalidReason,
		})
	}
	name := fmt.Sprintf("segment-%04d.json", c.Segment.Index)
	return writeJSONAtomic(filepath.Join(s.Dir, name), dc)
}

func (s *Store) ensureMetadataLocked() error {
	return s.ensureMetadata()
}

func LoadAll(dir string, plan model.Plan, sourceFP string) (map[int]*engine.SegmentCommit, LoadStatus, error) {
	status := LoadStatus{}
	out := make(map[int]*engine.SegmentCommit)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			status.Valid = false
			status.Reason = "no checkpoint directory"
			return out, status, nil
		}
		return nil, status, &apperr.Error{Code: apperr.ECheckpoint, Message: "read checkpoint directory", Path: dir, Details: err.Error()}
	}
	metaPath := filepath.Join(dir, "metadata.json")
	var meta metadata
	if raw, err := os.ReadFile(metaPath); err != nil || json.Unmarshal(raw, &meta) != nil {
		status.Discarded = true
		status.Reason = "missing or corrupt checkpoint metadata"
		_ = clearAll(dir)
		return out, status, nil
	}
	planFP := model.PlanFingerprint(plan)
	if meta.Schema != "texlab-checkpoint-meta-v1" || meta.PlanFingerprint != planFP ||
		meta.SourceFingerprint != sourceFP || meta.ParamVersion != plan.Revision ||
		meta.AlgorithmVersion != model.AlgorithmVersion {
		status.Discarded = true
		status.Reason = "plan, source, parameter version, or algorithm version mismatch"
		_ = clearAll(dir)
		return out, status, nil
	}
	var paths []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), "segment-") || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		paths = append(paths, filepath.Join(dir, e.Name()))
	}
	sort.Strings(paths)
	status.Files = len(paths)
	if len(paths) == 0 {
		status.Valid = true
		return out, status, nil
	}
	for pos, path := range paths {
		raw, err := os.ReadFile(path)
		if err != nil {
			return discard(out, status, dir, "read segment checkpoint: "+err.Error())
		}
		var dc diskCheckpoint
		if err := json.Unmarshal(raw, &dc); err != nil {
			return discard(out, status, dir, "decode segment checkpoint: "+err.Error())
		}
		if dc.Schema != model.CheckpointSchema || dc.PlanFingerprint != planFP ||
			dc.SourceFingerprint != sourceFP || dc.ParamVersion != plan.Revision ||
			dc.AlgorithmVersion != model.AlgorithmVersion {
			return discard(out, status, dir, "segment checkpoint version mismatch")
		}
		if dc.CommitSeq != dc.Segment.Index || dc.Segment.Index != pos {
			return discard(out, status, dir, "segment checkpoint sequence is not contiguous")
		}
		if len(dc.Stages) != len(plan.Entries) {
			return discard(out, status, dir, "segment checkpoint stage count mismatch")
		}
		sc := &engine.SegmentCommit{
			Segment: dc.Segment, FinalHash: dc.FinalHash, CommitSeq: dc.CommitSeq,
			PlanFingerprint: dc.PlanFingerprint, SourceFingerprint: dc.SourceFingerprint,
			ParamVersion: dc.ParamVersion, AlgorithmVersion: dc.AlgorithmVersion, Reused: dc.Reused,
		}
		sc.Final, err = decodeImage(dc.Final)
		if err != nil || sc.Final.ContentHash() != dc.FinalHash {
			return discard(out, status, dir, "final image content hash mismatch")
		}
		upstream := model.HashParts("texlab-source-root-v1", sourceFP)
		previousContent := ""
		for i, ds := range dc.Stages {
			entry := plan.Entries[i]
			if ds.EntryID != entry.ID || ds.EntryIndex != entry.Index || ds.Segment != dc.Segment.Index {
				return discard(out, status, dir, "stage entry/segment identity mismatch")
			}
			if ds.ParamHash != ds.Entry.ParamSignature() || ds.Entry.ParamSignature() != entry.ParamSignature() {
				return discard(out, status, dir, "stage parameter snapshot mismatch")
			}
			if ds.UpstreamKey != upstream {
				return discard(out, status, dir, "upstream cache key chain mismatch")
			}
			if i > 0 && ds.InputHash != previousContent {
				return discard(out, status, dir, "stage input content hash chain mismatch")
			}
			wantKey := model.CacheKey(ds.RegionHash, ds.EntryIndex, ds.ParamHash, upstream)
			if ds.CacheKey != wantKey {
				return discard(out, status, dir, "stage cache key mismatch")
			}
			im, err := decodeImage(ds.Image)
			if err != nil || im.ContentHash() != ds.ContentHash {
				return discard(out, status, dir, "stage image content hash mismatch")
			}
			if im.Width != dc.Segment.W || im.Height != dc.Segment.H {
				return discard(out, status, dir, "stage image dimensions mismatch")
			}
			rec := &engine.StageRecord{
				EntryID: ds.EntryID, EntryIndex: ds.EntryIndex, Entry: ds.Entry, Segment: ds.Segment,
				Image: im, CacheKey: ds.CacheKey, InputHash: ds.InputHash, RegionHash: ds.RegionHash,
				ContentHash: ds.ContentHash, ParamHash: ds.ParamHash, CoverageHash: ds.CoverageHash,
				Coverage: ds.Coverage, UpstreamKey: ds.UpstreamKey, Reused: ds.Reused,
				CacheHit: ds.CacheHit, Invalidated: ds.Invalidated, InvalidReason: ds.InvalidReason,
			}
			sc.Stages = append(sc.Stages, rec)
			upstream = ds.CacheKey
			previousContent = ds.ContentHash
		}
		if len(sc.Stages) > 0 && sc.Stages[len(sc.Stages)-1].ContentHash != dc.FinalHash {
			return discard(out, status, dir, "final image does not match last stage")
		}
		out[sc.Segment.Index] = sc
		status.CommitSeqs = append(status.CommitSeqs, sc.CommitSeq)
	}
	status.Valid = true
	return out, status, nil
}

func discard(out map[int]*engine.SegmentCommit, status LoadStatus, dir, reason string) (map[int]*engine.SegmentCommit, LoadStatus, error) {
	status.Valid = false
	status.Discarded = true
	status.Reason = reason
	_ = clearAll(dir)
	return map[int]*engine.SegmentCommit{}, status, nil
}

func clearSegments(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return &apperr.Error{Code: apperr.ECheckpoint, Message: "read checkpoint directory", Path: dir, Details: err.Error()}
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "segment-") && strings.HasSuffix(e.Name(), ".json") {
			if err := os.Remove(filepath.Join(dir, e.Name())); err != nil && !os.IsNotExist(err) {
				return &apperr.Error{Code: apperr.ECheckpoint, Message: "remove stale checkpoint", Path: e.Name(), Details: err.Error()}
			}
		}
	}
	return nil
}

func clearAll(dir string) error {
	if err := os.RemoveAll(dir); err != nil {
		return &apperr.Error{Code: apperr.ECheckpoint, Message: "discard checkpoint snapshot", Path: dir, Details: err.Error()}
	}
	return nil
}

func encodeImage(im model.Image) diskImage {
	var raw bytes.Buffer
	var tmp [2]byte
	for _, v := range im.Pixels {
		binary.LittleEndian.PutUint16(tmp[:], v)
		raw.Write(tmp[:])
	}
	var z bytes.Buffer
	w, _ := zlib.NewWriterLevel(&z, zlib.BestSpeed)
	_, _ = w.Write(raw.Bytes())
	_ = w.Close()
	return diskImage{
		Width: im.Width, Height: im.Height, Depth: im.Depth, Channels: im.Channels,
		Layout: im.Layout, Alpha: im.Alpha, PixelZ: base64.StdEncoding.EncodeToString(z.Bytes()),
	}
}

func decodeImage(d diskImage) (model.Image, error) {
	zraw, err := base64.StdEncoding.DecodeString(d.PixelZ)
	if err != nil {
		return model.Image{}, err
	}
	zr, err := zlib.NewReader(bytes.NewReader(zraw))
	if err != nil {
		return model.Image{}, err
	}
	raw, err := io.ReadAll(zr)
	_ = zr.Close()
	if err != nil {
		return model.Image{}, err
	}
	if d.Width <= 0 || d.Height <= 0 || len(raw) != d.Width*d.Height*4*2 {
		return model.Image{}, fmt.Errorf("invalid checkpoint pixel length")
	}
	im := model.NewImage(d.Width, d.Height, d.Depth, d.Channels, d.Layout, d.Alpha)
	for i := range im.Pixels {
		im.Pixels[i] = binary.LittleEndian.Uint16(raw[i*2 : i*2+2])
	}
	return im, nil
}

func writeJSONAtomic(path string, v any) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return &apperr.Error{Code: apperr.ECheckpoint, Message: "create checkpoint directory", Path: dir, Details: err.Error()}
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return &apperr.Error{Code: apperr.ECheckpoint, Message: "encode checkpoint", Path: path, Details: err.Error()}
	}
	tmp, err := os.CreateTemp(dir, ".checkpoint-*.tmp")
	if err != nil {
		return &apperr.Error{Code: apperr.ECheckpoint, Message: "create checkpoint temp", Path: path, Details: err.Error()}
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(raw); err != nil {
		_ = tmp.Close()
		return &apperr.Error{Code: apperr.ECheckpoint, Message: "write checkpoint temp", Path: path, Details: err.Error()}
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return &apperr.Error{Code: apperr.ECheckpoint, Message: "sync checkpoint temp", Path: path, Details: err.Error()}
	}
	if err := tmp.Close(); err != nil {
		return &apperr.Error{Code: apperr.ECheckpoint, Message: "close checkpoint temp", Path: path, Details: err.Error()}
	}
	if err := os.Rename(tmpName, path); err != nil {
		return &apperr.Error{Code: apperr.ECheckpoint, Message: "atomically replace checkpoint", Path: path, Details: err.Error()}
	}
	if dh, err := os.Open(dir); err == nil {
		_ = dh.Sync()
		_ = dh.Close()
	}
	return nil
}
