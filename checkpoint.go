package dialogue

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
)

// CheckpointEntry is one committed frame interval persisted to disk.
type CheckpointEntry struct {
	ClipID         string      `json:"clip_id"`
	FrameNo        int         `json:"frame_no"`
	RangeID        string      `json:"range_id"`
	ContentHash    string      `json:"content_hash"`
	FeatureKey     string      `json:"feature_key"`
	CalibrationKey string      `json:"calibration_key"`
	Result         FrameResult `json:"result"`
}

// Snapshot is the atomic checkpoint file shape.
type Snapshot struct {
	SchemaVersion    string            `json:"schema_version"`
	AlgorithmVersion string            `json:"algorithm_version"`
	QuantVersion     string            `json:"quant_version"`
	ProjectHash      string            `json:"project_hash"`
	CommittedClips   []string          `json:"committed_clips"`
	Entries          []CheckpointEntry `json:"entries"`
}

// FileCheckpointStore serializes all checkpoint writes with a mutex and
// atomic temp-file rename.
type FileCheckpointStore struct {
	mu       sync.Mutex
	path     string
	project  *Project
	snapshot Snapshot
}

func ProjectIdentityHash(p *Project) string {
	type clipIdentity struct {
		ID       string
		Start    int64
		GainBP   int64
		Hash     string
		Declared int
		Fault    bool
	}
	identity := struct {
		DialogueID    string
		Seed          int64
		SampleRate    int
		FrameRate     int
		SessionGainBP int64
		TargetAmp     int64
		TargetLoudMDB int64
		MaxAdjustBP   int64
		TotalBudgetBP int64
		Clips         []clipIdentity
	}{
		DialogueID:    p.DialogueID,
		Seed:          p.Seed,
		SampleRate:    p.SampleRate,
		FrameRate:     p.FrameRate,
		SessionGainBP: p.SessionGainBP,
		TargetAmp:     p.TargetAmpMicro,
		TargetLoudMDB: p.TargetLoudMDB,
		MaxAdjustBP:   p.MaxAdjustBP,
		TotalBudgetBP: p.TotalBudgetBP,
		Clips:         make([]clipIdentity, 0, len(p.Clips)),
	}
	for _, c := range p.Clips {
		identity.Clips = append(identity.Clips, clipIdentity{
			ID:       c.ID,
			Start:    c.StartSample,
			GainBP:   c.GainBP,
			Hash:     c.AudioHash,
			Declared: c.DeclaredSampleCount,
			Fault:    c.ForceStageFailure,
		})
	}
	return canonicalHash(identity)
}

func NewFileCheckpointStore(path string, p *Project) (*FileCheckpointStore, error) {
	store := &FileCheckpointStore{
		path:    path,
		project: p,
		snapshot: Snapshot{
			SchemaVersion:    CacheSchemaVer,
			AlgorithmVersion: AlgorithmVersion,
			QuantVersion:     QuantVersion,
			ProjectHash:      ProjectIdentityHash(p),
			CommittedClips:   []string{},
			Entries:          []CheckpointEntry{},
		},
	}
	if dir := filepath.Dir(path); dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
	}
	return store, nil
}

// SaveClip atomically writes all frame intervals for a committed clip.
func (s *FileCheckpointStore) SaveClip(out *ClipOutput) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if out == nil {
		return errors.New("nil clip output")
	}
	clip, err := findClip(s.project, out.ClipID)
	if err != nil {
		return err
	}
	if !containsString(s.snapshot.CommittedClips, out.ClipID) {
		s.snapshot.CommittedClips = append(s.snapshot.CommittedClips, out.ClipID)
	}
	for i, result := range out.Results {
		entry := CheckpointEntry{
			ClipID:         out.ClipID,
			FrameNo:        result.FrameNo,
			RangeID:        result.RangeID,
			ContentHash:    clip.AudioHash,
			FeatureKey:     out.FeatureKeys[i],
			CalibrationKey: out.CalibrationKeys[i],
			Result:         result,
		}
		s.snapshot.Entries = append(s.snapshot.Entries, entry)
	}
	data, err := json.MarshalIndent(s.snapshot, "", "  ")
	if err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

func findClip(p *Project, id string) (*Clip, error) {
	for _, c := range p.Clips {
		if c.ID == id {
			return c, nil
		}
	}
	return nil, &Error{Code: CodeState, Message: "unknown clip", ClipID: id}
}

func containsString(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

func LoadSnapshot(path string) (*Snapshot, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var snap Snapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		return nil, err
	}
	return &snap, nil
}

// ValidateSnapshot returns a human-readable discard reason. Empty reason means
// the snapshot can be trusted.
func ValidateSnapshot(snap *Snapshot, p *Project) string {
	if snap == nil {
		return "empty snapshot"
	}
	if snap.SchemaVersion != CacheSchemaVer {
		return "checkpoint schema version mismatch"
	}
	if snap.AlgorithmVersion != AlgorithmVersion {
		return "algorithm version mismatch"
	}
	if snap.QuantVersion != QuantVersion {
		return "quantization version mismatch"
	}
	if snap.ProjectHash != ProjectIdentityHash(p) {
		return "project content hash mismatch"
	}
	hashByClip := make(map[string]string, len(p.Clips))
	for _, c := range p.Clips {
		hashByClip[c.ID] = c.AudioHash
	}
	for _, entry := range snap.Entries {
		if hash, ok := hashByClip[entry.ClipID]; !ok || hash != entry.ContentHash {
			return "frame content hash mismatch"
		}
	}
	return ""
}

// RecoveryReport explains breakpoint restoration.
type RecoveryReport struct {
	CheckpointPath string   `json:"checkpoint_path"`
	Loaded         bool     `json:"loaded"`
	Discarded      bool     `json:"discarded"`
	DiscardReason  string   `json:"discard_reason,omitempty"`
	RestoredClips  []string `json:"restored_clips"`
	RebuiltClips   []string `json:"rebuilt_clips"`
	TotalFrames    int      `json:"total_frames"`
}

// RecoverOrBuild loads committed frame intervals and rebuilds only remaining
// clips. A mismatching snapshot is discarded wholesale.
func RecoverOrBuild(p *Project, checkpointPath string) (*Engine, *RecoveryReport, *Error) {
	report := &RecoveryReport{
		CheckpointPath: checkpointPath,
		RestoredClips:  []string{},
		RebuiltClips:   []string{},
	}
	engine := NewEngine(p)
	snap, err := LoadSnapshot(checkpointPath)
	if err == nil {
		if reason := ValidateSnapshot(snap, p); reason != "" {
			report.Discarded = true
			report.DiscardReason = reason
		} else {
			report.Loaded = true
			for _, clipID := range snap.CommittedClips {
				rows := make([]FrameResult, 0)
				for _, entry := range snap.Entries {
					if entry.ClipID == clipID {
						rows = append(rows, entry.Result)
						engine.FeatureCache[entry.FeatureKey] = entry.Result.Feature
						engine.CalibCache[entry.CalibrationKey] = entry.Result.Calibration
					}
				}
				engine.FrozenClips[clipID] = true
				engine.FrozenResults[clipID] = rows
				report.RestoredClips = append(report.RestoredClips, clipID)
			}
		}
	}

	buffer := make([]int16, FrameBufferCap)
	for _, clip := range p.Clips {
		if engine.FrozenClips[clip.ID] {
			continue
		}
		out, pErr := ProcessClip(p, clip, buffer)
		if pErr != nil {
			return nil, report, pErr
		}
		for i := range out.Features {
			engine.FeatureCache[out.FeatureKeys[i]] = out.Features[i]
			engine.CalibCache[out.CalibrationKeys[i]] = out.Calibration.Frames[i]
		}
		report.RebuiltClips = append(report.RebuiltClips, clip.ID)
	}
	results := engine.Results()
	report.TotalFrames = len(results)
	return engine, report, nil
}

// FullBuild is the one-shot path used for three-path comparison.
func FullBuild(p *Project) ([]FrameResult, *Error) {
	engine := NewEngine(p)
	return engine.BuildAll()
}

// CompareFrames reports exact field-level equality and first mismatch.
func CompareFrames(a, b []FrameResult) *FrameComparison {
	cmp := &FrameComparison{Equal: len(a) == len(b), CountsA: len(a), CountsB: len(b), Mismatches: []FrameMismatch{}}
	if len(a) != len(b) {
		return cmp
	}
	for i := range a {
		if !frameResultEqual(a[i], b[i]) {
			cmp.Equal = false
			if len(cmp.Mismatches) < 20 {
				cmp.Mismatches = append(cmp.Mismatches, FrameMismatch{
					Index:   i,
					ClipID:  a[i].ClipID,
					FrameNo: a[i].FrameNo,
					A:       a[i],
					B:       b[i],
					Fields:  diffFrameFields(a[i], b[i]),
				})
			}
		}
	}
	return cmp
}

type FrameComparison struct {
	Equal      bool            `json:"equal"`
	CountsA    int             `json:"counts_a"`
	CountsB    int             `json:"counts_b"`
	Mismatches []FrameMismatch `json:"mismatches"`
}

type FrameMismatch struct {
	Index   int         `json:"index"`
	ClipID  string      `json:"clip_id"`
	FrameNo int         `json:"frame_no"`
	Fields  []string    `json:"fields"`
	A       FrameResult `json:"a"`
	B       FrameResult `json:"b"`
}

func frameResultEqual(a, b FrameResult) bool {
	return canonicalHash(a.Feature) == canonicalHash(b.Feature) &&
		canonicalHash(a.Calibration) == canonicalHash(b.Calibration) &&
		a.RangeID == b.RangeID && a.ClipID == b.ClipID && a.FrameNo == b.FrameNo
}

func diffFrameFields(a, b FrameResult) []string {
	fields := []string{}
	if a.Feature.EnergyFixed != b.Feature.EnergyFixed {
		fields = append(fields, "feature.energy_fixed")
	}
	if a.Feature.ZCRBP != b.Feature.ZCRBP {
		fields = append(fields, "feature.zcr_bp")
	}
	if a.Feature.PeakSample != b.Feature.PeakSample {
		fields = append(fields, "feature.peak_sample")
	}
	if a.Calibration.LoudAfterMDB != b.Calibration.LoudAfterMDB {
		fields = append(fields, "calibration.loud_after_mdb")
	}
	if a.Calibration.GainAdjustBP != b.Calibration.GainAdjustBP {
		fields = append(fields, "calibration.gain_adjust_bp")
	}
	if len(fields) == 0 {
		fields = append(fields, "truncation_or_other_detail")
	}
	return fields
}
