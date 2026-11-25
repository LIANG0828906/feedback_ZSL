package qos

import (
	"context"
	"fmt"
	"sort"
)

const (
	DropTokenShortage = 1
	DropEgressBudget  = 2
	DropQueueExpired  = 3
	DropQueueLimit    = 4
)

func DropReasonLabel(code int) string {
	switch code {
	case DropTokenShortage:
		return "TOKEN_SHORTAGE"
	case DropEgressBudget:
		return "EGRESS_BUDGET"
	case DropQueueExpired:
		return "QUEUE_EXPIRED"
	case DropQueueLimit:
		return "QUEUE_DEPTH"
	default:
		return "UNKNOWN"
	}
}

type TokenState struct {
	ChannelID            string `json:"channel_id"`
	TokensBytes          int64  `json:"tokens_bytes"`
	TokensPacketsMilli   int64  `json:"tokens_packets_milli"`
	CapacityBytes        int64  `json:"capacity_bytes"`
	CapacityPacketsMilli int64  `json:"capacity_packets_milli"`
	RefillAddedBytes     int64  `json:"refill_added_bytes"`
	RefillNumerator      int64  `json:"refill_numerator"`
	RefillRemainder      int64  `json:"refill_remainder"`
	Saturated            bool   `json:"saturated"`
}

type DelayEvent struct {
	PacketID   string `json:"packet_id"`
	ChannelID  string `json:"channel_id"`
	Reason     string `json:"reason"`
	DelayMS    int64  `json:"delay_ms"`
	ExpireAtMS int64  `json:"expire_at_ms"`
	DeadlineMS int64  `json:"deadline_ms,omitempty"`
}

type DropEvent struct {
	PacketID   string `json:"packet_id"`
	ChannelID  string `json:"channel_id"`
	SizeBytes  int64  `json:"size_bytes"`
	AtMS       int64  `json:"at_ms"`
	ReasonCode int    `json:"reason_code"`
	Reason     string `json:"reason"`
	Detail     string `json:"detail,omitempty"`
}

type Emission struct {
	PacketID          string `json:"packet_id"`
	ChannelID         string `json:"channel_id"`
	SizeBytes         int64  `json:"size_bytes"`
	AtMS              int64  `json:"at_ms"`
	PacketPriority    int    `json:"packet_priority"`
	EffectivePriority int    `json:"effective_priority"`
	ChannelOrder      int    `json:"channel_order"`
	WeightBasisID     string `json:"weight_basis_id"`
	EmitSeq           int64  `json:"emit_seq"`
	DSCP              *int   `json:"dscp,omitempty"`
}

type SuppressedInfo struct {
	ChannelID       string `json:"channel_id"`
	PendingPackets  int    `json:"pending_packets"`
	PendingBytes    int64  `json:"pending_bytes"`
	HeadBytes       int64  `json:"head_bytes"`
	BudgetRemaining int64  `json:"budget_remaining_bytes"`
	WeightBasisID   string `json:"weight_basis_id"`
}

type QueueEntry struct {
	PacketID    string `json:"packet_id"`
	ChannelID   string `json:"channel_id"`
	ExpireAtMS  int64  `json:"expire_at_ms"`
	RemainingMS int64  `json:"remaining_ms"`
	DeadlineMS  int64  `json:"deadline_ms,omitempty"`
	ReplaySeq   int64  `json:"replay_seq"`
	Reason      string `json:"reason"`
	SizeBytes   int64  `json:"size_bytes"`
}

type ReplayEvent struct {
	PacketID   string `json:"packet_id"`
	ChannelID  string `json:"channel_id"`
	ExpireAtMS int64  `json:"expire_at_ms"`
	ReplaySeq  int64  `json:"replay_seq"`
}

type ExpiredEvent struct {
	PacketID   string `json:"packet_id"`
	ChannelID  string `json:"channel_id"`
	ExpireAtMS int64  `json:"expire_at_ms"`
	DeadlineMS int64  `json:"deadline_ms"`
}

type StepResult struct {
	Index      int              `json:"step"`
	AtMS       int64            `json:"at_ms"`
	AtTick     int64            `json:"at_tick"`
	Tokens     []TokenState     `json:"tokens"`
	Delayed    []DelayEvent     `json:"delayed"`
	Dropped    []DropEvent      `json:"dropped"`
	Emitted    []Emission       `json:"emitted"`
	Queue      []QueueEntry     `json:"delay_queue"`
	Replayed   []ReplayEvent    `json:"replayed"`
	Expired    []ExpiredEvent   `json:"expired"`
	Suppressed []SuppressedInfo `json:"suppressed"`
	SourceID   string           `json:"invalidation_source,omitempty"`
	Reused     bool             `json:"reused_cache"`
	CacheToken string           `json:"cache_token,omitempty"`
}

type RunResult struct {
	Steps       []StepResult     `json:"steps"`
	Emissions   []Emission       `json:"emissions"`
	Delays      []DelayEvent     `json:"delays"`
	Drops       []DropEvent      `json:"drops"`
	Expired     []ExpiredEvent   `json:"expired"`
	TotalSteps  int              `json:"total_steps"`
	FinalTokens map[string]int64 `json:"final_tokens"`
}

type packetStatus string

const (
	statusPending packetStatus = "pending"
	statusReady   packetStatus = "ready"
	statusDelayed packetStatus = "delayed"
	statusEmitted packetStatus = "emitted"
	statusDropped packetStatus = "dropped"
)

type packetRT struct {
	p          *Packet
	status     packetStatus
	readyAtMS  int64
	replaySeq  int64
	deadlineMS int64
	delayCause string
}

type delayRT struct {
	pkt        *packetRT
	expireAtMS int64
	deadlineMS int64
	replaySeq  int64
	cause      string
}

type channelRT struct {
	ch          *Channel
	tokens      int64
	remainder   int64
	credit      int64
	ready       []*packetRT
	outstanding int
}

type Engine struct {
	p             *Project
	channels      []*channelRT
	chByID        map[string]*channelRT
	packets       map[string]*packetRT
	arrivals      map[int64][]*packetRT
	nextStep      int
	nextMS        int64
	replayCounter int64
	emitCounter   int64
	unresolved    int
	emitted       []Emission
	dropped       []DropEvent
	delayedEvents []DelayEvent
	expired       []ExpiredEvent
}

func NewEngine(p *Project) (*Engine, error) {
	if err := ValidateProject(p); err != nil {
		return nil, err
	}
	cp := CloneProject(p)
	e := &Engine{
		p: cp, chByID: make(map[string]*channelRT), packets: make(map[string]*packetRT),
		arrivals: make(map[int64][]*packetRT), unresolved: len(cp.Packets),
	}
	for i := range cp.Channels {
		ch := &cp.Channels[i]
		cr := &channelRT{ch: ch, tokens: ch.InitialTokens}
		e.channels = append(e.channels, cr)
		e.chByID[ch.ID] = cr
	}
	for i := range cp.Packets {
		pkt := &cp.Packets[i]
		rt := &packetRT{p: pkt, status: statusPending, deadlineMS: deadlineFor(pkt, *e.chByID[pkt.ChannelID].ch)}
		e.packets[pkt.ID] = rt
		e.arrivals[pkt.ArrivalMS] = append(e.arrivals[pkt.ArrivalMS], rt)
	}
	for ms := range e.arrivals {
		sort.SliceStable(e.arrivals[ms], func(i, j int) bool {
			a, b := e.arrivals[ms][i], e.arrivals[ms][j]
			if a.p.Seq != b.p.Seq {
				return a.p.Seq < b.p.Seq
			}
			return a.p.ID < b.p.ID
		})
	}
	return e, nil
}

func deadlineFor(pkt *Packet, ch Channel) int64 {
	if ch.MaxDelayMS <= 0 {
		return 0
	}
	return pkt.ArrivalMS + ch.MaxDelayMS
}

func (e *Engine) Done() bool { return e.unresolved == 0 }

func (e *Engine) NextStep() int     { return e.nextStep }
func (e *Engine) NextMS() int64     { return e.nextMS }
func (e *Engine) Project() *Project { return CloneProject(e.p) }

func (e *Engine) Step() (StepResult, error) {
	if e.Done() {
		return StepResult{}, NewError(EInternal, "engine.step", "engine already complete", map[string]any{"next_step": e.nextStep})
	}
	maxStep := e.p.HorizonMS / e.p.TickMS
	if int64(e.nextStep) > maxStep {
		return StepResult{}, NewError(EHorizon, "project.horizon_ms", "horizon reached with unresolved packets",
			map[string]any{"step": e.nextStep, "horizon_ms": e.p.HorizonMS, "unresolved": e.unresolved})
	}
	now := e.nextMS
	result := StepResult{
		Index: e.nextStep, AtMS: now, AtTick: now / e.p.TickMS,
		Tokens: make([]TokenState, 0, len(e.channels)),
	}
	for _, cr := range e.channels {
		refill := RefillResult{Remainder: cr.remainder, Tokens: cr.tokens}
		var err error
		if e.nextStep > 0 {
			refill, err = Refill(cr.tokens, cr.remainder, cr.ch.RatePermyriad, e.p.TickMS, cr.ch.CapacityBytes, e.p.Width)
			if err != nil {
				return result, err
			}
			cr.tokens = refill.Tokens
			cr.remainder = refill.Remainder
		}
		packetMillis, err := PacketsFromBytes(cr.tokens, e.p.NominalPacketBytes)
		if err != nil {
			return result, err
		}
		capacityMillis, err := PacketsFromBytes(cr.ch.CapacityBytes, e.p.NominalPacketBytes)
		if err != nil {
			return result, err
		}
		result.Tokens = append(result.Tokens, TokenState{
			ChannelID: cr.ch.ID, TokensBytes: cr.tokens,
			TokensPacketsMilli: packetMillis, CapacityBytes: cr.ch.CapacityBytes,
			CapacityPacketsMilli: capacityMillis, RefillAddedBytes: refill.Added,
			RefillNumerator: refill.Numerator, RefillRemainder: refill.Remainder,
			Saturated: refill.Saturated,
		})
	}

	// Delay queue entries expire before newly arriving packets replay.
	for _, de := range e.dueDelays(now) {
		if de.deadlineMS > 0 && now > de.deadlineMS {
			e.expireDelayed(de, now, &result)
			continue
		}
		e.replayCounter++
		de.pkt.replaySeq = e.replayCounter
		de.pkt.status = statusReady
		de.pkt.readyAtMS = now
		de.pkt.delayCause = ""
		cr := e.chByID[de.pkt.p.ChannelID]
		cr.ready = append(cr.ready, de.pkt)
		result.Replayed = append(result.Replayed, ReplayEvent{
			PacketID: de.pkt.p.ID, ChannelID: de.pkt.p.ChannelID,
			ExpireAtMS: de.expireAtMS, ReplaySeq: e.replayCounter,
		})
	}

	// New arrivals join the queue after expired delayed packets.
	arrivals := append([]*packetRT(nil), e.arrivals[now]...)
	for _, rt := range arrivals {
		cr := e.chByID[rt.p.ChannelID]
		if cr.outstanding >= e.p.QueueDepth {
			e.drop(rt, now, DropQueueLimit, "queue depth reached", &result)
			continue
		}
		rt.status = statusReady
		rt.readyAtMS = now
		cr.ready = append(cr.ready, rt)
		cr.outstanding++
	}
	delete(e.arrivals, now)

	for _, cr := range e.channels {
		sortChannelReady(cr.ready)
		if len(cr.ready) > 0 {
			cr.credit = saturatingCreditAdd(cr.credit, cr.ch.WeightPermyriad)
		}
	}

	budget := e.p.EgressBudgetBytes
	suppressedSeen := make(map[string]bool)
	for {
		budget = e.schedule(now, budget, &result)
		acted := false
		for _, cr := range e.channels {
			if len(cr.ready) == 0 {
				continue
			}
			head := cr.ready[0]
			if cr.tokens < head.p.SizeBytes {
				if cr.ch.Policy == PolicyDrop {
					e.popReady(cr)
					e.drop(head, now, DropTokenShortage, "insufficient tokens", &result)
				} else {
					e.popReady(cr)
					if err := e.deferPacket(cr, head, now, head.p.SizeBytes-cr.tokens, "DEFER_TOKEN", &result); err != nil {
						return result, err
					}
				}
				acted = true
				continue
			}
			if head.p.SizeBytes > budget {
				if !suppressedSeen[cr.ch.ID] {
					result.Suppressed = append(result.Suppressed, e.suppressedInfo(cr, budget))
					suppressedSeen[cr.ch.ID] = true
				}
				if cr.ch.Policy == PolicyDrop {
					e.popReady(cr)
					e.drop(head, now, DropEgressBudget, "shared egress budget exhausted", &result)
				} else {
					e.popReady(cr)
					if err := e.deferBudget(cr, head, now, &result); err != nil {
						return result, err
					}
				}
				acted = true
			}
		}
		if !acted {
			break
		}
	}

	// Remaining ready heads are suppressed by the shared link budget.
	for _, cr := range e.channels {
		if len(cr.ready) == 0 {
			if cr.outstanding == 0 {
				cr.credit = 0
			}
			continue
		}
		if !suppressedSeen[cr.ch.ID] {
			result.Suppressed = append(result.Suppressed, e.suppressedInfo(cr, budget))
		}
	}

	result.Queue = e.queueSnapshot(now)
	sort.SliceStable(result.Delayed, func(i, j int) bool { return result.Delayed[i].PacketID < result.Delayed[j].PacketID })
	sort.SliceStable(result.Dropped, func(i, j int) bool { return result.Dropped[i].PacketID < result.Dropped[j].PacketID })
	sort.SliceStable(result.Replayed, func(i, j int) bool {
		if result.Replayed[i].ReplaySeq != result.Replayed[j].ReplaySeq {
			return result.Replayed[i].ReplaySeq < result.Replayed[j].ReplaySeq
		}
		return result.Replayed[i].PacketID < result.Replayed[j].PacketID
	})
	sort.SliceStable(result.Suppressed, func(i, j int) bool {
		a, _ := e.p.Channel(result.Suppressed[i].ChannelID)
		b, _ := e.p.Channel(result.Suppressed[j].ChannelID)
		return a.Order < b.Order
	})

	e.nextStep++
	e.nextMS += e.p.TickMS
	return result, nil
}

func sortChannelReady(ready []*packetRT) {
	sort.SliceStable(ready, func(i, j int) bool {
		a, b := ready[i], ready[j]
		ap := a.p.Priority
		bp := b.p.Priority
		if ap != bp {
			return ap > bp
		}
		if a.readyAtMS != b.readyAtMS {
			return a.readyAtMS < b.readyAtMS
		}
		if a.p.Seq != b.p.Seq {
			return a.p.Seq < b.p.Seq
		}
		return a.p.ID < b.p.ID
	})
}

func saturatingCreditAdd(a, b int64) int64 {
	const limit int64 = 1 << 60
	if b > 0 && a > limit-b {
		return limit
	}
	return a + b
}

func effectivePriority(ch Channel, pkt *Packet) int {
	v := ch.Priority + pkt.Priority
	if pkt.DSCP != nil && ch.PriorityBoost != 0 {
		for _, d := range ch.DSCPBoostClasses {
			if *pkt.DSCP == d {
				v += ch.PriorityBoost
				break
			}
		}
	}
	return v
}

func basisID(ch Channel, pkt *packetRT, credit int64) string {
	return fmt.Sprintf("WB-p%d-w%d-o%d-c%d", effectivePriority(ch, pkt.p), ch.WeightPermyriad, ch.Order, credit)
}

type candidate struct {
	cr       *channelRT
	head     *packetRT
	priority int
	credit   int64
}

func (e *Engine) schedule(now, budget int64, result *StepResult) int64 {
	for {
		var candidates []candidate
		for _, cr := range e.channels {
			if len(cr.ready) == 0 {
				continue
			}
			head := cr.ready[0]
			if cr.tokens < head.p.SizeBytes || head.p.SizeBytes > budget {
				continue
			}
			candidates = append(candidates, candidate{
				cr: cr, head: head, priority: effectivePriority(*cr.ch, head.p), credit: cr.credit,
			})
		}
		if len(candidates) == 0 {
			return budget
		}
		sort.SliceStable(candidates, func(i, j int) bool {
			a, b := candidates[i], candidates[j]
			if a.priority != b.priority {
				return a.priority > b.priority
			}
			if a.credit != b.credit {
				return a.credit > b.credit
			}
			if a.cr.ch.Order != b.cr.ch.Order {
				return a.cr.ch.Order < b.cr.ch.Order
			}
			if a.head.p.Seq != b.head.p.Seq {
				return a.head.p.Seq < b.head.p.Seq
			}
			return a.head.p.ID < b.head.p.ID
		})
		winner := candidates[0]
		pkt := winner.head
		basis := basisID(*winner.cr.ch, pkt, winner.credit)

		winner.cr.tokens -= pkt.p.SizeBytes
		budget -= pkt.p.SizeBytes
		winner.cr.credit -= e.totalActiveWeight()
		if winner.cr.credit < -(1 << 60) {
			winner.cr.credit = -(1 << 60)
		}
		e.popReady(winner.cr)
		pkt.status = statusEmitted
		winner.cr.outstanding--
		e.unresolved--
		e.emitCounter++
		var dscp *int
		if pkt.p.DSCP != nil {
			d := *pkt.p.DSCP
			dscp = &d
		}
		em := Emission{
			PacketID: pkt.p.ID, ChannelID: pkt.p.ChannelID, SizeBytes: pkt.p.SizeBytes,
			AtMS: now, PacketPriority: pkt.p.Priority,
			EffectivePriority: winner.priority, ChannelOrder: winner.cr.ch.Order,
			WeightBasisID: basis, EmitSeq: e.emitCounter, DSCP: dscp,
		}
		e.emitted = append(e.emitted, em)
		result.Emitted = append(result.Emitted, em)
	}
}

func (e *Engine) totalActiveWeight() int64 {
	var total int64
	for _, cr := range e.channels {
		if len(cr.ready) > 0 {
			total = saturatingCreditAdd(total, cr.ch.WeightPermyriad)
		}
	}
	if total == 0 {
		return 1
	}
	return total
}

func (e *Engine) popReady(cr *channelRT) {
	if len(cr.ready) == 0 {
		return
	}
	cr.ready = cr.ready[1:]
}

func (e *Engine) deferBudget(cr *channelRT, pkt *packetRT, now int64, result *StepResult) error {
	delay := ceilToQuantum(e.p.TickMS, cr.ch.QueueAgeMS)
	return e.enqueueDelay(cr, pkt, now, delay, now, "DEFER_BUDGET", result)
}

func (e *Engine) deferPacket(cr *channelRT, pkt *packetRT, now, deficit int64, cause string, result *StepResult) error {
	delay, _, _, check, err := DelayForDeficit(deficit, cr.ch.RatePermyriad, cr.ch.QueueAgeMS, e.p.Width)
	if err != nil {
		return err
	}
	if !check.WithinTol {
		return NewError(EOverflow, "delay", "delay tolerance check failed", map[string]any{"packet": pkt.p.ID})
	}
	return e.enqueueDelay(cr, pkt, now, delay, now, cause, result)
}

func ceilToQuantum(value, quantum int64) int64 {
	if quantum <= 0 {
		return value
	}
	if value%quantum == 0 {
		return value
	}
	return value + quantum - value%quantum
}

func (e *Engine) enqueueDelay(cr *channelRT, pkt *packetRT, now, delay, _ int64, cause string, result *StepResult) error {
	expire, overflow := AddChecked(now, delay, 64)
	if overflow {
		return NewError(EOverflow, "delay.expire", "delay expiry overflow", map[string]any{"packet": pkt.p.ID, "now_ms": now, "delay_ms": delay})
	}
	deadline := deadlineFor(pkt.p, *cr.ch)
	if deadline > 0 && expire > deadline {
		e.expired = append(e.expired, ExpiredEvent{PacketID: pkt.p.ID, ChannelID: pkt.p.ChannelID, ExpireAtMS: expire, DeadlineMS: deadline})
		result.Expired = append(result.Expired, ExpiredEvent{PacketID: pkt.p.ID, ChannelID: pkt.p.ChannelID, ExpireAtMS: expire, DeadlineMS: deadline})
		e.drop(pkt, now, DropQueueExpired, "computed expiry exceeds queue age limit", result)
		return nil
	}
	pkt.status = statusDelayed
	pkt.readyAtMS = expire
	pkt.deadlineMS = deadline
	pkt.delayCause = cause
	event := DelayEvent{
		PacketID: pkt.p.ID, ChannelID: pkt.p.ChannelID, Reason: cause,
		DelayMS: delay, ExpireAtMS: expire, DeadlineMS: deadline,
	}
	e.delayedEvents = append(e.delayedEvents, event)
	result.Delayed = append(result.Delayed, event)
	return nil
}

func (e *Engine) dueDelays(now int64) []delayRT {
	var due []delayRT
	for _, cr := range e.channels {
		_ = cr
	}
	// Delay entries are reconstructed from per-channel queues in snapshot form
	// and held in a stable scan over packet state to keep this engine small.
	for _, rt := range e.packets {
		if rt.status != statusDelayed || rt.readyAtMS > now {
			continue
		}
		due = append(due, delayRT{
			pkt: rt, expireAtMS: rt.readyAtMS, deadlineMS: rt.deadlineMS,
			replaySeq: rt.replaySeq, cause: rt.delayCause,
		})
	}
	sort.SliceStable(due, func(i, j int) bool {
		if due[i].expireAtMS != due[j].expireAtMS {
			return due[i].expireAtMS < due[j].expireAtMS
		}
		if due[i].pkt.p.Seq != due[j].pkt.p.Seq {
			return due[i].pkt.p.Seq < due[j].pkt.p.Seq
		}
		return due[i].pkt.p.ID < due[j].pkt.p.ID
	})
	return due
}

func (e *Engine) expireDelayed(de delayRT, now int64, result *StepResult) {
	de.pkt.status = statusReady
	e.expired = append(e.expired, ExpiredEvent{
		PacketID: de.pkt.p.ID, ChannelID: de.pkt.p.ChannelID,
		ExpireAtMS: de.expireAtMS, DeadlineMS: de.deadlineMS,
	})
	result.Expired = append(result.Expired, ExpiredEvent{
		PacketID: de.pkt.p.ID, ChannelID: de.pkt.p.ChannelID,
		ExpireAtMS: de.expireAtMS, DeadlineMS: de.deadlineMS,
	})
	e.drop(de.pkt, now, DropQueueExpired, "delay queue entry exceeded its deadline", result)
}

func (e *Engine) drop(pkt *packetRT, now int64, reason int, detail string, result *StepResult) {
	if pkt.status == statusEmitted || pkt.status == statusDropped {
		return
	}
	if pkt.status == statusReady || pkt.status == statusDelayed {
		// Caller may already have popped a ready head. Outstanding is reduced
		// exactly once here for every non-final packet.
		cr := e.chByID[pkt.p.ChannelID]
		cr.outstanding--
	}
	pkt.status = statusDropped
	e.unresolved--
	event := DropEvent{
		PacketID: pkt.p.ID, ChannelID: pkt.p.ChannelID, SizeBytes: pkt.p.SizeBytes,
		AtMS: now, ReasonCode: reason, Reason: DropReasonLabel(reason), Detail: detail,
	}
	e.dropped = append(e.dropped, event)
	result.Dropped = append(result.Dropped, event)
}

func (e *Engine) queueSnapshot(now int64) []QueueEntry {
	var out []QueueEntry
	for _, rt := range e.packets {
		if rt.status != statusDelayed {
			continue
		}
		out = append(out, QueueEntry{
			PacketID: rt.p.ID, ChannelID: rt.p.ChannelID,
			ExpireAtMS: rt.readyAtMS, RemainingMS: rt.readyAtMS - now,
			DeadlineMS: rt.deadlineMS, ReplaySeq: rt.replaySeq,
			Reason: rt.delayCause, SizeBytes: rt.p.SizeBytes,
		})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].ExpireAtMS != out[j].ExpireAtMS {
			return out[i].ExpireAtMS < out[j].ExpireAtMS
		}
		a := e.packets[out[i].PacketID].p.Seq
		b := e.packets[out[j].PacketID].p.Seq
		if a != b {
			return a < b
		}
		return out[i].PacketID < out[j].PacketID
	})
	return out
}

func (e *Engine) suppressedInfo(cr *channelRT, budget int64) SuppressedInfo {
	head := cr.ready[0]
	var pendingBytes int64
	for _, rt := range cr.ready {
		pendingBytes += rt.p.SizeBytes
	}
	return SuppressedInfo{
		ChannelID: cr.ch.ID, PendingPackets: len(cr.ready),
		PendingBytes: pendingBytes, HeadBytes: head.p.SizeBytes,
		BudgetRemaining: budget,
		WeightBasisID:   basisID(*cr.ch, head, cr.credit),
	}
}

func (e *Engine) Run(ctx context.Context) (*RunResult, error) {
	steps := make([]StepResult, 0)
	for !e.Done() {
		select {
		case <-ctx.Done():
			return nil, NewError(EContextCanceled, "engine.run", "execution canceled", map[string]any{"reason": context.Cause(ctx).Error()})
		default:
		}
		step, err := e.Step()
		if err != nil {
			return nil, err
		}
		steps = append(steps, step)
	}
	final := make(map[string]int64, len(e.channels))
	for _, cr := range e.channels {
		final[cr.ch.ID] = cr.tokens
	}
	return &RunResult{
		Steps: steps, Emissions: cloneEmissions(e.emitted), Delays: append([]DelayEvent(nil), e.delayedEvents...),
		Drops: cloneDrops(e.dropped), Expired: append([]ExpiredEvent(nil), e.expired...),
		TotalSteps: e.nextStep, FinalTokens: final,
	}, nil
}

func cloneEmissions(in []Emission) []Emission {
	out := append([]Emission(nil), in...)
	for i := range out {
		if in[i].DSCP != nil {
			d := *in[i].DSCP
			out[i].DSCP = &d
		}
	}
	return out
}

func cloneDrops(in []DropEvent) []DropEvent { return append([]DropEvent(nil), in...) }
