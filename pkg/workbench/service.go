package workbench

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"

	"tacticalworkbench/pkg/tactical"
)

type ViewState struct {
	Scenario     tactical.Scenario       `json:"scenario"`
	EffectiveMap tactical.MapDef         `json:"effective_map"`
	Snapshot     *tactical.Snapshot      `json:"snapshot"`
	LastEdit     *tactical.EditResult    `json:"last_edit,omitempty"`
	Checkpoint   tactical.CheckpointMeta `json:"checkpoint"`
	Paused       bool                    `json:"paused"`
	MapHash      string                  `json:"map_hash"`
}

type Service struct {
	mu            sync.Mutex
	scenarioPath  string
	dbPath        string
	scenarioValue *tactical.Scenario
	scenario      tactical.Scenario
	engine        *tactical.Engine
	store         *tactical.CheckpointStore
	lastEdit      *tactical.EditResult
	checkpoint    tactical.CheckpointMeta
	paused        bool
}

func NewService(scenarioPath, dbPath string) *Service {
	return &Service{scenarioPath: scenarioPath, dbPath: dbPath}
}

func NewServiceWithScenario(scenario *tactical.Scenario, dbPath string) *Service {
	return &Service{scenarioPath: "embedded:deterministic-sample", scenarioValue: scenario, dbPath: dbPath}
}

func DefaultEngineOptions(scenario tactical.Scenario) tactical.EngineOptions {
	return tactical.EngineOptions{
		Search: tactical.SearchOptions{Neighbors: 8, HeuristicWeightMilli: 1000},
		Smoothing: tactical.SmoothingOptions{
			MaxSpacingMilli:  scenario.Formation.MaxSpacingMilli,
			FormationSamples: 65,
		},
		Simulation:       tactical.SimulationOptions{},
		MaxParallel:      4,
		StateBufferLimit: 4,
	}
}

func (s *Service) Initialize(ctx context.Context) (ViewState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.initializeLocked(ctx)
}

func (s *Service) initializeLocked(ctx context.Context) (ViewState, error) {
	var scenario *tactical.Scenario
	if s.scenarioValue != nil {
		copyScenario := *s.scenarioValue
		scenario = &copyScenario
	} else {
		loaded, err := tactical.LoadScenarioFile(s.scenarioPath)
		if err != nil {
			return ViewState{}, err
		}
		scenario = loaded
	}
	engine, err := tactical.NewEngine(*scenario, DefaultEngineOptions(*scenario))
	if err != nil {
		return ViewState{}, err
	}
	store, err := tactical.OpenCheckpointStore(s.dbPath)
	if err != nil {
		return ViewState{}, err
	}
	meta, _, err := engine.ResumeOrRebuild(ctx, store)
	if err != nil {
		store.Close()
		return ViewState{}, err
	}
	if s.store != nil {
		_ = s.store.Close()
	}
	s.scenario = *scenario
	s.scenarioValue = scenario
	s.engine = engine
	s.store = store
	s.checkpoint = meta
	s.lastEdit = nil
	s.paused = false
	return s.stateLocked(), nil
}

func (s *Service) State() ViewState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stateLocked()
}

func (s *Service) stateLocked() ViewState {
	if s.engine == nil {
		return ViewState{Checkpoint: s.checkpoint, Paused: s.paused}
	}
	return ViewState{
		Scenario:     s.engine.Scenario(),
		EffectiveMap: s.engine.EffectiveMap(),
		Snapshot:     s.engine.Snapshot(),
		LastEdit:     cloneEdit(s.lastEdit),
		Checkpoint:   s.checkpoint,
		Paused:       s.paused,
		MapHash:      s.engine.MapHash(),
	}
}

func (s *Service) SetCellCost(ctx context.Context, x, y int, cost int64) (EditResultView, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.readyLocked(); err != nil {
		return EditResultView{}, err
	}
	if s.paused {
		return EditResultView{}, fmt.Errorf("workbench is paused; resume before editing")
	}
	result, err := s.engine.ApplyCostEdit(ctx, tactical.GridPoint{X: x, Y: y}, cost)
	if err != nil {
		return EditResultView{State: s.stateLocked()}, err
	}
	meta, saveErr := s.engine.SaveCheckpoint(ctx, s.store)
	if saveErr != nil {
		return EditResultView{State: s.stateLocked()}, saveErr
	}
	s.checkpoint = meta
	s.lastEdit = result
	editedScenario := s.engine.Scenario()
	s.scenarioValue = &editedScenario
	return EditResultView{State: s.stateLocked(), Result: *result}, nil
}

func (s *Service) SetRegionActive(ctx context.Context, regionID string, active bool) (EditResultView, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.readyLocked(); err != nil {
		return EditResultView{}, err
	}
	if s.paused {
		return EditResultView{}, fmt.Errorf("workbench is paused; resume before editing")
	}
	result, err := s.engine.ApplyRegionEdit(ctx, regionID, active)
	if err != nil {
		return EditResultView{State: s.stateLocked()}, err
	}
	meta, saveErr := s.engine.SaveCheckpoint(ctx, s.store)
	if saveErr != nil {
		return EditResultView{State: s.stateLocked()}, saveErr
	}
	s.checkpoint = meta
	s.lastEdit = result
	editedScenario := s.engine.Scenario()
	s.scenarioValue = &editedScenario
	return EditResultView{State: s.stateLocked(), Result: *result}, nil
}

func (s *Service) RetryMember(ctx context.Context, memberID string) (ViewState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.readyLocked(); err != nil {
		return ViewState{}, err
	}
	if s.paused {
		return ViewState{}, fmt.Errorf("workbench is paused; resume before retrying")
	}
	if _, err := s.engine.RetryMember(ctx, memberID); err != nil {
		return s.stateLocked(), err
	}
	meta, err := s.engine.SaveCheckpoint(ctx, s.store)
	if err != nil {
		return s.stateLocked(), err
	}
	s.checkpoint = meta
	return s.stateLocked(), nil
}

// Pause persists the current checkpoint and blocks new edits until Resume.
func (s *Service) Pause(ctx context.Context) (ViewState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.readyLocked(); err != nil {
		return ViewState{}, err
	}
	meta, err := s.engine.SaveCheckpoint(ctx, s.store)
	if err != nil {
		return s.stateLocked(), err
	}
	s.checkpoint = meta
	s.paused = true
	return s.stateLocked(), nil
}

func (s *Service) Resume(ctx context.Context) (ViewState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.readyLocked(); err != nil {
		return ViewState{}, err
	}
	s.paused = false
	return s.stateLocked(), nil
}

// Restart simulates a full process restart against the same SQLite file.
func (s *Service) Restart(ctx context.Context) (ViewState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.engine != nil {
		editedScenario := s.engine.Scenario()
		s.scenarioValue = &editedScenario
	}
	if s.store != nil {
		_ = s.store.Close()
		s.store = nil
	}
	return s.initializeLocked(ctx)
}

func (s *Service) Shutdown() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.engine != nil && s.store != nil {
		_, _ = s.engine.SaveCheckpoint(context.Background(), s.store)
	}
	if s.store != nil {
		_ = s.store.Close()
		s.store = nil
	}
}

func (s *Service) readyLocked() error {
	if s.engine == nil || s.store == nil {
		return fmt.Errorf("workbench is not initialized")
	}
	return nil
}

type EditResultView struct {
	State  ViewState           `json:"state"`
	Result tactical.EditResult `json:"result"`
}

func cloneEdit(result *tactical.EditResult) *tactical.EditResult {
	if result == nil {
		return nil
	}
	clone := *result
	if result.Snapshot != nil {
		payload, _ := json.Marshal(result.Snapshot)
		var snapshot tactical.Snapshot
		_ = json.Unmarshal(payload, &snapshot)
		clone.Snapshot = &snapshot
	}
	return &clone
}
