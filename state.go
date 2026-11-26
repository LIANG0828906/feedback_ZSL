package quantforensics

import "sync"

type BlockStage string

const (
	StageLoad        BlockStage = "load"
	StageQuantize    BlockStage = "quantize"
	StageReconstruct BlockStage = "reconstruct"
	StageVerify      BlockStage = "verify"
)

func (s BlockStage) String() string { return string(s) }

var OrderedStages = []BlockStage{StageLoad, StageQuantize, StageReconstruct, StageVerify}

type LifecycleState string

const (
	StatePending   LifecycleState = "pending"
	StateRunning   LifecycleState = "running"
	StateCommitted LifecycleState = "committed"
	StateFailed    LifecycleState = "failed"
)

func (s LifecycleState) String() string { return string(s) }

type StateTransition struct {
	Subject string         `json:"subject"`
	Stage   string         `json:"stage,omitempty"`
	From    LifecycleState `json:"from"`
	To      LifecycleState `json:"to"`
	Reason  string         `json:"reason,omitempty"`
}

type StateMachine struct {
	mu      sync.Mutex
	blocks  map[string]map[BlockStage]LifecycleState
	panels  map[string]LifecycleState
	History []StateTransition `json:"history"`
}

func NewStateMachine() *StateMachine {
	return &StateMachine{
		blocks: map[string]map[BlockStage]LifecycleState{},
		panels: map[string]LifecycleState{},
	}
}

var legalBlockTransitions = map[LifecycleState]map[LifecycleState]bool{
	StatePending:   {StateRunning: true},
	StateRunning:   {StateCommitted: true, StateFailed: true},
	StateFailed:    {StateRunning: true, StatePending: true},
	StateCommitted: {StatePending: true},
}

var legalPanelTransitions = map[LifecycleState]map[LifecycleState]bool{
	StatePending:   {StateRunning: true},
	StateRunning:   {StateCommitted: true, StateFailed: true},
	StateFailed:    {StateRunning: true, StatePending: true},
	StateCommitted: {StatePending: true},
}

func (sm *StateMachine) ensureBlockLocked(id string) map[BlockStage]LifecycleState {
	stages, ok := sm.blocks[id]
	if !ok {
		stages = map[BlockStage]LifecycleState{}
		for _, stage := range OrderedStages {
			stages[stage] = StatePending
		}
		sm.blocks[id] = stages
	}
	return stages
}

func (sm *StateMachine) BlockState(id string, stage BlockStage) LifecycleState {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	if stages, ok := sm.blocks[id]; ok {
		if s, ok := stages[stage]; ok {
			return s
		}
	}
	return StatePending
}

func (sm *StateMachine) TransitionBlock(id string, stage BlockStage, to LifecycleState) error {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	stages := sm.ensureBlockLocked(id)
	from := stages[stage]
	if from == to {
		return nil
	}
	if !legalBlockTransitions[from][to] {
		return stateError(id+":"+string(stage), from, to)
	}
	stages[stage] = to
	sm.History = append(sm.History, StateTransition{Subject: id, Stage: string(stage), From: from, To: to})
	return nil
}

// InvalidateBlock is the graph-driven committed/failed -> pending edge.
func (sm *StateMachine) InvalidateBlock(id string, stage BlockStage, reason string) error {
	sm.mu.Lock()
	stages := sm.ensureBlockLocked(id)
	from := stages[stage]
	to := StatePending
	if from == StatePending {
		sm.mu.Unlock()
		return nil
	}
	if !legalBlockTransitions[from][to] {
		sm.mu.Unlock()
		return stateError(id+":"+string(stage), from, to)
	}
	stages[stage] = to
	sm.History = append(sm.History, StateTransition{Subject: id, Stage: string(stage), From: from, To: to, Reason: reason})
	sm.mu.Unlock()
	return nil
}

func (sm *StateMachine) InitPanel(id string) {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	if _, ok := sm.panels[id]; !ok {
		sm.panels[id] = StatePending
	}
}

func (sm *StateMachine) PanelState(id string) LifecycleState {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	return sm.panels[id]
}

func (sm *StateMachine) TransitionPanel(id string, to LifecycleState) error {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	from, ok := sm.panels[id]
	if !ok {
		from = StatePending
		sm.panels[id] = from
	}
	if from == to {
		return nil
	}
	if !legalPanelTransitions[from][to] {
		return stateError("panel:"+id, from, to)
	}
	sm.panels[id] = to
	sm.History = append(sm.History, StateTransition{Subject: "panel:" + id, From: from, To: to})
	return nil
}

func (sm *StateMachine) Snapshot() map[string]any {
	sm.mu.Lock()
	defer sm.mu.Unlock()
	blocks := map[string]map[string]string{}
	for id, stages := range sm.blocks {
		blocks[id] = map[string]string{}
		for stage, state := range stages {
			blocks[id][string(stage)] = string(state)
		}
	}
	panels := map[string]string{}
	for id, state := range sm.panels {
		panels[id] = string(state)
	}
	return map[string]any{"blocks": blocks, "panels": panels, "historyLength": len(sm.History)}
}
