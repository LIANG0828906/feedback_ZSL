package dialogue

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
)

type CheckpointRecord struct {
	Sequence            int                 `json:"sequence"`
	SegmentID           string              `json:"segment_id"`
	ContentHash         string              `json:"content_hash"`
	SignalHash          string              `json:"signal_hash"`
	EnvelopeDigest      string              `json:"envelope_digest"`
	GainAdjustment      int64               `json:"gain_adjustment"`
	SourceVersion       string              `json:"source_version"`
	QuantizationVersion string              `json:"quantization_version"`
	AlgorithmVersion    string              `json:"algorithm_version"`
	Envelope            *SegmentEnvelope    `json:"envelope"`
	Calibration         *SegmentCalibration `json:"calibration"`
}

type checkpointDocument struct {
	Format    string             `json:"format"`
	ProjectID string             `json:"project_id"`
	Records   []CheckpointRecord `json:"records"`
}

type CheckpointStore struct {
	mu   sync.Mutex
	path string
	doc  checkpointDocument
}

func NewCheckpointStore(path string) *CheckpointStore {
	return &CheckpointStore{path: path, doc: checkpointDocument{Format: CheckpointFormat}}
}

func (s *CheckpointStore) Path() string {
	return s.path
}

func (s *CheckpointStore) Load(projectID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			s.doc = checkpointDocument{Format: CheckpointFormat, ProjectID: projectID}
			return nil
		}
		return &Error{Code: CodeIO, Path: s.path, Message: err.Error()}
	}
	var doc checkpointDocument
	if err := json.Unmarshal(data, &doc); err != nil {
		return &Error{Code: CodeIO, Path: s.path, Message: "corrupt checkpoint: " + err.Error()}
	}
	if doc.Format != CheckpointFormat {
		return &Error{Code: CodeIO, Path: s.path, Message: "unsupported checkpoint format " + doc.Format}
	}
	if doc.ProjectID != projectID {
		return &Error{Code: CodeIO, Path: s.path, Message: fmt.Sprintf("checkpoint belongs to %s, not %s", doc.ProjectID, projectID)}
	}
	s.doc = doc
	return nil
}

// Commit writes one record atomically and serially. Existing segments keep their
// original sequence number; new segments receive the next stable sequence.
func (s *CheckpointStore) Commit(projectID string, record CheckpointRecord) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.doc.Format == "" {
		s.doc.Format = CheckpointFormat
	}
	if s.doc.ProjectID == "" {
		s.doc.ProjectID = projectID
	}
	if s.doc.ProjectID != projectID {
		return 0, &Error{Code: CodeIO, Path: s.path, Message: "checkpoint project id mismatch"}
	}
	sequence := 0
	for i := range s.doc.Records {
		if s.doc.Records[i].SegmentID == record.SegmentID {
			sequence = s.doc.Records[i].Sequence
			record.Sequence = sequence
			s.doc.Records[i] = record
			break
		}
	}
	if sequence == 0 {
		sequence = len(s.doc.Records) + 1
		record.Sequence = sequence
		s.doc.Records = append(s.doc.Records, record)
	}
	if err := s.writeLocked(); err != nil {
		return 0, err
	}
	return sequence, nil
}

func (s *CheckpointStore) writeLocked() error {
	if err := os.MkdirAll(filepath.Dir(s.path), 0o755); err != nil {
		return &Error{Code: CodeIO, Path: s.path, Message: err.Error()}
	}
	data, err := json.MarshalIndent(s.doc, "", "  ")
	if err != nil {
		return &Error{Code: CodeIO, Path: s.path, Message: err.Error()}
	}
	temp := s.path + fmt.Sprintf(".tmp-%d", os.Getpid())
	file, err := os.OpenFile(temp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return &Error{Code: CodeIO, Path: temp, Message: err.Error()}
	}
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		_ = os.Remove(temp)
		return &Error{Code: CodeIO, Path: temp, Message: err.Error()}
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		_ = os.Remove(temp)
		return &Error{Code: CodeIO, Path: temp, Message: err.Error()}
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(temp)
		return &Error{Code: CodeIO, Path: temp, Message: err.Error()}
	}
	if err := os.Rename(temp, s.path); err != nil {
		_ = os.Remove(temp)
		return &Error{Code: CodeIO, Path: s.path, Message: err.Error()}
	}
	if dir, err := os.Open(filepath.Dir(s.path)); err == nil {
		_ = dir.Sync()
		_ = dir.Close()
	}
	return nil
}

func (s *CheckpointStore) Record(segmentID string) (CheckpointRecord, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, record := range s.doc.Records {
		if record.SegmentID == segmentID {
			return record, true
		}
	}
	return CheckpointRecord{}, false
}

func (s *CheckpointStore) Records() []CheckpointRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := append([]CheckpointRecord(nil), s.doc.Records...)
	return out
}

// Validate returns checkpoint records that exactly match the current segment
// hashes and kernel versions. Any mismatch invalidates the whole segment.
func (s *CheckpointStore) Validate(project *Project) (map[string]CheckpointRecord, []string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	valid := map[string]CheckpointRecord{}
	var invalid []string
	segments := map[string]Segment{}
	for _, segment := range project.Segments() {
		segments[segment.ID] = segment
	}
	for _, record := range s.doc.Records {
		segment, exists := segments[record.SegmentID]
		if !exists {
			invalid = append(invalid, record.SegmentID)
			continue
		}
		if record.ContentHash != project.ContentHash(segment) ||
			record.SignalHash != project.SignalHash(segment) ||
			record.SourceVersion != project.Source.SchemaVersion ||
			record.QuantizationVersion != QuantizationVersion ||
			record.AlgorithmVersion != AlgorithmVersion ||
			record.Envelope == nil || record.Calibration == nil ||
			record.EnvelopeDigest != record.Envelope.Digest() ||
			record.Calibration.EnvelopeDigest != record.Envelope.Digest() ||
			record.GainAdjustment != record.Calibration.GainAdjustment {
			invalid = append(invalid, record.SegmentID)
			continue
		}
		valid[record.SegmentID] = record
	}
	return valid, invalid, nil
}

func (s *CheckpointStore) Bytes() int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	data, err := json.MarshalIndent(s.doc, "", "  ")
	if err != nil {
		return 0
	}
	return int64(len(data))
}

func (s *CheckpointStore) Reset(projectID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.doc = checkpointDocument{Format: CheckpointFormat, ProjectID: projectID}
	return s.writeLocked()
}
