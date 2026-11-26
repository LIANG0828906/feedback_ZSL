package budget

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

// Checkpoint is the complete recoverable boundary for one committed window.
type Checkpoint struct {
	WindowIndex       int                   `json:"window_index"`
	FirstTick         int                   `json:"first_tick"`
	LastTick          int                   `json:"last_tick"`
	IntentOffset      int                   `json:"consumed_intent_offset"`
	ResourceCurrent   BP                    `json:"resource_current"`
	SkillStates       []SkillState          `json:"skill_states"`
	GroupStates       []GroupState          `json:"group_states"`
	ContentHash       string                `json:"content_hash"`
	SourceVersion     SourceVersion         `json:"source_version"`
	AlgorithmVersion  string                `json:"algorithm_version"`
	ParamVersion      string                `json:"param_version"`
	WindowStates      []TickState           `json:"window_states"`
	WindowArbitration [][]ArbitrationRecord `json:"window_arbitration"`
}

type CheckpointStore struct {
	Dir string
	mu  sync.Mutex
}

var atomicWriteMu sync.Mutex

func NewCheckpointStore(dir string) (*CheckpointStore, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, &Error{Code: ECodeIO, Message: "cannot create checkpoint directory", Field: dir, Cause: err.Error()}
	}
	return &CheckpointStore{Dir: dir}, nil
}

func (s *CheckpointStore) PathFor(windowIndex int) string {
	return filepath.Join(s.Dir, fmt.Sprintf("window-%06d.json", windowIndex))
}

// Save atomically writes and renames one window. The store mutex plus a package
// write mutex make demo writes serial; readers never observe a half file.
func (s *CheckpointStore) Save(cp *Checkpoint) (*WindowCommit, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	atomicWriteMu.Lock()
	defer atomicWriteMu.Unlock()

	data, err := json.MarshalIndent(cp, "", "  ")
	if err != nil {
		return nil, &Error{Code: ECodeIO, Message: "cannot encode checkpoint", Cause: err.Error()}
	}
	path := s.PathFor(cp.WindowIndex)
	tmp, err := os.CreateTemp(s.Dir, ".window-*.tmp")
	if err != nil {
		return nil, &Error{Code: ECodeIO, Message: "cannot create temporary checkpoint", Field: path, Cause: err.Error()}
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return nil, &Error{Code: ECodeIO, Message: "cannot write checkpoint bytes", Field: path, Cause: err.Error()}
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return nil, &Error{Code: ECodeIO, Message: "cannot sync checkpoint", Field: path, Cause: err.Error()}
	}
	if err := tmp.Close(); err != nil {
		return nil, &Error{Code: ECodeIO, Message: "cannot close checkpoint", Field: path, Cause: err.Error()}
	}
	if err := os.Rename(tmpName, path); err != nil {
		return nil, &Error{Code: ECodeIO, Message: "cannot atomically rename checkpoint", Field: path, Cause: err.Error()}
	}
	syncDir(s.Dir)
	return &WindowCommit{
		WindowIndex: cp.WindowIndex, FirstTick: cp.FirstTick, LastTick: cp.LastTick,
		IntentOffset: cp.IntentOffset, Path: filepath.Base(path),
	}, nil
}

func syncDir(dir string) {
	d, err := os.Open(dir)
	if err != nil {
		return
	}
	defer d.Close()
	_ = d.Sync()
}

func LoadCheckpointFile(path string) (*Checkpoint, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, &Error{Code: ECodeIO, Message: "cannot read checkpoint", Field: path, Cause: err.Error()}
	}
	var cp Checkpoint
	if err := json.Unmarshal(data, &cp); err != nil {
		return nil, &Error{Code: ECodeDecode, Message: "corrupt checkpoint JSON", Field: path, Cause: err.Error(), Path: path}
	}
	if cp.AlgorithmVersion == "" || cp.ContentHash == "" {
		return nil, &Error{Code: ECodeDecode, Message: "checkpoint missing version/hash envelope", Field: path, Path: path}
	}
	return &cp, nil
}

func (s *CheckpointStore) ListPaths() ([]string, error) {
	entries, err := os.ReadDir(s.Dir)
	if err != nil {
		return nil, &Error{Code: ECodeIO, Message: "cannot list checkpoint directory", Field: s.Dir, Cause: err.Error()}
	}
	paths := []string{}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if strings.HasPrefix(name, "window-") && strings.HasSuffix(name, ".json") {
			paths = append(paths, filepath.Join(s.Dir, name))
		}
	}
	sort.Strings(paths)
	return paths, nil
}

func (s *CheckpointStore) LoadAll() ([]*Checkpoint, []string, error) {
	paths, err := s.ListPaths()
	if err != nil {
		return nil, nil, err
	}
	cps := make([]*Checkpoint, 0, len(paths))
	for _, path := range paths {
		cp, err := LoadCheckpointFile(path)
		if err != nil {
			return nil, nil, err
		}
		cps = append(cps, cp)
	}
	return cps, paths, nil
}

func (s *CheckpointStore) LoadLatest() (*Checkpoint, string, error) {
	cps, paths, err := s.LoadAll()
	if err != nil {
		return nil, "", err
	}
	if len(cps) == 0 {
		return nil, "", nil
	}
	return cps[len(cps)-1], paths[len(paths)-1], nil
}

func Compatible(cp *Checkpoint, project *Project, params Params) bool {
	return cp != nil &&
		cp.AlgorithmVersion == AlgorithmVersion &&
		cp.ParamVersion == params.ParamVersion &&
		cp.ContentHash == project.ContentHash &&
		cp.SourceVersion == project.SourceVersion
}

// RunRealtime steps all ticks and commits one atomic checkpoint per window.
func RunRealtime(project *Project, params Params, dir string) (*RunReport, error) {
	return RunRealtimeContext(context.Background(), project, params, dir)
}

// RunRealtimeContext stops at the next window boundary after cancellation.
// A fully renamed checkpoint remains recoverable; an uncommitted partial
// window is never used.
func RunRealtimeContext(ctx context.Context, project *Project, params Params, dir string) (*RunReport, error) {
	store, err := NewCheckpointStore(dir)
	if err != nil {
		return nil, err
	}
	return runToCompletion(ctx, project, params, store, -1)
}

// RunFirstWindows is the test/demo crash precursor. maxWindows limits commits;
// pass -1 for all windows.
func RunFirstWindows(project *Project, params Params, dir string, maxWindows int) (*RunReport, error) {
	store, err := NewCheckpointStore(dir)
	if err != nil {
		return nil, err
	}
	return runToCompletion(context.Background(), project, params, store, maxWindows)
}

func runToCompletion(ctx context.Context, project *Project, params Params, store *CheckpointStore, maxWindows int) (*RunReport, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	sim, err := NewSimulation(project, params)
	if err != nil {
		return nil, err
	}
	commits := []WindowCommit{}
	windowTicks := params.WindowTicks
	if windowTicks <= 0 {
		windowTicks = 4
	}
	windowIndex := 0
	for !sim.Done() {
		if err := ctx.Err(); err != nil {
			report := sim.Report()
			report.Commits = commits
			return report, err
		}
		if maxWindows >= 0 && windowIndex >= maxWindows {
			break
		}
		firstTick := sim.NextTick()
		var windowStates []TickState
		var windowArb [][]ArbitrationRecord
		var last TickState
		for i := 0; i < windowTicks && !sim.Done(); i++ {
			state, records, err := sim.Step()
			if err != nil {
				return nil, err
			}
			last = state
			windowStates = append(windowStates, state)
			windowArb = append(windowArb, records)
		}
		cp := &Checkpoint{
			WindowIndex: windowIndex, FirstTick: firstTick, LastTick: last.Tick,
			IntentOffset:    last.ConsumedIntentOffset,
			ResourceCurrent: last.ResourceCurrent, SkillStates: last.Skills, GroupStates: last.Groups,
			ContentHash: sim.hash, SourceVersion: project.SourceVersion,
			AlgorithmVersion: AlgorithmVersion, ParamVersion: params.ParamVersion,
			WindowStates: windowStates, WindowArbitration: windowArb,
		}
		commit, err := store.Save(cp)
		if err != nil {
			return nil, err
		}
		commits = append(commits, *commit)
		if params.OnWindowCommitted != nil {
			params.OnWindowCommitted(*commit)
		}
		windowIndex++
	}
	report := sim.Report()
	report.Commits = commits
	return report, nil
}

// ResumeFromStore rebuilds all persisted windows, verifies the latest boundary
// matches current rule versions, then settles remaining ticks.
func ResumeFromStore(project *Project, params Params, dir string) (*RunReport, error) {
	store, err := NewCheckpointStore(dir)
	if err != nil {
		return nil, err
	}
	cps, _, err := store.LoadAll()
	if err != nil {
		return nil, err
	}
	if len(cps) == 0 {
		return RunRealtime(project, params, dir)
	}
	latest := cps[len(cps)-1]
	if !Compatible(latest, project, params) {
		return nil, &Error{
			Code: ECodeSchema, Message: "refusing to resume from version or hash mismatched checkpoint",
			Field: dir, Expected: map[string]string{
				"algorithm_version": AlgorithmVersion,
				"param_version":     params.ParamVersion,
				"content_hash":      project.ContentHash,
				"source_version":    string(project.SourceVersion),
			}, Actual: map[string]string{
				"algorithm_version": latest.AlgorithmVersion,
				"param_version":     latest.ParamVersion,
				"content_hash":      latest.ContentHash,
				"source_version":    string(latest.SourceVersion),
			},
		}
	}

	sim, err := RestoreSimulation(project, params, latest)
	if err != nil {
		return nil, err
	}
	// RestoreSimulation only restores latest window payload. Rebuild complete
	// deterministic history from all serial persisted windows.
	sim.states = nil
	sim.records = nil
	commits := make([]WindowCommit, 0, len(cps))
	for _, cp := range cps {
		sim.states = append(sim.states, cp.WindowStates...)
		sim.records = append(sim.records, cp.WindowArbitration...)
		commits = append(commits, WindowCommit{
			WindowIndex: cp.WindowIndex, FirstTick: cp.FirstTick, LastTick: cp.LastTick,
			IntentOffset: cp.IntentOffset, Path: store.PathFor(cp.WindowIndex),
		})
	}

	windowTicks := params.WindowTicks
	if windowTicks <= 0 {
		windowTicks = 4
	}
	windowIndex := latest.WindowIndex + 1
	for !sim.Done() {
		firstTick := sim.NextTick()
		var windowStates []TickState
		var windowArb [][]ArbitrationRecord
		var last TickState
		for i := 0; i < windowTicks && !sim.Done(); i++ {
			state, records, err := sim.Step()
			if err != nil {
				return nil, err
			}
			last = state
			windowStates = append(windowStates, state)
			windowArb = append(windowArb, records)
		}
		cp := &Checkpoint{
			WindowIndex: windowIndex, FirstTick: firstTick, LastTick: last.Tick,
			IntentOffset:    last.ConsumedIntentOffset,
			ResourceCurrent: last.ResourceCurrent, SkillStates: last.Skills, GroupStates: last.Groups,
			ContentHash: sim.hash, SourceVersion: project.SourceVersion,
			AlgorithmVersion: AlgorithmVersion, ParamVersion: params.ParamVersion,
			WindowStates: windowStates, WindowArbitration: windowArb,
		}
		commit, err := store.Save(cp)
		if err != nil {
			return nil, err
		}
		commits = append(commits, *commit)
		windowIndex++
	}
	report := sim.Report()
	report.Commits = normalizeCommitPaths(commits)
	return report, nil
}

func normalizeCommitPaths(commits []WindowCommit) []WindowCommit {
	for i := range commits {
		if commits[i].Path != "" {
			commits[i].Path = filepath.Base(commits[i].Path)
		}
	}
	return commits
}

// CrashResumeThreePaths runs all required persistence modes and compares them.
type ThreePathResult struct {
	Realtime *RunReport `json:"realtime"`
	Resumed  *RunReport `json:"resumed"`
	Full     *RunReport `json:"full"`
}

func RunThreePathCheck(project *Project, params Params, baseDir string, crashAfter int) (*ThreePathResult, error) {
	realtimeDir := filepath.Join(baseDir, "realtime")
	resumeDir := filepath.Join(baseDir, "resume")
	fullDir := filepath.Join(baseDir, "full")
	for _, dir := range []string{realtimeDir, resumeDir, fullDir} {
		if err := os.RemoveAll(dir); err != nil {
			return nil, &Error{Code: ECodeIO, Message: "cannot reset managed checkpoint directory", Field: dir, Cause: err.Error()}
		}
	}

	realtime, err := RunRealtime(project, params, realtimeDir)
	if err != nil {
		return nil, err
	}
	if _, err := RunFirstWindows(project, params, resumeDir, crashAfter); err != nil {
		return nil, err
	}
	resumed, err := ResumeFromStore(project, params, resumeDir)
	if err != nil {
		return nil, err
	}
	full, err := RunRealtime(project, params, fullDir)
	if err != nil {
		return nil, err
	}
	for _, report := range []*RunReport{resumed, full} {
		if err := CompareReports(realtime, report); err != nil {
			return nil, err
		}
	}
	return &ThreePathResult{Realtime: realtime, Resumed: resumed, Full: full}, nil
}
