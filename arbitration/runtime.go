package arbitration

import (
	"context"
	"fmt"
	"sort"

	"lipsync/domain"
	"lipsync/parallel"
)

const (
	packetPending   = "pending"
	packetActive    = "active"
	packetPreempted = "preempted"
	packetDone      = "done"
	packetAbandoned = "abandoned"
)

// Runtime advances the deterministically ordered arbitration clock one frame
// at a time. It is deliberately serializable through Snapshot so SQLite
// checkpoint resume uses the same state machine as a live run.
type Runtime struct {
	project   domain.Project
	packets   map[string]*packetRuntime
	byChannel map[string][]*packetRuntime
	nextFrame int64
}

type packetRuntime struct {
	packet domain.Packet
	domain.PacketState
}

type FrameStep struct {
	Result       domain.FrameResult
	Preemptions  []domain.PreemptEvent
	Resumes      []domain.ResumeEvent
	Abandonments []domain.AbandonEvent
}

type ChannelFailure struct {
	ChannelID string
	Err       error
}

func (e *ChannelFailure) Error() string {
	return fmt.Sprintf("channel %s: %v", e.ChannelID, e.Err)
}

func (e *ChannelFailure) Unwrap() error { return e.Err }

type channelStep struct {
	Result       domain.FrameChannelResult
	Preemptions  []domain.PreemptEvent
	Resumes      []domain.ResumeEvent
	Abandonments []domain.AbandonEvent
}

// ValidateProject checks structural/range failures that should be rejected
// before any checkpoint can be written.
func ValidateProject(project domain.Project, verifyHash bool) error {
	if project.SampleRate <= 0 {
		return &domain.Error{Code: domain.CodeDecode, Path: "sample_rate", Message: "sample rate must be positive"}
	}
	if project.FrameRate <= 0 {
		return &domain.Error{Code: domain.CodeDecode, Path: "frame_rate", Message: "frame rate must be positive"}
	}
	if len(project.Channels) == 0 {
		return &domain.Error{Code: domain.CodeDecode, Path: "channels", Message: "project has no channels"}
	}
	channels := map[string]domain.Channel{}
	for i, channel := range project.Channels {
		path := fmt.Sprintf("channels[%d]", i)
		if channel.ID == "" {
			return &domain.Error{Code: domain.CodeDecode, Path: path + ".id", Message: "channel id is empty"}
		}
		if channel.SlotLimit < 0 {
			return &domain.Error{Code: domain.CodeSchema, Path: path + ".slot_limit", ChannelID: channel.ID, Message: "slot limit cannot be negative"}
		}
		if channel.FrameBudgetBasisPoints < 0 {
			return &domain.Error{Code: domain.CodeSchema, Path: path + ".frame_budget", ChannelID: channel.ID, Message: "frame budget cannot be negative"}
		}
		channels[channel.ID] = channel
	}
	if len(project.ArbitrationOrder) != len(project.Channels) {
		return &domain.Error{Code: domain.CodeDecode, Path: "arbitration_order", Message: "arbitration order does not cover every channel"}
	}
	seenOrder := map[string]bool{}
	for i, id := range project.ArbitrationOrder {
		if _, ok := channels[id]; !ok {
			return &domain.Error{Code: domain.CodeDecode, Path: fmt.Sprintf("arbitration_order[%d]", i), ChannelID: id, Message: "unknown channel in arbitration order"}
		}
		if seenOrder[id] {
			return &domain.Error{Code: domain.CodeDecode, Path: fmt.Sprintf("arbitration_order[%d]", i), ChannelID: id, Message: "duplicate channel in arbitration order"}
		}
		seenOrder[id] = true
	}
	packets := project.Packets()
	for i, packet := range packets {
		path := fmt.Sprintf("packets[%d]", i)
		if packet.ID == "" || packet.ChannelID == "" {
			return &domain.Error{Code: domain.CodeDecode, Path: path, PacketID: packet.ID, Message: "packet identity is incomplete"}
		}
		channel, ok := channels[packet.ChannelID]
		if !ok {
			return &domain.Error{Code: domain.CodeDecode, Path: path + ".channel_id", ChannelID: packet.ChannelID, PacketID: packet.ID, Message: "packet references unknown channel"}
		}
		if packet.StartFrame < 0 || packet.DurationFrames <= 0 {
			return &domain.Error{Code: domain.CodeDecode, Path: path, ChannelID: packet.ChannelID, PacketID: packet.ID, Message: "packet has invalid frame span"}
		}
		if packet.CostBasisPoints <= 0 {
			return &domain.Error{Code: domain.CodeCapacity, Path: path + ".cost", ChannelID: packet.ChannelID, PacketID: packet.ID, Message: "packet cost must be positive"}
		}
		if channel.SlotLimit <= 0 {
			return &domain.Error{Code: domain.CodeCapacity, Path: path, ChannelID: channel.ID, PacketID: packet.ID, Message: "channel has candidates but no usable slots"}
		}
		if channel.FrameBudgetBasisPoints <= 0 {
			return &domain.Error{Code: domain.CodeCapacity, Path: path, ChannelID: channel.ID, PacketID: packet.ID, Message: "channel has candidates but no frame budget"}
		}
		if packet.CostBasisPoints > channel.FrameBudgetBasisPoints {
			return &domain.Error{
				Code: domain.CodeCapacity, Path: path + ".cost", ChannelID: channel.ID, PacketID: packet.ID,
				OperandA: packet.CostBasisPoints, OperandB: channel.FrameBudgetBasisPoints,
				Message: "packet cost exceeds channel frame budget",
			}
		}
	}
	if verifyHash && project.ContentHash != "" && domain.ComputeContentHash(project) != project.ContentHash {
		return &domain.Error{Code: domain.CodeDecode, Path: "content_hash", Message: "project content hash does not match semantic content"}
	}
	if err := validateRecovery(project.Recovery); err != nil {
		return err
	}
	return nil
}

func validateRecovery(policy domain.RecoveryPolicy) error {
	switch policy.Strategy {
	case domain.StrategyNextFrame, domain.StrategyNextWindow, domain.StrategyFixedDelay:
	default:
		return &domain.Error{Code: domain.CodeDecode, Path: "recovery.strategy", Message: "unsupported recovery strategy"}
	}
	if policy.RecoveryOffsetFrame < 0 || policy.MinContinuousFrames < 1 || policy.WindowFrames < 1 {
		return &domain.Error{Code: domain.CodeDecode, Path: "recovery", Message: "invalid recovery policy values"}
	}
	return nil
}

func NewRuntime(project domain.Project, snapshot []domain.PacketState, nextFrame int64) (*Runtime, error) {
	if project.ContentHash == "" {
		project.ContentHash = domain.ComputeContentHash(project)
	}
	if err := ValidateProject(project, true); err != nil {
		return nil, err
	}
	runtime := &Runtime{
		project:   project,
		packets:   map[string]*packetRuntime{},
		byChannel: map[string][]*packetRuntime{},
		nextFrame: nextFrame,
	}
	for _, packet := range project.Packets() {
		state := domain.PacketState{
			ID: packet.ID, ChannelID: packet.ChannelID, Status: packetPending,
			Remaining: packet.DurationFrames, EligibleFrame: packet.StartFrame,
		}
		runtime.packets[packet.ID] = &packetRuntime{packet: packet, PacketState: state}
		runtime.byChannel[packet.ChannelID] = append(runtime.byChannel[packet.ChannelID], runtime.packets[packet.ID])
	}
	for _, restored := range snapshot {
		current, ok := runtime.packets[restored.ID]
		if !ok {
			return nil, &domain.Error{Code: domain.CodeState, Path: "checkpoint.packet_state", PacketID: restored.ID, Message: "checkpoint references unknown packet"}
		}
		if restored.Delivered < 0 || restored.Remaining < 0 || restored.Delivered+restored.Remaining != current.packet.DurationFrames {
			return nil, &domain.Error{Code: domain.CodeState, Path: "checkpoint.packet_state", PacketID: restored.ID, Message: "checkpoint packet delivery counters are inconsistent"}
		}
		switch restored.Status {
		case packetPending, packetActive, packetPreempted, packetDone, packetAbandoned:
		default:
			return nil, &domain.Error{Code: domain.CodeState, Path: "checkpoint.packet_state", PacketID: restored.ID, Message: "checkpoint has unknown packet status"}
		}
		current.PacketState = restored
	}
	for _, states := range runtime.byChannel {
		sort.Slice(states, func(i, j int) bool {
			if states[i].packet.StartFrame != states[j].packet.StartFrame {
				return states[i].packet.StartFrame < states[j].packet.StartFrame
			}
			if states[i].packet.Priority != states[j].packet.Priority {
				return states[i].packet.Priority > states[j].packet.Priority
			}
			return states[i].packet.ID < states[j].packet.ID
		})
	}
	return runtime, nil
}

func (r *Runtime) NextFrame() int64 { return r.nextFrame }

func (r *Runtime) MaxFrame() int64 {
	max := int64(0)
	for _, packet := range r.packets {
		if packet.packet.DeadlineFrame > max {
			max = packet.packet.DeadlineFrame
		}
	}
	return max
}

// Advance processes exactly the next frame. Reversed, repeated or skipped
// frames are rejected with E_ORDER before any state is mutated.
func (r *Runtime) Advance(frame int64) (FrameStep, error) {
	if frame != r.nextFrame {
		return FrameStep{}, &domain.Error{
			Code: domain.CodeOrder, Path: "frame_sequence", OperandA: r.nextFrame, OperandB: frame,
			Message: "frame sequence must advance by exactly one in ascending order",
		}
	}
	before := r.Snapshot()
	step := FrameStep{Result: domain.FrameResult{Frame: frame, Channels: make([]domain.FrameChannelResult, 0, len(r.project.ArbitrationOrder))}}
	for _, channelID := range r.project.ArbitrationOrder {
		channel, ok := r.project.ChannelByID(channelID)
		if !ok {
			return FrameStep{}, &domain.Error{Code: domain.CodeDecode, Path: "arbitration_order", ChannelID: channelID, Message: "missing channel"}
		}
		channelResult, preemptions, resumes, abandonments, err := r.advanceChannel(channel, frame)
		if err != nil {
			_ = r.restore(before)
			return FrameStep{}, &ChannelFailure{ChannelID: channelID, Err: err}
		}
		step.Result.Channels = append(step.Result.Channels, channelResult)
		step.Preemptions = append(step.Preemptions, preemptions...)
		step.Resumes = append(step.Resumes, resumes...)
		step.Abandonments = append(step.Abandonments, abandonments...)
	}
	r.nextFrame++
	return step, nil
}

// AdvanceParallel executes independent channels through a bounded worker pool.
// Candidate validation is chunked to the configured per-worker buffer cap.
// Results are always reassembled in arbitration-order, not completion-order.
func (r *Runtime) AdvanceParallel(frame int64, pool *parallel.Pool, fault func(channelID string, frame int64) error) (FrameStep, error) {
	if frame != r.nextFrame {
		return FrameStep{}, &domain.Error{
			Code: domain.CodeOrder, Path: "frame_sequence", OperandA: r.nextFrame, OperandB: frame,
			Message: "frame sequence must advance by exactly one in ascending order",
		}
	}
	if pool == nil {
		return FrameStep{}, &domain.Error{Code: domain.CodeState, Path: "parallel_pool", Message: "parallel pool is required"}
	}
	before := r.Snapshot()
	works := make([]parallel.Work, 0, len(r.project.ArbitrationOrder))
	for _, channelID := range r.project.ArbitrationOrder {
		channel, ok := r.project.ChannelByID(channelID)
		if !ok {
			return FrameStep{}, &domain.Error{Code: domain.CodeDecode, Path: "arbitration_order", ChannelID: channelID, Message: "missing channel"}
		}
		candidates := r.candidateStates(channel.ID, frame)
		hooked := false
		works = append(works, parallel.Work{
			ID: channelID, CandidateCount: len(candidates),
			Run: func(_ context.Context, from, to int, last bool) (any, error) {
				if !hooked {
					hooked = true
					if fault != nil {
						if err := fault(channelID, frame); err != nil {
							return nil, err
						}
					}
				}
				if err := validateCandidateSlice(channel, candidates, from, to); err != nil {
					return nil, err
				}
				if !last {
					return nil, nil
				}
				result, preemptions, resumes, abandonments, err := r.advanceChannel(channel, frame)
				if err != nil {
					return nil, err
				}
				return channelStep{Result: result, Preemptions: preemptions, Resumes: resumes, Abandonments: abandonments}, nil
			},
		})
	}
	outcomes, _, err := pool.Execute(context.Background(), works)
	if err != nil {
		_ = r.restore(before)
		failedChannel := ""
		for _, outcome := range outcomes {
			if outcome.Err != nil {
				failedChannel = outcome.ID
				break
			}
		}
		if failedChannel == "" && len(works) > 0 {
			failedChannel = works[0].ID
		}
		return FrameStep{}, &ChannelFailure{ChannelID: failedChannel, Err: err}
	}
	step := FrameStep{Result: domain.FrameResult{Frame: frame, Channels: make([]domain.FrameChannelResult, 0, len(outcomes))}}
	for i, outcome := range outcomes {
		channelStep, ok := outcome.Value.(channelStep)
		if !ok {
			_ = r.restore(before)
			return FrameStep{}, &ChannelFailure{
				ChannelID: works[i].ID,
				Err:       &domain.Error{Code: domain.CodeState, Path: "parallel_outcome", Message: "channel task returned no result"},
			}
		}
		step.Result.Channels = append(step.Result.Channels, channelStep.Result)
		step.Preemptions = append(step.Preemptions, channelStep.Preemptions...)
		step.Resumes = append(step.Resumes, channelStep.Resumes...)
		step.Abandonments = append(step.Abandonments, channelStep.Abandonments...)
	}
	r.nextFrame++
	return step, nil
}

func (r *Runtime) advanceChannel(channel domain.Channel, frame int64) (domain.FrameChannelResult, []domain.PreemptEvent, []domain.ResumeEvent, []domain.AbandonEvent, error) {
	result := domain.FrameChannelResult{
		ChannelID: channel.ID,
		Accepted:  []string{},
		Delayed:   []string{},
		Preempted: []string{},
		Resumed:   []string{},
	}
	var preemptions []domain.PreemptEvent
	var resumes []domain.ResumeEvent
	var abandonments []domain.AbandonEvent
	candidates := make([]*packetRuntime, 0)
	accepted := make([]*packetRuntime, 0, channel.SlotLimit)
	usedBudget := int64(0)
	acceptedSet := map[string]bool{}
	for _, packet := range r.byChannel[channel.ID] {
		switch packet.Status {
		case packetActive:
			candidates = append(candidates, packet)
			accepted = append(accepted, packet)
			acceptedSet[packet.ID] = true
			usedBudget += packet.packet.CostBasisPoints
		case packetPending, packetPreempted:
			if packet.EligibleFrame > frame {
				continue
			}
			if err := r.expireIfImpossible(packet, frame, channel.ID); err != nil {
				return result, nil, nil, nil, err
			}
			if packet.Status == packetAbandoned {
				abandonments = append(abandonments, domain.AbandonEvent{
					Frame: frame, ChannelID: channel.ID, PacketID: packet.ID,
					Reason: packet.AbandonReason, AvailableFrames: maxInt64(0, packet.packet.DeadlineFrame-frame+1),
					RequiredFrames: packet.Remaining,
				})
				continue
			}
			candidates = append(candidates, packet)
		}
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].packet.Priority != candidates[j].packet.Priority {
			return candidates[i].packet.Priority > candidates[j].packet.Priority
		}
		iActive := candidates[i].Status == packetActive
		jActive := candidates[j].Status == packetActive
		if iActive != jActive {
			return iActive
		}
		if candidates[i].packet.StartFrame != candidates[j].packet.StartFrame {
			return candidates[i].packet.StartFrame < candidates[j].packet.StartFrame
		}
		return candidates[i].packet.ID < candidates[j].packet.ID
	})
	blocked := map[string]bool{}
	for _, candidate := range candidates {
		if blocked[candidate.packet.ID] {
			continue
		}
		if candidate.Status != packetPending && candidate.Status != packetPreempted {
			continue
		}
		if acceptedSet[candidate.packet.ID] {
			continue
		}
		if r.fits(channel, accepted, candidate, usedBudget) {
			r.accept(candidate, frame, &resumes)
			accepted = append(accepted, candidate)
			acceptedSet[candidate.packet.ID] = true
			usedBudget += candidate.packet.CostBasisPoints
			continue
		}
		victims := r.preemptionVictims(accepted, candidate, frame)
		removed := map[string]bool{}
		for _, victim := range victims {
			if r.fits(channel, acceptedWithout(accepted, removed), candidate, usedBudget-removedBudget(removed, accepted)) {
				break
			}
			event, abandonment, err := r.preempt(victim, candidate, frame, channel.ID)
			if err != nil {
				return result, nil, nil, nil, err
			}
			removed[victim.packet.ID] = true
			blocked[victim.packet.ID] = true
			usedBudget -= victim.packet.CostBasisPoints
			preemptions = append(preemptions, event)
			result.Preempted = append(result.Preempted, victim.packet.ID)
			if abandonment != nil {
				abandonments = append(abandonments, *abandonment)
			}
		}
		accepted = acceptedWithout(accepted, removed)
		acceptedSet = acceptedMap(accepted)
		if r.fits(channel, accepted, candidate, usedBudget) {
			r.accept(candidate, frame, &resumes)
			accepted = append(accepted, candidate)
			acceptedSet[candidate.packet.ID] = true
			usedBudget += candidate.packet.CostBasisPoints
		} else {
			result.Delayed = append(result.Delayed, candidate.packet.ID)
		}
	}
	sort.Strings(result.Delayed)
	sort.Strings(result.Preempted)
	for _, packet := range accepted {
		result.Accepted = append(result.Accepted, packet.packet.ID)
		packet.Delivered++
		packet.Remaining--
		packet.LastDelivery = frame
		if packet.Remaining == 0 {
			packet.Status = packetDone
		}
	}
	sort.Strings(result.Accepted)
	for _, resume := range resumes {
		result.Resumed = append(result.Resumed, resume.PacketID)
	}
	sort.Strings(result.Resumed)
	result.OccupiedSlot = len(accepted)
	result.UsedBudget = usedBudget
	if result.OccupiedSlot > channel.SlotLimit || result.UsedBudget > channel.FrameBudgetBasisPoints {
		return result, nil, nil, nil, &domain.Error{
			Code: domain.CodeCapacity, Path: "frame_result", ChannelID: channel.ID,
			OperandA: result.UsedBudget, OperandB: channel.FrameBudgetBasisPoints,
			Message: "arbitration exceeded declared channel capacity",
		}
	}
	return result, preemptions, resumes, abandonments, nil
}

func (r *Runtime) expireIfImpossible(packet *packetRuntime, frame int64, channelID string) error {
	if packet.Status != packetPending && packet.Status != packetPreempted {
		return nil
	}
	available := packet.packet.DeadlineFrame - frame + 1
	required := packet.Remaining
	if packet.Status == packetPreempted && int64(r.project.Recovery.MinContinuousFrames) < required {
		required = int64(r.project.Recovery.MinContinuousFrames)
	}
	if available >= packet.Remaining && available >= required {
		return nil
	}
	if r.project.Recovery.AllowAbandon {
		packet.Status = packetAbandoned
		packet.AbandonedAt = frame
		packet.AbandonReason = "window_exhausted"
		return nil
	}
	return &domain.Error{
		Code: domain.CodeResume, Path: "packet." + packet.ID, ChannelID: channelID, PacketID: packet.ID,
		Available: available, Required: required,
		Message: "remaining window cannot satisfy delivery or minimum continuous read frames",
	}
}

func (r *Runtime) fits(channel domain.Channel, accepted []*packetRuntime, candidate *packetRuntime, usedBudget int64) bool {
	return len(accepted)+1 <= channel.SlotLimit && usedBudget+candidate.packet.CostBasisPoints <= channel.FrameBudgetBasisPoints
}

func (r *Runtime) accept(packet *packetRuntime, frame int64, resumes *[]domain.ResumeEvent) {
	wasPreempted := packet.Status == packetPreempted
	packet.Status = packetActive
	if wasPreempted {
		run := r.project.Recovery.MinContinuousFrames
		if int64(run) > packet.Remaining {
			run = int(packet.Remaining)
		}
		packet.ProtectedUntil = frame + int64(run) - 1
		*resumes = append(*resumes, domain.ResumeEvent{
			Frame: frame, ChannelID: packet.packet.ChannelID, PacketID: packet.ID,
			ProtectedUntil: packet.ProtectedUntil,
		})
	}
}

func (r *Runtime) preemptionVictims(accepted []*packetRuntime, candidate *packetRuntime, frame int64) []*packetRuntime {
	var victims []*packetRuntime
	for _, active := range accepted {
		if active.packet.Priority >= candidate.packet.Priority {
			continue
		}
		if active.ProtectedUntil > 0 && active.ProtectedUntil >= frame {
			continue
		}
		victims = append(victims, active)
	}
	sort.Slice(victims, func(i, j int) bool {
		if victims[i].packet.Priority != victims[j].packet.Priority {
			return victims[i].packet.Priority < victims[j].packet.Priority
		}
		if victims[i].packet.StartFrame != victims[j].packet.StartFrame {
			return victims[i].packet.StartFrame > victims[j].packet.StartFrame
		}
		return victims[i].packet.ID < victims[j].packet.ID
	})
	return victims
}

func (r *Runtime) preempt(victim, by *packetRuntime, frame int64, channelID string) (domain.PreemptEvent, *domain.AbandonEvent, error) {
	victim.Status = packetPreempted
	victim.PreemptedAt = frame
	recoveryFrame := r.recoveryFrame(frame)
	victim.RecoveryFrame = recoveryFrame
	event := domain.PreemptEvent{
		Frame: frame, ChannelID: channelID, PacketID: victim.ID, ByPacketID: by.ID,
		DeliveredFrames: victim.Delivered, RemainingFrames: victim.Remaining,
		RecoveryFrame: recoveryFrame,
	}
	available := victim.packet.DeadlineFrame - recoveryFrame + 1
	required := victim.Remaining
	if int64(r.project.Recovery.MinContinuousFrames) < required {
		required = int64(r.project.Recovery.MinContinuousFrames)
	}
	if available >= victim.Remaining && available >= required {
		victim.EligibleFrame = recoveryFrame
		return event, nil, nil
	}
	if r.project.Recovery.AllowAbandon {
		victim.Status = packetAbandoned
		victim.AbandonedAt = frame
		victim.AbandonReason = "insufficient_recovery_window"
		event.RecoveryFrame = 0
		return event, &domain.AbandonEvent{
			Frame: frame, ChannelID: channelID, PacketID: victim.ID,
			Reason: "insufficient_recovery_window", AvailableFrames: maxInt64(0, available), RequiredFrames: required,
		}, nil
	}
	return event, nil, &domain.Error{
		Code: domain.CodeResume, Path: "packet." + victim.ID, ChannelID: channelID, PacketID: victim.ID,
		Available: available, Required: required,
		Message: "recovery window is too short and abandonment is disabled",
	}
}

func (r *Runtime) recoveryFrame(preemptFrame int64) int64 {
	policy := r.project.Recovery
	base := preemptFrame + 1
	if policy.RecoveryOffsetFrame > 0 && preemptFrame+policy.RecoveryOffsetFrame > base {
		base = preemptFrame + policy.RecoveryOffsetFrame
	}
	switch policy.Strategy {
	case domain.StrategyNextWindow:
		window := policy.WindowFrames
		remainder := base % window
		if remainder != 0 {
			base += window - remainder
		}
	}
	return base
}

func (r *Runtime) Snapshot() []domain.PacketState {
	states := make([]domain.PacketState, 0, len(r.packets))
	for _, packet := range r.packets {
		state := packet.PacketState
		state.ChannelID = packet.packet.ChannelID
		state.Remaining = packet.Remaining
		state.Delivered = packet.Delivered
		states = append(states, state)
	}
	sort.Slice(states, func(i, j int) bool { return states[i].ID < states[j].ID })
	return states
}

// Restore replaces packet state with a previously captured snapshot. It is
// used for transactional window rollback when a channel fails before commit.
func (r *Runtime) Restore(states []domain.PacketState) error {
	return r.restore(states)
}

func (r *Runtime) RestoreAt(states []domain.PacketState, nextFrame int64) error {
	if nextFrame < 0 {
		return &domain.Error{Code: domain.CodeState, Path: "runtime.next_frame", Message: "next frame cannot be negative"}
	}
	if err := r.restore(states); err != nil {
		return err
	}
	r.nextFrame = nextFrame
	return nil
}

func (r *Runtime) restore(states []domain.PacketState) error {
	seen := map[string]bool{}
	for _, state := range states {
		packet, ok := r.packets[state.ID]
		if !ok {
			return &domain.Error{Code: domain.CodeState, Path: "packet_state", PacketID: state.ID, Message: "snapshot references unknown packet"}
		}
		packet.PacketState = state
		seen[state.ID] = true
	}
	if len(seen) != len(r.packets) {
		return &domain.Error{Code: domain.CodeState, Path: "packet_state", Message: "snapshot does not cover every packet"}
	}
	return nil
}

func (r *Runtime) candidateStates(channelID string, frame int64) []*packetRuntime {
	var candidates []*packetRuntime
	for _, packet := range r.byChannel[channelID] {
		switch packet.Status {
		case packetActive:
			candidates = append(candidates, packet)
		case packetPending, packetPreempted:
			if packet.EligibleFrame <= frame {
				candidates = append(candidates, packet)
			}
		}
	}
	return candidates
}

func validateCandidateSlice(channel domain.Channel, candidates []*packetRuntime, from, to int) error {
	if from < 0 {
		from = 0
	}
	if to > len(candidates) {
		to = len(candidates)
	}
	for i := from; i < to; i++ {
		packet := candidates[i]
		if packet.packet.CostBasisPoints <= 0 || packet.packet.CostBasisPoints > channel.FrameBudgetBasisPoints {
			return &domain.Error{
				Code: domain.CodeCapacity, Path: "packet." + packet.packet.ID, ChannelID: channel.ID,
				PacketID: packet.packet.ID, OperandA: packet.packet.CostBasisPoints,
				OperandB: channel.FrameBudgetBasisPoints,
				Message:  "candidate cost is incompatible with channel frame budget",
			}
		}
		if channel.SlotLimit < 1 {
			return &domain.Error{
				Code: domain.CodeCapacity, Path: "channel." + channel.ID + ".slot_limit",
				ChannelID: channel.ID, PacketID: packet.packet.ID,
				Message: "candidate has no usable channel slot",
			}
		}
	}
	return nil
}

func (r *Runtime) AllResolved() bool {
	for _, packet := range r.packets {
		if packet.Status != packetDone && packet.Status != packetAbandoned {
			return false
		}
	}
	return true
}

func Run(project domain.Project) (*domain.ArbitrationResult, error) {
	runtime, err := NewRuntime(project, nil, 0)
	if err != nil {
		return nil, err
	}
	result := &domain.ArbitrationResult{
		Frames: []domain.FrameResult{}, Preemptions: []domain.PreemptEvent{},
		Resumes: []domain.ResumeEvent{}, Abandonments: []domain.AbandonEvent{},
	}
	for !runtime.AllResolved() {
		step, err := runtime.Advance(runtime.NextFrame())
		if err != nil {
			return nil, err
		}
		result.Frames = append(result.Frames, step.Result)
		result.Preemptions = append(result.Preemptions, step.Preemptions...)
		result.Resumes = append(result.Resumes, step.Resumes...)
		result.Abandonments = append(result.Abandonments, step.Abandonments...)
		if runtime.NextFrame() > runtime.MaxFrame()+1 {
			break
		}
	}
	result.PacketStates = runtime.Snapshot()
	for _, state := range result.PacketStates {
		if state.Status != packetDone && state.Status != packetAbandoned {
			if project.Recovery.AllowAbandon {
				// This branch is defensive; impossible-window checks normally
				// abandon before the runtime exhausts all deadlines.
				continue
			}
			available := int64(0)
			return nil, &domain.Error{
				Code: domain.CodeResume, Path: "packet." + state.ID, ChannelID: state.ChannelID,
				PacketID: state.ID, Available: available, Required: state.Remaining,
				Message: "packet did not resolve before arbitration horizon",
			}
		}
	}
	return result, nil
}

// RunFrames consumes an explicitly supplied frame sequence. It is used to test
// order inversion without mutating the runtime.
func RunFrames(project domain.Project, frames []int64) (*domain.ArbitrationResult, error) {
	runtime, err := NewRuntime(project, nil, frames[0])
	if err != nil {
		return nil, err
	}
	result := &domain.ArbitrationResult{
		Frames: []domain.FrameResult{}, Preemptions: []domain.PreemptEvent{},
		Resumes: []domain.ResumeEvent{}, Abandonments: []domain.AbandonEvent{},
	}
	for _, frame := range frames {
		step, err := runtime.Advance(frame)
		if err != nil {
			return nil, err
		}
		result.Frames = append(result.Frames, step.Result)
		result.Preemptions = append(result.Preemptions, step.Preemptions...)
		result.Resumes = append(result.Resumes, step.Resumes...)
		result.Abandonments = append(result.Abandonments, step.Abandonments...)
	}
	result.PacketStates = runtime.Snapshot()
	return result, nil
}

func acceptedWithout(list []*packetRuntime, removed map[string]bool) []*packetRuntime {
	out := make([]*packetRuntime, 0, len(list))
	for _, item := range list {
		if !removed[item.packet.ID] {
			out = append(out, item)
		}
	}
	return out
}

func acceptedMap(list []*packetRuntime) map[string]bool {
	out := make(map[string]bool, len(list))
	for _, item := range list {
		out[item.packet.ID] = true
	}
	return out
}

func removedBudget(removed map[string]bool, accepted []*packetRuntime) int64 {
	var total int64
	for _, item := range accepted {
		if removed[item.packet.ID] {
			total += item.packet.CostBasisPoints
		}
	}
	return total
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
