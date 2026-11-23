package gait

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
)

const CheckpointSchemaVersion = "1.0"

type SnapshotEnvelope struct {
	SchemaVersion    string      `json:"schema_version"`
	SourceID         string      `json:"source_id"`
	TargetID         string      `json:"target_id"`
	Cycle            int         `json:"cycle"`
	InputHash        string      `json:"input_hash"`
	ParameterVersion string      `json:"parameter_version"`
	AlgorithmVersion string      `json:"algorithm_version"`
	ContentHash      string      `json:"content_hash"`
	Payload          CycleOutput `json:"payload"`
}

type SnapshotReport struct {
	LoadedCycles []int  `json:"loaded_cycles"`
	Invalidated  bool   `json:"invalidated"`
	Reason       string `json:"reason,omitempty"`
}

type SnapshotStore struct {
	dir string
	mu  sync.Mutex
}

func NewSnapshotStore(dir string) *SnapshotStore {
	return &SnapshotStore{dir: dir}
}

func (s *SnapshotStore) Dir() string { return s.dir }

func (s *SnapshotStore) Write(env SnapshotEnvelope) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if env.SchemaVersion == "" {
		env.SchemaVersion = CheckpointSchemaVersion
	}
	hash, err := snapshotHash(env)
	if err != nil {
		return err
	}
	env.ContentHash = hash
	data, err := json.MarshalIndent(env, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	path := filepath.Join(s.dir, snapshotFilename(env.Cycle))
	if existing, err := os.ReadFile(path); err == nil {
		var old SnapshotEnvelope
		if json.Unmarshal(existing, &old) == nil {
			if old.ContentHash == env.ContentHash {
				return nil
			}
			e := errCycle(CodeState, fmt.Sprintf("checkpoint for cycle %d is frozen with different content", env.Cycle), env.Cycle)
			return e
		}
	}
	if err := atomicWriteFile(path, data, 0o644); err != nil {
		return fmt.Errorf("write checkpoint cycle %d: %w", env.Cycle, err)
	}
	return nil
}

func (s *SnapshotStore) Load(expected SourceLoop, target TargetSkeleton, mapping Mapping,
	inputHash, parameterVersion, algorithmVersion string) (map[int]*CycleOutput, SnapshotReport, error) {
	report := SnapshotReport{}
	outputs := make(map[int]*CycleOutput)
	entries, err := os.ReadDir(s.dir)
	if os.IsNotExist(err) {
		return outputs, report, nil
	}
	if err != nil {
		return nil, report, err
	}
	paths := make([]string, 0)
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if strings.HasPrefix(name, "cycle-") && strings.HasSuffix(name, ".json") {
			paths = append(paths, filepath.Join(s.dir, name))
		}
	}
	sort.Strings(paths)
	for _, path := range paths {
		env, err := readSnapshotEnvelope(path)
		if err != nil {
			s.invalidateAll()
			report.Invalidated = true
			report.Reason = err.Error()
			return map[int]*CycleOutput{}, report, nil
		}
		reason := s.validateEnvelope(env, expected, target, mapping, inputHash, parameterVersion, algorithmVersion)
		if reason != "" {
			s.invalidateAll()
			report.Invalidated = true
			report.Reason = reason
			return map[int]*CycleOutput{}, report, nil
		}
		if _, exists := outputs[env.Cycle]; exists {
			s.invalidateAll()
			report.Invalidated = true
			report.Reason = fmt.Sprintf("duplicate checkpoint for cycle %d", env.Cycle)
			return map[int]*CycleOutput{}, report, nil
		}
		payload := env.Payload
		outputs[env.Cycle] = &payload
		report.LoadedCycles = append(report.LoadedCycles, env.Cycle)
	}
	return outputs, report, nil
}

func readSnapshotEnvelope(path string) (SnapshotEnvelope, error) {
	var env SnapshotEnvelope
	data, err := os.ReadFile(path)
	if err != nil {
		return env, err
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&env); err != nil {
		return env, fmt.Errorf("decode %s: %w", filepath.Base(path), err)
	}
	return env, nil
}

func (s *SnapshotStore) validateEnvelope(env SnapshotEnvelope, source SourceLoop, target TargetSkeleton,
	mapping Mapping, inputHash, parameterVersion, algorithmVersion string) string {
	if env.SchemaVersion != CheckpointSchemaVersion {
		return fmt.Sprintf("checkpoint schema %q does not match %q", env.SchemaVersion, CheckpointSchemaVersion)
	}
	if env.SourceID != source.ID || env.TargetID != target.ID {
		return "checkpoint source/target identity mismatch"
	}
	if env.Cycle < 0 || env.Cycle >= len(mapping.Cycles) {
		return fmt.Sprintf("checkpoint cycle %d is outside mapping", env.Cycle)
	}
	if env.InputHash != inputHash {
		return fmt.Sprintf("input hash mismatch at cycle %d", env.Cycle)
	}
	if env.ParameterVersion != parameterVersion {
		return fmt.Sprintf("parameter version mismatch at cycle %d", env.Cycle)
	}
	if env.AlgorithmVersion != algorithmVersion {
		return fmt.Sprintf("algorithm version mismatch at cycle %d", env.Cycle)
	}
	hash, err := snapshotHash(env)
	if err != nil {
		return err.Error()
	}
	if hash != env.ContentHash {
		return fmt.Sprintf("content hash mismatch at cycle %d", env.Cycle)
	}
	mc := mapping.Cycles[env.Cycle]
	if env.Payload.Cycle != env.Cycle ||
		env.Payload.SourceStartFrame != mc.SourceStartFrame ||
		env.Payload.SourceEndFrame != mc.SourceEndFrame ||
		env.Payload.TargetStartFrame != mc.TargetStartFrame ||
		env.Payload.TargetEndFrame != mc.TargetEndFrame ||
		env.Payload.Scale != mc.Scale {
		return fmt.Sprintf("checkpoint payload boundaries/scaling mismatch at cycle %d", env.Cycle)
	}
	if len(env.Payload.Frames) != mc.TargetEndFrame-mc.TargetStartFrame+1 {
		return fmt.Sprintf("checkpoint frame count mismatch at cycle %d", env.Cycle)
	}
	return ""
}

func snapshotHash(env SnapshotEnvelope) (string, error) {
	clone := env
	clone.ContentHash = ""
	return canonicalHash(clone)
}

func snapshotFilename(cycle int) string {
	return fmt.Sprintf("cycle-%06d.json", cycle)
}

func (s *SnapshotStore) invalidateAll() {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() {
			continue
		}
		if strings.HasPrefix(name, "cycle-") && strings.HasSuffix(name, ".json") ||
			strings.HasPrefix(name, ".tmp-") {
			_ = os.Remove(filepath.Join(s.dir, name))
		}
	}
}

// SnapshotNameForCycle is exposed for ordered-write assertions and tooling.
func SnapshotNameForCycle(cycle int) string {
	if cycle < 0 {
		return ""
	}
	return snapshotFilename(cycle)
}

// ParseSnapshotCycle extracts the cycle index from a canonical snapshot name.
func ParseSnapshotCycle(name string) (int, bool) {
	if !strings.HasPrefix(name, "cycle-") || !strings.HasSuffix(name, ".json") {
		return 0, false
	}
	body := strings.TrimSuffix(strings.TrimPrefix(name, "cycle-"), ".json")
	n, err := strconv.Atoi(body)
	if err != nil {
		return 0, false
	}
	return n, true
}
