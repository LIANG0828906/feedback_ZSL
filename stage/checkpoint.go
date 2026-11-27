package stage

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

const checkpointFormat = "stage.checkpoint.v1"

type checkpointEnvelope struct {
	Format           string       `json:"format"`
	Sequence         int          `json:"sequence"`
	WindowNumber     int          `json:"window_number"`
	ParameterVersion string       `json:"parameter_version"`
	AlgorithmVersion string       `json:"algorithm_version"`
	ContentHash      string       `json:"content_hash"`
	Result           WindowResult `json:"result"`
}

// CheckpointStore uses one file per committed window, written to a temp file,
// fsynced, renamed, then directory-fsynced. Commit calls are serialized.
type CheckpointStore struct {
	dir string
	mu  sync.Mutex
}

func NewCheckpointStore(dir string) *CheckpointStore {
	return &CheckpointStore{dir: dir}
}

func (s *CheckpointStore) Dir() string {
	if s == nil {
		return ""
	}
	return s.dir
}

func (s *CheckpointStore) Commit(w WindowResult, sequence int) error {
	if s == nil {
		return domainError(CodeValue, "nil checkpoint store", "checkpoint")
	}
	if sequence <= 0 {
		return &Error{Code: CodeValue, Message: "checkpoint sequence must be positive", Field: "checkpoint.sequence"}
	}
	if w.WindowNumber <= 0 {
		return &Error{Code: CodeValue, Message: "window number must be positive", Field: "checkpoint.window_number"}
	}
	hash, err := windowContentHash(w)
	if err != nil {
		return err
	}
	if w.ContentHash != "" && w.ContentHash != hash {
		return &Error{
			Code: CodeVersion, Message: "window content hash mismatch before commit",
			Field: "checkpoint.content_hash", WindowNumber: w.WindowNumber,
		}
	}
	w.ContentHash = hash
	env := checkpointEnvelope{
		Format: checkpointFormat, Sequence: sequence, WindowNumber: w.WindowNumber,
		ParameterVersion: w.ParameterVersion, AlgorithmVersion: w.AlgorithmVersion,
		ContentHash: hash, Result: w,
	}
	data, err := json.MarshalIndent(env, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal checkpoint: %w", err)
	}
	data = append(data, '\n')

	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return fmt.Errorf("create checkpoint dir: %w", err)
	}
	tmp, err := os.CreateTemp(s.dir, ".window-*.tmp")
	if err != nil {
		return fmt.Errorf("create checkpoint temp: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write checkpoint temp: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync checkpoint temp: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close checkpoint temp: %w", err)
	}
	final := filepath.Join(s.dir, checkpointName(w.WindowNumber))
	if err := os.Rename(tmpName, final); err != nil {
		return fmt.Errorf("rename checkpoint: %w", err)
	}
	dirFile, err := os.Open(s.dir)
	if err == nil {
		_ = dirFile.Sync()
		_ = dirFile.Close()
	}
	return nil
}

func checkpointName(window int) string {
	return fmt.Sprintf("window-%06d.json", window)
}

// LoadAll is all-or-nothing. A malformed file, hash mismatch, duplicate
// sequence, or parameter/algorithm mismatch rejects the entire recovery.
func (s *CheckpointStore) LoadAll(expectedParameterVersion, expectedAlgorithmVersion string) ([]WindowResult, error) {
	if s == nil {
		return nil, domainError(CodeValue, "nil checkpoint store", "checkpoint")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, &Error{Code: CodeVersion, Message: "cannot read checkpoint directory: " + err.Error(), Field: "checkpoint.dir"}
	}
	names := make([]string, 0)
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if strings.HasSuffix(entry.Name(), ".json") && !strings.HasPrefix(entry.Name(), "window-") {
			return nil, versionError(entry.Name(), "unexpected JSON file in checkpoint directory")
		}
		if !strings.HasPrefix(entry.Name(), "window-") || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		names = append(names, entry.Name())
	}
	sort.Strings(names)
	out := make([]WindowResult, 0, len(names))
	seenFileWindow := map[int]struct{}{}
	for i, name := range names {
		path := filepath.Join(s.dir, name)
		env, err := readCheckpoint(path)
		if err != nil {
			return nil, err
		}
		if env.Format != checkpointFormat {
			return nil, versionError(name, "unknown checkpoint format")
		}
		if env.ParameterVersion != expectedParameterVersion {
			return nil, &Error{
				Code: CodeVersion, Message: "checkpoint parameter version mismatch",
				Field: "checkpoint.parameter_version", Version: env.ParameterVersion,
				Expected: expectedParameterVersion,
			}
		}
		if env.AlgorithmVersion != expectedAlgorithmVersion {
			return nil, &Error{
				Code: CodeVersion, Message: "checkpoint algorithm version mismatch",
				Field: "checkpoint.algorithm_version", Version: env.AlgorithmVersion,
				Expected: expectedAlgorithmVersion,
			}
		}
		if env.Sequence != i+1 {
			return nil, versionError(name, "non-contiguous commit sequence")
		}
		if env.WindowNumber <= 0 {
			return nil, versionError(name, "invalid window number")
		}
		if name != checkpointName(env.WindowNumber) {
			return nil, versionError(name, "window number does not match file name")
		}
		if _, ok := seenFileWindow[env.WindowNumber]; ok {
			return nil, versionError(name, "duplicate window")
		}
		seenFileWindow[env.WindowNumber] = struct{}{}
		computed, err := windowContentHash(env.Result)
		if err != nil {
			return nil, versionError(name, "cannot hash checkpoint result")
		}
		if env.ContentHash == "" || computed != env.ContentHash || env.Result.ContentHash != env.ContentHash {
			return nil, &Error{
				Code: CodeVersion, Message: "checkpoint content hash mismatch",
				Field: "checkpoint.content_hash", WindowNumber: env.WindowNumber,
			}
		}
		if env.Result.ParameterVersion != expectedParameterVersion || env.Result.AlgorithmVersion != expectedAlgorithmVersion {
			return nil, versionError(name, "checkpoint result version mismatch")
		}
		out = append(out, env.Result)
	}
	return out, nil
}

func readCheckpoint(path string) (checkpointEnvelope, error) {
	f, err := os.Open(path)
	if err != nil {
		return checkpointEnvelope{}, versionError(filepath.Base(path), "cannot open checkpoint")
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, 64<<20))
	if err != nil {
		return checkpointEnvelope{}, versionError(filepath.Base(path), "cannot read checkpoint")
	}
	var env checkpointEnvelope
	if err := json.Unmarshal(data, &env); err != nil {
		return checkpointEnvelope{}, versionError(filepath.Base(path), "invalid checkpoint JSON")
	}
	return env, nil
}

func versionError(field, message string) *Error {
	return &Error{Code: CodeVersion, Message: message, Field: "checkpoint." + field, Version: checkpointFormat}
}
