package checkpoint

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"obsproj/internal/model"
)

const (
	FormatVersion = "obs-checkpoint/v1"
	FileName      = "session.ckpt"
)

type Record struct {
	Sequence    int                `json:"sequence"`
	SessionHash string             `json:"session_hash"`
	Algorithm   string             `json:"algorithm_version"`
	PrevHash    string             `json:"prev_hash"`
	Commit      model.WindowCommit `json:"commit"`
	RecordHash  string             `json:"record_hash"`
}

type document struct {
	FormatVersion    string   `json:"format_version"`
	SessionHash      string   `json:"session_hash"`
	AlgorithmVersion string   `json:"algorithm_version"`
	Records          []Record `json:"records"`
}

type Store struct {
	mu          sync.Mutex
	dir         string
	path        string
	lockPath    string
	sessionHash string
	records     []Record
	loaded      bool
	invalidFrom int
	invalidWhy  string
}

func Open(dir, sessionHash string) (*Store, error) {
	if dir == "" {
		return nil, model.NewError(model.CodeIO, "checkpoint directory is empty")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, &model.KernelError{
			Code:    model.CodeIO,
			Message: "failed to create checkpoint directory",
			Cause:   err.Error(),
		}
	}
	return &Store{
		dir:         dir,
		path:        filepath.Join(dir, FileName),
		lockPath:    filepath.Join(dir, FileName+".lock"),
		sessionHash: sessionHash,
	}, nil
}

func (s *Store) Path() string { return s.path }

func (s *Store) Commit(commit model.WindowCommit) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	unlock, err := lockFile(s.lockPath)
	if err != nil {
		return err
	}
	defer unlock()
	s.resetCache()
	if err := s.ensureLoaded(); err != nil {
		return err
	}
	if commit.SessionHash != s.sessionHash {
		return &model.KernelError{
			Code:    model.CodeState,
			Message: "checkpoint commit belongs to a different session",
			Window:  commit.Window,
			Field:   "session_hash",
		}
	}
	if commit.AlgorithmVersion != model.AlgorithmVersion {
		return &model.KernelError{
			Code:    model.CodeState,
			Message: "checkpoint algorithm version mismatch",
			Window:  commit.Window,
			Field:   "algorithm_version",
		}
	}
	if err := validateCommitHash(commit); err != nil {
		return err
	}
	if len(s.records) > 0 {
		last := s.records[len(s.records)-1]
		if commit.Window <= last.Commit.Window {
			return &model.KernelError{
				Code:        model.CodeState,
				Message:     "checkpoint window is not strictly increasing",
				Window:      commit.Window,
				State:       fmt.Sprintf("last_window=%d", last.Commit.Window),
				TargetState: fmt.Sprintf("window>%d", last.Commit.Window),
				Progress:    last.Commit.Window,
			}
		}
	}
	prevHash := genesisHash(s.sessionHash)
	if len(s.records) > 0 {
		prevHash = s.records[len(s.records)-1].RecordHash
	}
	record := Record{
		Sequence:    len(s.records) + 1,
		SessionHash: s.sessionHash,
		Algorithm:   model.AlgorithmVersion,
		PrevHash:    prevHash,
		Commit:      commit,
	}
	record.RecordHash = computeRecordHash(record)
	records := append(append([]Record(nil), s.records...), record)
	if err := s.writeDocument(records); err != nil {
		return err
	}
	s.records = records
	return nil
}

func (s *Store) Load() ([]model.WindowCommit, model.RecoveryReport, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	unlock, err := lockFile(s.lockPath)
	if err != nil {
		return nil, model.RecoveryReport{}, err
	}
	defer unlock()
	s.resetCache()
	if err := s.ensureLoaded(); err != nil {
		return nil, model.RecoveryReport{}, err
	}
	report := model.RecoveryReport{ValidRecords: len(s.records)}
	if s.invalidFrom > 0 {
		report.InvalidatedFrom = s.invalidFrom
		report.Reason = s.invalidWhy
	}
	commits := make([]model.WindowCommit, 0, len(s.records))
	for _, record := range s.records {
		commits = append(commits, record.Commit)
	}
	return commits, report, nil
}

func (s *Store) resetCache() {
	s.loaded = false
	s.records = nil
	s.invalidFrom = 0
	s.invalidWhy = ""
}

func (s *Store) ensureLoaded() error {
	if s.loaded {
		return nil
	}
	data, err := os.ReadFile(s.path)
	if err != nil {
		if os.IsNotExist(err) {
			s.loaded = true
			return nil
		}
		return &model.KernelError{
			Code:    model.CodeIO,
			Message: "failed to read checkpoint",
			Cause:   err.Error(),
		}
	}
	var doc document
	if err := json.Unmarshal(data, &doc); err != nil {
		return &model.KernelError{
			Code:    model.CodeDecode,
			Message: "checkpoint document is not valid JSON",
			Cause:   err.Error(),
		}
	}
	if doc.FormatVersion != FormatVersion {
		return &model.KernelError{
			Code:    model.CodeDecode,
			Message: "unknown checkpoint format",
			Field:   "format_version",
		}
	}
	s.loaded = true
	if doc.SessionHash != s.sessionHash {
		return &model.KernelError{
			Code:    model.CodeState,
			Message: "checkpoint session hash mismatch; checkpoint belongs to another session",
			Field:   "session_hash",
		}
	}
	if doc.AlgorithmVersion != model.AlgorithmVersion {
		return &model.KernelError{
			Code:    model.CodeState,
			Message: "checkpoint algorithm version mismatch",
			Field:   "algorithm_version",
		}
	}
	prev := genesisHash(s.sessionHash)
	var valid []Record
	for i, record := range doc.Records {
		if record.Sequence != i+1 ||
			record.SessionHash != s.sessionHash ||
			record.Algorithm != model.AlgorithmVersion ||
			record.Commit.AlgorithmVersion != model.AlgorithmVersion ||
			record.Commit.SessionHash != s.sessionHash ||
			record.PrevHash != prev ||
			computeRecordHash(record) != record.RecordHash {
			s.invalidFrom = i + 1
			s.invalidWhy = "checkpoint record hash, sequence, or algorithm version mismatch"
			break
		}
		if err := validateCommitHash(record.Commit); err != nil {
			s.invalidFrom = i + 1
			s.invalidWhy = err.Error()
			break
		}
		if len(valid) > 0 && record.Commit.Window <= valid[len(valid)-1].Commit.Window {
			s.invalidFrom = i + 1
			s.invalidWhy = "checkpoint window order is not strictly increasing"
			break
		}
		valid = append(valid, record)
		prev = record.RecordHash
	}
	s.records = valid
	if s.invalidFrom == 0 && len(valid) < len(doc.Records) {
		s.invalidFrom = len(valid) + 1
		s.invalidWhy = "checkpoint record validation stopped early"
	}
	return nil
}

func validateCommitHash(commit model.WindowCommit) *model.KernelError {
	if commit.ContentHash == "" {
		return &model.KernelError{
			Code:    model.CodeDecode,
			Message: "checkpoint window has no content hash",
			Window:  commit.Window,
			Field:   "content_hash",
		}
	}
	copyCommit := commit
	copyCommit.ContentHash = ""
	expected := model.HashJSON(copyCommit)
	if expected != commit.ContentHash {
		return &model.KernelError{
			Code:    model.CodeDecode,
			Message: "checkpoint window content hash mismatch",
			Window:  commit.Window,
			Field:   "content_hash",
		}
	}
	for _, screen := range commit.Screens {
		if screen.ContentHash == "" {
			return &model.KernelError{
				Code:     model.CodeDecode,
				Message:  "checkpoint screen has no content hash",
				Window:   commit.Window,
				ScreenID: screen.ScreenID,
				Field:    "content_hash",
			}
		}
		copyScreen := screen
		copyScreen.ContentHash = ""
		if model.HashJSON(copyScreen) != screen.ContentHash {
			return &model.KernelError{
				Code:     model.CodeDecode,
				Message:  "checkpoint screen content hash mismatch",
				Window:   commit.Window,
				ScreenID: screen.ScreenID,
				Field:    "content_hash",
			}
		}
	}
	return nil
}

func (s *Store) writeDocument(records []Record) error {
	doc := document{
		FormatVersion:    FormatVersion,
		SessionHash:      s.sessionHash,
		AlgorithmVersion: model.AlgorithmVersion,
		Records:          records,
	}
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return &model.KernelError{
			Code:    model.CodeDecode,
			Message: "failed to encode checkpoint",
			Cause:   err.Error(),
		}
	}
	data = append(data, '\n')
	tmp, err := os.CreateTemp(s.dir, ".session-ckpt-*")
	if err != nil {
		return &model.KernelError{
			Code:    model.CodeIO,
			Message: "failed to create checkpoint temp file",
			Cause:   err.Error(),
		}
	}
	tmpName := tmp.Name()
	cleanup := func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
	}
	if err := tmp.Chmod(0o600); err != nil {
		cleanup()
		return &model.KernelError{Code: model.CodeIO, Message: "failed to chmod checkpoint", Cause: err.Error()}
	}
	if _, err := tmp.Write(data); err != nil {
		cleanup()
		return &model.KernelError{Code: model.CodeIO, Message: "failed to write checkpoint", Cause: err.Error()}
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return &model.KernelError{Code: model.CodeIO, Message: "failed to fsync checkpoint", Cause: err.Error()}
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return &model.KernelError{Code: model.CodeIO, Message: "failed to close checkpoint", Cause: err.Error()}
	}
	if err := os.Rename(tmpName, s.path); err != nil {
		_ = os.Remove(tmpName)
		return &model.KernelError{Code: model.CodeIO, Message: "failed to atomically replace checkpoint", Cause: err.Error()}
	}
	dir, err := os.Open(s.dir)
	if err == nil {
		_ = dir.Sync()
		_ = dir.Close()
	}
	return nil
}

func genesisHash(sessionHash string) string {
	sum := sha256.Sum256([]byte(FormatVersion + "|" + model.AlgorithmVersion + "|" + sessionHash))
	return hex.EncodeToString(sum[:])
}

func computeRecordHash(record Record) string {
	copyRecord := record
	copyRecord.RecordHash = ""
	return model.HashJSON(copyRecord)
}
