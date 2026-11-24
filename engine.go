package syncgame

import (
	"fmt"
	"sort"
	"sync"
)

type Engine struct {
	sample *Sample
	skills map[string]map[string]SkillDef
}

func NewEngine(sample *Sample) (*Engine, error) {
	if sample == nil {
		return nil, tickErr(0, "*", "sample", "sample is nil")
	}
	if err := ValidateSample(sample); err != nil {
		return nil, err
	}
	skills := make(map[string]map[string]SkillDef, len(sample.Units))
	for _, u := range sample.Units {
		m := make(map[string]SkillDef, len(u.Skills))
		for _, skill := range u.Skills {
			m[skill.ID] = skill
		}
		skills[u.PlayerID] = m
	}
	return &Engine{sample: sample, skills: skills}, nil
}

func (e *Engine) InitialState() WorldState {
	return e.sample.InitialState()
}

// Step advances exactly one tick and consumes the intent whose Tick equals
// state.Tick. It is the serial reference implementation.
func (e *Engine) Step(state WorldState, intent TickIntent) (WorldState, error) {
	return e.step(state, intent, 1)
}

// StepParallel uses bounded parallel phases. The result is bit-for-bit equal to
// Step because every phase writes by canonical index and all reads come from a
// stable snapshot.
func (e *Engine) StepParallel(state WorldState, intent TickIntent, workers int) (WorldState, error) {
	if workers < 1 {
		workers = 1
	}
	return e.step(state, intent, workers)
}

type unitActionKind int

const (
	actionNone unitActionKind = iota
	actionMove
	actionGather
	actionItem
)

type preparedAction struct {
	kind   unitActionKind
	input  Intent
	valid  bool
	amount int
	item   string
}

type attackPlan struct {
	attacker string
	target   string
	skill    SkillDef
	damage   int
	heal     int
	valid    bool
	input    Intent
}

func (e *Engine) step(state WorldState, intent TickIntent, workers int) (WorldState, error) {
	if state.Tick != intent.Tick {
		return WorldState{}, tickErr(intent.Tick, "*", "tick",
			fmt.Sprintf("state tick %d cannot consume intent tick %d", state.Tick, intent.Tick))
	}
	out := CloneWorld(state)
	out.Tick = state.Tick + 1
	if state.Finished {
		return out, nil
	}

	index := e.indexUnits(out)
	tick := state.Tick

	// Phase 1: per-unit cooldown and respawn state depend only on that unit.
	parallelFor(len(out.Units), workers, func(i int) {
		u := &out.Units[i]
		for skillID, cd := range u.SkillCooldown {
			if cd > 0 {
				u.SkillCooldown[skillID] = cd - 1
			}
		}
		if !u.Alive {
			if u.RespawnAt > 0 && tick >= u.RespawnAt {
				seed, _ := e.sample.Unit(u.PlayerID)
				u.X = seed.SpawnX
				u.Y = seed.SpawnY
				u.Direction = seed.Direction
				u.Health = maxInt(1, u.MaxHealth/2)
				u.Alive = true
				u.RespawnAt = 0
			}
			return
		}
		if u.Health > u.MaxHealth {
			u.Health = u.MaxHealth
		}
	})

	inputs := append([]Intent(nil), intent.Inputs...)
	sort.SliceStable(inputs, func(i, j int) bool {
		if inputs[i].PlayerID == inputs[j].PlayerID {
			return inputs[i].Kind < inputs[j].Kind
		}
		return inputs[i].PlayerID < inputs[j].PlayerID
	})

	// Phase 2: movement, gathering and item use are self-only operations and
	// therefore parallelizable.
	actions := make([]preparedAction, len(inputs))
	parallelFor(len(inputs), workers, func(i int) {
		in := inputs[i]
		u, ok := index[in.PlayerID]
		if !ok || !out.Units[u].Alive {
			return
		}
		switch in.Kind {
		case IntentMove:
			actions[i] = preparedAction{kind: actionMove, input: in, valid: true}
		case IntentGather:
			actions[i] = preparedAction{
				kind:   actionGather,
				input:  in,
				valid:  true,
				amount: deriveRange(e.sample.Seed, tick, in.PlayerID, "gather", 3, 7),
			}
		case IntentItem:
			actions[i] = prepareItem(in)
		}
	})
	for i := range inputs {
		a := actions[i]
		if !a.valid || a.kind == actionNone || a.kind == actionItem {
			continue
		}
		u := &out.Units[index[a.input.PlayerID]]
		switch a.kind {
		case actionMove:
			speed := e.unitMoveSpeed(a.input.PlayerID)
			dx := clampInt(a.input.DX, -speed, speed)
			dy := clampInt(a.input.DY, -speed, speed)
			u.X = clampInt(u.X+dx, 0, e.sample.Arena.Width)
			u.Y = clampInt(u.Y+dy, 0, e.sample.Arena.Height)
			if dx != 0 || dy != 0 {
				u.Direction = cardinalDirection(dx, dy)
			}
		case actionGather:
			u.Resource = minInt(1000000, u.Resource+a.amount)
		}
	}

	// Items mutate the unit and resource. Applying them in canonical player
	// order keeps the same result without sharing a write target.
	for i := range inputs {
		a := actions[i]
		if !a.valid || a.kind != actionItem {
			continue
		}
		u := &out.Units[index[a.input.PlayerID]]
		cost := itemCost(a.item)
		if u.Resource < cost {
			continue
		}
		switch a.item {
		case "medkit":
			u.Resource -= cost
			u.Health = minInt(u.MaxHealth, u.Health+30)
		case "energy_cell":
			u.Resource -= cost
			u.Health = minInt(u.MaxHealth, u.Health+12)
			u.Resource = minInt(1000000, u.Resource+4)
		}
	}

	// Phase 3: skill validation and damage are computed against the same
	// post-movement snapshot, in canonical player order.
	plans := make([]attackPlan, len(inputs))
	parallelFor(len(inputs), workers, func(i int) {
		in := inputs[i]
		if in.Kind != IntentSkill {
			return
		}
		attackerIdx, ok := index[in.PlayerID]
		if !ok || !out.Units[attackerIdx].Alive {
			return
		}
		skill, ok := e.skills[in.PlayerID][in.Skill]
		if !ok {
			return
		}
		attacker := out.Units[attackerIdx]
		if attacker.Resource < skill.Cost || attacker.SkillCooldown[skill.ID] > 0 {
			return
		}
		plan := attackPlan{attacker: in.PlayerID, skill: skill, input: in, valid: true}
		if skill.Damage > 0 {
			if in.TargetID == "" {
				return
			}
			targetIdx, exists := index[in.TargetID]
			if !exists || !out.Units[targetIdx].Alive {
				return
			}
			target := out.Units[targetIdx]
			if squaredDistance(attacker, target) > skill.Range*skill.Range {
				return
			}
			base := skill.Damage
			plan.damage = base * deriveRange(e.sample.Seed, tick, in.PlayerID, "skill:"+skill.ID, 80, 100) / 100
			if plan.damage < 1 {
				plan.damage = 1
			}
			plan.target = in.TargetID
		}
		if skill.Heal > 0 {
			plan.heal = skill.Heal
		}
		plans[i] = plan
	})

	for i := range inputs {
		plan := plans[i]
		if !plan.valid {
			continue
		}
		attackerIdx := index[plan.attacker]
		attacker := &out.Units[attackerIdx]
		if !attacker.Alive || attacker.Resource < plan.skill.Cost ||
			attacker.SkillCooldown[plan.skill.ID] > 0 {
			continue
		}
		if plan.damage > 0 {
			targetIdx, ok := index[plan.target]
			if !ok || !out.Units[targetIdx].Alive {
				continue
			}
		}
		attacker.Resource -= plan.skill.Cost
		attacker.SkillCooldown[plan.skill.ID] = plan.skill.CooldownTicks
		if plan.heal > 0 {
			attacker.Health = minInt(attacker.MaxHealth, attacker.Health+plan.heal)
		}
		if plan.damage <= 0 {
			continue
		}
		targetIdx := index[plan.target]
		target := &out.Units[targetIdx]
		if target.Health > plan.damage {
			target.Health -= plan.damage
			continue
		}
		target.Health = 0
		target.Alive = false
		seed, _ := e.sample.Unit(target.PlayerID)
		target.RespawnAt = tick + 1 + seed.RespawnTicks
		attacker.Kills++
		attacker.Resource = minInt(1000000, attacker.Resource+12)
	}

	e.resolveVictory(&out, tick+1)
	return out, nil
}

func (e *Engine) indexUnits(state WorldState) map[string]int {
	idx := make(map[string]int, len(state.Units))
	for i := range state.Units {
		idx[state.Units[i].PlayerID] = i
	}
	return idx
}

func (e *Engine) unitMoveSpeed(playerID string) int {
	seed, ok := e.sample.Unit(playerID)
	if !ok {
		return 0
	}
	return seed.MoveSpeed
}

func (e *Engine) resolveVictory(state *WorldState, nextTick int) {
	alive := make([]UnitState, 0, len(state.Units))
	for _, u := range state.Units {
		if u.Alive {
			alive = append(alive, u)
		}
	}
	if len(alive) == 1 && len(state.Units) > 1 {
		state.Finished = true
		state.Winner = alive[0].PlayerID
		state.LockedTick = nextTick
		return
	}

	var winners []UnitState
	for _, u := range alive {
		if u.Resource >= e.sample.ResourceGoal {
			winners = append(winners, u)
		}
	}
	if len(winners) > 0 {
		sort.Slice(winners, func(i, j int) bool {
			if winners[i].Resource != winners[j].Resource {
				return winners[i].Resource > winners[j].Resource
			}
			return winners[i].PlayerID < winners[j].PlayerID
		})
		state.Finished = true
		state.Winner = winners[0].PlayerID
		state.LockedTick = nextTick
		return
	}

	if nextTick >= e.sample.MaxTicks && len(alive) > 0 {
		sort.Slice(alive, func(i, j int) bool {
			if alive[i].Resource != alive[j].Resource {
				return alive[i].Resource > alive[j].Resource
			}
			if alive[i].Health != alive[j].Health {
				return alive[i].Health > alive[j].Health
			}
			return alive[i].PlayerID < alive[j].PlayerID
		})
		state.Finished = true
		state.Winner = alive[0].PlayerID
		state.LockedTick = nextTick
	}
}

func (e *Engine) Settlement(state WorldState) Settlement {
	players := make([]PlayerResult, 0, len(state.Units))
	for _, u := range state.Units {
		players = append(players, PlayerResult{
			PlayerID: u.PlayerID,
			Health:   u.Health,
			Resource: u.Resource,
			Alive:    u.Alive,
			Kills:    u.Kills,
		})
	}
	return Settlement{
		LockedTick: state.LockedTick,
		Winner:     state.Winner,
		Reason:     "locked",
		Players:    players,
	}
}

func prepareItem(in Intent) preparedAction {
	switch in.Item {
	case "medkit", "energy_cell":
		return preparedAction{kind: actionItem, input: in, valid: true, item: in.Item}
	default:
		return preparedAction{}
	}
}

func itemCost(item string) int {
	switch item {
	case "medkit":
		return 8
	case "energy_cell":
		return 4
	default:
		return 0
	}
}

func squaredDistance(a, b UnitState) int {
	dx := a.X - b.X
	dy := a.Y - b.Y
	return dx*dx + dy*dy
}

func cardinalDirection(dx, dy int) int {
	// Degrees counter-clockwise from east, using only exact integer cardinal
	// and diagonal directions.
	switch {
	case dx > 0 && dy == 0:
		return 0
	case dx > 0 && dy < 0:
		return 45
	case dx == 0 && dy < 0:
		return 90
	case dx < 0 && dy < 0:
		return 135
	case dx < 0 && dy == 0:
		return 180
	case dx < 0 && dy > 0:
		return 225
	case dx == 0 && dy > 0:
		return 270
	case dx > 0 && dy > 0:
		return 315
	default:
		return 0
	}
}

func clampInt(v, low, high int) int {
	if v < low {
		return low
	}
	if v > high {
		return high
	}
	return v
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func parallelFor(n, workers int, fn func(int)) {
	if n <= 0 {
		return
	}
	if workers <= 1 || n == 1 {
		for i := 0; i < n; i++ {
			fn(i)
		}
		return
	}
	if workers > n {
		workers = n
	}
	var wg sync.WaitGroup
	chunk := (n + workers - 1) / workers
	for start := 0; start < n; start += chunk {
		end := start + chunk
		if end > n {
			end = n
		}
		wg.Add(1)
		go func(start, end int) {
			defer wg.Done()
			for i := start; i < end; i++ {
				fn(i)
			}
		}(start, end)
	}
	wg.Wait()
}
