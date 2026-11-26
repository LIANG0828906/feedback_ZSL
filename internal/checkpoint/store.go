package checkpoint

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"

	"mirrorcast/internal/bizerror"
	"mirrorcast/internal/engine"
	"mirrorcast/internal/session"
)

const FormatVersion = 1

const globalFileName = "checkpoint.json"

type Global struct {
	FormatVersion     int    `json:"format_version"`
	SessionID         string `json:"session_id"`
	CommittedTick     int64  `json:"committed_tick"`
	TopologyVersion   int64  `json:"topology_version"`
	TotalBudgetKbps   int64  `json:"total_budget_kbps"`
	UsedBudgetKbps    int64  `json:"used_budget_kbps"`
	ScreenStateFile   string `json:"screen_state_file"`
	ScreenStateSHA256 string `json:"screen_state_sha256"`
}

type ScreenStates struct {
	FormatVersion int                     `json:"format_version"`
	Topology      session.Topology        `json:"topology"`
	Allocations   []session.Allocation    `json:"allocations"`
	Steps         map[string]StepProgress `json:"steps,omitempty"`
}

type StepProgress struct {
	LogicalTick int64    `json:"logical_tick"`
	Completed   []string `json:"completed"`
}

type Checkpoint struct {
	Global Global
	State  engine.State
	Steps  map[string]StepProgress
}

type Store struct {
	dir       string
	sessionID string
}

func NewStore(dir, sessionID string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, bizerror.New(bizerror.EInternal, "cannot create checkpoint directory: "+err.Error()).WithDetail("dir", dir)
	}
	return &Store{dir: dir, sessionID: sessionID}, nil
}

func (s *Store) SaveState(state engine.State, steps map[string]StepProgress) error {
	global, err := s.PrepareScreenStates(state, steps)
	if err != nil {
		return err
	}
	return s.CommitGlobal(global)
}

// PrepareScreenStates executes phase one: all controlled-screen state is
// atomically persisted and fsynced before the global tick marker is updated.
func (s *Store) PrepareScreenStates(state engine.State, steps map[string]StepProgress) (Global, error) {
	screenStates := ScreenStates{
		FormatVersion: FormatVersion,
		Topology:      state.Topology,
		Allocations:   append([]session.Allocation(nil), state.Allocations...),
		Steps:         cloneSteps(steps),
	}
	screenBytes, err := json.MarshalIndent(screenStates, "", "  ")
	if err != nil {
		return Global{}, bizerror.New(bizerror.EInternal, "cannot encode screen states: "+err.Error())
	}
	sum := sha256.Sum256(screenBytes)
	screenName := "screens-" + itoa64(state.CommittedTick) + "-" + hex.EncodeToString(sum[:])[:12] + ".json"
	if err := writeAtomic(filepath.Join(s.dir, screenName), screenBytes); err != nil {
		return Global{}, err
	}

	return Global{
		FormatVersion:     FormatVersion,
		SessionID:         s.sessionID,
		CommittedTick:     state.CommittedTick,
		TopologyVersion:   state.TopologyVersion,
		TotalBudgetKbps:   state.TotalBudget,
		UsedBudgetKbps:    state.UsedBudget,
		ScreenStateFile:   screenName,
		ScreenStateSHA256: hex.EncodeToString(sum[:]),
	}, nil
}

// CommitGlobal executes phase two: the monotonic committed tick marker is
// atomically published only after every screen state file is durable.
func (s *Store) CommitGlobal(global Global) error {
	if global.FormatVersion == 0 {
		global.FormatVersion = FormatVersion
	}
	if global.SessionID == "" {
		global.SessionID = s.sessionID
	}
	globalBytes, err := json.MarshalIndent(global, "", "  ")
	if err != nil {
		return bizerror.New(bizerror.EInternal, "cannot encode global checkpoint: "+err.Error())
	}
	if err := writeAtomic(filepath.Join(s.dir, globalFileName), globalBytes); err != nil {
		return err
	}
	return nil
}

func (s *Store) Load() (*Checkpoint, error) {
	globalPath := filepath.Join(s.dir, globalFileName)
	globalBytes, err := os.ReadFile(globalPath)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, bizerror.New(bizerror.ECheckpointNotFound, "global checkpoint does not exist").WithPath(globalPath)
		}
		return nil, bizerror.New(bizerror.ECheckpointTruncated, "cannot read global checkpoint: "+err.Error()).WithPath(globalPath)
	}
	var global Global
	if err := json.Unmarshal(globalBytes, &global); err != nil {
		return nil, bizerror.New(bizerror.ECheckpointTruncated, "global checkpoint is truncated or malformed: "+err.Error()).WithPath(globalPath)
	}
	if global.FormatVersion != FormatVersion {
		return nil, bizerror.New(bizerror.ECheckpointVersion, "checkpoint format version does not match this binary; recompute whole session").
			WithPath(globalPath).
			WithDetail("checkpoint_version", global.FormatVersion).
			WithDetail("binary_version", FormatVersion).
			WithConstraint("C-CHECKPOINT-VERSION")
	}
	if s.sessionID != "" && global.SessionID != s.sessionID {
		return nil, bizerror.New(bizerror.ECheckpointVersion, "checkpoint session id does not match; recompute whole session").
			WithPath(globalPath).
			WithDetail("checkpoint_session_id", global.SessionID).
			WithDetail("expected_session_id", s.sessionID).
			WithConstraint("C-CHECKPOINT-VERSION")
	}

	screenPath := filepath.Join(s.dir, global.ScreenStateFile)
	screenBytes, err := os.ReadFile(screenPath)
	if err != nil {
		return nil, bizerror.New(bizerror.ECheckpointTruncated, "referenced screen state is missing or unreadable: "+err.Error()).
			WithPath(screenPath).WithDetail("screen_state_file", global.ScreenStateFile)
	}
	sum := sha256.Sum256(screenBytes)
	if hex.EncodeToString(sum[:]) != global.ScreenStateSHA256 {
		return nil, bizerror.New(bizerror.ECheckpointChecksum, "screen state checksum does not match committed global checkpoint").
			WithPath(screenPath).WithConstraint("C-CHECKPOINT-INTEGRITY")
	}
	var screens ScreenStates
	if err := json.Unmarshal(screenBytes, &screens); err != nil {
		return nil, bizerror.New(bizerror.ECheckpointTruncated, "screen state checkpoint is truncated or malformed: "+err.Error()).WithPath(screenPath)
	}
	if screens.FormatVersion != FormatVersion {
		return nil, bizerror.New(bizerror.ECheckpointVersion, "screen-state format version mismatch; recompute whole session").
			WithPath(screenPath).
			WithDetail("checkpoint_version", screens.FormatVersion).
			WithDetail("binary_version", FormatVersion)
	}

	return &Checkpoint{
		Global: global,
		State: engine.State{
			CommittedTick:   global.CommittedTick,
			TopologyVersion: global.TopologyVersion,
			TotalBudget:     global.TotalBudgetKbps,
			UsedBudget:      global.UsedBudgetKbps,
			Topology:        screens.Topology,
			Allocations:     screens.Allocations,
		},
		Steps: cloneSteps(screens.Steps),
	}, nil
}

func writeAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return bizerror.New(bizerror.EInternal, "cannot create temporary checkpoint: "+err.Error()).WithPath(path)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return bizerror.New(bizerror.EInternal, "cannot write checkpoint: "+err.Error()).WithPath(path)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return bizerror.New(bizerror.EInternal, "cannot sync checkpoint: "+err.Error()).WithPath(path)
	}
	if err := tmp.Close(); err != nil {
		return bizerror.New(bizerror.EInternal, "cannot close checkpoint: "+err.Error()).WithPath(path)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return bizerror.New(bizerror.EInternal, "cannot atomically publish checkpoint: "+err.Error()).WithPath(path)
	}
	if dirHandle, err := os.Open(dir); err == nil {
		_ = dirHandle.Sync()
		_ = dirHandle.Close()
	}
	return nil
}

func cloneSteps(in map[string]StepProgress) map[string]StepProgress {
	if in == nil {
		return nil
	}
	out := make(map[string]StepProgress, len(in))
	for key, value := range in {
		values := append([]string(nil), value.Completed...)
		out[key] = StepProgress{LogicalTick: value.LogicalTick, Completed: values}
	}
	return out
}

func itoa64(v int64) string {
	return strconv.FormatInt(v, 10)
}
