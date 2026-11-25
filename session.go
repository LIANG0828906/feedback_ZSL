package acoustic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
)

type SessionOptions struct {
	Store           CheckpointStore
	WorkerOptions   WorkerOptions
	FailureInjector func(roomID int) error
	AfterCommit     func(roomID, committed int) error
}

type CommitRecord struct {
	RoomID         int    `json:"roomId"`
	CommitSequence int    `json:"commitSequence"`
	ContentHash    string `json:"contentHash"`
}

type RestoreReport struct {
	LoadedRooms   []int  `json:"loadedRooms"`
	Invalidated   bool   `json:"invalidated"`
	InvalidReason string `json:"invalidReason,omitempty"`
	FullRecompute bool   `json:"fullRecompute"`
}

type RunResult struct {
	ProjectFingerprint string             `json:"projectFingerprint"`
	CommitSequence     []int              `json:"commitSequence"`
	Commits            []CommitRecord     `json:"commits"`
	Snapshots          []RoomSnapshot     `json:"snapshots"`
	ListenerSolutions  []ListenerSolution `json:"listenerSolutions"`
	ChainSummaries     []ChainSummary     `json:"chainSummaries"`
	States             []RoomStatus       `json:"states"`
	Progress           Progress           `json:"progress"`
	WorkerStats        []WorkerStats      `json:"workerStats"`
	Restore            *RestoreReport     `json:"restore,omitempty"`
	Failure            *RoomFailure       `json:"failure,omitempty"`
}

type RoomFailure struct {
	RoomID  int    `json:"roomId"`
	Code    Code   `json:"code"`
	Message string `json:"message"`
}

type Session struct {
	runMu           sync.Mutex
	mu              sync.RWMutex
	project         *Project
	fingerprint     string
	options         SessionOptions
	machine         *StateMachine
	snapshots       map[int]RoomSnapshot
	committedOrder  []int
	restore         *RestoreReport
	lastWorkerStats []WorkerStats
	failure         *RoomFailure
	roomIDs         []int
}

func NewSession(project *Project, options SessionOptions) (*Session, error) {
	if project == nil {
		return nil, &Error{Code: CodeSchema, Op: "session", Message: "project is nil"}
	}
	if err := project.Validate(); err != nil {
		return nil, err
	}
	fingerprint, err := ProjectFingerprint(project)
	if err != nil {
		return nil, err
	}
	clone, err := cloneProject(project)
	if err != nil {
		return nil, err
	}
	roomIDs := make([]int, 0, len(project.Rooms))
	for _, room := range project.Rooms {
		roomIDs = append(roomIDs, room.ID)
	}
	sortInts(roomIDs)
	machine, err := NewStateMachine(roomIDs)
	if err != nil {
		return nil, err
	}
	return &Session{
		project:     clone,
		fingerprint: fingerprint,
		options:     options,
		machine:     machine,
		snapshots:   make(map[int]RoomSnapshot, len(roomIDs)),
		roomIDs:     roomIDs,
		restore:     &RestoreReport{FullRecompute: true},
	}, nil
}

func (s *Session) Project() *Project {
	s.mu.RLock()
	defer s.mu.RUnlock()
	clone, _ := cloneProject(s.project)
	return clone
}

func (s *Session) Restore(ctx context.Context) (*RestoreReport, error) {
	s.runMu.Lock()
	defer s.runMu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.options.Store == nil {
		s.restore = &RestoreReport{}
		return cloneRestore(s.restore), nil
	}
	load, err := s.options.Store.Load(ctx, s.fingerprint, s.project.ParamsVersion, s.project.AlgorithmVersion)
	if err != nil {
		return nil, err
	}
	if load.Invalidated {
		s.restore = &RestoreReport{Invalidated: true, InvalidReason: load.InvalidReason, FullRecompute: true}
		return cloneRestore(s.restore), nil
	}
	if len(load.Snapshots) > len(s.roomIDs) {
		if err := s.options.Store.Discard(ctx); err != nil {
			return nil, err
		}
		s.restore = &RestoreReport{Invalidated: true, InvalidReason: "checkpoint contains more rooms than the project", FullRecompute: true}
		return cloneRestore(s.restore), nil
	}
	for i, snapshot := range load.Snapshots {
		if snapshot.RoomID != s.roomIDs[i] || snapshot.CommitSequence != i+1 {
			if err := s.options.Store.Discard(ctx); err != nil {
				return nil, err
			}
			s.restore = &RestoreReport{Invalidated: true, InvalidReason: "checkpoint rooms are not a stable committed prefix", FullRecompute: true}
			return cloneRestore(s.restore), nil
		}
		if snapshot.ParamsVersion != s.project.ParamsVersion || snapshot.AlgorithmVersion != s.project.AlgorithmVersion || snapshot.ProjectFingerprint != s.fingerprint {
			if err := s.options.Store.Discard(ctx); err != nil {
				return nil, err
			}
			s.restore = &RestoreReport{Invalidated: true, InvalidReason: "checkpoint metadata mismatch", FullRecompute: true}
			return cloneRestore(s.restore), nil
		}
		computedHash, err := SnapshotDigest(snapshot)
		if err != nil {
			return nil, err
		}
		if computedHash != snapshot.ContentHash {
			if err := s.options.Store.Discard(ctx); err != nil {
				return nil, err
			}
			s.restore = &RestoreReport{Invalidated: true, InvalidReason: "checkpoint content hash mismatch", FullRecompute: true}
			return cloneRestore(s.restore), nil
		}
		if err := s.machine.RestoreCommitted(snapshot.RoomID, snapshot.CommitSequence); err != nil {
			return nil, err
		}
		s.snapshots[snapshot.RoomID] = cloneSnapshot(snapshot)
		s.committedOrder = append(s.committedOrder, snapshot.RoomID)
	}
	loaded := make([]int, len(load.Snapshots))
	for i := range load.Snapshots {
		loaded[i] = load.Snapshots[i].RoomID
	}
	s.restore = &RestoreReport{LoadedRooms: loaded, FullRecompute: len(loaded) == 0}
	return cloneRestore(s.restore), nil
}

func (s *Session) Run(ctx context.Context) (*RunResult, error) {
	s.runMu.Lock()
	defer s.runMu.Unlock()
	for _, roomID := range s.roomIDs {
		if err := ctx.Err(); err != nil {
			return s.collectResult(), err
		}
		status, ok := s.machine.State(roomID)
		if !ok {
			return s.collectResult(), &Error{Code: CodeState, Op: "run", RoomIDs: []int{roomID}, Message: "room state is missing"}
		}
		switch status.State {
		case RoomCommitted:
			continue
		case RoomFailed:
			return s.collectResult(), &Error{Code: CodeState, Op: "run", RoomIDs: []int{roomID}, Message: "room is failed and requires retry"}
		}
		if err := s.solveRoom(ctx, roomID); err != nil {
			var resultErr error
			if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
				resultErr = err
			}
			return s.collectResult(), resultErrOrContext(err, resultErr)
		}
	}
	return s.collectResult(), nil
}

func resultErrOrContext(original, other error) error {
	if other != nil {
		return other
	}
	return original
}

func (s *Session) solveRoom(ctx context.Context, roomID int) error {
	status, ok := s.machine.State(roomID)
	if !ok {
		return &Error{Code: CodeState, Op: "solve_room", RoomIDs: []int{roomID}, Message: "unknown room"}
	}
	if status.State != RoomPending {
		return &Error{Code: CodeState, Op: "solve_room", RoomIDs: []int{roomID}, Message: "room is not pending"}
	}
	if err := s.ensurePrefixCommitted(roomID); err != nil {
		return err
	}
	if err := s.machine.Transition(roomID, RoomAnalyzingChain); err != nil {
		return err
	}
	if s.options.FailureInjector != nil {
		if err := s.options.FailureInjector(roomID); err != nil {
			s.machine.Fail(roomID, err.Error())
			s.setFailure(roomID, err)
			return err
		}
	}
	chains, err := roomChainMap(s.project, roomID)
	if err != nil {
		s.machine.Fail(roomID, err.Error())
		s.setFailure(roomID, err)
		return err
	}
	if err := ctx.Err(); err != nil {
		s.machine.ResetPending(roomID)
		return err
	}
	if err := s.machine.Transition(roomID, RoomSolvingReflection); err != nil {
		return err
	}
	solutions, stats, err := SolveRoomListenersConcurrentWithChains(ctx, s.project, roomID, s.options.WorkerOptions, chains)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			s.machine.ResetPending(roomID)
			return err
		}
		s.machine.Fail(roomID, err.Error())
		s.setFailure(roomID, err)
		return err
	}
	s.mu.Lock()
	s.lastWorkerStats = append(s.lastWorkerStats, stats)
	s.mu.Unlock()

	s.mu.RLock()
	nextSequence := len(s.committedOrder) + 1
	s.mu.RUnlock()
	snapshot, err := s.buildSnapshot(roomID, nextSequence, solutions)
	if err != nil {
		s.machine.Fail(roomID, err.Error())
		s.setFailure(roomID, err)
		return err
	}
	if s.options.Store != nil {
		if err := s.options.Store.Commit(ctx, snapshot); err != nil {
			s.machine.Fail(roomID, err.Error())
			s.setFailure(roomID, err)
			return err
		}
	}
	if err := s.machine.Transition(roomID, RoomCommitted); err != nil {
		return err
	}
	s.mu.Lock()
	s.snapshots[roomID] = cloneSnapshot(snapshot)
	s.committedOrder = append(s.committedOrder, roomID)
	s.failure = nil
	s.mu.Unlock()
	if s.options.AfterCommit != nil {
		if err := s.options.AfterCommit(roomID, s.machine.Progress().Committed); err != nil {
			return err
		}
	}
	return nil
}

func (s *Session) ensurePrefixCommitted(roomID int) error {
	for _, candidate := range s.roomIDs {
		if candidate == roomID {
			return nil
		}
		status, _ := s.machine.State(candidate)
		if status.State != RoomCommitted {
			return &Error{Code: CodeState, Op: "commit_order", RoomIDs: []int{roomID, candidate}, Message: "earlier room must be committed first"}
		}
	}
	return nil
}

func (s *Session) buildSnapshot(roomID, sequence int, solutions []ListenerSolution) (RoomSnapshot, error) {
	reflectors := reflectorsForRoom(s.project, roomID)
	openings := openingsForRoom(s.project, roomID)
	var chains []ChainSummary
	for _, solution := range solutions {
		for _, source := range solution.Sources {
			chains = append(chains, source.Chain)
		}
	}
	sortChains(chains)
	snapshot := RoomSnapshot{
		RoomID:             roomID,
		CommitSequence:     sequence,
		ParamsVersion:      s.project.ParamsVersion,
		AlgorithmVersion:   s.project.AlgorithmVersion,
		ProjectFingerprint: s.fingerprint,
		Reflectors:         append([]Reflector(nil), reflectors...),
		Openings:           append([]Opening(nil), openings...),
		ChainSummaries:     chains,
		ListenerSolutions:  solutions,
	}
	hash, err := SnapshotDigest(snapshot)
	if err != nil {
		return RoomSnapshot{}, err
	}
	snapshot.ContentHash = hash
	return snapshot, nil
}

func (s *Session) ResetRoomFailure(roomID int) error {
	s.runMu.Lock()
	defer s.runMu.Unlock()
	status, ok := s.machine.State(roomID)
	if !ok {
		return &Error{Code: CodeState, Op: "retry", RoomIDs: []int{roomID}, Message: "unknown room"}
	}
	if status.State != RoomFailed {
		return &Error{Code: CodeState, Op: "retry", RoomIDs: []int{roomID}, Message: "only failed rooms can be retried"}
	}
	if err := s.machine.Transition(roomID, RoomPending); err != nil {
		return err
	}
	s.mu.Lock()
	if s.failure != nil && s.failure.RoomID == roomID {
		s.failure = nil
	}
	s.mu.Unlock()
	return nil
}

func (s *Session) Progress() Progress {
	return s.machine.Progress()
}

func (s *Session) States() []RoomStatus {
	return s.machine.All()
}

func (s *Session) Snapshot(roomID int) (RoomSnapshot, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	snapshot, ok := s.snapshots[roomID]
	if !ok {
		return RoomSnapshot{}, false
	}
	return cloneSnapshot(snapshot), true
}

func (s *Session) WorkerStats() []WorkerStats {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return append([]WorkerStats(nil), s.lastWorkerStats...)
}

func (s *Session) collectResult() *RunResult {
	s.mu.RLock()
	defer s.mu.RUnlock()
	result := &RunResult{
		ProjectFingerprint: s.fingerprint,
		States:             s.machine.All(),
		Progress:           s.machine.Progress(),
		WorkerStats:        append([]WorkerStats(nil), s.lastWorkerStats...),
	}
	for _, roomID := range s.committedOrder {
		snapshot := s.snapshots[roomID]
		result.CommitSequence = append(result.CommitSequence, snapshot.CommitSequence)
		result.Commits = append(result.Commits, CommitRecord{
			RoomID:         snapshot.RoomID,
			CommitSequence: snapshot.CommitSequence,
			ContentHash:    snapshot.ContentHash,
		})
		result.Snapshots = append(result.Snapshots, cloneSnapshot(snapshot))
		result.ListenerSolutions = append(result.ListenerSolutions, cloneListenerSolutions(snapshot.ListenerSolutions)...)
		result.ChainSummaries = append(result.ChainSummaries, cloneChains(snapshot.ChainSummaries)...)
	}
	result.Restore = cloneRestore(s.restore)
	if s.failure != nil {
		failure := *s.failure
		result.Failure = &failure
	}
	return result
}

func (s *Session) setFailure(roomID int, err error) {
	code := ErrorCode(err)
	if code == "" {
		code = CodeInternal
	}
	s.mu.Lock()
	s.failure = &RoomFailure{RoomID: roomID, Code: code, Message: err.Error()}
	s.mu.Unlock()
}

func openingsForRoom(project *Project, roomID int) []Opening {
	var openings []Opening
	for _, opening := range project.Openings {
		wall, ok := project.wallByID(opening.WallID)
		if ok && (wall.RoomA == roomID || wall.RoomB == roomID) {
			openings = append(openings, opening)
		}
	}
	sortOpenings(openings)
	return openings
}

func sortChains(chains []ChainSummary) {
	for i := 1; i < len(chains); i++ {
		for j := i; j > 0; j-- {
			left, right := chains[j-1], chains[j]
			if left.ListenerID < right.ListenerID || (left.ListenerID == right.ListenerID && left.SourceID <= right.SourceID) {
				break
			}
			chains[j], chains[j-1] = chains[j-1], chains[j]
		}
	}
}

func cloneSnapshot(snapshot RoomSnapshot) RoomSnapshot {
	clone := snapshot
	clone.Reflectors = append([]Reflector(nil), snapshot.Reflectors...)
	clone.Openings = append([]Opening(nil), snapshot.Openings...)
	clone.ChainSummaries = cloneChains(snapshot.ChainSummaries)
	clone.ListenerSolutions = cloneListenerSolutions(snapshot.ListenerSolutions)
	return clone
}

func cloneChains(chains []ChainSummary) []ChainSummary {
	result := make([]ChainSummary, len(chains))
	for i, chain := range chains {
		result[i] = chain
		result[i].RoomIDs = append([]int(nil), chain.RoomIDs...)
		result[i].OpeningIDs = append([]int(nil), chain.OpeningIDs...)
		result[i].Segments = append([]PathSegment(nil), chain.Segments...)
	}
	return result
}

func cloneListenerSolutions(solutions []ListenerSolution) []ListenerSolution {
	result := make([]ListenerSolution, len(solutions))
	for i, solution := range solutions {
		result[i] = solution
		result[i].Sources = make([]SourceReflectionResult, len(solution.Sources))
		for j, source := range solution.Sources {
			result[i].Sources[j] = source
			result[i].Sources[j].Chain = cloneChains([]ChainSummary{source.Chain})[0]
			result[i].Sources[j].Paths = append([]ReflectionPath(nil), source.Paths...)
			result[i].Sources[j].Rejections = append([]ReflectionRejection(nil), source.Rejections...)
		}
		result[i].Paths = append([]ReflectionPath(nil), solution.Paths...)
	}
	return result
}

func cloneProject(project *Project) (*Project, error) {
	data, err := marshalProject(project)
	if err != nil {
		return nil, err
	}
	var clone Project
	if err := unmarshalProject(data, &clone); err != nil {
		return nil, err
	}
	return &clone, nil
}

func cloneRestore(report *RestoreReport) *RestoreReport {
	if report == nil {
		return nil
	}
	clone := *report
	clone.LoadedRooms = append([]int(nil), report.LoadedRooms...)
	return &clone
}

func sortInts(values []int) {
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && values[j] < values[j-1]; j-- {
			values[j], values[j-1] = values[j-1], values[j]
		}
	}
}

func marshalProject(project *Project) ([]byte, error) {
	return jsonMarshal(project)
}

func unmarshalProject(data []byte, target *Project) error {
	if err := jsonUnmarshal(data, target); err != nil {
		return &Error{Code: CodeInternal, Op: "clone_project", Message: "cannot clone project", Cause: err.Error()}
	}
	return nil
}

func jsonMarshal(value any) ([]byte, error) {
	return json.Marshal(value)
}

func jsonUnmarshal(data []byte, target any) error {
	return json.Unmarshal(data, target)
}

func (r *RunResult) String() string {
	return fmt.Sprintf("commits=%v progress=%d/%d", r.CommitSequence, r.Progress.Committed, r.Progress.Total)
}
