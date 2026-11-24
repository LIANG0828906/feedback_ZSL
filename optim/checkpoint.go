// Package optim implements photometric optimization and versioned, atomic
// optimization-window checkpoints.
package optim

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sync"
	"syscall"

	"diffrender/errs"
	"diffrender/render"
	"diffrender/scene"
)

const CheckpointFormatVersion = 1

// Metadata binds checkpoints to a task, source schema, algorithm, and config.
type Metadata struct {
	SourceVersion    string `json:"sourceVersion"`
	AlgorithmVersion string `json:"algorithmVersion"`
	TaskHash         string `json:"taskHash"`
	ConfigHash       string `json:"configHash"`
}

// State is the complete deterministic optimizer state at a committed window.
type State struct {
	Iteration     int     `json:"iteration"`
	Loss          float64 `json:"loss"`
	StepScale     float64 `json:"stepScale"`
	GradientNorm  float64 `json:"gradientNorm"`
	AcceptedSteps int     `json:"acceptedSteps"`
	RejectedSteps int     `json:"rejectedSteps"`
}

// BlockGradient is a serialized per-block gradient accumulation buffer.
type BlockGradient struct {
	View   int       `json:"view"`
	Index  int       `json:"index"`
	Start  int       `json:"start"`
	End    int       `json:"end"`
	Values []float64 `json:"values"`
}

// Checkpoint is one immutable committed optimization window.
type Checkpoint struct {
	FormatVersion    int             `json:"formatVersion"`
	Window           int             `json:"window"`
	State            State           `json:"state"`
	Scene            *scene.Scene    `json:"scene"`
	Parameters       []float64       `json:"parameters"`
	ParameterIDs     []string        `json:"parameterIds"`
	BlockGradients   []BlockGradient `json:"blockGradients"`
	Losses           []float64       `json:"losses"`
	SourceVersion    string          `json:"sourceVersion"`
	WorkingVersion   string          `json:"workingVersion"`
	AlgorithmVersion string          `json:"algorithmVersion"`
	TaskHash         string          `json:"taskHash"`
	ConfigHash       string          `json:"configHash"`
	ContentHash      string          `json:"contentHash"`
}

// WindowInfo is the stable commit sequence record returned to callers.
type WindowInfo struct {
	Index       int     `json:"index"`
	Iteration   int     `json:"iteration"`
	Loss        float64 `json:"loss"`
	ContentHash string  `json:"contentHash"`
}

// Snapshot is the result of validating a checkpoint directory. Invalidated is
// true when any hash or version differs; callers must then recompute from zero.
type Snapshot struct {
	Valid       bool
	Invalidated bool
	Reason      string
	Windows     []WindowInfo
	Checkpoints []*Checkpoint
}

type manifest struct {
	FormatVersion    int    `json:"formatVersion"`
	Count            int    `json:"count"`
	LastContentHash  string `json:"lastContentHash"`
	SourceVersion    string `json:"sourceVersion"`
	AlgorithmVersion string `json:"algorithmVersion"`
	TaskHash         string `json:"taskHash"`
	ConfigHash       string `json:"configHash"`
}

// Store is an atomic, serialized checkpoint directory.
type Store struct {
	dir  string
	meta Metadata
	mu   sync.Mutex
}

func NewStore(dir string, meta Metadata) *Store {
	return &Store{dir: dir, meta: meta}
}

func (s *Store) Dir() string { return s.dir }

func (s *Store) windowPath(i int) string {
	return filepath.Join(s.dir, fmt.Sprintf("window-%06d.json", i))
}

func (s *Store) manifestPath() string { return filepath.Join(s.dir, "manifest.json") }

// Load validates the manifest and every committed window. It intentionally
// rejects the entire snapshot on the first mismatch or missing file.
func (s *Store) Load() (Snapshot, error) {
	lock, err := s.acquire(false)
	if err != nil {
		return Snapshot{}, err
	}
	defer s.release(lock)
	return s.loadUnlocked()
}

func (s *Store) loadUnlocked() (Snapshot, error) {
	data, err := os.ReadFile(s.manifestPath())
	if errors.Is(err, os.ErrNotExist) {
		return Snapshot{}, nil
	}
	if err != nil {
		return Snapshot{Invalidated: true, Reason: "cannot read manifest: " + err.Error()}, nil
	}
	var m manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return invalidSnapshot("manifest JSON is invalid: " + err.Error()), nil
	}
	if m.FormatVersion != CheckpointFormatVersion {
		return invalidSnapshot(fmt.Sprintf("manifest format version %d is unsupported", m.FormatVersion)), nil
	}
	if m.Count < 0 {
		return invalidSnapshot("manifest count is negative"), nil
	}
	if m.SourceVersion != s.meta.SourceVersion ||
		m.AlgorithmVersion != s.meta.AlgorithmVersion ||
		m.TaskHash != s.meta.TaskHash ||
		m.ConfigHash != s.meta.ConfigHash {
		return invalidSnapshot("manifest task, source, algorithm, or config metadata mismatch"), nil
	}
	snap := Snapshot{Valid: true}
	lastIteration := -1
	for i := 0; i < m.Count; i++ {
		raw, err := os.ReadFile(s.windowPath(i))
		if err != nil {
			return invalidSnapshot(fmt.Sprintf("cannot read window %d: %v", i, err)), nil
		}
		var cp Checkpoint
		if err := json.Unmarshal(raw, &cp); err != nil {
			return invalidSnapshot(fmt.Sprintf("window %d JSON is invalid: %v", i, err)), nil
		}
		if err := validateCheckpoint(&cp, s.meta, i); err != nil {
			return invalidSnapshot(fmt.Sprintf("window %d is invalid: %v", i, err)), nil
		}
		if cp.State.Iteration <= lastIteration {
			return invalidSnapshot(fmt.Sprintf("window %d iteration %d is not strictly increasing", i, cp.State.Iteration)), nil
		}
		lastIteration = cp.State.Iteration
		snap.Checkpoints = append(snap.Checkpoints, cloneCheckpoint(&cp))
		snap.Windows = append(snap.Windows, WindowInfo{
			Index: i, Iteration: cp.State.Iteration, Loss: cp.State.Loss, ContentHash: cp.ContentHash,
		})
	}
	if m.Count > 0 {
		last := snap.Checkpoints[len(snap.Checkpoints)-1]
		if last.ContentHash != m.LastContentHash || last.Window != m.Count-1 {
			return invalidSnapshot("manifest head does not match the last window"), nil
		}
	}
	return snap, nil
}

// Commit appends exactly the next window. Existing files are never overwritten.
func (s *Store) Commit(cp *Checkpoint) error {
	if cp == nil {
		return errs.New(errs.CodeCheckpt, s.dir, "checkpoint", "checkpoint is nil")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return errs.Wrap(err, errs.CodeCheckpt, s.dir, "checkpoint", "cannot create checkpoint directory")
	}
	lock, err := s.acquire(true)
	if err != nil {
		return err
	}
	defer s.release(lock)

	snap, err := s.loadUnlocked()
	if err != nil {
		return err
	}
	if snap.Invalidated {
		return errs.New(errs.CodeCheckpt, s.dir, "manifest", "existing snapshot is invalid: %s", snap.Reason)
	}
	count := len(snap.Windows)
	if cp.Window != count {
		return errs.New(errs.CodeCheckpt, s.dir, fmt.Sprintf("window[%d]", cp.Window), "expected next window %d", count)
	}
	out := cloneCheckpoint(cp)
	out.FormatVersion = CheckpointFormatVersion
	out.SourceVersion = s.meta.SourceVersion
	out.WorkingVersion = scene.WorkingVersion
	out.AlgorithmVersion = s.meta.AlgorithmVersion
	out.TaskHash = s.meta.TaskHash
	out.ConfigHash = s.meta.ConfigHash
	hash, err := checkpointHash(out)
	if err != nil {
		return err
	}
	out.ContentHash = hash
	if err := validateCheckpoint(out, s.meta, count); err != nil {
		return errs.New(errs.CodeCheckpt, s.dir, fmt.Sprintf("window[%d]", count), "refusing invalid checkpoint: %v", err)
	}
	data, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return errs.Wrap(err, errs.CodeCheckpt, s.dir, "checkpoint", "cannot encode checkpoint")
	}
	data = append(data, '\n')
	if err := writeAtomic(s.windowPath(count), data); err != nil {
		return err
	}
	m := manifest{
		FormatVersion: CheckpointFormatVersion, Count: count + 1, LastContentHash: out.ContentHash,
		SourceVersion: s.meta.SourceVersion, AlgorithmVersion: s.meta.AlgorithmVersion,
		TaskHash: s.meta.TaskHash, ConfigHash: s.meta.ConfigHash,
	}
	mb, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		_ = os.Remove(s.windowPath(count))
		return errs.Wrap(err, errs.CodeCheckpt, s.dir, "manifest", "cannot encode manifest")
	}
	mb = append(mb, '\n')
	if err := writeAtomic(s.manifestPath(), mb); err != nil {
		_ = os.Remove(s.windowPath(count))
		return err
	}
	return nil
}

// Invalidate explicitly quarantines a rejected snapshot and removes its
// manifest/windows so subsequent recovery starts from iteration zero.
func (s *Store) Invalidate(reason string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return errs.Wrap(err, errs.CodeCheckpt, s.dir, "checkpoint", "cannot create checkpoint directory")
	}
	lock, err := s.acquire(true)
	if err != nil {
		return err
	}
	defer s.release(lock)
	if reason != "" {
		marker := []byte("snapshot invalidated: " + reason + "\n")
		_ = writeAtomic(filepath.Join(s.dir, "invalidated.txt"), marker)
	}
	if err := os.Remove(s.manifestPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return errs.Wrap(err, errs.CodeCheckpt, s.dir, "manifest", "cannot remove invalid manifest")
	}
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return errs.Wrap(err, errs.CodeCheckpt, s.dir, "checkpoint", "cannot list checkpoint directory")
	}
	for _, entry := range entries {
		if len(entry.Name()) >= len("window-") && entry.Name()[:len("window-")] == "window-" {
			if err := os.Remove(filepath.Join(s.dir, entry.Name())); err != nil && !errors.Is(err, os.ErrNotExist) {
				return errs.Wrap(err, errs.CodeCheckpt, s.dir, entry.Name(), "cannot remove invalid window")
			}
		}
	}
	return nil
}

func invalidSnapshot(reason string) Snapshot {
	return Snapshot{Invalidated: true, Reason: reason}
}

func validateCheckpoint(cp *Checkpoint, meta Metadata, window int) error {
	if cp.FormatVersion != CheckpointFormatVersion {
		return fmt.Errorf("format version %d is unsupported", cp.FormatVersion)
	}
	if cp.Window != window {
		return fmt.Errorf("window index is %d, expected %d", cp.Window, window)
	}
	if cp.SourceVersion != meta.SourceVersion || cp.AlgorithmVersion != meta.AlgorithmVersion ||
		cp.TaskHash != meta.TaskHash || cp.ConfigHash != meta.ConfigHash {
		return fmt.Errorf("task/source/algorithm/config metadata mismatch")
	}
	if cp.WorkingVersion != scene.WorkingVersion {
		return fmt.Errorf("working scene version %q is unsupported", cp.WorkingVersion)
	}
	if cp.Scene == nil {
		return fmt.Errorf("scene is missing")
	}
	if err := scene.Validate(cp.Scene, "<checkpoint>"); err != nil {
		return fmt.Errorf("scene is invalid: %w", err)
	}
	if cp.Scene.SourceVersion != cp.SourceVersion {
		return fmt.Errorf("scene source version %q does not match checkpoint source %q", cp.Scene.SourceVersion, cp.SourceVersion)
	}
	specs, err := render.ParameterSpecs(cp.Scene)
	if err != nil {
		return fmt.Errorf("parameters cannot be derived: %w", err)
	}
	if len(cp.ParameterIDs) != len(specs) || len(cp.Parameters) != len(specs) {
		return fmt.Errorf("parameter vector length mismatch")
	}
	for i, spec := range specs {
		if cp.ParameterIDs[i] != spec.ID {
			return fmt.Errorf("parameter id %d is %q, expected %q", i, cp.ParameterIDs[i], spec.ID)
		}
		if math.Float64bits(cp.Parameters[i]) != math.Float64bits(render.ParameterValues(cp.Scene, specs)[i]) {
			return fmt.Errorf("parameter %q does not match scene", cp.ParameterIDs[i])
		}
	}
	if len(cp.Losses) == 0 {
		return fmt.Errorf("loss sequence is empty")
	}
	if len(cp.Losses) != cp.State.Iteration+1 {
		return fmt.Errorf("loss sequence length %d does not match iteration %d", len(cp.Losses), cp.State.Iteration)
	}
	for i := 1; i < len(cp.Losses); i++ {
		if cp.Losses[i] > cp.Losses[i-1] {
			return fmt.Errorf("loss sequence increases at index %d", i)
		}
	}
	if cp.State.Iteration < 0 || cp.State.StepScale < 0 || !finite(cp.State.Loss) || !finite(cp.State.StepScale) {
		return fmt.Errorf("optimizer state is invalid")
	}
	if math.Float64bits(cp.Losses[len(cp.Losses)-1]) != math.Float64bits(cp.State.Loss) {
		return fmt.Errorf("last loss does not match optimizer state")
	}
	got, err := checkpointHash(cp)
	if err != nil {
		return err
	}
	if cp.ContentHash != got {
		return fmt.Errorf("content hash mismatch: want %s, got %s", got, cp.ContentHash)
	}
	for _, bg := range cp.BlockGradients {
		if bg.View < 0 || bg.Index < 0 || bg.Start < 0 || bg.End < bg.Start || len(bg.Values) != len(specs) {
			return fmt.Errorf("invalid block gradient %d:%d", bg.View, bg.Index)
		}
		for _, v := range bg.Values {
			if !finite(v) {
				return fmt.Errorf("block gradient contains a non-finite value")
			}
		}
	}
	return nil
}

func checkpointHash(cp *Checkpoint) (string, error) {
	copyCP := *cp
	copyCP.ContentHash = ""
	data, err := json.Marshal(copyCP)
	if err != nil {
		return "", errs.Wrap(err, errs.CodeCheckpt, "<checkpoint>", "contentHash", "cannot hash checkpoint")
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

// CheckpointContentHash exposes stable hashing for tests and diagnostics.
func CheckpointContentHash(cp *Checkpoint) (string, error) { return checkpointHash(cp) }

func cloneCheckpoint(cp *Checkpoint) *Checkpoint {
	if cp == nil {
		return nil
	}
	out := *cp
	out.Scene = cp.Scene.Clone()
	out.Parameters = append([]float64(nil), cp.Parameters...)
	out.ParameterIDs = append([]string(nil), cp.ParameterIDs...)
	out.Losses = append([]float64(nil), cp.Losses...)
	out.BlockGradients = make([]BlockGradient, len(cp.BlockGradients))
	for i, bg := range cp.BlockGradients {
		out.BlockGradients[i] = bg
		out.BlockGradients[i].Values = append([]float64(nil), bg.Values...)
	}
	return &out
}

func (s *Store) acquire(exclusive bool) (*os.File, error) {
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return nil, errs.Wrap(err, errs.CodeCheckpt, s.dir, "checkpoint", "cannot create checkpoint directory")
	}
	f, err := os.OpenFile(filepath.Join(s.dir, ".commit.lock"), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, errs.Wrap(err, errs.CodeCheckpt, s.dir, ".commit.lock", "cannot open lock")
	}
	how := syscall.LOCK_SH
	if exclusive {
		how = syscall.LOCK_EX
	}
	if err := syscall.Flock(int(f.Fd()), how); err != nil {
		_ = f.Close()
		return nil, errs.Wrap(err, errs.CodeCheckpt, s.dir, ".commit.lock", "cannot lock checkpoint directory")
	}
	return f, nil
}

func (s *Store) release(f *os.File) {
	if f == nil {
		return
	}
	_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	_ = f.Close()
}

func writeAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return errs.Wrap(err, errs.CodeCheckpt, path, "", "cannot create temporary file")
	}
	tmpName := tmp.Name()
	ok := false
	defer func() {
		_ = tmp.Close()
		if !ok {
			_ = os.Remove(tmpName)
		}
	}()
	if _, err := tmp.Write(data); err != nil {
		return errs.Wrap(err, errs.CodeCheckpt, path, "", "cannot write temporary file")
	}
	if err := tmp.Sync(); err != nil {
		return errs.Wrap(err, errs.CodeCheckpt, path, "", "cannot sync temporary file")
	}
	if err := tmp.Close(); err != nil {
		return errs.Wrap(err, errs.CodeCheckpt, path, "", "cannot close temporary file")
	}
	if err := os.Rename(tmpName, path); err != nil {
		return errs.Wrap(err, errs.CodeCheckpt, path, "", "cannot commit file")
	}
	ok = true
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		_ = d.Close()
	}
	return nil
}

func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }
