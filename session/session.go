package session

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"sort"
	"sync"
	"time"

	"performanceexpressions/workspace/domain"
	"performanceexpressions/workspace/engine"
)

type CheckpointSink interface {
	AppendBatch(ctx context.Context, checkpoints []domain.Checkpoint) (int64, error)
}

type Options struct {
	Sink         CheckpointSink
	KnownBatches []string
}

type Session struct {
	mu sync.Mutex

	projectID string
	base      domain.Project
	committed domain.Project
	working   domain.Project
	params    domain.Parameters
	sink      CheckpointSink

	committedResult domain.ResultSet
	workingResult   domain.ResultSet
	states          map[string]*domain.SegmentState
	pending         map[string][]domain.EditOp
	batches         map[string]bool
	faults          map[string]Fault
	faultBackup     map[string]domain.Segment
	revision        int64
}

func New(project domain.Project, params domain.Parameters, opts ...Options) (*Session, error) {
	if err := engine.ValidateProject(project, engine.ValidateOptions{RequireTruth: true}); err != nil {
		return nil, err
	}
	committed := domain.CloneProject(project)
	working := domain.CloneProject(project)
	committedResult, err := engine.EvaluateProject(committed, params)
	if err != nil {
		return nil, err
	}
	workingResult, err := engine.EvaluateProject(working, params)
	if err != nil {
		return nil, err
	}
	s := &Session{
		projectID:       project.ID,
		base:            domain.CloneProject(project),
		committed:       committed,
		working:         working,
		params:          params,
		committedResult: committedResult,
		workingResult:   workingResult,
		states:          make(map[string]*domain.SegmentState),
		pending:         make(map[string][]domain.EditOp),
		batches:         make(map[string]bool),
		faults:          make(map[string]Fault),
		faultBackup:     make(map[string]domain.Segment),
	}
	for _, seg := range project.Segments {
		s.states[seg.ID] = &domain.SegmentState{SegmentID: seg.ID}
	}
	if len(opts) > 0 {
		s.sink = opts[0].Sink
		for _, id := range opts[0].KnownBatches {
			s.batches[id] = true
		}
	}
	return s, nil
}

func (s *Session) ProjectID() string { return s.projectID }

func (s *Session) Parameters() domain.Parameters {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.params
}

func (s *Session) SetParameters(params domain.Parameters) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	result, err := engine.EvaluateProject(s.working, params)
	if err != nil {
		return err
	}
	s.params = params
	s.workingResult = result
	committedResult, err := engine.EvaluateProject(s.committed, params)
	if err != nil {
		return err
	}
	s.committedResult = committedResult
	return nil
}

func (s *Session) CommittedProject() domain.Project {
	s.mu.Lock()
	defer s.mu.Unlock()
	return domain.CloneProject(s.committed)
}

func (s *Session) WorkingProject() domain.Project {
	s.mu.Lock()
	defer s.mu.Unlock()
	return domain.CloneProject(s.working)
}

func (s *Session) CommittedResult() domain.ResultSet {
	s.mu.Lock()
	defer s.mu.Unlock()
	return cloneResult(s.committedResult)
}

func (s *Session) WorkingResult() domain.ResultSet {
	s.mu.Lock()
	defer s.mu.Unlock()
	return cloneResult(s.workingResult)
}

func (s *Session) States() []domain.SegmentState {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]domain.SegmentState, 0, len(s.states))
	for _, seg := range s.committed.Segments {
		state := s.states[seg.ID]
		if state == nil {
			state = &domain.SegmentState{SegmentID: seg.ID}
		}
		copy := *state
		if state.LastError != nil {
			errCopy := *state.LastError
			copy.LastError = &errCopy
		}
		out = append(out, copy)
	}
	return out
}

func (s *Session) ApplyEdit(op domain.EditOp) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	candidate := domain.CloneProject(s.working)
	segIndex := findSegment(candidate.Segments, op.SegmentID)
	if segIndex < 0 {
		return s.fail(op.SegmentID, domain.NewError(domain.CodeSchema, "unknown segment %q", op.SegmentID).
			WithSegment(op.SegmentID).WithChannel(op.ChannelID).WithLayer(op.LayerID))
	}
	seg := domain.CloneSegment(candidate.Segments[segIndex])
	if err := applyOp(&seg, op); err != nil {
		return s.fail(op.SegmentID, err)
	}
	if err := engine.ValidateSegment(seg, engine.ValidateOptions{RequireTruth: false}); err != nil {
		return s.fail(op.SegmentID, err)
	}
	candidate.Segments[segIndex] = seg
	candidateResult, err := engine.EvaluateProject(candidate, s.params)
	if err != nil {
		return s.fail(op.SegmentID, err)
	}
	s.working = candidate
	s.workingResult = candidateResult
	s.pending[op.SegmentID] = append(s.pending[op.SegmentID], op)
	state := s.state(op.SegmentID)
	state.Dirty = true
	state.Failed = false
	state.LastError = nil
	return nil
}

func applyOp(seg *domain.Segment, op domain.EditOp) error {
	switch op.Kind {
	case domain.EditMovePoint:
		chIndex := findChannel(seg.Channels, op.ChannelID)
		if chIndex < 0 {
			return domain.NewError(domain.CodeSchema, "unknown channel %q", op.ChannelID).
				WithSegment(seg.ID).WithChannel(op.ChannelID).WithLayer(op.LayerID)
		}
		layerIndex := findLayer(seg.Channels[chIndex].Layers, op.LayerID)
		if layerIndex < 0 {
			return domain.NewError(domain.CodeSchema, "unknown layer %q", op.LayerID).
				WithSegment(seg.ID).WithChannel(op.ChannelID).WithLayer(op.LayerID)
		}
		layer := &seg.Channels[chIndex].Layers[layerIndex]
		if op.PointIndex < 0 || op.PointIndex >= len(layer.Points) {
			return domain.NewError(domain.CodePosition, "control point index %d is out of range", op.PointIndex).
				WithSegment(seg.ID).WithChannel(op.ChannelID).WithLayer(op.LayerID).WithBeat(op.Beat)
		}
		if op.Beat < seg.StartBeat || op.Beat > seg.EndBeat {
			return domain.NewError(domain.CodePosition, "control point beat is outside segment range").
				WithSegment(seg.ID).WithChannel(op.ChannelID).WithLayer(op.LayerID).WithBeat(op.Beat)
		}
		layer.Points[op.PointIndex].Beat = op.Beat
		layer.Points[op.PointIndex].Value = op.Value
		sort.SliceStable(layer.Points, func(i, j int) bool { return layer.Points[i].Beat < layer.Points[j].Beat })
	case domain.EditAddLayer:
		if op.Layer == nil {
			return domain.NewError(domain.CodeSchema, "add_layer requires a layer payload").
				WithSegment(seg.ID).WithChannel(op.ChannelID)
		}
		chIndex := findChannel(seg.Channels, op.ChannelID)
		if chIndex < 0 {
			return domain.NewError(domain.CodeSchema, "unknown channel %q", op.ChannelID).
				WithSegment(seg.ID).WithChannel(op.ChannelID)
		}
		if findLayer(seg.Channels[chIndex].Layers, op.Layer.ID) >= 0 {
			return domain.NewError(domain.CodeSchema, "duplicate layer id %q", op.Layer.ID).
				WithSegment(seg.ID).WithChannel(op.ChannelID).WithLayer(op.Layer.ID)
		}
		newLayer := domain.Layer{ID: op.Layer.ID, Points: append([]domain.ControlPoint(nil), op.Layer.Points...)}
		seg.Channels[chIndex].Layers = append(seg.Channels[chIndex].Layers, newLayer)
	case domain.EditRemoveLayer:
		chIndex := findChannel(seg.Channels, op.ChannelID)
		if chIndex < 0 {
			return domain.NewError(domain.CodeSchema, "unknown channel %q", op.ChannelID).
				WithSegment(seg.ID).WithChannel(op.ChannelID)
		}
		layerIndex := findLayer(seg.Channels[chIndex].Layers, op.LayerID)
		if layerIndex < 0 {
			return domain.NewError(domain.CodeSchema, "unknown layer %q", op.LayerID).
				WithSegment(seg.ID).WithChannel(op.ChannelID).WithLayer(op.LayerID)
		}
		if len(seg.Channels[chIndex].Layers) == 1 {
			return domain.NewError(domain.CodeConflict, "cannot remove the final layer of a channel").
				WithSegment(seg.ID).WithChannel(op.ChannelID).WithLayer(op.LayerID)
		}
		seg.Channels[chIndex].Layers = append(seg.Channels[chIndex].Layers[:layerIndex], seg.Channels[chIndex].Layers[layerIndex+1:]...)
	case domain.EditSetPriority:
		seg.Priorities = append([]domain.PriorityRule(nil), op.Priorities...)
	default:
		return domain.NewError(domain.CodeSchema, "unknown edit kind %q", op.Kind).WithSegment(seg.ID)
	}
	return nil
}

func (s *Session) CommitSegment(ctx context.Context, batchID, segmentID string) (domain.CommitReceipt, error) {
	return s.CommitBatch(ctx, batchID, []string{segmentID})
}

func (s *Session) CommitBatch(ctx context.Context, batchID string, segmentIDs []string) (domain.CommitReceipt, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if batchID == "" {
		return domain.CommitReceipt{}, domain.NewError(domain.CodeSchema, "batch id is required")
	}
	if s.batches[batchID] {
		return domain.CommitReceipt{}, domain.NewError(domain.CodeDuplicate, "batch %q was already committed", batchID)
	}
	ids, err := s.normalizeBatch(segmentIDs)
	if err != nil {
		return domain.CommitReceipt{}, err
	}
	for _, id := range ids {
		if fault, ok := s.faults[id]; ok {
			if commitErr := s.faultError(id, fault); commitErr != nil {
				return domain.CommitReceipt{}, commitErr
			}
		}
	}
	candidate := domain.CloneProject(s.committed)
	for _, id := range ids {
		workingSeg, _, ok := s.working.SegmentByID(id)
		if !ok {
			return domain.CommitReceipt{}, s.fail(id, domain.NewError(domain.CodeSchema, "unknown segment %q", id).WithSegment(id))
		}
		if err := engine.ValidateSegment(workingSeg, engine.ValidateOptions{RequireTruth: false}); err != nil {
			return domain.CommitReceipt{}, s.fail(id, err)
		}
		candidateIndex := findSegment(candidate.Segments, id)
		candidate.Segments[candidateIndex] = domain.CloneSegment(workingSeg)
	}
	candidateResult, err := engine.EvaluateProject(candidate, s.params)
	if err != nil {
		return domain.CommitReceipt{}, s.fail(ids[0], err)
	}
	resultBySegment := make(map[string]domain.SegmentResult, len(candidateResult.Segments))
	for _, result := range candidateResult.Segments {
		resultBySegment[result.SegmentID] = result
	}
	checkpoints := make([]domain.Checkpoint, 0, len(ids))
	for _, id := range ids {
		candidateSeg, _, _ := candidate.SegmentByID(id)
		cp, err := domain.CheckpointSnapshot(candidateSeg, resultBySegment[id], append([]domain.EditOp(nil), s.pending[id]...), s.params)
		if err != nil {
			return domain.CommitReceipt{}, s.fail(id, domain.NewError(domain.CodeCommit, "build checkpoint: %v", err).WithSegment(id))
		}
		cp.ProjectID = s.projectID
		cp.BatchID = batchID
		checkpoints = append(checkpoints, cp)
	}
	workingCandidate := domain.CloneProject(s.working)
	for _, id := range ids {
		index := findSegment(workingCandidate.Segments, id)
		workingCandidate.Segments[index] = domain.CloneSegment(candidate.Segments[findSegment(candidate.Segments, id)])
	}
	workingResult, err := engine.EvaluateProject(workingCandidate, s.params)
	if err != nil {
		return domain.CommitReceipt{}, s.fail(ids[0], domain.NewError(domain.CodeCommit, "working cache refresh failed: %v", err).WithSegment(ids[0]))
	}
	sequence := int64(0)
	if s.sink != nil {
		var appendErr error
		sequence, appendErr = s.sink.AppendBatch(ctx, checkpoints)
		if appendErr != nil {
			appErr := domain.NewError(domain.CodeCommit, "batch persistence failed: %v", appendErr).
				WithSegment(ids[0])
			appErr.Cause = appendErr.Error()
			return domain.CommitReceipt{}, s.fail(ids[0], appErr)
		}
	} else {
		s.revision++
		sequence = s.revision
	}
	s.committed = candidate
	s.committedResult = candidateResult
	for _, id := range ids {
		delete(s.pending, id)
		state := s.state(id)
		state.Dirty = false
		state.Failed = false
		state.LastError = nil
		state.Committed = sequence
		delete(s.faults, id)
		delete(s.faultBackup, id)
	}
	s.working = workingCandidate
	s.workingResult = workingResult
	s.batches[batchID] = true
	if sequence == 0 {
		s.revision++
		sequence = s.revision
	}
	return domain.CommitReceipt{
		BatchID:     batchID,
		SegmentIDs:  append([]string(nil), ids...),
		Sequence:    sequence,
		CommittedAt: time.Now().UTC(),
	}, nil
}

func (s *Session) normalizeBatch(segmentIDs []string) ([]string, error) {
	if len(segmentIDs) == 0 {
		return nil, domain.NewError(domain.CodeSchema, "batch contains no segments")
	}
	seen := make(map[string]bool, len(segmentIDs))
	ids := make([]string, 0, len(segmentIDs))
	for _, id := range segmentIDs {
		if id == "" {
			return nil, domain.NewError(domain.CodeSchema, "batch contains an empty segment id")
		}
		if seen[id] {
			return nil, domain.NewError(domain.CodeConflict, "batch contains duplicate segment %q", id).WithSegment(id)
		}
		if _, _, ok := s.committed.SegmentByID(id); !ok {
			return nil, domain.NewError(domain.CodeSchema, "unknown segment %q", id).WithSegment(id)
		}
		seen[id] = true
		ids = append(ids, id)
	}
	return ids, nil
}

func (s *Session) RestoreCommitted(segments []domain.Segment, batchIDs []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	candidate := domain.CloneProject(s.base)
	for _, restored := range segments {
		index := findSegment(candidate.Segments, restored.ID)
		if index < 0 {
			return domain.NewError(domain.CodeSchema, "checkpoint references unknown segment %q", restored.ID).WithSegment(restored.ID)
		}
		truth := candidate.Segments[index].Truth
		clone := domain.CloneSegment(restored)
		clone.Truth = truth
		candidate.Segments[index] = clone
	}
	if err := engine.ValidateProject(candidate, engine.ValidateOptions{RequireTruth: true}); err != nil {
		return err
	}
	result, err := engine.EvaluateProject(candidate, s.params)
	if err != nil {
		return err
	}
	s.committed = candidate
	s.working = domain.CloneProject(candidate)
	s.committedResult = result
	s.workingResult = cloneResult(result)
	for _, id := range batchIDs {
		s.batches[id] = true
	}
	for _, seg := range candidate.Segments {
		state := s.state(seg.ID)
		state.Dirty = false
		state.Failed = false
		state.LastError = nil
	}
	return nil
}

func (s *Session) ExportCommitted(path string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return writeProject(path, s.committed)
}

func (s *Session) ExportWorking(path string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return writeProject(path, s.working)
}

func (s *Session) CommittedJSON() ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return json.MarshalIndent(s.committed, "", "  ")
}

func (s *Session) PlaybackAt(segmentID string, beat float64) (domain.SampleResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return engine.PlaybackAt(s.committed, s.params, segmentID, beat)
}

func (s *Session) PlaybackAtTick(segmentID string, tick int) (domain.SampleResult, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	beat := float64(tick) / float64(s.committed.SamplesPerBeat)
	for _, seg := range s.committed.Segments {
		if seg.ID != segmentID {
			continue
		}
		if beat < seg.StartBeat || beat >= seg.EndBeat {
			return domain.SampleResult{}, domain.NewError(domain.CodePosition, "playback tick is outside segment").
				WithSegment(segmentID).WithTick(tick).WithBeat(beat)
		}
		return engine.PlaybackAt(s.committed, s.params, segmentID, beat)
	}
	return domain.SampleResult{}, domain.NewError(domain.CodeSchema, "unknown segment %q", segmentID).WithSegment(segmentID)
}

func (s *Session) DiffWorkingAgainstCommitted() (engine.Comparison, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return engine.CompareResultSets(s.committedResult, s.workingResult)
}

func writeProject(path string, project domain.Project) error {
	b, err := json.MarshalIndent(project, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	return os.WriteFile(path, b, 0o644)
}

func cloneResult(in domain.ResultSet) domain.ResultSet {
	b, _ := json.Marshal(in)
	var out domain.ResultSet
	_ = json.Unmarshal(b, &out)
	return out
}

// ApplyProjectOps applies a sequence to a clone and evaluates only once. It is
// the one-shot full recomputation path and does not use any live-session cache.
func ApplyProjectOps(base domain.Project, ops []domain.EditOp, params domain.Parameters) (domain.Project, domain.ResultSet, error) {
	project := domain.CloneProject(base)
	for _, op := range ops {
		index := findSegment(project.Segments, op.SegmentID)
		if index < 0 {
			return domain.Project{}, domain.ResultSet{}, domain.NewError(domain.CodeSchema, "unknown segment %q", op.SegmentID).WithSegment(op.SegmentID)
		}
		seg := domain.CloneSegment(project.Segments[index])
		if err := applyOp(&seg, op); err != nil {
			return domain.Project{}, domain.ResultSet{}, err
		}
		if err := engine.ValidateSegment(seg, engine.ValidateOptions{RequireTruth: false}); err != nil {
			return domain.Project{}, domain.ResultSet{}, err
		}
		project.Segments[index] = seg
	}
	if err := engine.ValidateProject(project, engine.ValidateOptions{RequireTruth: false}); err != nil {
		return domain.Project{}, domain.ResultSet{}, err
	}
	result, err := engine.EvaluateProject(project, params)
	if err != nil {
		return domain.Project{}, domain.ResultSet{}, err
	}
	return project, result, nil
}

func findSegment(segments []domain.Segment, id string) int {
	for i := range segments {
		if segments[i].ID == id {
			return i
		}
	}
	return -1
}

func findChannel(channels []domain.Channel, id string) int {
	for i := range channels {
		if channels[i].ID == id {
			return i
		}
	}
	return -1
}

func findLayer(layers []domain.Layer, id string) int {
	for i := range layers {
		if layers[i].ID == id {
			return i
		}
	}
	return -1
}

func (s *Session) state(segmentID string) *domain.SegmentState {
	state := s.states[segmentID]
	if state == nil {
		state = &domain.SegmentState{SegmentID: segmentID}
		s.states[segmentID] = state
	}
	return state
}

func (s *Session) fail(segmentID string, err error) error {
	appErr, ok := domain.AsAppError(err)
	if !ok {
		appErr = domain.NewError(domain.CodeConflict, "%v", err).WithSegment(segmentID)
	}
	if appErr.SegmentID == "" {
		appErr.SegmentID = segmentID
	}
	state := s.state(segmentID)
	state.Failed = true
	state.LastError = appErr
	return appErr
}

func (s *Session) RetrySegment(segmentID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	state := s.state(segmentID)
	state.Failed = false
	state.LastError = nil
	if _, ok := s.faults[segmentID]; ok {
		return s.clearFaultLocked(segmentID)
	}
	return nil
}

var ErrNoFault = errors.New("no injected fault")
