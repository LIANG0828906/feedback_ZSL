package workbench

import (
	"encoding/json"
	"errors"
	"os"
	"sort"
	"sync"

	"storyboardstudio/internal/checkpoint"
	"storyboardstudio/internal/contenthash"
	"storyboardstudio/internal/dependency"
	"storyboardstudio/internal/engine"
	"storyboardstudio/internal/model"
)

var ErrInterrupted = errors.New("simulated session interruption after committed boundary")

type EditSpec struct {
	NodeID     string              `json:"node_id"`
	Kind       dependency.EditKind `json:"kind"`
	LocalIndex int                 `json:"local_index,omitempty"`
	Condition  *model.Condition    `json:"condition,omitempty"`
	Effects    *model.Effects      `json:"effects,omitempty"`
	DurationMS *int64              `json:"duration_ms,omitempty"`
}

type ReplayReport struct {
	Edit           EditSpec         `json:"edit"`
	DependencyEdit dependency.Edit  `json:"dependency_edit"`
	Plan           *dependency.Plan `json:"plan"`
	ReplayOrder    []string         `json:"replay_order"`
	CacheHits      []int            `json:"cache_hits"`
	CacheMisses    []int            `json:"cache_misses"`
	Result         *model.RunResult `json:"result"`
	FullResult     *model.RunResult `json:"-"`
	Comparison     *engine.Mismatch `json:"comparison"`
}

type ResumeReport struct {
	Result            *model.RunResult `json:"result"`
	RecoveredFullRun  bool             `json:"recovered_full_run"`
	DiscardedSnapshot *model.Error     `json:"discarded_snapshot,omitempty"`
}

type Workbench struct {
	Store *checkpoint.Store

	mu          sync.Mutex
	projects    map[string]*model.Project
	lastResults map[string]*model.RunResult
}

func New(dbPath string) (*Workbench, error) {
	store, err := checkpoint.Open(dbPath)
	if err != nil {
		return nil, err
	}
	return &Workbench{Store: store, projects: map[string]*model.Project{}, lastResults: map[string]*model.RunResult{}}, nil
}

func NewMemory() (*Workbench, error) {
	return New(":memory:")
}

func (w *Workbench) Close() error {
	if w.Store == nil {
		return nil
	}
	return w.Store.Close()
}

func (w *Workbench) LoadBytes(data []byte) (*model.Project, error) {
	project, err := model.LoadProject(data)
	if err != nil {
		return nil, err
	}
	w.mu.Lock()
	w.projects[project.ID] = project
	w.mu.Unlock()
	return project, nil
}

func (w *Workbench) LoadFile(path string) (*model.Project, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return w.LoadBytes(data)
}

func (w *Workbench) AddProject(project *model.Project) error {
	if err := model.ValidateProject(project); err != nil {
		return err
	}
	w.mu.Lock()
	w.projects[project.ID] = project
	w.mu.Unlock()
	return nil
}

func (w *Workbench) Get(projectID string) (*model.Project, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	project, ok := w.projects[projectID]
	if !ok {
		return nil, &model.Error{Code: model.CodeRef, ConflictNode: projectID, Message: "project not loaded"}
	}
	return project, nil
}

func (w *Workbench) ListProjects() []*model.Project {
	w.mu.Lock()
	defer w.mu.Unlock()
	result := make([]*model.Project, 0, len(w.projects))
	for _, project := range w.projects {
		result = append(result, project)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result
}

func (w *Workbench) RunContinuous(projectID string) (*model.RunResult, error) {
	project, err := w.Get(projectID)
	if err != nil {
		return nil, err
	}
	result, err := engine.Run(project)
	if err != nil {
		return result, err
	}
	w.mu.Lock()
	w.lastResults[projectID] = result
	w.mu.Unlock()
	for _, shot := range result.Shots {
		if err := w.Store.PutShot(projectID, shot); err != nil {
			return result, err
		}
	}
	return result, nil
}

func (w *Workbench) RunCheckpointed(projectID, sessionID string, interruptAt int) (*model.RunResult, error) {
	project, err := w.Get(projectID)
	if err != nil {
		return nil, err
	}
	contentHash := contenthash.Of(project)
	observer := func(shot model.PresentedShot, state *engine.State) error {
		if err := engine.ValidateBoundary(shot, state); err != nil {
			return err
		}
		cp := CheckpointFromState(sessionID, contentHash, project.Schema, shot, state)
		if err := w.Store.CommitBoundary(cp, shot); err != nil {
			return err
		}
		if interruptAt >= 0 && shot.GlobalIndex == interruptAt {
			return ErrInterrupted
		}
		return nil
	}
	result, err := engine.RunWithObserver(project, observer)
	if result != nil {
		w.mu.Lock()
		w.lastResults[projectID] = result
		w.mu.Unlock()
		status := "completed"
		if errors.Is(err, ErrInterrupted) {
			status = "interrupted"
		} else if err != nil {
			status = "failed"
		}
		_ = w.Store.SaveSession(checkpoint.SessionRecord{
			ID: sessionID, ProjectID: projectID, ContentHash: contentHash,
			StructureVersion: project.Schema, AlgorithmVersion: model.AlgorithmVersion, Status: status,
		}, result)
	}
	return result, err
}

func (w *Workbench) Resume(projectID, sessionID string, interruptAt int) (*ResumeReport, error) {
	project, err := w.Get(projectID)
	if err != nil {
		return nil, err
	}
	expectedHash := contenthash.Of(project)
	cp, err := w.Store.LoadValidCheckpoint(sessionID, expectedHash, project.Schema, model.AlgorithmVersion)
	if err != nil {
		var coded *model.Error
		if errors.As(err, &coded) && (coded.Code == model.CodeHash || coded.Code == model.CodeVersion) {
			full, runErr := w.RunCheckpointed(projectID, sessionID, -1)
			if runErr != nil {
				return nil, runErr
			}
			return &ResumeReport{Result: full, RecoveredFullRun: true, DiscardedSnapshot: coded}, nil
		}
		return nil, err
	}
	if cp == nil {
		result, runErr := w.RunCheckpointed(projectID, sessionID, interruptAt)
		return &ResumeReport{Result: result}, runErr
	}
	rows, err := w.Store.ListCheckpoints(sessionID)
	if err != nil {
		return nil, err
	}
	prefix := []model.PresentedShot{}
	for _, row := range rows {
		shot, ok, err := w.Store.GetShot(row.ShotHash)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, &model.Error{Code: model.CodeHash, ConflictShot: row.ShotHash,
				Message: "committed checkpoint shot cache is missing"}
		}
		prefix = append(prefix, *shot)
	}

	state := StateFromCheckpoint(cp)
	observer := func(shot model.PresentedShot, next *engine.State) error {
		if err := engine.ValidateBoundary(shot, next); err != nil {
			return err
		}
		nextCP := CheckpointFromState(sessionID, expectedHash, project.Schema, shot, next)
		if err := w.Store.CommitBoundary(nextCP, shot); err != nil {
			return err
		}
		if interruptAt >= 0 && shot.GlobalIndex == interruptAt {
			return ErrInterrupted
		}
		return nil
	}
	remaining, err := engine.ResumeWithObserver(project, state, observer)
	if err != nil && !errors.Is(err, ErrInterrupted) {
		return &ResumeReport{Result: remaining}, err
	}
	full := &model.RunResult{
		ProjectID: project.ID, ContentHash: expectedHash, AlgorithmVersion: model.AlgorithmVersion,
		StructureVersion: project.Schema, EventOrder: remaining.EventOrder,
		ReachableNodes: remaining.ReachableNodes, TerminalFlags: remaining.TerminalFlags,
		Characters: remaining.Characters, ConsumedChoices: remaining.ConsumedChoices,
		Shots: append(append([]model.PresentedShot{}, prefix...), remaining.Shots...),
	}
	w.mu.Lock()
	w.lastResults[projectID] = full
	w.mu.Unlock()
	status := "completed"
	if errors.Is(err, ErrInterrupted) {
		status = "interrupted"
	}
	if saveErr := w.Store.SaveSession(checkpoint.SessionRecord{
		ID: sessionID, ProjectID: projectID, ContentHash: expectedHash,
		StructureVersion: project.Schema, AlgorithmVersion: model.AlgorithmVersion, Status: status,
	}, full); saveErr != nil {
		return &ResumeReport{Result: full}, saveErr
	}
	return &ResumeReport{Result: full}, err
}

func (w *Workbench) EditAndReplay(projectID, sessionID string, edit EditSpec) (*ReplayReport, error) {
	w.mu.Lock()
	project, ok := w.projects[projectID]
	baseline := w.lastResults[projectID]
	w.mu.Unlock()
	if !ok {
		return nil, &model.Error{Code: model.CodeRef, ConflictNode: projectID, Message: "project not loaded"}
	}
	if baseline == nil {
		var err error
		baseline, err = w.RunContinuous(projectID)
		if err != nil {
			return nil, err
		}
	}

	graph := dependency.Build(project)
	depEdit := dependency.Edit{NodeID: edit.NodeID, Kind: edit.Kind, LocalIndex: edit.LocalIndex}
	plan, err := graph.PlanEdit(depEdit, baseline.Shots)
	if err != nil {
		return nil, err
	}
	if err := ApplyEdit(project, edit); err != nil {
		return nil, err
	}
	if err := model.ValidateProject(project); err != nil {
		return nil, err
	}

	topo, err := model.TopologicalOrder(project.Nodes)
	if err != nil {
		return nil, err
	}
	dirtySet := map[string]bool{}
	for _, id := range plan.ReplayNodes {
		dirtySet[id] = true
	}
	replayOrder := []string{}
	for _, id := range topo {
		if dirtySet[id] {
			replayOrder = append(replayOrder, id)
		}
	}

	replayed, err := engine.Run(project)
	if err != nil {
		return nil, err
	}

	cacheHits := []int{}
	cacheMisses := []int{}
	changedShots := []model.PresentedShot{}
	for i, shot := range replayed.Shots {
		forceInvalid := dirtySet[shot.NodeID] && edit.Kind != dependency.EditDuration
		if plan.InvalidShotIndex[shot.GlobalIndex] {
			forceInvalid = true
		}
		if !forceInvalid {
			if cached, found, getErr := w.Store.GetShot(shot.ContentHash); getErr == nil && found {
				replayed.Shots[i] = *cached
				cacheHits = append(cacheHits, shot.GlobalIndex)
				continue
			}
		}
		cacheMisses = append(cacheMisses, shot.GlobalIndex)
		changedShots = append(changedShots, replayed.Shots[i])
	}

	clone, err := CloneProject(project)
	if err != nil {
		return nil, err
	}
	full, err := engine.Run(clone)
	if err != nil {
		return &ReplayReport{Edit: edit, DependencyEdit: depEdit, Plan: plan, ReplayOrder: replayOrder,
			CacheHits: cacheHits, CacheMisses: cacheMisses, Result: replayed}, err
	}
	comparison := engine.CompareResults(replayed, full)

	w.mu.Lock()
	w.lastResults[projectID] = replayed
	w.mu.Unlock()
	if sessionID != "" {
		if err := w.Store.CommitReplay(checkpoint.SessionRecord{
			ID: sessionID, ProjectID: projectID, ContentHash: replayed.ContentHash,
			StructureVersion: project.Schema, AlgorithmVersion: model.AlgorithmVersion, Status: "edited-replayed",
		}, replayed, changedShots); err != nil {
			return nil, err
		}
	} else {
		for _, shot := range changedShots {
			if err := w.Store.PutShot(projectID, shot); err != nil {
				return nil, err
			}
		}
	}
	return &ReplayReport{
		Edit: edit, DependencyEdit: depEdit, Plan: plan, ReplayOrder: replayOrder,
		CacheHits: cacheHits, CacheMisses: cacheMisses, Result: replayed,
		FullResult: full, Comparison: comparison,
	}, nil
}

func ApplyEdit(project *model.Project, edit EditSpec) error {
	nodeIndex := -1
	for i := range project.Nodes {
		if project.Nodes[i].ID == edit.NodeID {
			nodeIndex = i
			break
		}
	}
	if nodeIndex < 0 {
		return &model.Error{Code: model.CodeRef, ConflictNode: edit.NodeID, Message: "edit targets unknown node"}
	}
	node := &project.Nodes[nodeIndex]
	switch edit.Kind {
	case dependency.EditPreconditions:
		if edit.Condition == nil {
			return &model.Error{Code: model.CodeSchema, Path: "condition", Message: "precondition edit requires condition"}
		}
		node.Preconditions = *edit.Condition
	case dependency.EditEffects:
		if edit.Effects == nil {
			return &model.Error{Code: model.CodeSchema, Path: "effects", Message: "effects edit requires effects"}
		}
		node.OnArrive = *edit.Effects
	case dependency.EditDuration:
		if edit.DurationMS == nil {
			return &model.Error{Code: model.CodeSchema, Path: "duration_ms", Message: "duration edit requires duration_ms"}
		}
		if *edit.DurationMS < 0 {
			return &model.Error{Code: model.CodeSchema, Path: "duration_ms", Message: "duration_ms must be non-negative"}
		}
		if edit.LocalIndex < 0 || edit.LocalIndex >= len(node.Shots) {
			return &model.Error{Code: model.CodeRef, ConflictNode: node.ID, Message: "duration edit shot index out of range"}
		}
		node.Shots[edit.LocalIndex].DurationMS = *edit.DurationMS
		node.Shots[edit.LocalIndex].DurationPresent = true
	default:
		return &model.Error{Code: model.CodeSchema, Path: "kind", Actual: string(edit.Kind), Message: "unsupported edit kind"}
	}
	return nil
}

func CheckpointFromState(sessionID, hash, structure string, shot model.PresentedShot, state *engine.State) checkpoint.Checkpoint {
	return checkpoint.Checkpoint{
		SessionID: sessionID, ShotGlobalIndex: shot.GlobalIndex, NodeID: shot.NodeID,
		ShotLocalIndex: shot.LocalIndex, ConsumedChoiceOffset: state.ConsumedChoices,
		CursorMS: state.CursorMS, Flags: cloneFlags(state.Flags), ShotHash: shot.ContentHash,
		ContentHash: hash, StructureVersion: structure, AlgorithmVersion: model.AlgorithmVersion,
		Summary: checkpoint.NodeSummary{
			LastNodeID: state.LastNodeID, EventOrder: append([]string(nil), state.EventOrder...),
			Arrived: cloneFlags(state.Arrived), Characters: cloneCharacters(state.Characters),
		},
	}
}

func StateFromCheckpoint(cp *checkpoint.Checkpoint) *engine.State {
	state := engine.NewState()
	state.Flags = cloneFlags(cp.Flags)
	state.Arrived = cloneFlags(cp.Summary.Arrived)
	state.EventOrder = append([]string(nil), cp.Summary.EventOrder...)
	state.Characters = cloneCharacters(cp.Summary.Characters)
	state.ConsumedChoices = cp.ConsumedChoiceOffset
	state.CursorMS = cp.CursorMS
	state.LastGlobalIndex = cp.ShotGlobalIndex
	state.LastNodeID = cp.NodeID
	state.LastLocalIndex = cp.ShotLocalIndex
	return state
}

func CloneProject(project *model.Project) (*model.Project, error) {
	data, err := json.Marshal(project)
	if err != nil {
		return nil, err
	}
	var clone model.Project
	if err := json.Unmarshal(data, &clone); err != nil {
		return nil, err
	}
	return &clone, nil
}

func cloneFlags(input map[string]bool) map[string]bool {
	output := make(map[string]bool, len(input))
	for key, value := range input {
		output[key] = value
	}
	return output
}

func cloneCharacters(input map[string]map[string]string) map[string]map[string]string {
	output := make(map[string]map[string]string, len(input))
	for id, values := range input {
		output[id] = make(map[string]string, len(values))
		for key, value := range values {
			output[id][key] = value
		}
	}
	return output
}
