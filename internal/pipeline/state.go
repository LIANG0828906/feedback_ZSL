package pipeline

import (
	"fmt"
	"sync"

	"maskpipe/internal/model"
)

type Chunk struct {
	Index         int        `json:"index"`
	X             int        `json:"x"`
	Y             int        `json:"y"`
	PixelX        int        `json:"pixel_x"`
	PixelY        int        `json:"pixel_y"`
	Width         int        `json:"width"`
	Height        int        `json:"height"`
	Pixels        int        `json:"pixels"`
	CanonicalBase int        `json:"canonical_base"`
	Rect          model.BBox `json:"rect"`
}

func PlanChunks(width, height int, tile model.Tile) []Chunk {
	cols := (width + tile.Width - 1) / tile.Width
	rows := (height + tile.Height - 1) / tile.Height
	chunks := make([]Chunk, 0, cols*rows)
	index := 0
	for cy := 0; cy < rows; cy++ {
		for cx := 0; cx < cols; cx++ {
			x0 := cx * tile.Width
			y0 := cy * tile.Height
			x1 := minInt(width-1, x0+tile.Width-1)
			y1 := minInt(height-1, y0+tile.Height-1)
			w := x1 - x0 + 1
			h := y1 - y0 + 1
			chunks = append(chunks, Chunk{
				Index: index, X: cx, Y: cy, PixelX: x0, PixelY: y0,
				Width: w, Height: h, Pixels: w * h,
				CanonicalBase: index * 3,
				Rect: model.BBox{
					MinX: x0, MinY: y0, MaxX: x1, MaxY: y1,
					Width: w, Height: h,
				},
			})
			index++
		}
	}
	return chunks
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

type StageState struct {
	Key           model.ChunkKey    `json:"key"`
	Status        model.StageStatus `json:"status"`
	ParamVersion  string            `json:"param_version,omitempty"`
	Attempts      int               `json:"attempts"`
	LastErrorCode string            `json:"last_error_code,omitempty"`
	LastError     string            `json:"last_error,omitempty"`
}

type StateMachine struct {
	mu     sync.Mutex
	states map[string]*StageState
}

var legalTransitions = map[model.StageStatus]map[model.StageStatus]bool{
	model.StatusPending: {
		model.StatusProcessing: true,
	},
	model.StatusProcessing: {
		model.StatusCommitted: true,
		model.StatusFailed:    true,
	},
	model.StatusFailed: {
		model.StatusPending: true,
	},
	model.StatusCommitted: {},
}

func NewStateMachine(chunks []Chunk) *StateMachine {
	m := &StateMachine{states: map[string]*StageState{}}
	for _, chunk := range chunks {
		for _, stage := range []model.Stage{model.StageGrow, model.StageClean, model.StageStats} {
			key := model.ChunkKey{ChunkCoord: model.ChunkCoord{X: chunk.X, Y: chunk.Y}, Stage: stage}
			m.states[key.ID()] = &StageState{Key: key, Status: model.StatusPending}
		}
	}
	return m
}

func (m *StateMachine) Get(key model.ChunkKey) StageState {
	m.mu.Lock()
	defer m.mu.Unlock()
	return *m.states[key.ID()]
}

func (m *StateMachine) Transition(key model.ChunkKey, to model.StageStatus, paramVersion string, err error) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	state, ok := m.states[key.ID()]
	if !ok {
		return model.NewError(model.CodeState, "stage", "unknown stage %s", key.ID())
	}
	from := state.Status
	if !legalTransitions[from][to] {
		return model.WithDetails(model.NewError(model.CodeState, "status",
			"illegal transition %s -> %s for %s", from, to, key.ID()), map[string]any{
			"stage_id": key.ID(), "from": from, "to": to, "current": from,
		})
	}
	state.Status = to
	if to == model.StatusProcessing {
		state.Attempts++
		state.ParamVersion = paramVersion
		state.LastErrorCode = ""
		state.LastError = ""
	}
	if to == model.StatusFailed && err != nil {
		if pe, ok := model.AsError(err); ok {
			state.LastErrorCode = pe.Code
		}
		state.LastError = err.Error()
	}
	return nil
}

func (m *StateMachine) MarkExistingCommitted(key model.ChunkKey, paramVersion string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	state := m.states[key.ID()]
	state.Status = model.StatusCommitted
	state.ParamVersion = paramVersion
}

func (m *StateMachine) Snapshot() []StageState {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]StageState, 0, len(m.states))
	for _, state := range m.states {
		out = append(out, *state)
	}
	return out
}

func (m *StateMachine) Progress(chunkCount int) (committed int, total int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	total = chunkCount * 3
	for _, state := range m.states {
		if state.Status == model.StatusCommitted {
			committed++
		}
	}
	return committed, total
}

func (m *StateMachine) Matrix() map[string]model.StageStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[string]model.StageStatus{}
	for id, state := range m.states {
		out[id] = state.Status
	}
	return out
}

func StageIndex(stage model.Stage) int {
	switch stage {
	case model.StageGrow:
		return 0
	case model.StageClean:
		return 1
	case model.StageStats:
		return 2
	}
	panic(fmt.Sprintf("unknown stage %q", stage))
}

func CanonicalOrder(chunk Chunk, stage model.Stage) int {
	return chunk.Index*3 + StageIndex(stage)
}
