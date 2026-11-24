package session

import (
	"errors"

	"performanceexpressions/workspace/domain"
	"performanceexpressions/workspace/engine"
)

type FaultKind string

const (
	FaultPriorityCycle FaultKind = "priority_cycle"
	FaultPosition      FaultKind = "position_out_of_range"
	FaultBatchConflict FaultKind = "batch_conflict"
	FaultCommit        FaultKind = "commit_failure"
)

type Fault struct {
	Kind      FaultKind `json:"kind"`
	SegmentID string    `json:"segment_id"`
	ChannelID string    `json:"channel_id,omitempty"`
	LayerID   string    `json:"layer_id,omitempty"`
	Value     float64   `json:"value,omitempty"`
}

func (s *Session) InjectFault(fault Fault) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if fault.SegmentID == "" {
		return domain.NewError(domain.CodeSchema, "fault segment_id is required")
	}
	if _, exists := s.faults[fault.SegmentID]; exists {
		return domain.NewError(domain.CodeConflict, "segment already has an injected fault").WithSegment(fault.SegmentID)
	}
	index := findSegment(s.working.Segments, fault.SegmentID)
	if index < 0 {
		return domain.NewError(domain.CodeSchema, "unknown segment %q", fault.SegmentID).WithSegment(fault.SegmentID)
	}
	backup := domain.CloneSegment(s.working.Segments[index])
	s.faultBackup[fault.SegmentID] = backup
	seg := domain.CloneSegment(backup)
	switch fault.Kind {
	case FaultPriorityCycle:
		if len(seg.Priorities) == 0 {
			return domain.NewError(domain.CodePriority, "no priority rule available for cycle injection").WithSegment(seg.ID)
		}
		rule := seg.Priorities[0]
		fault.ChannelID = rule.Lower
		seg.Priorities = append(seg.Priorities, domain.PriorityRule{
			Higher: rule.Lower,
			Lower:  rule.Higher,
			Basis:  "injected-cycle",
		})
	case FaultPosition:
		chIndex := findChannel(seg.Channels, fault.ChannelID)
		if chIndex < 0 {
			chIndex = 0
			fault.ChannelID = seg.Channels[0].ID
		}
		if len(seg.Channels[chIndex].Layers) == 0 {
			return domain.NewError(domain.CodeSchema, "channel has no layers").WithSegment(seg.ID).WithChannel(fault.ChannelID)
		}
		fault.LayerID = seg.Channels[chIndex].Layers[0].ID
		outOfRange := seg.EndBeat + 0.75
		seg.Channels[chIndex].Layers[0].Points[0].Beat = outOfRange
	case FaultBatchConflict, FaultCommit:
		if fault.ChannelID == "" {
			fault.ChannelID = "velocity.accent"
		}
	default:
		return domain.NewError(domain.CodeSchema, "unknown fault kind %q", fault.Kind).WithSegment(fault.SegmentID)
	}
	s.working.Segments[index] = seg
	s.faults[fault.SegmentID] = fault
	state := s.state(fault.SegmentID)
	state.Failed = false
	state.LastError = nil
	return nil
}

func (s *Session) ClearFault(segmentID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.clearFaultLocked(segmentID)
}

func (s *Session) clearFaultLocked(segmentID string) error {
	if _, ok := s.faults[segmentID]; !ok {
		return ErrNoFault
	}
	if backup, ok := s.faultBackup[segmentID]; ok {
		index := findSegment(s.working.Segments, segmentID)
		if index >= 0 {
			s.working.Segments[index] = domain.CloneSegment(backup)
			result, err := engine.EvaluateProject(s.working, s.params)
			if err != nil {
				return err
			}
			s.workingResult = result
		}
	}
	delete(s.faults, segmentID)
	delete(s.faultBackup, segmentID)
	state := s.state(segmentID)
	state.Failed = false
	state.LastError = nil
	return nil
}

func (s *Session) faultError(segmentID string, fault Fault) error {
	switch fault.Kind {
	case FaultPriorityCycle:
		seg, _, _ := s.working.SegmentByID(segmentID)
		err := engine.ValidateSegment(seg, engine.ValidateOptions{RequireTruth: false})
		if err == nil {
			err = domain.NewError(domain.CodeCycle, "injected priority cycle was not detected").WithSegment(segmentID)
		}
		if appErr, ok := domain.AsAppError(err); ok {
			if len(appErr.Channels) == 0 {
				appErr.Channels = []string{fault.ChannelID}
			}
		}
		return s.fail(segmentID, err)
	case FaultPosition:
		seg, _, _ := s.working.SegmentByID(segmentID)
		err := engine.ValidateSegment(seg, engine.ValidateOptions{RequireTruth: false})
		if err == nil {
			err = domain.NewError(domain.CodePosition, "injected out-of-range beat was not detected").
				WithSegment(segmentID).WithChannel(fault.ChannelID).WithLayer(fault.LayerID)
		}
		return s.fail(segmentID, err)
	case FaultBatchConflict:
		err := domain.NewError(domain.CodeConflict, "injected conflict inside segment batch").
			WithSegment(segmentID).WithChannel(fault.ChannelID).WithLayer(fault.LayerID)
		return s.fail(segmentID, err)
	case FaultCommit:
		err := domain.NewError(domain.CodeCommit, "injected batch persistence failure").
			WithSegment(segmentID).WithChannel(fault.ChannelID).WithLayer(fault.LayerID)
		return s.fail(segmentID, err)
	default:
		return s.fail(segmentID, domain.NewError(domain.CodeSchema, "unknown fault kind").WithSegment(segmentID))
	}
}

func IsNoFault(err error) bool {
	return errors.Is(err, ErrNoFault)
}
