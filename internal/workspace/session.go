package workspace

import (
	"context"
	"fmt"
	"sort"
	"sync"

	"economycore/internal/core"
	"economycore/internal/samples"
)

type Edit struct {
	Kind            string            `json:"kind"`
	NodeID          string            `json:"node_id,omitempty"`
	IntentIndex     *int              `json:"intent_index,omitempty"`
	Outputs         *map[string]int64 `json:"outputs,omitempty"`
	Inputs          *map[string]int64 `json:"inputs,omitempty"`
	Capacity        *map[string]int64 `json:"capacity,omitempty"`
	WorkersRequired *int              `json:"workers_required,omitempty"`
	Workers         *int              `json:"workers,omitempty"`
	Active          *bool             `json:"active,omitempty"`
	Timestamp       *int64            `json:"timestamp,omitempty"`
	Level           *int              `json:"level,omitempty"`
	Recipe          *core.Params      `json:"recipe,omitempty"`
	FinalTick       *int64            `json:"final_tick,omitempty"`
}

type Batch struct {
	BatchID string `json:"batch_id"`
	Edits   []Edit `json:"edits"`
}

type CommitResult struct {
	Session *View  `json:"session"`
	BatchID string `json:"batch_id"`
}

type NodeView struct {
	core.Node
	Depth int `json:"depth"`
}

type Heatmap struct {
	Ticks  []int64   `json:"ticks"`
	Nodes  []string  `json:"nodes"`
	Cells  [][]int64 `json:"cells"`
	MaxAbs int64     `json:"max_abs"`
	Equal  bool      `json:"equal"`
}

type View struct {
	SessionID     string                `json:"session_id"`
	SampleID      string                `json:"sample_id"`
	Version       int64                 `json:"version"`
	Revision      int64                 `json:"revision"`
	ParamsVersion string                `json:"params_version"`
	SceneHash     string                `json:"scene_hash"`
	FinalTick     int64                 `json:"final_tick"`
	Dirty         bool                  `json:"dirty"`
	Nodes         []NodeView            `json:"nodes"`
	Intents       []core.Intent         `json:"intents"`
	PreviewStep   int64                 `json:"preview_step"`
	Preview       *core.PreviewResult   `json:"preview"`
	Exact         *core.WindowRunResult `json:"exact"`
	Heatmap       Heatmap               `json:"heatmap"`
	LastError     *core.Error           `json:"last_error,omitempty"`
}

type Session struct {
	mu                sync.Mutex
	id                string
	sampleID          string
	baseParams        string
	committed         *core.Scenario
	draft             *core.Scenario
	version           int64
	revision          int64
	committedRevision int64
	previewStep       int64
	sampleTicks       []int64
	preview           *core.PreviewResult
	exact             *core.WindowRunResult
	heatmap           Heatmap
	lastError         *core.Error
}

type memoryCheckpoints struct {
	windows []core.WindowResult
	failure *core.WindowFailure
}

func (m *memoryCheckpoints) LoadResume(context.Context, *core.Scenario) (core.ResumePoint, error) {
	return core.ResumePoint{}, nil
}

func (m *memoryCheckpoints) CommitWindow(_ context.Context, cp core.WindowCheckpoint) error {
	m.windows = append(m.windows, core.WindowResult{
		Index:             cp.WindowIndex,
		StartTick:         cp.StartTick,
		EndTick:           cp.EndTick,
		StartIntentOffset: cp.StartIntentOffset,
		EndIntentOffset:   cp.EndIntentOffset,
	})
	return nil
}

func (m *memoryCheckpoints) RecordFailure(_ context.Context, f core.WindowFailure) error {
	cp := f
	m.failure = &cp
	return nil
}

type Manager struct {
	mu         sync.Mutex
	samplesDir string
	sessions   map[string]*Session
	nextID     int64
}

func NewManager(samplesDir string) *Manager {
	return &Manager{samplesDir: samplesDir, sessions: make(map[string]*Session)}
}

func (m *Manager) Manifest() (*samples.Manifest, error) {
	return samples.LoadManifest(m.samplesDir)
}

func (m *Manager) Open(sampleID string) (*Session, error) {
	scene, err := samples.Load(m.samplesDir, sampleID)
	if err != nil {
		return nil, err
	}
	committed, err := core.CloneScenario(scene)
	if err != nil {
		return nil, err
	}
	draft, err := core.CloneScenario(scene)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	m.nextID++
	id := fmt.Sprintf("sess-%d", m.nextID)
	m.mu.Unlock()
	ticks := append([]int64(nil), scene.Truth.SampleTicks...)
	if len(ticks) == 0 {
		for i := 0; i <= 12; i++ {
			ticks = append(ticks, int64(i)*scene.FinalTick/12)
		}
	}
	if len(ticks) == 0 || ticks[len(ticks)-1] != scene.FinalTick {
		ticks = append(ticks, scene.FinalTick)
	}
	session := &Session{
		id:          id,
		sampleID:    sampleID,
		baseParams:  scene.ParamsVersion,
		committed:   committed,
		draft:       draft,
		previewStep: 8,
		sampleTicks: ticks,
	}
	if err := session.settleLocked(context.Background()); err != nil {
		return nil, err
	}
	m.mu.Lock()
	m.sessions[id] = session
	m.mu.Unlock()
	return session, nil
}

func (m *Manager) Get(id string) (*Session, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[id]
	return s, ok
}

func (s *Session) ID() string { return s.id }

func (s *Session) View() View {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.viewLocked()
}

func (s *Session) viewLocked() View {
	scene := s.draft
	depth := core.TopoDepth(scene)
	nodes := make([]NodeView, 0, len(scene.Nodes))
	for _, n := range scene.Nodes {
		nodes = append(nodes, NodeView{Node: n, Depth: depth[n.ID]})
	}
	return View{
		SessionID:     s.id,
		SampleID:      s.sampleID,
		Version:       s.version,
		Revision:      s.revision,
		ParamsVersion: scene.ParamsVersion,
		SceneHash:     scene.SceneHash,
		FinalTick:     scene.FinalTick,
		Dirty:         s.dirtyLocked(),
		Nodes:         nodes,
		Intents:       scene.Intents,
		PreviewStep:   s.previewStep,
		Preview:       s.preview,
		Exact:         s.exact,
		Heatmap:       s.heatmap,
		LastError:     s.lastError,
	}
}

func (s *Session) ApplyDraft(batch Batch) (View, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	next, err := core.CloneScenario(s.draft)
	if err != nil {
		return View{}, err
	}
	if err := applyEdits(next, batch.Edits); err != nil {
		e := txnError(batch.BatchID, err)
		s.lastError = e
		return View{}, e
	}
	s.revision++
	next.ParamsVersion = fmt.Sprintf("%s/r%d", s.baseParams, s.revision)
	next.Truth = core.Truth{}
	hash, hashErr := core.ComputeSceneHash(next)
	if hashErr != nil {
		s.revision--
		e := txnError(batch.BatchID, core.NewError(core.CodeState, "failed to hash working copy").Wrap(hashErr))
		s.lastError = e
		return View{}, e
	}
	next.SceneHash = hash
	if err := core.ValidateScenario(next); err != nil {
		s.revision--
		e := txnError(batch.BatchID, err)
		s.lastError = e
		return View{}, e
	}
	s.draft = next
	s.lastError = nil
	return s.viewLocked(), nil
}

func (s *Session) Commit(ctx context.Context, batch Batch) (View, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return View{}, core.NewError(core.CodeCancel, "commit cancelled").Wrap(err)
	}
	next, err := core.CloneScenario(s.draft)
	if err != nil {
		return View{}, err
	}
	if err := applyEdits(next, batch.Edits); err != nil {
		e := txnError(batch.BatchID, err)
		s.lastError = e
		return View{}, e
	}
	oldRevision := s.revision
	s.revision++
	next.ParamsVersion = fmt.Sprintf("%s/r%d", s.baseParams, s.revision)
	next.Truth = core.Truth{}
	hash, err := core.ComputeSceneHash(next)
	if err != nil {
		s.revision = oldRevision
		e := txnError(batch.BatchID, core.NewError(core.CodeState, "failed to hash edited scene").Wrap(err))
		s.lastError = e
		return View{}, e
	}
	next.SceneHash = hash
	if err := core.ValidateScenario(next); err != nil {
		s.revision = oldRevision
		e := txnError(batch.BatchID, err)
		s.lastError = e
		return View{}, e
	}
	oldCommitted, oldDraft := s.committed, s.draft
	s.committed, s.draft = next, next
	if err := s.settleLocked(ctx); err != nil {
		s.committed, s.draft = oldCommitted, oldDraft
		s.revision = oldRevision
		if core.CodeOf(err) == core.CodeCancel {
			return View{}, err
		}
		e := txnError(batch.BatchID, err)
		s.lastError = e
		return View{}, e
	}
	s.version++
	s.committedRevision = s.revision
	s.lastError = nil
	return s.viewLocked(), nil
}

// PreviewDraft applies a candidate batch to a clone only. It never commits or
// mutates the server-side working copy, which keeps repeated drag previews
// side-effect free.
func (s *Session) PreviewDraft(ctx context.Context, batch Batch) (*core.PreviewResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, core.NewError(core.CodeCancel, "preview cancelled").Wrap(err)
	}
	next, err := core.CloneScenario(s.draft)
	if err != nil {
		return nil, err
	}
	if err := applyEdits(next, batch.Edits); err != nil {
		return nil, err
	}
	next.ParamsVersion = fmt.Sprintf("%s/r%d", s.baseParams, s.revision+1)
	next.Truth = core.Truth{}
	hash, err := core.ComputeSceneHash(next)
	if err != nil {
		return nil, core.NewError(core.CodeState, "failed to hash preview scene").Wrap(err)
	}
	next.SceneHash = hash
	if err := core.ValidateScenario(next); err != nil {
		return nil, err
	}
	ticks := trimTicks(s.sampleTicks, next.FinalTick)
	return core.RunPreview(next, s.previewStep, ticks, next.FinalTick)
}

func (s *Session) Discard() View {
	s.mu.Lock()
	defer s.mu.Unlock()
	if clone, err := core.CloneScenario(s.committed); err == nil {
		s.draft = clone
	}
	s.revision = s.committedRevision
	s.lastError = nil
	return s.viewLocked()
}

func (s *Session) settleLocked(ctx context.Context) error {
	ticks := trimTicks(s.sampleTicks, s.committed.FinalTick)
	sink := &memoryCheckpoints{}
	exact, err := core.RunWindowed(ctx, s.committed, sink, core.WindowOptions{
		WindowSize: 64, SampleTicks: ticks,
	})
	if err != nil {
		return err
	}
	preview, err := core.RunPreview(s.committed, s.previewStep, ticks, s.committed.FinalTick)
	if err != nil {
		return err
	}
	if diff := core.CompareSnapshots(exact.Final, preview.Final); diff != nil {
		return diff
	}
	s.exact = exact
	s.preview = preview
	s.heatmap = buildHeatmap(exact, preview, ticks)
	return nil
}

func trimTicks(input []int64, finalTick int64) []int64 {
	ticks := append([]int64(nil), input...)
	if len(ticks) == 0 || ticks[len(ticks)-1] != finalTick {
		ticks = append(ticks, finalTick)
	}
	out := ticks[:0]
	for _, tick := range ticks {
		if tick > finalTick {
			continue
		}
		if tick < 0 {
			tick = 0
		}
		out = append(out, tick)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return compactTicks(out)
}

func (s *Session) dirtyLocked() bool {
	if s.committed == nil || s.draft == nil {
		return false
	}
	return s.draft.SceneHash != s.committed.SceneHash
}

func applyEdits(scene *core.Scenario, edits []Edit) *core.Error {
	nodes := make(map[string]*core.Node, len(scene.Nodes))
	for i := range scene.Nodes {
		nodes[scene.Nodes[i].ID] = &scene.Nodes[i]
	}
	sortIntents := false
	for i, edit := range edits {
		path := fmt.Sprintf("edits[%d]", i)
		switch edit.Kind {
		case "node_params":
			node, ok := nodes[edit.NodeID]
			if !ok {
				return core.NewError(core.CodeUnit, "edit targets a missing node").At(path).ForNode(edit.NodeID)
			}
			if edit.Outputs != nil {
				node.Outputs = copyMap(*edit.Outputs)
			}
			if edit.Inputs != nil {
				node.Inputs = copyMap(*edit.Inputs)
			}
			if edit.Capacity != nil {
				node.Capacity = copyMap(*edit.Capacity)
			}
			if edit.WorkersRequired != nil {
				if *edit.WorkersRequired < 0 {
					return core.NewError(core.CodeParam, "workers_required must be non-negative").
						At(path + ".workers_required").ForNode(edit.NodeID)
				}
				node.WorkersRequired = *edit.WorkersRequired
			}
		case "node_workers":
			node, ok := nodes[edit.NodeID]
			if !ok {
				return core.NewError(core.CodeUnit, "edit targets a missing node").At(path).ForNode(edit.NodeID)
			}
			if edit.Workers == nil {
				return core.NewError(core.CodeParam, "node_workers edit is missing workers").
					At(path + ".workers").ForNode(edit.NodeID)
			}
			node.Workers = *edit.Workers
		case "node_active":
			node, ok := nodes[edit.NodeID]
			if !ok {
				return core.NewError(core.CodeUnit, "edit targets a missing node").At(path).ForNode(edit.NodeID)
			}
			if edit.Active == nil {
				return core.NewError(core.CodeParam, "node_active edit is missing active").
					At(path + ".active").ForNode(edit.NodeID)
			}
			node.Active = *edit.Active
		case "intent_time":
			if err := checkIntentEdit(scene, edit, path); err != nil {
				return err
			}
			if edit.Timestamp == nil {
				return core.NewError(core.CodeParam, "intent_time edit is missing timestamp").
					At(path + ".timestamp")
			}
			scene.Intents[*edit.IntentIndex].Timestamp = *edit.Timestamp
			sortIntents = true
		case "intent_workers":
			if err := checkIntentEdit(scene, edit, path); err != nil {
				return err
			}
			if edit.Workers == nil {
				return core.NewError(core.CodeParam, "intent_workers edit is missing workers").
					At(path + ".workers")
			}
			scene.Intents[*edit.IntentIndex].Workers = edit.Workers
		case "intent_level":
			if err := checkIntentEdit(scene, edit, path); err != nil {
				return err
			}
			if edit.Level == nil {
				return core.NewError(core.CodeParam, "intent_level edit is missing level").
					At(path + ".level")
			}
			scene.Intents[*edit.IntentIndex].Level = edit.Level
		case "intent_recipe":
			if err := checkIntentEdit(scene, edit, path); err != nil {
				return err
			}
			if edit.Recipe == nil {
				return core.NewError(core.CodeParam, "intent_recipe edit is missing recipe").
					At(path + ".recipe")
			}
			cp := edit.Recipe.Clone()
			scene.Intents[*edit.IntentIndex].Recipe = &cp
		case "final_tick":
			if edit.FinalTick == nil {
				return core.NewError(core.CodeParam, "final_tick edit is missing final_tick").
					At(path + ".final_tick")
			}
			scene.FinalTick = *edit.FinalTick
		default:
			return core.NewError(core.CodeParam, "unknown edit kind").At(path+".kind").
				WithValues(edit.Kind, "node_params|node_workers|node_active|intent_time|intent_workers|intent_level|intent_recipe|final_tick")
		}
	}
	if sortIntents {
		sort.SliceStable(scene.Intents, func(i, j int) bool {
			if scene.Intents[i].Timestamp == scene.Intents[j].Timestamp {
				return scene.Intents[i].Seq < scene.Intents[j].Seq
			}
			return scene.Intents[i].Timestamp < scene.Intents[j].Timestamp
		})
	}
	return nil
}

func checkIntentEdit(scene *core.Scenario, edit Edit, path string) *core.Error {
	if edit.IntentIndex == nil || *edit.IntentIndex < 0 || *edit.IntentIndex >= len(scene.Intents) {
		return core.NewError(core.CodeParam, "intent_index is outside the intent sequence").
			At(path + ".intent_index")
	}
	return nil
}

func txnError(batchID string, err error) *core.Error {
	cause := core.AsError(err)
	e := core.NewError(core.CodeTxn, "batch commit failed and was rolled back").
		WithBatch(batchID)
	e.Path = cause.Path
	e.NodeID = cause.NodeID
	e.IntentID = cause.IntentID
	e.IntentIdx = cause.IntentIdx
	e.Field = cause.Field
	e.Left = cause.Left
	e.Right = cause.Right
	e.Tick = cause.Tick
	e.Cause = cause.Error()
	return e
}

func copyMap(in map[string]int64) map[string]int64 {
	out := make(map[string]int64, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func compactTicks(in []int64) []int64 {
	out := in[:0]
	for _, v := range in {
		if len(out) == 0 || out[len(out)-1] != v {
			out = append(out, v)
		}
	}
	return out
}

func buildHeatmap(exact *core.WindowRunResult, preview *core.PreviewResult, ticks []int64) Heatmap {
	hm := Heatmap{Ticks: append([]int64(nil), ticks...)}
	exactByTick := make(map[int64]core.Snapshot, len(exact.Snapshots))
	for _, s := range exact.Snapshots {
		exactByTick[s.Tick] = s
	}
	previewByTick := make(map[int64]core.Snapshot, len(preview.Points))
	for _, p := range preview.Points {
		previewByTick[p.Tick] = p.Snapshot
	}
	nodeSet := make(map[string]struct{})
	for _, s := range exact.Snapshots {
		for _, n := range s.Nodes {
			nodeSet[n.ID] = struct{}{}
		}
	}
	for id := range nodeSet {
		hm.Nodes = append(hm.Nodes, id)
	}
	sort.Strings(hm.Nodes)
	hm.Equal = true
	hm.Cells = make([][]int64, len(hm.Nodes))
	for i, id := range hm.Nodes {
		hm.Cells[i] = make([]int64, len(ticks))
		for j, tick := range ticks {
			e := nodeStock(exactByTick[tick], id)
			p := nodeStock(previewByTick[tick], id)
			keys := make(map[string]struct{}, len(e)+len(p))
			for k := range e {
				keys[k] = struct{}{}
			}
			for k := range p {
				keys[k] = struct{}{}
			}
			var diff int64
			for k := range keys {
				d := e[k] - p[k]
				if d < 0 {
					d = -d
				}
				diff += d
			}
			hm.Cells[i][j] = diff
			if diff > hm.MaxAbs {
				hm.MaxAbs = diff
			}
			if diff != 0 {
				hm.Equal = false
			}
		}
	}
	return hm
}

func nodeStock(s core.Snapshot, id string) map[string]int64 {
	for _, n := range s.Nodes {
		if n.ID == id {
			return n.Stock
		}
	}
	return nil
}
