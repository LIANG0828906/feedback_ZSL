package dialogue

import (
	"encoding/json"
	"fmt"
	"sync"
)

type SegmentResult struct {
	SegmentID      string              `json:"segment_id"`
	Status         SegmentStatus       `json:"status"`
	SignalHash     string              `json:"signal_hash"`
	ContentHash    string              `json:"content_hash"`
	EnvelopeDigest string              `json:"envelope_digest"`
	Envelope       *SegmentEnvelope    `json:"envelope,omitempty"`
	Calibration    *SegmentCalibration `json:"calibration,omitempty"`
	CommitSequence int                 `json:"commit_sequence,omitempty"`
}

type BudgetUsage struct {
	Frames            int   `json:"frames"`
	Segments          int   `json:"segments"`
	CommittedSegments int   `json:"committed_segments"`
	CacheBytes        int   `json:"cache_bytes"`
	CheckpointBytes   int64 `json:"checkpoint_bytes"`
	InvalidatedNodes  int   `json:"invalidated_nodes"`
	ReusedNodes       int   `json:"reused_nodes"`
}

type BuildResult struct {
	Segments          []SegmentResult   `json:"segments"`
	CommitOrder       []string          `json:"commit_order"`
	Plan              *InvalidationPlan `json:"invalidation,omitempty"`
	Progress          Progress          `json:"progress"`
	Budget            BudgetUsage       `json:"budget"`
	CheckpointInvalid []string          `json:"checkpoint_invalid,omitempty"`
}

type cacheEntry struct {
	Key         string
	Envelope    *SegmentEnvelope
	Calibration *SegmentCalibration
}

type Pipeline struct {
	mu                sync.Mutex
	project           *Project
	graph             *DependencyGraph
	state             *StateMachine
	store             *CheckpointStore
	cache             map[string]cacheEntry
	forceInvalid      map[string]bool
	lastPlan          *InvalidationPlan
	lastResult        *BuildResult
	checkpointInvalid []string
}

// NewPipeline constructs the in-memory/on-disk pipeline. Valid checkpoint
// records are restored only when content hashes and all declared versions match.
func NewPipeline(project *Project, store *CheckpointStore) (*Pipeline, error) {
	if project == nil {
		return nil, newError(CodeSchema, "", "project", "project is nil")
	}
	pipeline := &Pipeline{
		project:      project.Clone(),
		graph:        NewDependencyGraph(project),
		state:        NewStateMachine(project),
		store:        store,
		cache:        map[string]cacheEntry{},
		forceInvalid: map[string]bool{},
	}
	if store != nil {
		if err := store.Load(project.ID); err != nil {
			return nil, err
		}
		valid, invalid, err := store.Validate(pipeline.project)
		if err != nil {
			return nil, err
		}
		pipeline.checkpointInvalid = append(pipeline.checkpointInvalid, invalid...)
		for _, segment := range pipeline.project.Segments() {
			record, ok := valid[segment.ID]
			if !ok {
				continue
			}
			pipeline.cache[segmentNodeID(segment.ID, "envelope")] = cacheEntry{
				Key:      pipeline.graph.Nodes[segmentNodeID(segment.ID, "envelope")].Key,
				Envelope: cloneEnvelope(record.Envelope),
			}
			pipeline.cache[segmentNodeID(segment.ID, "calibration")] = cacheEntry{
				Key:         pipeline.graph.Nodes[segmentNodeID(segment.ID, "calibration")].Key,
				Calibration: cloneCalibration(record.Calibration),
			}
		}
	}
	pipeline.lastPlan = pipeline.fullPlan("initial-build")
	return pipeline, nil
}

func (p *Pipeline) Project() *Project {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.project.Clone()
}

func (p *Pipeline) Graph() *DependencyGraph {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.graph
}

func (p *Pipeline) State() *StateMachine {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.state
}

func (p *Pipeline) LastPlan() *InvalidationPlan {
	p.mu.Lock()
	defer p.mu.Unlock()
	return clonePlan(p.lastPlan)
}

func (p *Pipeline) ClearCache() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.cache = map[string]cacheEntry{}
	for _, segment := range p.project.Segments() {
		p.forceInvalid[segment.ID] = true
	}
	p.lastPlan = p.fullPlan("full-recompute")
}

func (p *Pipeline) LastResult() *BuildResult {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.lastResult == nil {
		return nil
	}
	return cloneBuildResult(p.lastResult)
}

func (p *Pipeline) ApplyEdit(edit Edit) (*InvalidationPlan, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	project := p.project.Clone()
	switch edit.Kind {
	case EditSegmentGain:
		if err := editSegmentGain(project, edit.SegmentID, edit.Value); err != nil {
			return nil, err
		}
	case EditTargetLoudness:
		if edit.Value < 0 {
			return nil, newError(CodeSchema, project.ID, "target_loudness", "must be non-negative")
		}
		project.TargetLoudness = edit.Value
	case EditGainLimit:
		if edit.Value < 0 {
			return nil, newError(CodeSchema, project.ID, "gain_limit", "must be non-negative")
		}
		project.GainLimit = edit.Value
	case EditFrameRate:
		if edit.FrameLength <= 0 || edit.Hop <= 0 {
			return nil, newError(CodeSchema, project.ID, "frame_rate", "frame length and hop must be positive")
		}
		for _, segment := range project.Segments() {
			if segment.Length() < edit.FrameLength {
				return nil, newError(CodeSchema, project.ID, "segments."+segment.ID, "new frame length is longer than segment")
			}
		}
		project.FrameLength = edit.FrameLength
		project.Hop = edit.Hop
	case EditSegmentContent:
		if !edit.ReplacePCM {
			return nil, newError(CodeSchema, project.ID, "edit", "segment content edit must specify PCM replacement")
		}
		segment, ok := project.SegmentByID(edit.SegmentID)
		if !ok {
			return nil, newError(CodeSchema, project.ID, "segments."+edit.SegmentID, "unknown segment")
		}
		if int64(len(edit.PCM)) != segment.Length() {
			return nil, newError(CodeSchema, project.ID, "edit.pcm", fmt.Sprintf("replacement has %d samples, expected %d", len(edit.PCM), segment.Length()))
		}
		copy(project.PCM[segment.StartSample:segment.EndSample], edit.PCM)
	default:
		return nil, newError(CodeSchema, project.ID, "edit.kind", "unsupported edit kind "+string(edit.Kind))
	}
	newGraph := NewDependencyGraph(project)
	plan := newGraph.PlanForEdit(edit)
	for nodeID := range p.cache {
		if _, exists := newGraph.Nodes[nodeID]; !exists {
			delete(p.cache, nodeID)
		}
	}
	for _, nodeID := range plan.Invalidated {
		delete(p.cache, nodeID)
	}
	for _, segmentID := range plan.SegmentIDs {
		p.forceInvalid[segmentID] = true
		if err := p.state.Reset(segmentID); err != nil {
			return nil, err
		}
	}
	p.project = project
	p.graph = newGraph
	p.lastPlan = plan
	return clonePlan(plan), nil
}

func (p *Pipeline) Build() (*BuildResult, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	result := &BuildResult{
		Plan:              clonePlan(p.lastPlan),
		CheckpointInvalid: append([]string(nil), p.checkpointInvalid...),
	}
	for _, segment := range p.project.Segments() {
		envelopeID := segmentNodeID(segment.ID, "envelope")
		calibrationID := segmentNodeID(segment.ID, "calibration")
		envelopeNode := p.graph.Nodes[envelopeID]
		calibrationNode := p.graph.Nodes[calibrationID]
		if envelopeNode == nil || calibrationNode == nil {
			return nil, newError(CodeState, p.project.ID, "segments."+segment.ID, "missing dependency nodes")
		}
		envelopeEntry, envelopeCached := p.cache[envelopeID]
		envelopeCached = envelopeCached && envelopeEntry.Key == envelopeNode.Key && envelopeEntry.Envelope != nil
		calibrationEntry, calibrationCached := p.cache[calibrationID]
		calibrationCached = calibrationCached && calibrationEntry.Key == calibrationNode.Key && calibrationEntry.Calibration != nil
		force := p.forceInvalid[segment.ID]
		var envelope *SegmentEnvelope
		var calibration *SegmentCalibration
		if envelopeCached && calibrationCached && !force {
			envelope = cloneEnvelope(envelopeEntry.Envelope)
			calibration = cloneCalibration(calibrationEntry.Calibration)
			if status, _ := p.state.Status(segment.ID); status != StatusCommitted {
				if err := p.state.RestoreCommitted(segment.ID, envelope, calibration); err != nil {
					return nil, err
				}
			}
			result.Budget.ReusedNodes += 3
		} else {
			if err := p.state.Reset(segment.ID); err != nil {
				return nil, err
			}
			if err := p.state.Transition(segment.ID, StatusMeasuring); err != nil {
				return nil, err
			}
			if envelopeCached {
				envelope = cloneEnvelope(envelopeEntry.Envelope)
				result.Budget.ReusedNodes++
			} else {
				measured, err := MeasureSegmentEnvelope(p.project, segment)
				if err != nil {
					_ = p.state.Fail(segment.ID, err)
					return result, err
				}
				envelope = measured
				p.cache[envelopeID] = cacheEntry{Key: envelopeNode.Key, Envelope: cloneEnvelope(measured)}
			}
			if err := p.state.SetEnvelope(segment.ID, envelope); err != nil {
				return nil, err
			}
			if err := p.state.Transition(segment.ID, StatusCalibrating); err != nil {
				return nil, err
			}
			if calibrationCached {
				calibration = cloneCalibration(calibrationEntry.Calibration)
				result.Budget.ReusedNodes++
			} else {
				computed, err := calibrateSegment(p.project, segment, envelope)
				if err != nil {
					_ = p.state.Fail(segment.ID, err)
					return result, err
				}
				if !computed.Reached {
					loudError := p.loudnessErrorLocked(envelopeID, envelope)
					_ = p.state.Fail(segment.ID, loudError)
					return result, loudError
				}
				calibration = &computed
				p.cache[calibrationID] = cacheEntry{Key: calibrationNode.Key, Calibration: cloneCalibration(&computed)}
			}
			if err := p.state.SetCalibration(segment.ID, calibration); err != nil {
				return nil, err
			}
			if err := p.state.Commit(segment.ID); err != nil {
				return nil, err
			}
		}
		sequence := 0
		if p.store != nil && (force || !envelopeCached || !calibrationCached) {
			var err error
			sequence, err = p.store.Commit(p.project.ID, CheckpointRecord{
				SegmentID: segment.ID, ContentHash: p.project.ContentHash(segment),
				SignalHash: p.project.SignalHash(segment), EnvelopeDigest: envelope.Digest(),
				GainAdjustment: calibration.GainAdjustment, SourceVersion: p.project.Source.SchemaVersion,
				QuantizationVersion: QuantizationVersion, AlgorithmVersion: AlgorithmVersion,
				Envelope: cloneEnvelope(envelope), Calibration: cloneCalibration(calibration),
			})
			if err != nil {
				_ = p.state.Fail(segment.ID, err)
				return result, err
			}
		}
		if p.store != nil {
			if record, ok := p.store.Record(segment.ID); ok {
				sequence = record.Sequence
			}
		}
		delete(p.forceInvalid, segment.ID)
		result.CommitOrder = append(result.CommitOrder, segment.ID)
		result.Segments = append(result.Segments, SegmentResult{
			SegmentID: segment.ID, Status: StatusCommitted,
			SignalHash: p.project.SignalHash(segment), ContentHash: p.project.ContentHash(segment),
			EnvelopeDigest: envelope.Digest(), Envelope: cloneEnvelope(envelope),
			Calibration: cloneCalibration(calibration), CommitSequence: sequence,
		})
	}
	result.Progress = p.state.Progress()
	if p.store != nil {
		result.Budget.CheckpointBytes = p.store.Bytes()
	}
	result.Budget.Segments = len(result.Segments)
	result.Budget.CommittedSegments = result.Progress.Committed
	if p.lastPlan != nil {
		result.Budget.InvalidatedNodes = len(p.lastPlan.Invalidated)
	}
	for _, segment := range result.Segments {
		if segment.Envelope != nil {
			result.Budget.Frames += len(segment.Envelope.Frames)
		}
	}
	if data, err := json.Marshal(p.cache); err == nil {
		result.Budget.CacheBytes = len(data)
	}
	p.lastResult = result
	p.checkpointInvalid = nil
	return result, nil
}

func (p *Pipeline) fullPlan(reason string) *InvalidationPlan {
	plan := &InvalidationPlan{Reason: reason}
	for _, id := range p.graph.Order {
		plan.Invalidated = append(plan.Invalidated, id)
		plan.RebuildOrder = append(plan.RebuildOrder, id)
	}
	for _, segment := range p.project.Segments() {
		plan.SegmentIDs = append(plan.SegmentIDs, segment.ID)
	}
	return plan
}

func (p *Pipeline) loudnessErrorLocked(currentEnvelopeID string, currentEnvelope *SegmentEnvelope) error {
	envelopes := make([]*SegmentEnvelope, 0, len(p.project.Segments()))
	for _, segment := range p.project.Segments() {
		nodeID := segmentNodeID(segment.ID, "envelope")
		if nodeID == currentEnvelopeID {
			envelopes = append(envelopes, cloneEnvelope(currentEnvelope))
			continue
		}
		if entry, ok := p.cache[nodeID]; ok && entry.Envelope != nil && p.graph.Nodes[nodeID] != nil && entry.Key == p.graph.Nodes[nodeID].Key {
			envelopes = append(envelopes, cloneEnvelope(entry.Envelope))
			continue
		}
		measured, err := MeasureSegmentEnvelope(p.project, segment)
		if err != nil {
			return err
		}
		envelopes = append(envelopes, measured)
	}
	_, err := CalibrateEnvelopes(p.project, envelopes)
	if err != nil {
		return err
	}
	return newError(CodeLoud, p.project.ID, "target_loudness", "target loudness is unreachable")
}

func editSegmentGain(project *Project, segmentID string, gain int64) error {
	if gain < project.GainMin || gain > project.GainMax {
		return newError(CodeSchema, project.ID, "segments."+segmentID+".session_gain", fmt.Sprintf("gain %d outside declared range [%d,%d]", gain, project.GainMin, project.GainMax))
	}
	for sceneIndex := range project.Scenes {
		for segmentIndex := range project.Scenes[sceneIndex].Segments {
			if project.Scenes[sceneIndex].Segments[segmentIndex].ID == segmentID {
				project.Scenes[sceneIndex].Segments[segmentIndex].SessionGain = gain
				return nil
			}
		}
	}
	return newError(CodeSchema, project.ID, "segments."+segmentID, "unknown segment")
}

func clonePlan(plan *InvalidationPlan) *InvalidationPlan {
	if plan == nil {
		return nil
	}
	out := *plan
	out.Invalidated = append([]string(nil), plan.Invalidated...)
	out.RebuildOrder = append([]string(nil), plan.RebuildOrder...)
	out.Reused = append([]string(nil), plan.Reused...)
	out.SegmentIDs = append([]string(nil), plan.SegmentIDs...)
	return &out
}

func cloneBuildResult(result *BuildResult) *BuildResult {
	if result == nil {
		return nil
	}
	out := *result
	out.CommitOrder = append([]string(nil), result.CommitOrder...)
	out.CheckpointInvalid = append([]string(nil), result.CheckpointInvalid...)
	out.Plan = clonePlan(result.Plan)
	out.Segments = make([]SegmentResult, len(result.Segments))
	for i, segment := range result.Segments {
		out.Segments[i] = segment
		out.Segments[i].Envelope = cloneEnvelope(segment.Envelope)
		out.Segments[i].Calibration = cloneCalibration(segment.Calibration)
	}
	return &out
}
