package session

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"stitchworkbench/internal/checkpoint"
	"stitchworkbench/internal/errs"
	"stitchworkbench/internal/imaging"
	"stitchworkbench/internal/model"
)

type Service struct {
	dataDir string
	workDir string
	mu      sync.Mutex
	sets    map[string]*SetSession
}

type SetInfo struct {
	ID          string       `json:"id"`
	Description string       `json:"description"`
	Images      int          `json:"images"`
	Canvas      model.Canvas `json:"canvas"`
	Version     int64        `json:"version"`
	Committed   int          `json:"committed"`
	Warning     string       `json:"warning,omitempty"`
}

func NewService(dataDir, workDir string) (*Service, error) {
	if err := os.MkdirAll(workDir, 0o755); err != nil {
		return nil, errs.Wrap(errs.ECheckpoint, "cannot create work directory", err)
	}
	return &Service{dataDir: dataDir, workDir: workDir, sets: make(map[string]*SetSession)}, nil
}

func (s *Service) SetDirs() ([]string, error) {
	entries, err := os.ReadDir(s.dataDir)
	if err != nil {
		return nil, errs.Wrap(errs.EDecode, "cannot read sample directory", err)
	}
	var out []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(s.dataDir, e.Name())
		if _, err := os.Stat(filepath.Join(dir, "manifest.json")); err == nil {
			out = append(out, dir)
		}
	}
	sort.Strings(out)
	return out, nil
}

func (s *Service) List() ([]SetInfo, error) {
	dirs, err := s.SetDirs()
	if err != nil {
		return nil, err
	}
	var out []SetInfo
	for _, dir := range dirs {
		m, _, err := imaging.ReadManifest(dir)
		if err != nil {
			continue
		}
		id := m.SetID
		if id == "" {
			id = filepath.Base(dir)
		}
		info := SetInfo{ID: id, Description: m.Description, Images: len(m.Images), Canvas: m.Canvas}
		if sess, err := s.Open(id); err == nil {
			latest := sess.snapshots.Latest()
			info.Version = latest.Version
			info.Committed = sess.CommitCount()
			info.Warning = sess.Warning()
		}
		out = append(out, info)
	}
	return out, nil
}

func (s *Service) Open(id string) (*SetSession, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if sess := s.sets[id]; sess != nil {
		return sess, nil
	}
	dirs, err := s.SetDirs()
	if err != nil {
		return nil, err
	}
	var dir string
	for _, d := range dirs {
		m, _, err := imaging.ReadManifest(d)
		if err == nil && (m.SetID == id || filepath.Base(d) == id) {
			dir = d
			break
		}
	}
	if dir == "" {
		return nil, errs.New(errs.EDecode, "sample set not found: "+id)
	}
	set, err := imaging.LoadSet(dir)
	if err != nil {
		return nil, err
	}
	if set.Manifest.SetID == "" {
		set.Manifest.SetID = id
	}
	work := filepath.Join(s.workDir, sanitize(id))
	snaps := checkpoint.NewSnapshotStore(filepath.Join(work, "params"))
	if err := snaps.Load(); err != nil {
		return nil, err
	}
	sess := &SetSession{
		ID: id, Dir: dir, Set: set, snapshots: snaps,
		store:   checkpoint.NewStore(filepath.Join(work, "checkpoints")),
		commits: make(map[int]model.PairStats), previews: make(map[int64]*previewCache),
		tasks: make(map[string]*Task), exactPairs: make(map[int64]map[int]model.PairStats),
		artifacts: make(map[string][]byte),
	}
	if err := sess.loadCheckpoint(); err != nil {
		sess.warning = err.Error()
	}
	s.sets[id] = sess
	return sess, nil
}

func sanitize(s string) string {
	s = strings.ReplaceAll(s, "..", "_")
	s = strings.ReplaceAll(s, "/", "_")
	s = strings.ReplaceAll(s, "\\", "_")
	return s
}

type previewCache struct {
	Result *model.PreviewResult
	Pairs  map[int]model.PairStats
}

type SetSession struct {
	mu         sync.Mutex
	previewMu  sync.Mutex
	ID         string
	Dir        string
	Set        *imaging.Set
	snapshots  *checkpoint.SnapshotStore
	store      *checkpoint.Store
	commits    map[int]model.PairStats
	warning    string
	previews   map[int64]*previewCache
	tasks      map[string]*Task
	nextTask   int64
	exactPairs map[int64]map[int]model.PairStats
	artifacts  map[string][]byte
	sequence   int64
}

func (s *SetSession) Warning() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.warning
}

func (s *SetSession) CommitCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.commits)
}

func (s *SetSession) Latest() model.ParamSnapshot { return s.snapshots.Latest() }

func (s *SetSession) Params() model.Params { return s.snapshots.Latest().Params }

func (s *SetSession) Snapshot(version int64) (model.ParamSnapshot, bool) {
	return s.snapshots.Get(version)
}

func (s *SetSession) UpdateParams(raw map[string]json.RawMessage) (model.ParamSnapshot, error) {
	return s.snapshots.Update(raw)
}

func (s *SetSession) loadCheckpoint() error {
	snap, err := s.store.Load()
	if err != nil {
		_ = s.store.Invalidate("checkpoint unreadable: " + err.Error())
		return fmt.Errorf("checkpoint invalidated: %v", err)
	}
	if snap == nil {
		return nil
	}
	if err := s.validateCheckpoint(snap); err != nil {
		_ = s.store.Invalidate(err.Error())
		s.commits = make(map[int]model.PairStats)
		return fmt.Errorf("checkpoint invalidated: %v", err)
	}
	for _, rec := range snap.Records {
		im := s.Set.Images[rec.PairIndex]
		h, _ := model.MatrixFromSlice(rec.Homography)
		st := model.PairStats{
			PairIndex: rec.PairIndex, PairID: rec.PairID, SourceImage: rec.SourceImage,
			TargetImage: rec.TargetImage, Homography: rec.Homography,
			TruthHomography: im.Homography.Slice(), HomographyErr: h.MaxAbsDiff(im.Homography),
			Matches: rec.Matches, Inliers: rec.Inliers, ResidualMedian: rec.ResidualMedian,
			ConditionNumber: rec.ConditionNumber,
			ExposureGain:    rec.ExposureGain, ExposureOffset: rec.ExposureOffset,
			ContentHash: rec.ContentHash, ParamVersion: rec.ParamVersion,
			Committed: true, CommittedVersion: rec.ParamVersion,
			AlgorithmVersion: rec.AlgorithmVersion, Decision: true,
		}
		if st.Matches > 0 {
			st.InlierRate = float64(st.Inliers) / float64(st.Matches)
		}
		s.commits[rec.PairIndex] = st
		s.sequence = max64(s.sequence, snap.Header.Sequence)
	}
	return nil
}

func (s *SetSession) validateCheckpoint(snap *model.CheckpointSnapshot) error {
	if snap.Header.Format != "stitchworkbench.checkpoint/v1" {
		return fmt.Errorf("checkpoint format mismatch: %s", snap.Header.Format)
	}
	if snap.Header.SetID != s.ID {
		return fmt.Errorf("checkpoint set id mismatch: %s", snap.Header.SetID)
	}
	if snap.Header.AlgorithmVersion != model.AlgorithmVersion {
		return fmt.Errorf("algorithm version mismatch: %s", snap.Header.AlgorithmVersion)
	}
	if snap.Header.BaseContentHash != s.Set.ContentHash {
		return fmt.Errorf("base content hash mismatch")
	}
	if _, ok := s.snapshots.Get(snap.Header.ParamVersion); !ok {
		return fmt.Errorf("header parameter version %d is missing", snap.Header.ParamVersion)
	}
	if len(snap.Header.ContentHashes) != len(s.Set.Images) {
		return fmt.Errorf("checkpoint image hash count mismatch")
	}
	for _, im := range s.Set.Images {
		if snap.Header.ContentHashes[im.Truth.File] != im.ContentHash {
			return fmt.Errorf("content hash mismatch for %s", im.Truth.File)
		}
	}
	seen := make(map[int]bool)
	for _, rec := range snap.Records {
		if rec.PairIndex <= 0 || rec.PairIndex >= len(s.Set.Images) {
			return fmt.Errorf("checkpoint pair index %d out of range", rec.PairIndex)
		}
		if seen[rec.PairIndex] {
			return fmt.Errorf("duplicate checkpoint pair %d", rec.PairIndex)
		}
		seen[rec.PairIndex] = true
		if rec.AlgorithmVersion != model.AlgorithmVersion {
			return fmt.Errorf("record algorithm version mismatch for %s", rec.PairID)
		}
		if _, ok := s.snapshots.Get(rec.ParamVersion); !ok {
			return fmt.Errorf("record parameter version %d is missing for %s", rec.ParamVersion, rec.PairID)
		}
		if rec.ContentHash != pairHash(s.Set, rec.PairIndex) {
			return fmt.Errorf("record content hash mismatch for %s", rec.PairID)
		}
		h, ok := model.MatrixFromSlice(rec.Homography)
		if !ok {
			return fmt.Errorf("record homography is not finite for %s", rec.PairID)
		}
		if _, ok := h.Inverse(); !ok {
			return fmt.Errorf("record homography is not invertible for %s", rec.PairID)
		}
	}
	return nil
}

func (s *SetSession) saveCheckpointLocked(version int64) error {
	snap, ok := s.snapshots.Get(version)
	if !ok {
		return errs.Field(errs.EVersion, "parameter version does not exist", "version")
	}
	hashes := make(map[string]string, len(s.Set.Images))
	for _, im := range s.Set.Images {
		hashes[im.Truth.File] = im.ContentHash
	}
	records := make([]model.CheckpointRecord, 0, len(s.commits))
	for _, st := range s.commits {
		records = append(records, model.CheckpointRecord{
			PairIndex: st.PairIndex, PairID: st.PairID, SourceImage: st.SourceImage,
			TargetImage: st.TargetImage, Homography: st.Homography,
			Matches: st.Matches, Inliers: st.Inliers, ResidualMedian: st.ResidualMedian,
			ConditionNumber: st.ConditionNumber, ExposureGain: st.ExposureGain,
			ExposureOffset: st.ExposureOffset, ContentHash: st.ContentHash,
			ParamVersion: st.ParamVersion, AlgorithmVersion: model.AlgorithmVersion,
			CommittedAt: time.Now().UTC(),
		})
	}
	sort.Slice(records, func(i, j int) bool { return records[i].PairIndex < records[j].PairIndex })
	s.sequence++
	header := model.CheckpointHeader{
		Format: "stitchworkbench.checkpoint/v1", SetID: s.ID,
		ParamVersion: snap.Version, ParamsHash: snap.Hash,
		BaseContentHash: s.Set.ContentHash, ContentHashes: hashes,
		AlgorithmVersion: model.AlgorithmVersion, Sequence: s.sequence,
	}
	if err := s.store.Save(model.CheckpointSnapshot{Header: header, Records: records}); err != nil {
		s.sequence--
		return err
	}
	return nil
}

func (s *SetSession) commitStatsLocked(st model.PairStats) error {
	if old, ok := s.commits[st.PairIndex]; ok {
		st = old
		return nil
	}
	if !st.Decision || st.ErrorCode != "" {
		return errs.Field(errs.EConflict, "cannot commit a failed registration pair", "pair_index")
	}
	s.commits[st.PairIndex] = st
	if err := s.saveCheckpointLocked(st.ParamVersion); err != nil {
		delete(s.commits, st.PairIndex)
		return err
	}
	s.previews = make(map[int64]*previewCache)
	return nil
}

func (s *SetSession) Committed() map[int]model.PairStats {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[int]model.PairStats, len(s.commits))
	for k, v := range s.commits {
		out[k] = v
	}
	return out
}

func (s *SetSession) Artifact(key string) ([]byte, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.artifacts[key]
	return b, ok
}

func (s *SetSession) putArtifact(key string, data []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.artifacts[key] = data
}

func pairHash(set *imaging.Set, idx int) string {
	sum := sha256.Sum256([]byte(set.Images[0].ContentHash + "|" + set.Images[idx].ContentHash))
	return hex.EncodeToString(sum[:])
}

func (s *SetSession) Info() map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	latest := s.snapshots.Latest()
	return map[string]any{
		"id": s.ID, "description": s.Set.Manifest.Description,
		"images": len(s.Set.Images), "canvas": s.Set.Manifest.Canvas,
		"content_hash": s.Set.ContentHash, "algorithm_version": model.AlgorithmVersion,
		"version": latest.Version, "params": latest.Params, "params_hash": latest.Hash,
		"committed": len(s.commits), "warning": s.warning,
		"truth": map[string]any{
			"homography_tolerance":    s.Set.Manifest.HomographyTol,
			"residual_tolerance":      s.Set.Manifest.ResidualTol,
			"seam_gradient_tolerance": s.Set.Manifest.SeamTol,
		},
	}
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
