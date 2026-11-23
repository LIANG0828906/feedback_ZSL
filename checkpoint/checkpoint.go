// Package checkpoint provides a process-local serialized, atomically replaced
// snapshot store. No external service or database is used.
package checkpoint

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"sync"

	"loudnessworkbench/apperr"
	"loudnessworkbench/dsp"
)

const (
	Schema           = "lw-checkpoint/1"
	AlgorithmVersion = "lw-algo/1.0.0"
)

var ErrNoSnapshot = errors.New("no checkpoint snapshot")

type Record struct {
	Index                     int             `json:"index"`
	SegmentID                 string          `json:"segment_id"`
	State                     string          `json:"state"`
	SourceVersion             string          `json:"source_version"`
	ParametersHash            string          `json:"parameters_hash"`
	AlgorithmVersion          string          `json:"algorithm_version"`
	InputHash                 string          `json:"input_hash"`
	OutputHash                string          `json:"output_hash"`
	NormalizationGainDB       float64         `json:"normalization_gain_db"`
	SegmentIdealGainDB        float64         `json:"segment_ideal_gain_db"`
	CompressionMaxReductionDB float64         `json:"compression_max_reduction_db"`
	LimiterSafetyTrimDB       float64         `json:"limiter_safety_trim_db"`
	SourceMeasurement         dsp.Measurement `json:"source_measurement"`
	CompressedMeasurement     dsp.Measurement `json:"compressed_measurement"`
	OutputMeasurement         dsp.Measurement `json:"output_measurement"`
	OutputFrames              int             `json:"output_frames"`
	OutputChannels            int             `json:"output_channels"`
}

type Snapshot struct {
	Schema              string   `json:"schema"`
	SnapshotHash        string   `json:"snapshot_hash"`
	AlgorithmVersion    string   `json:"algorithm_version"`
	ProjectID           string   `json:"project_id"`
	SourceVersion       string   `json:"source_version"`
	ParametersHash      string   `json:"parameters_hash"`
	TargetLUFS          float64  `json:"target_lufs"`
	ProgramLoudnessLUFS float64  `json:"program_loudness_lufs"`
	ProgramTruePeakDBTP float64  `json:"program_true_peak_dbtp"`
	GlobalGainDB        float64  `json:"global_gain_db"`
	Records             []Record `json:"records"`
}

type hashBody struct {
	Schema              string   `json:"schema"`
	AlgorithmVersion    string   `json:"algorithm_version"`
	ProjectID           string   `json:"project_id"`
	SourceVersion       string   `json:"source_version"`
	ParametersHash      string   `json:"parameters_hash"`
	TargetLUFS          float64  `json:"target_lufs"`
	ProgramLoudnessLUFS float64  `json:"program_loudness_lufs"`
	ProgramTruePeakDBTP float64  `json:"program_true_peak_dbtp"`
	GlobalGainDB        float64  `json:"global_gain_db"`
	Records             []Record `json:"records"`
}

func (s Snapshot) computeHash() string {
	records := append([]Record(nil), s.Records...)
	sort.Slice(records, func(i, j int) bool { return records[i].Index < records[j].Index })
	body := hashBody{
		Schema:              s.Schema,
		AlgorithmVersion:    s.AlgorithmVersion,
		ProjectID:           s.ProjectID,
		SourceVersion:       s.SourceVersion,
		ParametersHash:      s.ParametersHash,
		TargetLUFS:          s.TargetLUFS,
		ProgramLoudnessLUFS: s.ProgramLoudnessLUFS,
		ProgramTruePeakDBTP: s.ProgramTruePeakDBTP,
		GlobalGainDB:        s.GlobalGainDB,
		Records:             records,
	}
	data, _ := json.Marshal(body)
	sum := sha256.Sum256(data)
	return fmt.Sprintf("%x", sum)
}

type Store struct {
	mu   sync.Mutex
	dir  string
	path string
}

var safeName = regexp.MustCompile(`[^a-zA-Z0-9._-]+`)

func NewStore(dir, projectID string) *Store {
	name := safeName.ReplaceAllString(projectID, "_")
	if name == "" {
		name = "project"
	}
	sum := sha256.Sum256([]byte(projectID))
	name = fmt.Sprintf("%s-%x.json", name, sum[:6])
	return &Store{dir: dir, path: filepath.Join(dir, name)}
}

func (s *Store) Path() string {
	return s.path
}

// Save serializes writers with a mutex and atomically replaces the snapshot.
func (s *Store) Save(snapshot Snapshot) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if snapshot.AlgorithmVersion == "" {
		snapshot.AlgorithmVersion = AlgorithmVersion
	}
	snapshot.Schema = Schema
	sort.Slice(snapshot.Records, func(i, j int) bool { return snapshot.Records[i].Index < snapshot.Records[j].Index })
	snapshot.SnapshotHash = snapshot.computeHash()
	data, err := json.MarshalIndent(snapshot, "", "  ")
	if err != nil {
		return apperr.Wrap(apperr.CodeCheckpoint, "cannot encode checkpoint", err)
	}
	data = append(data, '\n')
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return apperr.Wrap(apperr.CodeCheckpoint, "cannot create checkpoint directory", err)
	}
	temp, err := os.CreateTemp(s.dir, ".checkpoint-*.tmp")
	if err != nil {
		return apperr.Wrap(apperr.CodeCheckpoint, "cannot create checkpoint temp file", err)
	}
	tempName := temp.Name()
	cleanup := func() {
		_ = temp.Close()
		_ = os.Remove(tempName)
	}
	if _, err := temp.Write(data); err != nil {
		cleanup()
		return apperr.Wrap(apperr.CodeCheckpoint, "cannot write checkpoint", err)
	}
	if err := temp.Sync(); err != nil {
		cleanup()
		return apperr.Wrap(apperr.CodeCheckpoint, "cannot sync checkpoint", err)
	}
	if err := temp.Close(); err != nil {
		_ = os.Remove(tempName)
		return apperr.Wrap(apperr.CodeCheckpoint, "cannot close checkpoint", err)
	}
	if err := os.Rename(tempName, s.path); err != nil {
		_ = os.Remove(tempName)
		return apperr.Wrap(apperr.CodeCheckpoint, "cannot atomically replace checkpoint", err)
	}
	if dir, err := os.Open(s.dir); err == nil {
		_ = dir.Sync()
		_ = dir.Close()
	}
	return nil
}

func (s *Store) Load() (*Snapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNoSnapshot
	}
	if err != nil {
		return nil, apperr.Wrap(apperr.CodeCheckpoint, "cannot read checkpoint", err).WithPath(s.path)
	}
	var snapshot Snapshot
	if err := json.Unmarshal(data, &snapshot); err != nil {
		return nil, apperr.Wrap(apperr.CodeCheckpoint, "checkpoint JSON is corrupt", err).WithPath(s.path)
	}
	if snapshot.Schema != Schema {
		return nil, apperr.New(apperr.CodeCheckpoint, "checkpoint schema mismatch").
			WithField("schema").WithActual(snapshot.Schema)
	}
	expected := snapshot.computeHash()
	if snapshot.SnapshotHash != expected {
		return nil, apperr.New(apperr.CodeCheckpoint, "checkpoint hash mismatch").
			WithField("snapshot_hash").WithExpected(expected).WithActual(snapshot.SnapshotHash)
	}
	sort.Slice(snapshot.Records, func(i, j int) bool { return snapshot.Records[i].Index < snapshot.Records[j].Index })
	return &snapshot, nil
}

func (s *Store) Invalidate() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.Remove(s.path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return apperr.Wrap(apperr.CodeCheckpoint, "cannot invalidate checkpoint", err).WithPath(s.path)
	}
	return nil
}
