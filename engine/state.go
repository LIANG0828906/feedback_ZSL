package engine

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
)

type Stage string

const (
	StageFlatten   Stage = "flatten"
	StageCoverage  Stage = "coverage"
	StageComposite Stage = "composite"
)

type State string

const (
	StatePending    State = "pending"
	StateProcessing State = "processing"
	StateCommitted  State = "committed"
	StateFailed     State = "failed"
)

type StateCell struct {
	Time  int64  `json:"timeUs"`
	Path  string `json:"path"`
	Stage Stage  `json:"stage"`
	State State  `json:"state"`
}

type StageMachine struct {
	mu     sync.Mutex
	states map[string]map[Stage]State
}

func NewStageMachine(scene *Scene) *StageMachine {
	m := &StageMachine{states: map[string]map[Stage]State{}}
	if scene == nil {
		return m
	}
	for _, t := range scene.Samples {
		for _, p := range scene.Paths {
			key := stateKey(t, p.ID)
			m.states[key] = map[Stage]State{
				StageFlatten:   StatePending,
				StageCoverage:  StatePending,
				StageComposite: StatePending,
			}
		}
	}
	return m
}

func stateKey(time int64, path string) string {
	return fmt.Sprintf("%d/%s", time, path)
}

func allowedState(from, to State) bool {
	switch from {
	case StatePending:
		return to == StateProcessing
	case StateProcessing:
		return to == StateCommitted || to == StateFailed || to == StatePending
	case StateFailed:
		return to == StateProcessing
	default:
		return false
	}
}

// Transition advances a stage only along the documented state graph.
func (m *StageMachine) Transition(time int64, path string, stage Stage, to State) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := stateKey(time, path)
	stages, ok := m.states[key]
	if !ok {
		return &Error{Code: CodeState, Path: fmt.Sprintf("%d/%s/%s", time, path, stage), Message: "unknown state cell"}
	}
	from, ok := stages[stage]
	if !ok {
		return &Error{Code: CodeState, Path: fmt.Sprintf("%d/%s/%s", time, path, stage), Message: "unknown stage"}
	}
	if !allowedState(from, to) {
		return &Error{Code: CodeState, Path: fmt.Sprintf("%d/%s/%s", time, path, stage), Left: string(from), Right: string(to), Message: "illegal state transition"}
	}
	stages[stage] = to
	return nil
}

func (m *StageMachine) StateOf(time int64, path string, stage Stage) State {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s, ok := m.states[stateKey(time, path)]; ok {
		return s[stage]
	}
	return ""
}

func (m *StageMachine) Matrix() []StateCell {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]StateCell, 0, len(m.states)*3)
	for key, stages := range m.states {
		parts := strings.SplitN(key, "/", 2)
		t, _ := strconv.ParseInt(parts[0], 10, 64)
		path := ""
		if len(parts) > 1 {
			path = parts[1]
		}
		for _, stage := range []Stage{StageFlatten, StageCoverage, StageComposite} {
			out = append(out, StateCell{Time: t, Path: path, Stage: stage, State: stages[stage]})
		}
	}
	stageOrder := map[Stage]int{StageFlatten: 0, StageCoverage: 1, StageComposite: 2}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Time != out[j].Time {
			return out[i].Time < out[j].Time
		}
		if out[i].Path != out[j].Path {
			return out[i].Path < out[j].Path
		}
		return stageOrder[out[i].Stage] < stageOrder[out[j].Stage]
	})
	return out
}
