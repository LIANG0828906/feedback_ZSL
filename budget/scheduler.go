package budget

import (
	"fmt"
	"sort"
)

type compiledRules struct {
	regen BP
	cdr   map[string]int64
}

type Simulation struct {
	project  *Project
	params   Params
	hash     string
	compiled compiledRules

	nextTick     int
	resource     BP
	cooldowns    map[string]int64
	groupCD      map[string]int64
	intentOffset int

	states  []TickState
	records [][]ArbitrationRecord
}

func CompileRules(project *Project) (compiledRules, error) {
	compiled := compiledRules{cdr: map[string]int64{}}
	regenNode := project.Resource.RegenNode
	if regenNode == "" {
		regenNode = "resource:" + project.Resource.ID + ":regen"
	}
	var regenValue int64
	var err error
	if project.Resource.RegenExpr != nil {
		regenValue, err = Eval(project.Resource.RegenExpr, project.Resource.RegenExpr.Vars, "resource.regen_expression")
		if err != nil {
			return compiledRules{}, err
		}
	} else {
		regenValue = int64(project.Resource.RegenFlat)
	}
	argMap := map[string]int64{"regen": regenValue}
	if err := ValidateRegenBoundary(regenNode, BP(regenValue), argMap); err != nil {
		return compiledRules{}, err
	}
	compiled.regen = BP(regenValue)

	for _, skill := range project.Skills {
		node := skill.CDRNode
		if node == "" {
			node = "cdr:" + skill.ID
		}
		var cdr int64
		if skill.CDRExpr != nil {
			cdr, err = Eval(skill.CDRExpr, skill.CDRExpr.Vars, "skills."+skill.ID+".cdr_expression")
			if err != nil {
				return compiledRules{}, err
			}
		} else {
			cdr = skill.CDRFlat
		}
		if cdr < 0 || cdr > MaxCDRBasisPoints {
			return compiledRules{}, &Error{
				Code:    ECodeEval,
				Message: "cooldown reduction coefficient outside declared [0,10000] boundary",
				Node:    node, SkillID: skill.ID, Field: "skills.cdr",
				Arguments: map[string]int64{"cdr_bp": cdr},
			}
		}
		compiled.cdr[skill.ID] = cdr
	}
	return compiled, nil
}

func NewSimulation(project *Project, params Params) (*Simulation, error) {
	if params.ParamVersion == "" {
		params.ParamVersion = DefaultParamVersion
	}
	if params.FailTick == 0 && params.FailSkillID == "" {
		params.FailTick = -1
	}
	compiled, err := CompileRules(project)
	if err != nil {
		return nil, err
	}
	hash := project.ContentHash
	if hash == "" {
		hash, err = project.SemanticHash()
		if err != nil {
			return nil, err
		}
		project.ContentHash = hash
	}
	sim := &Simulation{
		project: project, params: params, hash: hash, compiled: compiled,
		nextTick:  0,
		resource:  project.Resource.Initial,
		cooldowns: map[string]int64{},
		groupCD:   map[string]int64{},
	}
	for _, skill := range project.Skills {
		sim.cooldowns[skill.ID] = 0
	}
	for _, group := range project.Groups {
		sim.groupCD[group.ID] = 0
	}
	return sim, nil
}

func ReferenceSchedule(project *Project, params Params) (*RunReport, error) {
	sim, err := NewSimulation(project, params)
	if err != nil {
		return nil, err
	}
	for !sim.Done() {
		if _, _, err := sim.Step(); err != nil {
			return nil, err
		}
	}
	return sim.Report(), nil
}

func (s *Simulation) Done() bool        { return s.nextTick >= s.project.TickCount }
func (s *Simulation) NextTick() int     { return s.nextTick }
func (s *Simulation) IntentOffset() int { return s.intentOffset }
func (s *Simulation) Resource() BP      { return s.resource }

// Step advances exactly one integer tick. It is safe for chunked/checkpoint
// callers because all mutable state lives on Simulation or a checkpoint.
func (s *Simulation) Step() (TickState, []ArbitrationRecord, error) {
	if s.Done() {
		return TickState{}, nil, newError(ECodeStage, "simulation is already complete")
	}
	tick := s.nextTick
	depSet := map[string]bool{}
	addDep := func(dep string) {
		if dep != "" {
			depSet[dep] = true
		}
	}
	if tick > 0 {
		addDep(fmt.Sprintf("tick:%d", tick-1))
	} else {
		addDep(fmt.Sprintf("resource:%s:initial", s.project.Resource.ID))
	}

	// Resource accumulation happens before casts and is capped at Max.
	addDep(s.project.Resource.RegenNode)
	addDep(fmt.Sprintf("resource:%s:max", s.project.Resource.ID))
	beforeRegen := s.resource
	s.resource += s.compiled.regen
	if s.resource > s.project.Resource.Max {
		s.resource = s.project.Resource.Max
	}
	regenApplied := s.resource - beforeRegen

	// Cooldowns and shared-group cooldowns advance by one tick toward zero.
	for _, skill := range s.project.Skills {
		if s.cooldowns[skill.ID] > 0 {
			s.cooldowns[skill.ID]--
		}
	}
	for _, group := range s.project.Groups {
		if s.groupCD[group.ID] > 0 {
			s.groupCD[group.ID]--
		}
	}
	preCastable := map[string]bool{}
	for _, skill := range s.project.Skills {
		groupCD := int64(0)
		if skill.GroupID != "" {
			groupCD = s.groupCD[skill.GroupID]
		}
		preCastable[skill.ID] = s.cooldowns[skill.ID] == 0 &&
			groupCD == 0 && s.resource >= skill.Cost
	}

	tickIntents := s.intentsForTick(tick)
	ordered, arbErr := OrderTickIntents(tickIntents, s.project, s.params)
	if arbErr != nil {
		return TickState{}, nil, arbErr
	}

	records := make([]ArbitrationRecord, 0, len(ordered))
	casts := make([]CastRecord, 0, len(ordered))
	spent := BP(0)

	for _, item := range ordered {
		intent := item.Intent
		skill := item.Skill
		addDep(fmt.Sprintf("skill:%s:priority", skill.ID))
		addDep(fmt.Sprintf("skill:%s:cost", skill.ID))

		if s.params.FailTick == tick && s.params.FailSkillID == skill.ID {
			err := &Error{
				Code: ECodeStage, Message: "injected single-window settlement failure",
				Tick: &tick, SkillID: skill.ID,
				Field: fmt.Sprintf("tick[%d].skill[%s]", tick, skill.ID),
				Basis: "fault_injection=fail_before_settlement",
			}
			return TickState{}, nil, err
		}

		contention := contentionFor(item, s.project, tickIntents)
		reason := "ok"
		if s.cooldowns[skill.ID] > 0 {
			reason = "skill_cooldown"
		} else if skill.GroupID != "" && s.groupCD[skill.GroupID] > 0 {
			reason = "group_cooldown"
		} else if s.resource < skill.Cost {
			reason = "insufficient_resource"
		}

		if reason != "ok" {
			record := ArbitrationRecord{
				Tick: tick, IntentID: intent.IntentID, SkillID: skill.ID,
				Priority: skill.Priority, Ordinal: intent.Ordinal,
				Contention: contention, Decision: "rejected", Reason: reason,
				Basis: basisFor(item,
					fmt.Sprintf("resource=%d", s.resource),
					fmt.Sprintf("cost=%d", skill.Cost),
					fmt.Sprintf("skill_cd=%d", s.cooldowns[skill.ID]),
					fmt.Sprintf("group_cd=%d", s.groupCD[skill.GroupID]),
				),
			}
			records = append(records, record)
			continue
		}

		cdr := s.compiled.cdr[skill.ID]
		effectiveCD, err := EffectiveCooldown(skill.CooldownTicks, cdr, fmt.Sprintf("skills.%s.cooldown", skill.ID))
		if err != nil {
			be, _ := AsError(err)
			if be != nil {
				be.Tick = &tick
				be.SkillID = skill.ID
			}
			return TickState{}, nil, err
		}

		s.resource -= skill.Cost
		spent += skill.Cost
		s.cooldowns[skill.ID] = effectiveCD
		if skill.GroupID != "" {
			group, ok := s.project.groupByID(skill.GroupID)
			if !ok {
				return TickState{}, nil, &Error{
					Code: ECodeSchema, Message: "skill references unknown cooldown group",
					Tick: &tick, SkillID: skill.ID, Field: "skills.shared_cooldown_group",
				}
			}
			s.groupCD[skill.GroupID] = group.CooldownTicks
			addDep(fmt.Sprintf("group:%s", group.ID))
		}
		addDep(fmt.Sprintf("skill:%s:cooldown", skill.ID))
		cdrNode := skill.CDRNode
		if cdrNode == "" {
			cdrNode = "cdr:" + skill.ID
		}
		addDep(cdrNode)

		casts = append(casts, CastRecord{
			IntentID: intent.IntentID, SkillID: skill.ID, Accepted: true,
			CostPaid: skill.Cost, EffectiveCooldown: effectiveCD,
		})
		records = append(records, ArbitrationRecord{
			Tick: tick, IntentID: intent.IntentID, SkillID: skill.ID,
			Priority: skill.Priority, Ordinal: intent.Ordinal,
			Contention: contention, Decision: "accepted", Reason: "cast_settled",
			Basis: basisFor(item,
				fmt.Sprintf("paid=%d", skill.Cost),
				fmt.Sprintf("effective_cd=%d", effectiveCD),
				fmt.Sprintf("resource_after=%d", s.resource),
			),
			Winners: []string{skill.ID},
		})
	}

	s.intentOffset += len(tickIntents)
	s.nextTick++

	state := s.snapshot(tick, regenApplied, spent, casts, s.intentOffset, depSet, preCastable)
	s.states = append(s.states, state)
	s.records = append(s.records, records)
	return state, records, nil
}

func (s *Simulation) intentsForTick(tick int) []Intent {
	var out []Intent
	for _, intent := range s.project.Intents {
		if intent.Tick == tick {
			out = append(out, intent)
		}
	}
	return out
}

func (s *Simulation) snapshot(tick int, regen, spent BP, casts []CastRecord, offset int, deps map[string]bool, preCastable map[string]bool) TickState {
	ids := sortedSkillIDs(s.project.Skills)
	skillStates := make([]SkillState, 0, len(ids))
	for _, id := range ids {
		skill, _ := s.project.skillByID(id)
		cd := s.cooldowns[id]
		groupCD := int64(0)
		if skill.GroupID != "" {
			groupCD = s.groupCD[skill.GroupID]
		}
		available := cd == 0 && groupCD == 0 && s.resource >= skill.Cost
		skillStates = append(skillStates, SkillState{
			SkillID: id, Available: available,
			CastableBefore: preCastable[id], CooldownRemaining: cd,
		})
	}

	groupIDs := make([]string, 0, len(s.project.Groups))
	for _, group := range s.project.Groups {
		groupIDs = append(groupIDs, group.ID)
	}
	sort.Strings(groupIDs)
	groupStates := make([]GroupState, 0, len(groupIDs))
	for _, id := range groupIDs {
		groupStates = append(groupStates, GroupState{GroupID: id, CooldownRemaining: s.groupCD[id]})
	}

	dependencies := make([]string, 0, len(deps))
	for dep := range deps {
		dependencies = append(dependencies, dep)
	}
	sort.Strings(dependencies)

	return TickState{
		Tick: tick, ResourceCurrent: s.resource, ResourceMax: s.project.Resource.Max,
		RegenApplied: regen, Spent: spent, Skills: skillStates, Groups: groupStates,
		Casts: casts, ConsumedIntentOffset: offset, Dependencies: dependencies,
	}
}

func (s *Simulation) Report() *RunReport {
	ticks := append([]TickState(nil), s.states...)
	arb := make([][]ArbitrationRecord, len(s.records))
	for i := range s.records {
		arb[i] = append([]ArbitrationRecord(nil), s.records[i]...)
	}
	return &RunReport{
		SourceVersion:    s.project.SourceVersion,
		ContentHash:      s.hash,
		AlgorithmVersion: AlgorithmVersion,
		ParamVersion:     s.params.ParamVersion,
		Ticks:            ticks, Arbitration: arb,
	}
}

func RestoreSimulation(project *Project, params Params, cp *Checkpoint) (*Simulation, error) {
	if cp == nil {
		return nil, newError(ECodeDecode, "nil checkpoint")
	}
	sim, err := NewSimulation(project, params)
	if err != nil {
		return nil, err
	}
	sim.nextTick = cp.LastTick + 1
	sim.resource = cp.ResourceCurrent
	sim.intentOffset = cp.IntentOffset
	for _, state := range cp.SkillStates {
		if _, ok := sim.cooldowns[state.SkillID]; ok {
			sim.cooldowns[state.SkillID] = state.CooldownRemaining
		}
	}
	for _, state := range cp.GroupStates {
		if _, ok := sim.groupCD[state.GroupID]; ok {
			sim.groupCD[state.GroupID] = state.CooldownRemaining
		}
	}
	if cp.WindowStates != nil {
		sim.states = append(sim.states, cp.WindowStates...)
	}
	if cp.WindowArbitration != nil {
		sim.records = append(sim.records, cp.WindowArbitration...)
	}
	return sim, nil
}
