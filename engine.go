package skillcore

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"sync"
)

type Intent struct {
	Tick     int64  `json:"tick"`
	SkillID  string `json:"skillId"`
	TargetID string `json:"targetId,omitempty"`
	Sequence int64  `json:"sequence"`
}

type RuntimeState struct {
	NextTick int64                   `json:"nextTick"`
	Pools    map[string]int64        `json:"pools"`
	Skills   map[string]SkillRuntime `json:"skills"`
}

type SkillRuntime struct {
	CooldownTicks      int64 `json:"cooldownTicks"`
	CastRemainingTicks int64 `json:"castRemainingTicks"`
	CastCount          int64 `json:"castCount"`
}

type PoolSnapshot struct {
	ID     string `json:"id"`
	Amount int64  `json:"amount"`
	Max    int64  `json:"max"`
}

type SkillSnapshot struct {
	ID                 string `json:"id"`
	Enabled            bool   `json:"enabled"`
	TimersReady        bool   `json:"timersReady"`
	Affordable         bool   `json:"affordable"`
	Available          bool   `json:"available"`
	CooldownTicks      int64  `json:"cooldownTicks"`
	CastRemainingTicks int64  `json:"castRemainingTicks"`
}

type Decision struct {
	Tick          int64  `json:"tick"`
	Order         int    `json:"order"`
	SkillID       string `json:"skillId"`
	TargetID      string `json:"targetId,omitempty"`
	Priority      int    `json:"priority"`
	ConflictType  string `json:"conflictType"`
	Result        string `json:"result"`
	Reason        string `json:"reason"`
	PoolAfter     int64  `json:"poolAfter"`
	CooldownAfter int64  `json:"cooldownAfter"`
}

type TickSnapshot struct {
	Tick      int64           `json:"tick"`
	Pools     []PoolSnapshot  `json:"pools"`
	Skills    []SkillSnapshot `json:"skills"`
	Decisions []Decision      `json:"decisions"`
}

type WorkerEvent struct {
	Tick   int64  `json:"tick"`
	PoolID string `json:"poolId,omitempty"`
	Kind   string `json:"kind"`
	Reason string `json:"reason"`
}

type SimOptions struct {
	SampleEvery int
	Workers     int
	BufferLimit int
}

type SimResult struct {
	StartTick  int64          `json:"startTick"`
	EndTick    int64          `json:"endTick"`
	StartState *RuntimeState  `json:"startState"`
	FinalState *RuntimeState  `json:"finalState"`
	Ticks      []TickSnapshot `json:"ticks"`
	Events     []WorkerEvent  `json:"events,omitempty"`
}

type Engine struct {
	project    *Project
	skillIndex map[string]int
}

func NewEngine(project *Project) (*Engine, error) {
	if project == nil {
		return nil, domainErr(CodeInput, "", "nil project")
	}
	clone := project.Clone()
	if err := clone.Normalize(); err != nil {
		return nil, err
	}
	engine := &Engine{project: clone, skillIndex: make(map[string]int, len(clone.Skills))}
	for i := range clone.Skills {
		engine.skillIndex[clone.Skills[i].ID] = i
	}
	return engine, nil
}

func (e *Engine) Project() *Project { return e.project.Clone() }

func (e *Engine) InitialState() *RuntimeState {
	state := &RuntimeState{
		NextTick: 0,
		Pools:    make(map[string]int64, len(e.project.Pools)),
		Skills:   make(map[string]SkillRuntime, len(e.project.Skills)),
	}
	for i := range e.project.Pools {
		pool := &e.project.Pools[i]
		state.Pools[pool.ID] = pool.Initial
	}
	for i := range e.project.Skills {
		state.Skills[e.project.Skills[i].ID] = SkillRuntime{}
	}
	return state
}

func (e *Engine) ValidateState(state *RuntimeState) error {
	if state == nil {
		return domainErr(CodeState, "state", "nil runtime state")
	}
	if state.NextTick < 0 {
		return domainErr(CodeState, "state.nextTick", "must be non-negative")
	}
	if len(state.Pools) != len(e.project.Pools) {
		return domainErr(CodeState, "state.pools", "resource pool set does not match project")
	}
	for i := range e.project.Pools {
		pool := &e.project.Pools[i]
		amount, ok := state.Pools[pool.ID]
		if !ok {
			return domainErr(CodeState, "state.pools."+pool.ID, "resource pool missing")
		}
		if amount < 0 || amount > pool.Max {
			return domainErr(CodeState, "state.pools."+pool.ID, "resource amount outside [0,max]")
		}
	}
	if len(state.Skills) != len(e.project.Skills) {
		return domainErr(CodeState, "state.skills", "skill set does not match project")
	}
	for i := range e.project.Skills {
		skill := &e.project.Skills[i]
		runtime, ok := state.Skills[skill.ID]
		if !ok {
			return domainErr(CodeState, "state.skills."+skill.ID, "skill runtime missing")
		}
		if runtime.CooldownTicks < 0 || runtime.CastRemainingTicks < 0 || runtime.CastCount < 0 {
			return domainErr(CodeState, "state.skills."+skill.ID, "runtime counters must be non-negative")
		}
	}
	return nil
}

func (s *RuntimeState) Clone() *RuntimeState {
	if s == nil {
		return nil
	}
	out := &RuntimeState{
		NextTick: s.NextTick,
		Pools:    make(map[string]int64, len(s.Pools)),
		Skills:   make(map[string]SkillRuntime, len(s.Skills)),
	}
	for id, value := range s.Pools {
		out.Pools[id] = value
	}
	for id, value := range s.Skills {
		out.Skills[id] = value
	}
	return out
}

func (e *Engine) Step(state *RuntimeState, intents []Intent) (*RuntimeState, TickSnapshot, error) {
	start := state
	if start == nil {
		start = e.InitialState()
	}
	result, err := e.Run(start, intents, start.NextTick+1, SimOptions{SampleEvery: 1, Workers: 1, BufferLimit: 1})
	if err != nil {
		return nil, TickSnapshot{}, err
	}
	return result.FinalState, result.Ticks[0], nil
}

// Run executes [start.NextTick,endTick). The supplied start state is never
// mutated, which makes a failed tick or window safe to discard and retry.
func (e *Engine) Run(start *RuntimeState, intents []Intent, endTick int64, opts SimOptions) (*SimResult, error) {
	state := start
	if state == nil {
		state = e.InitialState()
	}
	state = state.Clone()
	if err := e.ValidateState(state); err != nil {
		return nil, err
	}
	if endTick < state.NextTick {
		return nil, domainErr(CodeInput, "endTick", "must not precede state.NextTick")
	}
	if opts.SampleEvery == 0 {
		opts.SampleEvery = 1
	}
	if opts.SampleEvery < 1 {
		return nil, domainErr(CodeInput, "sampleEvery", "must be at least one")
	}
	if opts.Workers == 0 {
		opts.Workers = 1
	}
	if opts.Workers < 1 || opts.Workers > 64 {
		return nil, domainErr(CodeInput, "workers", "must be in [1,64]")
	}
	if opts.BufferLimit == 0 {
		opts.BufferLimit = 1
	}
	if opts.BufferLimit < 1 {
		return nil, domainErr(CodeInput, "bufferLimit", "must be at least one")
	}
	byTick, err := e.prepareIntents(intents, state.NextTick, endTick)
	if err != nil {
		return nil, err
	}
	if e.project.ConflictPolicy == ConflictStrictPriority {
		if err := e.checkStrictConflicts(byTick, state.NextTick, endTick); err != nil {
			return nil, err
		}
	}
	startState := state.Clone()
	regen, events, err := e.precomputeRegen(state.NextTick, endTick, opts)
	if err != nil {
		return nil, err
	}
	effectiveCooldowns, cooldownEvents, err := e.precomputeCooldowns(state.NextTick, endTick, opts)
	if err != nil {
		return nil, err
	}
	events = append(events, cooldownEvents...)
	snapshots := make([]TickSnapshot, 0, int(endTick-state.NextTick))
	for tick := state.NextTick; tick < endTick; tick++ {
		offset := int(tick - startState.NextTick)
		poolBase := offset * len(e.project.Pools)
		cooldownBase := offset * len(e.project.Skills)
		for i := range e.project.Pools {
			pool := &e.project.Pools[i]
			gain := regen[poolBase+i]
			if gain < 0 {
				return nil, domainErr(CodeInput, fmt.Sprintf("pools[%s].regen", pool.ID), "resource regeneration resolved to a negative value")
			}
			next, ok := addChecked(state.Pools[pool.ID], gain)
			if !ok {
				return nil, overflowError(fmt.Sprintf("pools[%s].regen", pool.ID), string(UnitResource), "resource accumulation overflow")
			}
			if next > pool.Max {
				next = pool.Max
			}
			state.Pools[pool.ID] = next
		}
		for i := range e.project.Skills {
			skill := &e.project.Skills[i]
			runtime := state.Skills[skill.ID]
			if runtime.CooldownTicks > 0 {
				runtime.CooldownTicks--
			}
			if runtime.CastRemainingTicks > 0 {
				runtime.CastRemainingTicks--
			}
			state.Skills[skill.ID] = runtime
		}
		tickIntents := byTick[tick]
		decisions, err := e.resolveTick(tick, tickIntents, state, effectiveCooldowns, cooldownBase)
		if err != nil {
			return nil, err
		}
		if (tick-state.NextTick)%int64(opts.SampleEvery) == 0 || tick == endTick-1 {
			snapshots = append(snapshots, e.snapshot(tick, state, decisions))
		}
		state.NextTick = tick + 1
	}
	return &SimResult{
		StartTick:  startState.NextTick,
		EndTick:    endTick,
		StartState: startState,
		FinalState: state,
		Ticks:      snapshots,
		Events:     events,
	}, nil
}

func (e *Engine) prepareIntents(intents []Intent, start, end int64) (map[int64][]Intent, error) {
	byTick := make(map[int64][]Intent)
	for _, intent := range intents {
		if intent.Tick < start || intent.Tick >= end {
			continue
		}
		if e.project.SkillByID(intent.SkillID) == nil {
			return nil, &Error{
				Code:    CodeInput,
				Path:    "intent.skillId",
				Message: fmt.Sprintf("unknown skill %q", intent.SkillID),
				Tick:    intent.Tick,
				HasTick: true,
				SkillID: intent.SkillID,
			}
		}
		byTick[intent.Tick] = append(byTick[intent.Tick], intent)
	}
	for tick := range byTick {
		sortIntents(e.project, byTick[tick])
	}
	return byTick, nil
}

func sortIntents(project *Project, intents []Intent) {
	sort.Slice(intents, func(i, j int) bool {
		left, right := intents[i], intents[j]
		ls, rs := project.SkillByID(left.SkillID), project.SkillByID(right.SkillID)
		if ls.Priority != rs.Priority {
			return ls.Priority > rs.Priority
		}
		if left.SkillID != right.SkillID {
			return left.SkillID < right.SkillID
		}
		if left.Sequence != right.Sequence {
			return left.Sequence < right.Sequence
		}
		return left.TargetID < right.TargetID
	})
}

func (e *Engine) checkStrictConflicts(byTick map[int64][]Intent, start, end int64) error {
	for tick := start; tick < end; tick++ {
		intents := byTick[tick]
		if len(intents) < 2 {
			continue
		}
		for i := 0; i < len(intents); i++ {
			left := e.project.SkillByID(intents[i].SkillID)
			for j := i + 1; j < len(intents); j++ {
				right := e.project.SkillByID(intents[j].SkillID)
				if left.Priority != right.Priority {
					continue
				}
				conflictType := sharedConflictType(left, right)
				if conflictType == "" {
					continue
				}
				skills := make([]string, 0, 2)
				for _, intent := range intents {
					skills = append(skills, intent.SkillID)
				}
				return &Error{
					Code:         CodeConflict,
					Path:         fmt.Sprintf("ticks[%d]", tick),
					Message:      "equal-priority intents cannot be resolved under strict_priority policy",
					Tick:         tick,
					HasTick:      true,
					ConflictType: conflictType,
					Skills:       uniqueSorted(skills),
				}
			}
		}
	}
	return nil
}

func sharedConflictType(left, right *Skill) string {
	resource := left.PoolID == right.PoolID
	group := left.SharedGroup != "" && left.SharedGroup == right.SharedGroup
	switch {
	case resource && group:
		return "resource+shared_cooldown"
	case resource:
		return "resource"
	case group:
		return "shared_cooldown"
	default:
		return ""
	}
}

func uniqueSorted(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func (e *Engine) resolveTick(tick int64, intents []Intent, state *RuntimeState, effectiveCooldowns []int64, cooldownBase int) ([]Decision, error) {
	if len(intents) == 0 {
		return nil, nil
	}
	groupUsed := make(map[string]string)
	decisions := make([]Decision, 0, len(intents))
	for order, intent := range intents {
		skill := e.project.SkillByID(intent.SkillID)
		conflictType := conflictTypeForIntent(e.project, intents, order)
		runtime := state.Skills[skill.ID]
		poolAmount := state.Pools[skill.PoolID]
		decision := Decision{
			Tick:          tick,
			Order:         order,
			SkillID:       skill.ID,
			TargetID:      intent.TargetID,
			Priority:      skill.Priority,
			ConflictType:  conflictType,
			PoolAfter:     poolAmount,
			CooldownAfter: runtime.CooldownTicks,
		}
		reject := func(reason string) {
			decision.Result = "rejected"
			decision.Reason = reason
			decisions = append(decisions, decision)
		}
		if !skill.Enabled {
			reject("disabled")
			continue
		}
		if runtime.CooldownTicks > 0 || runtime.CastRemainingTicks > 0 {
			reject("cooldown")
			continue
		}
		if skill.SharedGroup != "" {
			if caster, ok := groupUsed[skill.SharedGroup]; ok && caster != skill.ID {
				reject("shared_cooldown")
				continue
			}
		}
		if poolAmount < skill.Cost {
			reject("insufficient_resource")
			continue
		}
		effective := effectiveCooldowns[cooldownBase+e.skillIndex[skill.ID]]
		state.Pools[skill.PoolID] = poolAmount - skill.Cost
		runtime.CooldownTicks = effective
		runtime.CastRemainingTicks = skill.CastTicks
		runtime.CastCount++
		state.Skills[skill.ID] = runtime
		if skill.SharedGroup != "" {
			for _, member := range e.project.SkillsInGroup(skill.SharedGroup) {
				memberRuntime := state.Skills[member.ID]
				if memberRuntime.CooldownTicks < effective {
					memberRuntime.CooldownTicks = effective
				}
				state.Skills[member.ID] = memberRuntime
			}
			groupUsed[skill.SharedGroup] = skill.ID
		}
		decision.Result = "accepted"
		decision.Reason = "cast"
		decision.PoolAfter = state.Pools[skill.PoolID]
		decision.CooldownAfter = state.Skills[skill.ID].CooldownTicks
		decisions = append(decisions, decision)
	}
	return decisions, nil
}

type cooldownResult struct {
	index int
	value int64
	err   error
}

func (e *Engine) precomputeCooldowns(start, end int64, opts SimOptions) ([]int64, []WorkerEvent, error) {
	tickCount := int(end - start)
	skillCount := len(e.project.Skills)
	result := make([]int64, tickCount*skillCount)
	if tickCount == 0 || skillCount == 0 {
		return result, nil, nil
	}
	type job struct {
		index int
		tick  int64
		skill int
	}
	jobs := make([]job, 0, len(result))
	for tick := start; tick < end; tick++ {
		for i := range e.project.Skills {
			index := int(tick-start)*skillCount + i
			jobs = append(jobs, job{index: index, tick: tick, skill: i})
		}
	}
	compute := func(item job) (int64, error) {
		skill := &e.project.Skills[item.skill]
		base, err := skill.Cooldown.Eval(ExprContext{Tick: item.tick}, e.project.Rounding,
			fmt.Sprintf("skills[%s].cooldown", skill.ID))
		if err != nil {
			return 0, err
		}
		if base < 0 {
			return 0, domainErr(CodeInput, fmt.Sprintf("skills[%s].cooldown", skill.ID),
				"cooldown expression resolved to a negative value")
		}
		return EffectiveCooldown(base, skill.CDRBp, e.project.Rounding,
			fmt.Sprintf("skills[%s].cdrBp", skill.ID))
	}
	if opts.Workers == 1 {
		for _, item := range jobs {
			value, err := compute(item)
			if err != nil {
				return nil, nil, err
			}
			result[item.index] = value
		}
		return result, nil, nil
	}
	queueLimit := opts.Workers * opts.BufferLimit
	jobQueue := make(chan job, queueLimit)
	results := make(chan cooldownResult, len(jobs))
	var events []WorkerEvent
	var wg sync.WaitGroup
	for worker := 0; worker < opts.Workers; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for item := range jobQueue {
				func() {
					defer func() {
						if recovered := recover(); recovered != nil {
							results <- cooldownResult{
								index: item.index,
								err:   domainErr(CodeInternal, "worker", fmt.Sprintf("worker panic: %v", recovered)),
							}
						}
					}()
					value, err := compute(item)
					results <- cooldownResult{index: item.index, value: value, err: err}
				}()
			}
		}()
	}
	for _, item := range jobs {
		select {
		case jobQueue <- item:
		default:
			events = append(events, WorkerEvent{
				Tick:   item.tick,
				Kind:   "cooldown_queue",
				Reason: "worker buffer full; producer blocked instead of dropping work",
			})
			jobQueue <- item
		}
	}
	close(jobQueue)
	wg.Wait()
	close(results)
	var firstErr error
	for item := range results {
		if item.err != nil && firstErr == nil {
			firstErr = item.err
		}
		if item.index >= 0 && item.index < len(result) {
			result[item.index] = item.value
		}
	}
	if firstErr != nil {
		return nil, events, firstErr
	}
	return result, events, nil
}

func conflictTypeForIntent(project *Project, intents []Intent, intentIndex int) string {
	skill := project.SkillByID(intents[intentIndex].SkillID)
	resource := false
	group := false
	for i, other := range intents {
		if i == intentIndex {
			continue
		}
		otherSkill := project.SkillByID(other.SkillID)
		if skill.PoolID == otherSkill.PoolID {
			resource = true
		}
		if skill.SharedGroup != "" && skill.SharedGroup == otherSkill.SharedGroup {
			group = true
		}
	}
	switch {
	case resource && group:
		return "resource+shared_cooldown"
	case resource:
		return "resource"
	case group:
		return "shared_cooldown"
	default:
		return "none"
	}
}

func (e *Engine) snapshot(tick int64, state *RuntimeState, decisions []Decision) TickSnapshot {
	snapshot := TickSnapshot{
		Tick:      tick,
		Pools:     make([]PoolSnapshot, 0, len(e.project.Pools)),
		Skills:    make([]SkillSnapshot, 0, len(e.project.Skills)),
		Decisions: append([]Decision(nil), decisions...),
	}
	for i := range e.project.Pools {
		pool := &e.project.Pools[i]
		snapshot.Pools = append(snapshot.Pools, PoolSnapshot{
			ID:     pool.ID,
			Amount: state.Pools[pool.ID],
			Max:    pool.Max,
		})
	}
	for i := range e.project.Skills {
		skill := &e.project.Skills[i]
		runtime := state.Skills[skill.ID]
		timersReady := runtime.CooldownTicks == 0 && runtime.CastRemainingTicks == 0
		affordable := state.Pools[skill.PoolID] >= skill.Cost
		snapshot.Skills = append(snapshot.Skills, SkillSnapshot{
			ID:                 skill.ID,
			Enabled:            skill.Enabled,
			TimersReady:        timersReady,
			Affordable:         affordable,
			Available:          skill.Enabled && timersReady && affordable,
			CooldownTicks:      runtime.CooldownTicks,
			CastRemainingTicks: runtime.CastRemainingTicks,
		})
	}
	return snapshot
}

type regenResult struct {
	index int
	value int64
	err   error
}

func (e *Engine) precomputeRegen(start, end int64, opts SimOptions) ([]int64, []WorkerEvent, error) {
	tickCount := int(end - start)
	poolCount := len(e.project.Pools)
	result := make([]int64, tickCount*poolCount)
	if tickCount == 0 || poolCount == 0 {
		return result, nil, nil
	}
	type job struct {
		index int
		tick  int64
		pool  int
	}
	jobs := make([]job, 0, len(result))
	for tick := start; tick < end; tick++ {
		for i := range e.project.Pools {
			index := int(tick-start)*poolCount + i
			jobs = append(jobs, job{index: index, tick: tick, pool: i})
		}
	}
	if opts.Workers == 1 {
		for _, item := range jobs {
			pool := &e.project.Pools[item.pool]
			value, err := pool.Regen.Eval(ExprContext{Tick: item.tick}, e.project.Rounding,
				fmt.Sprintf("pools[%s].regen", pool.ID))
			if err != nil {
				return nil, nil, err
			}
			result[item.index] = value
		}
		return result, nil, nil
	}
	queueLimit := opts.Workers * opts.BufferLimit
	jobQueue := make(chan job, queueLimit)
	results := make(chan regenResult, len(jobs))
	var events []WorkerEvent
	var wg sync.WaitGroup
	for worker := 0; worker < opts.Workers; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for item := range jobQueue {
				func() {
					defer func() {
						if recovered := recover(); recovered != nil {
							results <- regenResult{
								index: item.index,
								err:   domainErr(CodeInternal, "worker", fmt.Sprintf("worker panic: %v", recovered)),
							}
						}
					}()
					pool := &e.project.Pools[item.pool]
					value, err := pool.Regen.Eval(ExprContext{Tick: item.tick}, e.project.Rounding,
						fmt.Sprintf("pools[%s].regen", pool.ID))
					results <- regenResult{index: item.index, value: value, err: err}
				}()
			}
		}()
	}
	for _, item := range jobs {
		select {
		case jobQueue <- item:
		default:
			events = append(events, WorkerEvent{
				Tick:   item.tick,
				PoolID: e.project.Pools[item.pool].ID,
				Kind:   "queue",
				Reason: "worker buffer full; producer blocked instead of dropping work",
			})
			jobQueue <- item
		}
	}
	close(jobQueue)
	wg.Wait()
	close(results)
	var firstErr error
	for item := range results {
		if item.err != nil && firstErr == nil {
			firstErr = item.err
		}
		if item.index >= 0 && item.index < len(result) {
			result[item.index] = item.value
		}
	}
	if firstErr != nil {
		return nil, events, firstErr
	}
	return result, events, nil
}

func DecisionDigest(decisions []Decision) string {
	data, _ := json.Marshal(decisions)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
