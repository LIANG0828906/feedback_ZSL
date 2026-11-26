package loudness

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

const CheckpointFormatVersion = 1

// ClipCheckpoint is the atomic persisted unit for one committed clip.
type ClipCheckpoint struct {
	FormatVersion              int             `json:"formatVersion"`
	ClipID                     int             `json:"clipId"`
	CommitIndex                int             `json:"commitIndex"`
	ProjectID                  string          `json:"projectId"`
	Seed                       int64           `json:"seed"`
	SourceVersion              Version         `json:"sourceVersion"`
	CurrentVersion             Version         `json:"currentVersion"`
	QuantVersion               string          `json:"quantVersion"`
	AlgorithmVersion           string          `json:"algorithmVersion"`
	ContentHash                string          `json:"contentHash"`
	PCMHash                    string          `json:"pcmHash"`
	MeasurementCacheKey        string          `json:"measurementCacheKey"`
	CalibrationCacheKey        string          `json:"calibrationCacheKey"`
	FrameLength                int64           `json:"frameLength"`
	FrameShift                 int64           `json:"frameShift"`
	TargetLoudnessDBQ8         int64           `json:"targetLoudnessDbQ8"`
	SessionGainQ12             int64           `json:"sessionGainQ12"`
	MaxGainQ12                 int64           `json:"maxGainQ12"`
	LoudnessToleranceQ8        int64           `json:"loudnessToleranceQ8"`
	EnvelopeDigest             string          `json:"envelopeDigest"`
	GainAdjustmentQ12          int64           `json:"gainAdjustmentQ12"`
	DesiredGainQ12             int64           `json:"desiredGainQ12"`
	GainLimited                bool            `json:"gainLimited"`
	MeasuredSegmentEnergyQ32   int64           `json:"measuredSegmentEnergyQ32"`
	MeasuredSegmentRmsQ16      int64           `json:"measuredSegmentRmsQ16"`
	MeasuredSegmentDBQ8        int64           `json:"measuredSegmentDbQ8"`
	CalibratedSegmentDBQ8      int64           `json:"calibratedSegmentDbQ8"`
	CalibratedSegmentRmsQ16    int64           `json:"calibratedSegmentRmsQ16"`
	CalibratedSegmentEnergyQ32 int64           `json:"calibratedSegmentEnergyQ32"`
	Frames                     []FrameEnvelope `json:"frames"`
	MeasurementTruncations     []Truncation    `json:"measurementTruncations,omitempty"`
	Truncations                []Truncation    `json:"truncations,omitempty"`
}

// CheckpointSummary is the compact diagnostic form of a checkpoint.
type CheckpointSummary struct {
	ClipID            int     `json:"clipId"`
	CommitIndex       int     `json:"commitIndex"`
	ContentHash       string  `json:"contentHash"`
	SourceVersion     Version `json:"sourceVersion"`
	QuantVersion      string  `json:"quantVersion"`
	AlgorithmVersion  string  `json:"algorithmVersion"`
	EnvelopeDigest    string  `json:"envelopeDigest"`
	GainAdjustmentQ12 int64   `json:"gainAdjustmentQ12"`
}

// CheckpointStore serializes atomic per-clip file writes in one directory.
type CheckpointStore struct {
	dir string
	mu  sync.Mutex
}

func NewCheckpointStore(dir string) (*CheckpointStore, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, &DomainError{Code: EPara, Path: "$.checkpointDir", Field: "checkpointDir", Detail: "directory must not be empty"}
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, &DomainError{Code: ESchema, Path: "$.checkpointDir", Detail: "cannot create checkpoint directory", Cause: err}
	}
	return &CheckpointStore{dir: dir}, nil
}

func (s *CheckpointStore) Dir() string { return s.dir }

func (s *CheckpointStore) pathForClip(clipID int) string {
	return filepath.Join(s.dir, fmt.Sprintf("clip-%06d.json", clipID))
}

func checkpointFromResult(project *Project, result ClipResult, commitIndex int) ClipCheckpoint {
	return ClipCheckpoint{
		FormatVersion: CheckpointFormatVersion, ClipID: result.ClipID, CommitIndex: commitIndex,
		ProjectID: project.ID, Seed: project.Seed,
		SourceVersion: project.Provenance.OriginalVersion, CurrentVersion: project.FormatVersion,
		QuantVersion: project.Quantization.Version, AlgorithmVersion: AlgorithmVersion,
		ContentHash: result.ContentHash, PCMHash: result.PCMHash,
		MeasurementCacheKey: result.MeasurementCacheKey, CalibrationCacheKey: result.CalibrationCacheKey,
		FrameLength: result.FrameLength, FrameShift: result.FrameShift,
		TargetLoudnessDBQ8: project.TargetLoudnessDBQ8, SessionGainQ12: project.SessionGainQ12,
		MaxGainQ12: project.MaxGainQ12, LoudnessToleranceQ8: project.LoudnessToleranceQ8,
		EnvelopeDigest: result.EnvelopeDigest, GainAdjustmentQ12: result.GainAdjustmentQ12,
		DesiredGainQ12: result.DesiredGainQ12, GainLimited: result.GainLimited,
		MeasuredSegmentEnergyQ32: result.MeasuredSegmentEnergyQ32, MeasuredSegmentRmsQ16: result.MeasuredSegmentRmsQ16,
		MeasuredSegmentDBQ8: result.MeasuredSegmentDBQ8, CalibratedSegmentDBQ8: result.CalibratedSegmentDBQ8,
		CalibratedSegmentRmsQ16:    result.CalibratedSegmentRmsQ16,
		CalibratedSegmentEnergyQ32: result.CalibratedSegmentEnergyQ32,
		Frames:                     cloneFrames(result.Frames),
		MeasurementTruncations:     append([]Truncation(nil), result.MeasurementTruncations...),
		Truncations:                append([]Truncation(nil), result.Truncations...),
	}
}

func cloneFrames(frames []FrameEnvelope) []FrameEnvelope {
	out := make([]FrameEnvelope, len(frames))
	for i, frame := range frames {
		out[i] = frame
		out[i].Truncations = append([]Truncation(nil), frame.Truncations...)
	}
	return out
}

func validateCheckpoint(checkpoint ClipCheckpoint) error {
	path := fmt.Sprintf("$.checkpoints[%d]", checkpoint.ClipID)
	if checkpoint.FormatVersion != CheckpointFormatVersion {
		return &DomainError{Code: ESchema, Path: path + ".formatVersion", Detail: "unsupported checkpoint format"}
	}
	if checkpoint.ClipID < 0 || checkpoint.CommitIndex < 0 {
		return &DomainError{Code: ESchema, Path: path, Detail: "clip id and commit index must be non-negative"}
	}
	if checkpoint.ContentHash == "" || checkpoint.QuantVersion == "" || checkpoint.AlgorithmVersion == "" || checkpoint.SourceVersion == "" {
		return &DomainError{Code: ESchema, Path: path, Detail: "checkpoint identity fields are required"}
	}
	if checkpoint.EnvelopeDigest == "" || len(checkpoint.Frames) == 0 {
		return &DomainError{Code: ESchema, Path: path + ".frames", Detail: "envelope summary and frames are required"}
	}
	if digestFrames(checkpoint.Frames) != checkpoint.EnvelopeDigest {
		return &DomainError{Code: ESchema, Path: path + ".envelopeDigest", Detail: "envelope digest does not match frames"}
	}
	return nil
}

// Write atomically replaces one clip file. The mutex covers temp-file creation,
// write, fsync, and rename so concurrent workers cannot interleave a commit.
func (s *CheckpointStore) Write(ctx context.Context, checkpoint ClipCheckpoint) error {
	if err := validateCheckpoint(checkpoint); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	data, err := json.MarshalIndent(checkpoint, "", "  ")
	if err != nil {
		return &DomainError{Code: ESchema, Path: "$.checkpoint", Detail: "checkpoint encode failed", Cause: err}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	temp, err := os.CreateTemp(s.dir, ".clip-checkpoint-*.tmp")
	if err != nil {
		return &DomainError{Code: ESchema, Path: "$.checkpointDir", Detail: "cannot create temp checkpoint", Cause: err}
	}
	tempName := temp.Name()
	cleanup := func() {
		_ = temp.Close()
		_ = os.Remove(tempName)
	}
	if _, err := temp.Write(data); err != nil {
		cleanup()
		return &DomainError{Code: ESchema, Path: "$.checkpoint", Detail: "cannot write temp checkpoint", Cause: err}
	}
	if err := temp.Sync(); err != nil {
		cleanup()
		return &DomainError{Code: ESchema, Path: "$.checkpoint", Detail: "cannot fsync temp checkpoint", Cause: err}
	}
	if err := temp.Close(); err != nil {
		_ = os.Remove(tempName)
		return &DomainError{Code: ESchema, Path: "$.checkpoint", Detail: "cannot close temp checkpoint", Cause: err}
	}
	if err := os.Rename(tempName, s.pathForClip(checkpoint.ClipID)); err != nil {
		_ = os.Remove(tempName)
		return &DomainError{Code: ESchema, Path: "$.checkpoint", Detail: "cannot atomically rename checkpoint", Cause: err}
	}
	if directory, err := os.Open(s.dir); err == nil {
		_ = directory.Sync()
		_ = directory.Close()
	}
	return nil
}

func (s *CheckpointStore) ReadClip(clipID int) (ClipCheckpoint, bool, error) {
	data, err := os.ReadFile(s.pathForClip(clipID))
	if err != nil {
		if os.IsNotExist(err) {
			return ClipCheckpoint{}, false, nil
		}
		return ClipCheckpoint{}, false, &DomainError{Code: ESchema, Path: s.pathForClip(clipID), Detail: "cannot read checkpoint", Cause: err}
	}
	var checkpoint ClipCheckpoint
	if err := json.Unmarshal(data, &checkpoint); err != nil {
		return ClipCheckpoint{}, false, &DomainError{Code: ESchema, Path: s.pathForClip(clipID), Detail: "invalid checkpoint JSON", Cause: err}
	}
	if err := validateCheckpoint(checkpoint); err != nil {
		return ClipCheckpoint{}, false, err
	}
	return checkpoint, true, nil
}

// ReadAll returns checkpoints ordered by commit index, then clip id.
func (s *CheckpointStore) ReadAll() ([]ClipCheckpoint, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, &DomainError{Code: ESchema, Path: s.dir, Detail: "cannot list checkpoints", Cause: err}
	}
	checkpoints := make([]ClipCheckpoint, 0)
	seenCommit := make(map[int]int)
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasPrefix(name, "clip-") || !strings.HasSuffix(name, ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(s.dir, name))
		if err != nil {
			return nil, &DomainError{Code: ESchema, Path: filepath.Join(s.dir, name), Detail: "cannot read checkpoint", Cause: err}
		}
		var checkpoint ClipCheckpoint
		if err := json.Unmarshal(data, &checkpoint); err != nil {
			return nil, &DomainError{Code: ESchema, Path: filepath.Join(s.dir, name), Detail: "invalid checkpoint JSON", Cause: err}
		}
		if err := validateCheckpoint(checkpoint); err != nil {
			return nil, err
		}
		if existing, exists := seenCommit[checkpoint.CommitIndex]; exists {
			return nil, &DomainError{Code: ESchema, Path: filepath.Join(s.dir, name), Detail: fmt.Sprintf("duplicate commit index %d (clips %d and %d)", checkpoint.CommitIndex, existing, checkpoint.ClipID)}
		}
		seenCommit[checkpoint.CommitIndex] = checkpoint.ClipID
		checkpoints = append(checkpoints, checkpoint)
	}
	sort.Slice(checkpoints, func(i, j int) bool {
		if checkpoints[i].CommitIndex != checkpoints[j].CommitIndex {
			return checkpoints[i].CommitIndex < checkpoints[j].CommitIndex
		}
		return checkpoints[i].ClipID < checkpoints[j].ClipID
	})
	return checkpoints, nil
}

func (s *CheckpointStore) RemoveClip(clipID int) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.Remove(s.pathForClip(clipID)); err != nil && !os.IsNotExist(err) {
		return &DomainError{Code: ESchema, Path: s.pathForClip(clipID), Detail: "cannot remove checkpoint", Cause: err}
	}
	return nil
}

func (s *CheckpointStore) Summaries() ([]CheckpointSummary, error) {
	checkpoints, err := s.ReadAll()
	if err != nil {
		return nil, err
	}
	summaries := make([]CheckpointSummary, 0, len(checkpoints))
	for _, checkpoint := range checkpoints {
		summaries = append(summaries, CheckpointSummary{
			ClipID: checkpoint.ClipID, CommitIndex: checkpoint.CommitIndex,
			ContentHash: checkpoint.ContentHash, SourceVersion: checkpoint.SourceVersion,
			QuantVersion: checkpoint.QuantVersion, AlgorithmVersion: checkpoint.AlgorithmVersion,
			EnvelopeDigest: checkpoint.EnvelopeDigest, GainAdjustmentQ12: checkpoint.GainAdjustmentQ12,
		})
	}
	return summaries, nil
}

// ResultFromCheckpoint restores an in-memory calibrated result.
func ResultFromCheckpoint(checkpoint ClipCheckpoint) ClipResult {
	return ClipResult{
		ClipID: checkpoint.ClipID, ContentHash: checkpoint.ContentHash, PCMHash: checkpoint.PCMHash,
		FrameLength: checkpoint.FrameLength, FrameShift: checkpoint.FrameShift,
		Frames: cloneFrames(checkpoint.Frames), GainAdjustmentQ12: checkpoint.GainAdjustmentQ12,
		DesiredGainQ12: checkpoint.DesiredGainQ12, GainLimited: checkpoint.GainLimited,
		MeasuredSegmentEnergyQ32:   checkpoint.MeasuredSegmentEnergyQ32,
		MeasuredSegmentRmsQ16:      checkpoint.MeasuredSegmentRmsQ16,
		MeasuredSegmentDBQ8:        checkpoint.MeasuredSegmentDBQ8,
		CalibratedSegmentDBQ8:      checkpoint.CalibratedSegmentDBQ8,
		CalibratedSegmentRmsQ16:    checkpoint.CalibratedSegmentRmsQ16,
		CalibratedSegmentEnergyQ32: checkpoint.CalibratedSegmentEnergyQ32,
		MeasurementTruncations:     append([]Truncation(nil), checkpoint.MeasurementTruncations...),
		Truncations:                append([]Truncation(nil), checkpoint.Truncations...),
		EnvelopeDigest:             checkpoint.EnvelopeDigest,
		MeasurementCacheKey:        checkpoint.MeasurementCacheKey,
		CalibrationCacheKey:        checkpoint.CalibrationCacheKey,
	}
}

func measurementFromCheckpoint(checkpoint ClipCheckpoint) ClipMeasurement {
	return ClipMeasurement{
		ClipID: checkpoint.ClipID, ContentHash: checkpoint.ContentHash, PCMHash: checkpoint.PCMHash,
		FrameLength: checkpoint.FrameLength, FrameShift: checkpoint.FrameShift,
		Frames:           cloneFrames(checkpoint.Frames),
		SegmentEnergyQ32: checkpoint.MeasuredSegmentEnergyQ32,
		SegmentRmsQ16:    checkpoint.MeasuredSegmentRmsQ16,
		SegmentDBQ8:      checkpoint.MeasuredSegmentDBQ8,
		Truncations:      append([]Truncation(nil), checkpoint.MeasurementTruncations...),
	}
}
