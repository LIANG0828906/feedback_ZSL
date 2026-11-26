package engine

import (
	"encoding/json"
	"fmt"
	"hash/fnv"
	"sort"
	"sync"
	"time"

	"warmcache/internal/domain"
)

const (
	ConstraintBandwidth   = "C_BW_BUDGET"
	ConstraintConcurrency = "C_NODE_CONCURRENCY"
	ConstraintBuffer      = "C_NODE_BUFFER"
	defaultMaxWorkers     = 32
	hardMaxBuffer         = 64
	maxTicks              = 10000
)

type leaseState struct {
	record domain.LeaseRecord
	index  int
}

type shardRuntime struct {
	spec              domain.Shard
	visibleVersion    int64
	sourceVersion     int64
	targetVersion     int64
	progress          int64
	committedProgress int64
	available         bool
	retired           bool
	lease             *leaseState
	generation        int
	queueSince        int64
	queueConstraint   string
	queuePosition     int
	resumeNode        string
	pendingReplay     []string
	lastCommittedTick int64
}

type nodeRuntime struct {
	spec       domain.Node
	workerSlot int
	observedMS float64
	tickOps    int
}

type simState struct {
	project domain.Project
	nodes   map[string]*nodeRuntime
	shards  map[string]*shardRuntime
	leases  []domain.LeaseRecord
}

type simulator struct {
	state         *simState
	opts          domain.Options
	requests      map[int64][]domain.Request
	edits         map[int64][]domain.Edit
	interrupts    map[int64][]domain.Interrupt
	timeline      []domain.TickState
	gateResults   []domain.GateResult
	issues        []domain.Issue
	backpressure  []domain.Issue
	diagnostics   domain.Diagnostics
	invalidations []domain.Invalidation
	recomputed    []int64
	reusedTicks   int
	stoppedAt     int64
	interrupted   bool
}

type simSnapshot struct {
	AfterTick     int64
	State         *simState
	Timeline      []domain.TickState
	GateResults   []domain.GateResult
	Issues        []domain.Issue
	Backpressure  []domain.Issue
	Diagnostics   domain.Diagnostics
	Invalidations []domain.Invalidation
	StoppedAt     int64
	Interrupted   bool
}

type nodePlan struct {
	NodeID     string
	Due        bool
	Held       int
	BufferFull bool
	WorkerSlot int
	Elapsed    time.Duration
}

func validateOptions(opts domain.Options, nodeCount int) (domain.Options, error) {
	if opts.Ticks == 0 {
		opts.Ticks = 30
	}
	if opts.Ticks < 1 || opts.Ticks > maxTicks {
		return opts, domain.NewBizError(domain.CodeBadRequest, "options", "ticks out of range", map[string]any{"ticks": opts.Ticks, "max": maxTicks})
	}
	if opts.MaxWorkers == 0 {
		opts.MaxWorkers = defaultMaxWorkers
	}
	if opts.MaxWorkers < 1 || opts.MaxWorkers > 256 {
		return opts, domain.NewBizError(domain.CodeThreadBudgetInvalid, "options", "max workers out of range", map[string]any{"max_workers": opts.MaxWorkers})
	}
	if opts.Workers == 0 {
		opts.Workers = nodeCount
		if opts.Workers < 1 {
			opts.Workers = 1
		}
		if opts.Workers > 4 {
			opts.Workers = 4
		}
	}
	if opts.Workers < 1 || opts.Workers > opts.MaxWorkers {
		return opts, domain.NewBizError(domain.CodeThreadBudgetInvalid, "options", "worker budget is outside the configured range", map[string]any{
			"workers": opts.Workers, "max_workers": opts.MaxWorkers,
		})
	}
	if opts.MaxBufferPerNode < 0 || opts.MaxBufferPerNode > hardMaxBuffer {
		return opts, domain.NewBizError(domain.CodeBufferBudgetInvalid, "options", "per-node buffer budget is outside the configured range", map[string]any{
			"max_buffer_per_node": opts.MaxBufferPerNode, "max": hardMaxBuffer,
		})
	}
	return opts, nil
}

func newSimulator(project domain.Project, requests []domain.Request, opts domain.Options) (*simulator, error) {
	if err := project.Validate(); err != nil {
		return nil, err
	}
	normalizedOpts, err := validateOptions(opts, len(project.Nodes))
	if err != nil {
		return nil, err
	}
	sim := &simulator{
		state: &simState{
			project: project.Clone(),
			nodes:   map[string]*nodeRuntime{},
			shards:  map[string]*shardRuntime{},
		},
		opts:       normalizedOpts,
		requests:   map[int64][]domain.Request{},
		edits:      map[int64][]domain.Edit{},
		interrupts: map[int64][]domain.Interrupt{},
	}
	sim.diagnostics = domain.Diagnostics{
		WorkersRequested: normalizedOpts.Workers,
		MaxWorkers:       normalizedOpts.MaxWorkers,
	}
	for _, node := range project.Nodes {
		sim.state.nodes[node.ID] = &nodeRuntime{spec: node, workerSlot: -1}
	}
	index := 0
	for _, shard := range project.Shards {
		sim.state.shards[shard.ID] = &shardRuntime{
			spec:           shard,
			visibleVersion: shard.PreviousVersion,
			sourceVersion:  shard.SourceVersion,
			targetVersion:  shard.SourceVersion,
		}
		index++
	}
	requestIDs := map[string]bool{}
	for _, request := range requests {
		if request.ID == "" {
			return nil, domain.NewBizError(domain.CodeBadRequest, "requests", "request id is required", nil)
		}
		if requestIDs[request.ID] {
			return nil, domain.NewBizError(domain.CodeBadRequest, "requests", "duplicate request id", map[string]any{"request": request.ID})
		}
		requestIDs[request.ID] = true
		if request.Tick < 1 || request.Tick > normalizedOpts.Ticks {
			continue
		}
		if request.Kind != domain.RequestRead && request.Kind != domain.RequestWrite {
			return nil, domain.NewBizError(domain.CodeBadRequest, "requests", "request kind must be read or write", map[string]any{"request": request.ID})
		}
		sim.requests[request.Tick] = append(sim.requests[request.Tick], request)
	}
	for tick := range sim.requests {
		sort.Slice(sim.requests[tick], func(i, j int) bool { return sim.requests[tick][i].ID < sim.requests[tick][j].ID })
	}
	for index, interrupt := range normalizedOpts.Interrupts {
		if interrupt.ID == "" {
			normalizedOpts.Interrupts[index].ID = fmt.Sprintf("interrupt-%02d", index+1)
		}
		if interrupt.Tick < 1 || interrupt.Tick > normalizedOpts.Ticks {
			return nil, domain.NewBizError(domain.CodeInvalidInterruption, "interrupts", "interrupt tick is out of range", map[string]any{"interrupt": normalizedOpts.Interrupts[index].ID, "tick": interrupt.Tick})
		}
		if _, ok := sim.state.shards[interrupt.ShardID]; !ok {
			return nil, domain.NewBizError(domain.CodeInvalidInterruption, "interrupts", "interrupt references an unknown shard", map[string]any{"interrupt": normalizedOpts.Interrupts[index].ID, "shard": interrupt.ShardID})
		}
		sim.interrupts[interrupt.Tick] = append(sim.interrupts[interrupt.Tick], normalizedOpts.Interrupts[index])
	}
	for index := range normalizedOpts.Edits {
		edit := normalizedOpts.Edits[index]
		if edit.ID == "" {
			edit.ID = fmt.Sprintf("edit-%02d", index+1)
			normalizedOpts.Edits[index].ID = edit.ID
		}
		if edit.AtTick < 1 || edit.AtTick > normalizedOpts.Ticks {
			return nil, domain.NewBizError(domain.CodeEditInvalid, "edits", "edit tick is out of range", map[string]any{"edit": edit.ID, "tick": edit.AtTick})
		}
		if err := sim.validateEdit(edit); err != nil {
			return nil, err
		}
		sim.edits[edit.AtTick] = append(sim.edits[edit.AtTick], edit)
	}
	for tick := range sim.edits {
		sort.Slice(sim.edits[tick], func(i, j int) bool { return sim.edits[tick][i].ID < sim.edits[tick][j].ID })
	}
	for tick := range sim.interrupts {
		sort.Slice(sim.interrupts[tick], func(i, j int) bool { return sim.interrupts[tick][i].ID < sim.interrupts[tick][j].ID })
	}
	sim.opts = normalizedOpts
	return sim, nil
}

func (s *simulator) validateEdit(edit domain.Edit) error {
	switch edit.Kind {
	case domain.EditBumpShard, domain.EditRetireShard:
		if _, ok := s.state.shards[edit.ShardID]; !ok {
			return domain.NewBizError(domain.CodeEditInvalid, "edits", "edit references an unknown shard", map[string]any{"edit": edit.ID, "shard": edit.ShardID})
		}
	case domain.EditRetireNode:
		if _, ok := s.state.nodes[edit.NodeID]; !ok {
			return domain.NewBizError(domain.CodeEditInvalid, "edits", "edit references an unknown node", map[string]any{"edit": edit.ID, "node": edit.NodeID})
		}
	case domain.EditJoinNode:
		if edit.NodeID == "" {
			return domain.NewBizError(domain.CodeEditInvalid, "edits", "join edit requires node_id", map[string]any{"edit": edit.ID})
		}
		if _, exists := s.state.nodes[edit.NodeID]; exists {
			return domain.NewBizError(domain.CodeEditInvalid, "edits", "join edit references an existing node", map[string]any{"edit": edit.ID, "node": edit.NodeID})
		}
		if edit.LeaseTicks < 0 || edit.MaxConcurrent < 0 || edit.BufferLimit < 0 || edit.ClaimIntervalTicks < 0 {
			return domain.NewBizError(domain.CodeEditInvalid, "edits", "join edit contains a negative limit", map[string]any{"edit": edit.ID})
		}
	case domain.EditSetBudget:
		if edit.BudgetMilli <= 0 {
			return domain.NewBizError(domain.CodeEditInvalid, "edits", "budget edit must be positive", map[string]any{"edit": edit.ID})
		}
	case domain.EditSetNodeLimits:
		if _, ok := s.state.nodes[edit.NodeID]; !ok {
			return domain.NewBizError(domain.CodeEditInvalid, "edits", "edit references an unknown node", map[string]any{"edit": edit.ID, "node": edit.NodeID})
		}
		if edit.MaxConcurrent < 0 || edit.BufferLimit < 0 || edit.ClaimIntervalTicks < 0 || edit.LeaseTicks < 0 {
			return domain.NewBizError(domain.CodeEditInvalid, "edits", "node limit edit contains a negative value", map[string]any{"edit": edit.ID})
		}
	default:
		return domain.NewBizError(domain.CodeEditInvalid, "edits", "unsupported edit kind", map[string]any{"edit": edit.ID, "kind": edit.Kind})
	}
	return nil
}

func (s *simulator) run(lastTick int64) error {
	return s.runFrom(1, lastTick)
}

func (s *simulator) runFrom(firstTick, lastTick int64) error {
	for tick := firstTick; tick <= lastTick; tick++ {
		state, err := s.step(tick)
		if err != nil {
			return err
		}
		s.timeline = append(s.timeline, state)
		s.recomputed = append(s.recomputed, tick)
		if s.stoppedAt > 0 && s.stoppedAt == tick {
			return nil
		}
	}
	return nil
}

func (s *simulator) step(tick int64) (domain.TickState, error) {
	tickState := domain.TickState{Tick: tick, Capacity: s.state.project.BudgetMilli}
	var acquired []domain.LeaseRecord
	var released []domain.LeaseRecord
	issues := make([]domain.Issue, 0)

	s.applyEdits(tick)
	s.applyRetirements(tick, &released)
	s.applyInterrupts(tick, &released)
	s.expireLeases(tick, &released)

	plans := s.collectNodePlans(tick, &issues)
	s.assignClaims(tick, plans, &acquired)
	for _, plan := range plans {
		node := s.state.nodes[plan.NodeID]
		if node == nil {
			continue
		}
		node.observedMS += float64(plan.Elapsed.Nanoseconds()) / 1_000_000
		node.tickOps++
		if node.workerSlot < 0 {
			node.workerSlot = plan.WorkerSlot
		}
	}

	activeByNode, usedByNode, usedTotal, queue, commits, moreReleased := s.settle(tick)
	released = append(released, moreReleased...)
	requests := s.gateRequests(tick)

	for _, lease := range acquired {
		tickState.LeasesAcquired = append(tickState.LeasesAcquired, lease)
	}
	for _, lease := range released {
		tickState.LeasesReleased = append(tickState.LeasesReleased, lease)
	}
	tickState.Commits = commits
	tickState.Requests = requests
	tickState.Queue = queue
	tickState.UsedBandwidth = usedTotal
	tickState.Capacity = s.state.project.BudgetMilli
	tickState.CacheHit = false
	if len(s.invalidations) > 0 {
		tickState.Invalidation = append([]domain.Invalidation(nil), s.invalidations...)
	}
	tickState.Nodes = s.nodeStates(tick, activeByNode, usedByNode)
	tickState.Shards = s.shardStates()
	tickState.Issues = issues
	s.gateResults = append(s.gateResults, requests...)

	if violations := s.auditTick(tick, tickState); len(violations) > 0 {
		tickState.Issues = append(tickState.Issues, violations...)
		s.issues = append(s.issues, violations...)
	}
	if len(issues) > 0 {
		s.issues = append(s.issues, issues...)
	}
	if len(plans) > 0 {
		workersUsed := s.opts.Workers
		if workersUsed > len(plans) {
			workersUsed = len(plans)
		}
		s.diagnostics.WorkersUsed = workersUsed
	}
	return tickState, nil
}

func (s *simulator) applyEdits(tick int64) {
	for _, edit := range s.edits[tick] {
		switch edit.Kind {
		case domain.EditBumpShard:
			shard := s.state.shards[edit.ShardID]
			s.releaseLease(shard, tick, "source_version_advanced", nil)
			previous := shard.visibleVersion
			next := shard.targetVersion + 1
			if edit.SourceVersion > 0 {
				next = edit.SourceVersion
			}
			if edit.PreviousVersion > 0 {
				previous = edit.PreviousVersion
			}
			shard.visibleVersion = previous
			shard.sourceVersion = next
			shard.targetVersion = next
			shard.progress = 0
			shard.committedProgress = 0
			shard.available = false
			shard.queueSince = tick
			if edit.Digest != 0 {
				shard.spec.Digest = edit.Digest
			} else {
				shard.spec.Digest = editDigest(s.state.project.Seed, shard.spec.ID, next)
			}
			shard.spec.SourceVersion = next
			shard.spec.PreviousVersion = previous
			s.state.project.Shards = replaceShard(s.state.project.Shards, shard.spec)
		case domain.EditRetireNode:
			node := s.state.nodes[edit.NodeID]
			if node != nil && (node.spec.RetireTick == 0 || node.spec.RetireTick > tick) {
				node.spec.RetireTick = tick
				s.retireNodeLeases(node.spec.ID, tick, "node_retired", nil)
			}
		case domain.EditJoinNode:
			joinAt := edit.JoinTick
			if joinAt == 0 {
				joinAt = tick
			}
			lease := edit.LeaseTicks
			if lease == 0 {
				lease = 6
			}
			interval := edit.ClaimIntervalTicks
			if interval == 0 {
				interval = 1
			}
			concurrency := edit.MaxConcurrent
			if concurrency == 0 {
				concurrency = 2
			}
			buffer := edit.BufferLimit
			if buffer == 0 {
				buffer = 2
			}
			node := domain.Node{
				ID: edit.NodeID, Group: "edited", JoinTick: joinAt,
				ClaimIntervalTicks: interval, LeaseTicks: lease,
				MaxConcurrent: concurrency, BufferLimit: buffer,
			}
			s.state.nodes[node.ID] = &nodeRuntime{spec: node, workerSlot: -1}
			s.state.project.Nodes = append(s.state.project.Nodes, node)
		case domain.EditSetBudget:
			s.state.project.BudgetMilli = edit.BudgetMilli
		case domain.EditRetireShard:
			shard := s.state.shards[edit.ShardID]
			s.releaseLease(shard, tick, "shard_retired", nil)
			shard.retired = true
		case domain.EditSetNodeLimits:
			node := s.state.nodes[edit.NodeID]
			if node == nil {
				continue
			}
			if edit.MaxConcurrent > 0 {
				node.spec.MaxConcurrent = edit.MaxConcurrent
			}
			if edit.BufferLimit > 0 {
				node.spec.BufferLimit = edit.BufferLimit
			}
			if edit.ClaimIntervalTicks > 0 {
				node.spec.ClaimIntervalTicks = edit.ClaimIntervalTicks
			}
			if edit.LeaseTicks > 0 {
				node.spec.LeaseTicks = edit.LeaseTicks
			}
			s.state.project.Nodes = replaceNode(s.state.project.Nodes, node.spec)
		}
		s.invalidations = append(s.invalidations, s.invalidationFor(edit))
	}
	s.state.project.Normalize()
}

func (s *simulator) invalidationFor(edit domain.Edit) domain.Invalidation {
	closure := []string{"lease-sequence", "budget-settlement", "staggered-queue", "write-gate"}
	switch edit.Kind {
	case domain.EditBumpShard, domain.EditRetireShard:
		closure = append([]string{"shard:" + edit.ShardID, "gate:" + edit.ShardID}, closure...)
		if shard := s.state.shards[edit.ShardID]; shard != nil {
			closure = append(closure, "node:"+shard.spec.NodeID)
			if shard.resumeNode != "" {
				closure = append(closure, "node:"+shard.resumeNode)
			}
		}
	case domain.EditRetireNode, domain.EditJoinNode, domain.EditSetNodeLimits:
		closure = append([]string{"node:" + edit.NodeID}, closure...)
		for _, shard := range s.state.shards {
			if shard.spec.NodeID == edit.NodeID || (shard.lease != nil && shard.lease.record.NodeID == edit.NodeID) {
				closure = append(closure, "shard:"+shard.spec.ID, "gate:"+shard.spec.ID)
			}
		}
	case domain.EditSetBudget:
		closure = append([]string{"budget:" + fmt.Sprint(edit.BudgetMilli)}, closure...)
	}
	closure = uniqueSorted(closure)
	return domain.Invalidation{
		EditID: edit.ID, Kind: edit.Kind, Source: string(edit.Kind),
		Closure: closure, AtTick: edit.AtTick, ReusedBefore: int64(s.reusedTicks),
	}
}

func (s *simulator) applyRetirements(tick int64, released *[]domain.LeaseRecord) {
	nodeIDs := make([]string, 0, len(s.state.nodes))
	for id := range s.state.nodes {
		nodeIDs = append(nodeIDs, id)
	}
	sort.Strings(nodeIDs)
	for _, id := range nodeIDs {
		node := s.state.nodes[id]
		if node.spec.RetireTick > 0 && node.spec.RetireTick == tick {
			s.retireNodeLeases(id, tick, "node_retired", released)
		}
	}
}

func (s *simulator) retireNodeLeases(nodeID string, tick int64, reason string, released *[]domain.LeaseRecord) {
	shardIDs := make([]string, 0)
	for id, shard := range s.state.shards {
		if shard.lease != nil && shard.lease.record.NodeID == nodeID {
			shardIDs = append(shardIDs, id)
		}
	}
	sort.Strings(shardIDs)
	for _, id := range shardIDs {
		s.releaseLease(s.state.shards[id], tick, reason, released)
	}
}

func (s *simulator) applyInterrupts(tick int64, released *[]domain.LeaseRecord) {
	for _, interrupt := range s.interrupts[tick] {
		shard := s.state.shards[interrupt.ShardID]
		if shard == nil {
			continue
		}
		if shard.lease != nil {
			shard.progress = shard.committedProgress
			s.releaseLease(shard, tick, "interrupted", released)
			shard.queueSince = tick
		}
		if interrupt.ExitSignal {
			s.stoppedAt = tick
			s.interrupted = true
		}
	}
}

func (s *simulator) expireLeases(tick int64, released *[]domain.LeaseRecord) {
	shardIDs := sortedShardIDs(s.state.shards)
	for _, id := range shardIDs {
		shard := s.state.shards[id]
		if shard.lease != nil && tick >= shard.lease.record.ExpiresTick {
			s.releaseLease(shard, tick, "lease_expired", released)
		}
	}
}

func (s *simulator) releaseLease(shard *shardRuntime, tick int64, reason string, released *[]domain.LeaseRecord) {
	if shard == nil || shard.lease == nil {
		return
	}
	lease := shard.lease
	record := &s.state.leases[lease.index]
	record.ReleasedTick = tick
	record.ReleaseReason = reason
	shard.resumeNode = record.NodeID
	shard.queueSince = tick
	shard.queuePosition = 0
	shard.queueConstraint = ""
	shard.lease = nil
	if released != nil {
		*released = append(*released, *record)
	}
}

func (s *simulator) collectNodePlans(tick int64, issues *[]domain.Issue) []nodePlan {
	nodeIDs := sortedNodeIDs(s.state.nodes)
	activeNodes := make([]*nodeRuntime, 0, len(nodeIDs))
	held := map[string]int{}
	for _, id := range nodeIDs {
		node := s.state.nodes[id]
		if node.spec.ActiveAt(tick) {
			activeNodes = append(activeNodes, node)
		}
	}
	for _, shard := range s.state.shards {
		if shard.lease != nil {
			held[shard.lease.record.NodeID]++
		}
	}
	if len(activeNodes) == 0 {
		return nil
	}
	workers := s.opts.Workers
	if workers > len(activeNodes) {
		workers = len(activeNodes)
	}
	if workers < len(activeNodes) {
		issue := domain.Issue{
			Code: domain.CodeBackpressureThreads, AtTick: tick,
			Detail:  "active nodes exceed worker budget; work remains queued in the bounded pool",
			Context: map[string]any{"workers": workers, "active_nodes": len(activeNodes)},
		}
		*issues = append(*issues, issue)
		s.backpressure = append(s.backpressure, issue)
	}
	jobs := make(chan int)
	plans := make([]nodePlan, len(activeNodes))
	var wait sync.WaitGroup
	for slot := 0; slot < workers; slot++ {
		wait.Add(1)
		go func(workerSlot int) {
			defer wait.Done()
			for index := range jobs {
				started := time.Now()
				node := activeNodes[index]
				plan := nodePlan{
					NodeID: node.spec.ID, Held: held[node.spec.ID], WorkerSlot: workerSlot,
				}
				plan.Due = nodeClaimDue(node.spec, tick)
				limit := node.spec.BufferLimit
				if s.opts.MaxBufferPerNode > 0 && s.opts.MaxBufferPerNode < limit {
					limit = s.opts.MaxBufferPerNode
				}
				plan.BufferFull = plan.Due && plan.Held >= limit
				plan.Elapsed = time.Since(started)
				plans[index] = plan
			}
		}(slot)
	}
	for index := range activeNodes {
		jobs <- index
	}
	close(jobs)
	wait.Wait()
	for _, plan := range plans {
		if plan.BufferFull {
			s.backpressure = append(s.backpressure, domain.Issue{
				Code: domain.CodeBackpressureBuffer, AtTick: tick, NodeID: plan.NodeID,
				Detail:  "node pending-shard buffer is full; claim was not admitted",
				Context: map[string]any{"held": plan.Held, "constraint": ConstraintBuffer},
			})
		}
	}
	return plans
}

func (s *simulator) assignClaims(tick int64, plans []nodePlan, acquired *[]domain.LeaseRecord) []domain.LeaseRecord {
	slots := make([]nodePlan, 0, len(plans))
	for _, plan := range plans {
		if plan.Due && !plan.BufferFull {
			slots = append(slots, plan)
		}
	}
	sort.Slice(slots, func(i, j int) bool {
		if slots[i].Held != slots[j].Held {
			return slots[i].Held < slots[j].Held
		}
		return slots[i].NodeID < slots[j].NodeID
	})
	pending := make([]*shardRuntime, 0)
	for _, id := range sortedShardIDs(s.state.shards) {
		shard := s.state.shards[id]
		if shard.available || shard.retired || shard.lease != nil {
			continue
		}
		pending = append(pending, shard)
	}
	assigned := map[string]bool{}
	for _, slot := range slots {
		selected := selectShardForSlot(pending, assigned, slot.NodeID, tick, s.state.nodes)
		if selected == "" {
			break
		}
		shard := s.state.shards[selected]
		node := s.state.nodes[slot.NodeID]
		if node == nil {
			continue
		}
		shard.generation++
		rec := domain.LeaseRecord{
			LeaseID:      fmt.Sprintf("lease-%s-%03d", shard.spec.ID, shard.generation),
			ShardID:      shard.spec.ID,
			NodeID:       node.spec.ID,
			AcquiredTick: tick,
			ExpiresTick:  tick + node.spec.LeaseTicks,
			Generation:   shard.generation,
		}
		shard.lease = &leaseState{record: rec, index: len(s.state.leases)}
		shard.queueSince = tick
		shard.queueConstraint = ""
		shard.queuePosition = 0
		s.state.leases = append(s.state.leases, rec)
		assigned[selected] = true
		*acquired = append(*acquired, rec)
	}
	return *acquired
}

func selectShardForSlot(pending []*shardRuntime, assigned map[string]bool, nodeID string, tick int64, nodes map[string]*nodeRuntime) string {
	for _, shard := range pending {
		if !assigned[shard.spec.ID] && shard.resumeNode == nodeID {
			return shard.spec.ID
		}
	}
	for _, shard := range pending {
		if !assigned[shard.spec.ID] && shard.spec.NodeID == nodeID {
			return shard.spec.ID
		}
	}
	for _, shard := range pending {
		if assigned[shard.spec.ID] {
			continue
		}
		owner := nodes[shard.spec.NodeID]
		ownerInactive := owner == nil || !owner.spec.ActiveAt(tick)
		if shard.generation > 0 || ownerInactive {
			return shard.spec.ID
		}
	}
	return ""
}

type queuedShard struct {
	shard      *shardRuntime
	constraint string
}

func (s *simulator) settle(tick int64) (map[string][]string, map[string]int64, int64, []domain.QueueEntry, []string, []domain.LeaseRecord) {
	for _, shard := range s.state.shards {
		shard.queuePosition = 0
		shard.queueConstraint = ""
	}
	candidates := make([]*shardRuntime, 0)
	for _, id := range sortedShardIDs(s.state.shards) {
		shard := s.state.shards[id]
		if shard.lease != nil && !shard.available && !shard.retired {
			candidates = append(candidates, shard)
		}
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].queueSince != candidates[j].queueSince {
			return candidates[i].queueSince < candidates[j].queueSince
		}
		return candidates[i].spec.ID < candidates[j].spec.ID
	})
	activeByNode := map[string][]string{}
	usedByNode := map[string]int64{}
	activeCount := map[string]int{}
	queued := make([]queuedShard, 0)
	commits := make([]string, 0)
	var usedTotal int64
	var released []domain.LeaseRecord
	for _, shard := range candidates {
		node := s.state.nodes[shard.lease.record.NodeID]
		if node == nil || !node.spec.ActiveAt(tick) {
			continue
		}
		constraint := ""
		if activeCount[node.spec.ID] >= node.spec.MaxConcurrent {
			constraint = ConstraintConcurrency
		} else if usedTotal+shard.spec.BandwidthMilli > s.state.project.BudgetMilli {
			constraint = ConstraintBandwidth
		}
		if constraint != "" {
			shard.queueConstraint = constraint
			shard.queuePosition = 0
			queued = append(queued, queuedShard{shard: shard, constraint: constraint})
			continue
		}
		activeCount[node.spec.ID]++
		usedByNode[node.spec.ID] += shard.spec.BandwidthMilli
		usedTotal += shard.spec.BandwidthMilli
		activeByNode[node.spec.ID] = append(activeByNode[node.spec.ID], shard.spec.ID)
		shard.progress++
		shard.committedProgress = shard.progress
		shard.queueConstraint = ""
		shard.queuePosition = 0
		if shard.progress >= shard.spec.PreheatTicks {
			shard.available = true
			shard.visibleVersion = shard.targetVersion
			shard.lastCommittedTick = tick
			shard.pendingReplay = nil
			commits = append(commits, shard.spec.ID)
			s.releaseLease(shard, tick, "preheat_completed", &released)
		}
	}
	queue := make([]domain.QueueEntry, 0, len(queued))
	for index, entry := range queued {
		entry.shard.queuePosition = index + 1
		queue = append(queue, domain.QueueEntry{
			ShardID: entry.shard.spec.ID, NodeID: entry.shard.lease.record.NodeID,
			Position: index + 1, Constraint: entry.constraint, SinceTick: entry.shard.queueSince,
		})
	}
	sort.Slice(commits, func(i, j int) bool { return commits[i] < commits[j] })
	return activeByNode, usedByNode, usedTotal, queue, commits, released
}

func (s *simulator) gateRequests(tick int64) []domain.GateResult {
	requests := s.requests[tick]
	results := make([]domain.GateResult, 0, len(requests))
	for _, request := range requests {
		shard := s.state.shards[request.ShardID]
		result := domain.GateResult{
			RequestID: request.ID, Tick: tick, Kind: request.Kind, ShardID: request.ShardID,
		}
		if shard == nil {
			result.Source = "gate"
			result.Rejected = true
			result.Code = domain.CodeNotFound
			result.Reason = "unknown_shard"
			results = append(results, result)
			continue
		}
		result.PendingReplay = len(shard.pendingReplay)
		if request.Kind == domain.RequestRead {
			if !shard.available {
				if request.Fresh {
					result.Source = "gate"
					result.Rejected = true
					result.Code = domain.CodeReadFreshUnavailable
					result.Reason = "fresh_read_required_during_preheat"
					result.ReturnedVersion = shard.visibleVersion
				} else {
					result.Source = "origin"
					result.Accepted = true
					result.ReturnedVersion = shard.visibleVersion
					result.Reason = "fallback_to_previous_committed_version"
				}
			} else {
				result.Source = "edge"
				result.Accepted = true
				result.ReturnedVersion = shard.targetVersion
				result.VisibleOnEdge = true
			}
			results = append(results, result)
			continue
		}
		version := request.Version
		if version == 0 {
			version = shard.targetVersion
		}
		if shard.retired {
			result.Source = "gate"
			result.Rejected = true
			result.Code = domain.CodeWriteGateClosed
			result.Reason = "shard_retired"
			results = append(results, result)
			continue
		}
		if version != shard.targetVersion {
			result.Source = "gate"
			result.Rejected = true
			result.Code = domain.CodeWriteVersionConflict
			result.Reason = "write_version_outside_preheat_window"
			result.ReturnedVersion = shard.visibleVersion
			results = append(results, result)
			continue
		}
		if !shard.available {
			shard.pendingReplay = append(shard.pendingReplay, request.ID)
			result.Source = "write-gate"
			result.Accepted = true
			result.ReturnedVersion = version
			result.PendingReplay = len(shard.pendingReplay)
			result.Reason = "deferred_until_preheat_commit"
		} else {
			result.Source = "edge"
			result.Accepted = true
			result.ReturnedVersion = version
			result.VisibleOnEdge = true
			result.PendingReplay = len(shard.pendingReplay)
		}
		results = append(results, result)
	}
	return results
}

func (s *simulator) nodeStates(tick int64, activeByNode map[string][]string, usedByNode map[string]int64) []domain.NodeTickState {
	states := make([]domain.NodeTickState, 0, len(s.state.nodes))
	for _, id := range sortedNodeIDs(s.state.nodes) {
		node := s.state.nodes[id]
		state := domain.NodeTickState{
			NodeID: id, Holdings: []string{}, ActiveShards: []string{},
			Active: node.spec.ActiveAt(tick), UsedBandwidth: usedByNode[id],
			Retired: node.spec.RetireTick > 0 && tick >= node.spec.RetireTick,
		}
		state.ActiveShards = append(state.ActiveShards, activeByNode[id]...)
		sort.Strings(state.ActiveShards)
		state.Concurrency = len(state.ActiveShards)
		for _, shardID := range sortedShardIDs(s.state.shards) {
			shard := s.state.shards[shardID]
			if shard.lease != nil && shard.lease.record.NodeID == id {
				state.Holdings = append(state.Holdings, shardID)
			}
		}
		state.PendingBuffer = len(state.Holdings)
		states = append(states, state)
	}
	return states
}

func (s *simulator) auditTick(tick int64, state domain.TickState) []domain.Issue {
	issues := make([]domain.Issue, 0)
	if state.UsedBandwidth > s.state.project.BudgetMilli {
		issues = append(issues, domain.Issue{
			Code: domain.CodeBudgetViolation, AtTick: tick,
			Detail:  "tick bandwidth exceeded the shared source budget",
			Context: map[string]any{"used": state.UsedBandwidth, "budget": s.state.project.BudgetMilli},
		})
	}
	for _, node := range state.Nodes {
		runtimeNode := s.state.nodes[node.NodeID]
		if runtimeNode != nil && node.Concurrency > runtimeNode.spec.MaxConcurrent {
			issues = append(issues, domain.Issue{
				Code: domain.CodeConcurrencyViolation, AtTick: tick, NodeID: node.NodeID,
				Detail:  "node concurrency exceeded its declaration",
				Context: map[string]any{"concurrency": node.Concurrency, "limit": runtimeNode.spec.MaxConcurrent},
			})
		}
	}
	leased := map[string]string{}
	available := map[string]bool{}
	for _, shard := range s.state.shards {
		if shard.lease != nil {
			if owner, exists := leased[shard.spec.ID]; exists && owner != shard.lease.record.NodeID {
				issues = append(issues, domain.Issue{
					Code: domain.CodeLeaseConflict, AtTick: tick, ShardID: shard.spec.ID,
					Detail:  "shard has more than one lease holder",
					Context: map[string]any{"first": owner, "second": shard.lease.record.NodeID},
				})
			}
			leased[shard.spec.ID] = shard.lease.record.NodeID
			node := s.state.nodes[shard.lease.record.NodeID]
			if node == nil || !node.spec.ActiveAt(tick) {
				issues = append(issues, domain.Issue{
					Code: domain.CodeRetiredNode, AtTick: tick, NodeID: shard.lease.record.NodeID, ShardID: shard.spec.ID,
					Detail: "lease is held by an inactive node",
				})
			}
		}
		if shard.available {
			if available[shard.spec.ID] {
				issues = append(issues, domain.Issue{
					Code: domain.CodeLeaseConflict, AtTick: tick, ShardID: shard.spec.ID,
					Detail: "shard is committed as available on more than one replica",
				})
			}
			available[shard.spec.ID] = true
		}
	}
	return issues
}

func (s *simulator) snapshot() *simSnapshot {
	stateCopy := &simState{
		project: s.state.project.Clone(),
		nodes:   map[string]*nodeRuntime{},
		shards:  map[string]*shardRuntime{},
		leases:  append([]domain.LeaseRecord(nil), s.state.leases...),
	}
	for id, node := range s.state.nodes {
		copyNode := *node
		stateCopy.nodes[id] = &copyNode
	}
	for id, shard := range s.state.shards {
		copyShard := *shard
		copyShard.pendingReplay = append([]string(nil), shard.pendingReplay...)
		if shard.lease != nil {
			copyLease := *shard.lease
			copyShard.lease = &copyLease
		}
		stateCopy.shards[id] = &copyShard
	}
	return &simSnapshot{
		AfterTick:     int64(len(s.timeline)),
		State:         stateCopy,
		Timeline:      append([]domain.TickState(nil), s.timeline...),
		GateResults:   append([]domain.GateResult(nil), s.gateResults...),
		Issues:        append([]domain.Issue(nil), s.issues...),
		Backpressure:  append([]domain.Issue(nil), s.backpressure...),
		Diagnostics:   s.diagnostics,
		Invalidations: append([]domain.Invalidation(nil), s.invalidations...),
		StoppedAt:     s.stoppedAt,
		Interrupted:   s.interrupted,
	}
}

func simulatorFromSnapshot(project domain.Project, requests []domain.Request, opts domain.Options, snapshot *simSnapshot) (*simulator, error) {
	sim, err := newSimulator(project, requests, opts)
	if err != nil {
		return nil, err
	}
	if snapshot == nil {
		return sim, nil
	}
	sim.state = cloneState(snapshot.State)
	sim.timeline = append([]domain.TickState(nil), snapshot.Timeline...)
	sim.gateResults = append([]domain.GateResult(nil), snapshot.GateResults...)
	sim.issues = append([]domain.Issue(nil), snapshot.Issues...)
	sim.backpressure = append([]domain.Issue(nil), snapshot.Backpressure...)
	sim.diagnostics = snapshot.Diagnostics
	sim.invalidations = append([]domain.Invalidation(nil), snapshot.Invalidations...)
	sim.stoppedAt = snapshot.StoppedAt
	sim.interrupted = snapshot.Interrupted
	sim.reusedTicks = len(snapshot.Timeline)
	for index := range sim.timeline {
		sim.timeline[index].CacheHit = true
	}
	return sim, nil
}

func cloneState(source *simState) *simState {
	if source == nil {
		return nil
	}
	stateCopy := &simState{
		project: source.project.Clone(),
		nodes:   map[string]*nodeRuntime{},
		shards:  map[string]*shardRuntime{},
		leases:  append([]domain.LeaseRecord(nil), source.leases...),
	}
	for id, node := range source.nodes {
		copyNode := *node
		stateCopy.nodes[id] = &copyNode
	}
	for id, shard := range source.shards {
		copyShard := *shard
		copyShard.pendingReplay = append([]string(nil), shard.pendingReplay...)
		if shard.lease != nil {
			copyLease := *shard.lease
			copyShard.lease = &copyLease
		}
		stateCopy.shards[id] = &copyShard
	}
	return stateCopy
}

func (s *simulator) result(edits []domain.Edit, incremental bool) domain.RunResult {
	shards := s.shardStates()
	nodes := make([]domain.NodeDiagnostic, 0, len(s.state.nodes))
	for _, id := range sortedNodeIDs(s.state.nodes) {
		node := s.state.nodes[id]
		nodes = append(nodes, domain.NodeDiagnostic{
			NodeID: id, ObservedMS: node.observedMS, WorkerSlot: node.workerSlot, TickOps: node.tickOps,
		})
	}
	s.diagnostics.Nodes = nodes
	s.diagnostics.RecomputedTicks = append([]int64(nil), s.recomputed...)
	s.diagnostics.ReusedTicks = s.reusedTicks
	runID := resultID(s.state.project, edits, s.opts, s.timeline)
	inputRequests := make([]domain.Request, 0)
	for _, tick := range sortedInt64Keys(s.requests) {
		inputRequests = append(inputRequests, s.requests[tick]...)
	}
	result := domain.RunResult{
		RunID: runID, Schema: s.state.project.Schema,
		SemanticHash: s.state.project.SemanticHash(), Project: s.state.project.Clone(),
		Edits: append([]domain.Edit(nil), edits...), InputRequests: inputRequests,
		Timeline: append([]domain.TickState(nil), s.timeline...),
		Leases:   append([]domain.LeaseRecord(nil), s.state.leases...), Shards: shards,
		Requests: append([]domain.GateResult(nil), s.gateResults...), Diagnostics: s.diagnostics,
		Backpressure: append([]domain.Issue(nil), s.backpressure...), Issues: append([]domain.Issue(nil), s.issues...),
		Incremental: incremental, ReusedTicks: s.reusedTicks, StoppedAtTick: s.stoppedAt, Interrupted: s.interrupted,
	}
	return result
}

func sortedInt64Keys(values map[int64][]domain.Request) []int64 {
	keys := make([]int64, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	return keys
}

func (s *simulator) shardStates() []domain.ShardState {
	shards := make([]domain.ShardState, 0, len(s.state.shards))
	for _, id := range sortedShardIDs(s.state.shards) {
		shard := s.state.shards[id]
		state := domain.ShardState{
			ShardID: shard.spec.ID, NodeID: shard.spec.NodeID,
			TargetVersion: shard.targetVersion, VisibleVersion: shard.visibleVersion,
			SourceVersion: shard.sourceVersion, Progress: shard.progress,
			CommittedProgress: shard.committedProgress, PreheatTicks: shard.spec.PreheatTicks,
			Available: shard.available, Generation: shard.generation, Rebirths: maxInt(0, shard.generation-1),
			QueuePosition: shard.queuePosition, QueueConstraint: shard.queueConstraint,
			PendingReplay: len(shard.pendingReplay), ReplayQueue: append([]string(nil), shard.pendingReplay...),
			LastCommittedTick: shard.lastCommittedTick, Retired: shard.retired,
		}
		if shard.lease != nil {
			state.LeaseID = shard.lease.record.LeaseID
			state.LeaseNode = shard.lease.record.NodeID
			state.LeaseExpiresTick = shard.lease.record.ExpiresTick
		}
		shards = append(shards, state)
	}
	return shards
}

func resultID(project domain.Project, edits []domain.Edit, opts domain.Options, timeline []domain.TickState) string {
	hasher := fnv.New64a()
	_, _ = fmt.Fprintf(hasher, "%s|%v|%v|%d", project.SemanticHash(), edits, opts, len(timeline))
	return fmt.Sprintf("%016x", hasher.Sum64())
}

func nodeClaimDue(node domain.Node, tick int64) bool {
	if !node.ActiveAt(tick) {
		return false
	}
	start := node.JoinTick
	if start < 1 {
		start = 1
	}
	if tick < start {
		return false
	}
	return (tick-start)%node.ClaimIntervalTicks == 0
}

func sortedNodeIDs(nodes map[string]*nodeRuntime) []string {
	ids := make([]string, 0, len(nodes))
	for id := range nodes {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func sortedShardIDs(shards map[string]*shardRuntime) []string {
	ids := make([]string, 0, len(shards))
	for id := range shards {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

func uniqueSorted(values []string) []string {
	seen := map[string]bool{}
	result := make([]string, 0, len(values))
	for _, value := range values {
		if !seen[value] {
			seen[value] = true
			result = append(result, value)
		}
	}
	sort.Strings(result)
	return result
}

func maxInt(left, right int) int {
	if left > right {
		return left
	}
	return right
}

func replaceShard(shards []domain.Shard, replacement domain.Shard) []domain.Shard {
	result := append([]domain.Shard(nil), shards...)
	for index := range result {
		if result[index].ID == replacement.ID {
			result[index] = replacement
			return result
		}
	}
	return append(result, replacement)
}

func replaceNode(nodes []domain.Node, replacement domain.Node) []domain.Node {
	result := append([]domain.Node(nil), nodes...)
	for index := range result {
		if result[index].ID == replacement.ID {
			result[index] = replacement
			return result
		}
	}
	return append(result, replacement)
}

func editDigest(seed int64, shardID string, version int64) uint64 {
	hasher := fnv.New64a()
	_, _ = fmt.Fprintf(hasher, "%d:%s:%d", seed, shardID, version)
	return hasher.Sum64() & 0x0000FFFFFFFFFFFF
}

func ComparisonKey(result domain.RunResult) string {
	copyResult := result
	copyResult.RunID = ""
	copyResult.Schema = ""
	copyResult.Project.Schema = ""
	copyResult.Incremental = false
	copyResult.ReusedTicks = 0
	copyResult.MatchesFull = false
	copyResult.FullComparison = ""
	copyResult.Diagnostics = domain.Diagnostics{}
	copyResult.Timeline = append([]domain.TickState(nil), result.Timeline...)
	for index := range copyResult.Timeline {
		copyResult.Timeline[index].CacheHit = false
		copyResult.Timeline[index].Invalidation = nil
	}
	value, _ := json.Marshal(copyResult)
	sum := fnv.New64a()
	_, _ = sum.Write(value)
	return fmt.Sprintf("%016x", sum.Sum64())
}

func CompareResults(left, right domain.RunResult) error {
	leftKey := ComparisonKey(left)
	rightKey := ComparisonKey(right)
	if leftKey == rightKey {
		return nil
	}
	var diffs []string
	if left.SemanticHash != right.SemanticHash {
		diffs = append(diffs, "semantic_hash")
	}
	if len(left.Timeline) != len(right.Timeline) {
		diffs = append(diffs, "timeline_length")
	} else {
		for index := range left.Timeline {
			leftTick := left.Timeline[index]
			rightTick := right.Timeline[index]
			leftTick.CacheHit, rightTick.CacheHit = false, false
			leftTick.Invalidation, rightTick.Invalidation = nil, nil
			leftJSON, _ := json.Marshal(leftTick)
			rightJSON, _ := json.Marshal(rightTick)
			if string(leftJSON) != string(rightJSON) {
				diffs = append(diffs, fmt.Sprintf("tick:%d", left.Timeline[index].Tick))
				if len(diffs) >= 8 {
					break
				}
			}
		}
	}
	if len(diffs) == 0 {
		diffs = append(diffs, "non-timeline-state")
	}
	return domain.NewBizError(domain.CodeIncrementalMismatch, "compare_results", "incremental result differs from full recompute", map[string]any{
		"incremental": leftKey, "full": rightKey, "diffs": diffs,
	})
}
