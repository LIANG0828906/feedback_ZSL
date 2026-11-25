package engine

import (
	"fmt"
	"sort"

	"chaosdiag/model"
)

func Schedule(p *model.Project) []model.TickSchedule {
	maxTick := 0
	for _, in := range p.Instructions {
		if in.EndTick > maxTick {
			maxTick = in.EndTick
		}
	}
	for _, req := range p.Requests {
		if req.Tick > maxTick {
			maxTick = req.Tick
		}
	}
	return ScheduleThrough(p, maxTick)
}

func ScheduleThrough(p *model.Project, throughTick int) []model.TickSchedule {
	active := map[string]bool{}
	usedBudget := int64(0)
	queue := []model.QueueItem{}
	out := []model.TickSchedule{}

	for tick := 1; tick <= throughTick; tick++ {
		schedule := model.TickSchedule{Tick: tick}

		released := []string{}
		for id := range active {
			ins := FindInstruction(p, id)
			if ins != nil && ins.EndTick < tick {
				released = append(released, id)
			}
		}
		sort.Strings(released)
		for _, id := range released {
			ins := FindInstruction(p, id)
			if ins != nil {
				usedBudget -= ins.BudgetCostMS
			}
			delete(active, id)
		}
		schedule.Released = released

		nextQueue := []model.QueueItem{}
		for _, item := range queue {
			ins := FindInstruction(p, item.InstructionID)
			if ins == nil {
				continue
			}
			violations := Violations(p, *ins, active, usedBudget)
			if len(violations) == 0 {
				active[ins.ID] = true
				usedBudget += ins.BudgetCostMS
				schedule.Admitted = append(schedule.Admitted, ins.ID)
				continue
			}
			item.ViolatedConstraints = violations
			item.ConstraintID = violations[0]
			nextQueue = append(nextQueue, item)
		}
		queue = nextQueue

		submitted := make([]model.Instruction, 0)
		for _, ins := range p.Instructions {
			if ins.SubmittedTick == tick {
				submitted = append(submitted, ins)
			}
		}
		sort.SliceStable(submitted, func(i, j int) bool { return submitted[i].Sequence < submitted[j].Sequence })
		if len(submitted) > 0 {
			schedule.Submitted = InstructionIDs(submitted)
		}
		for _, ins := range submitted {
			if active[ins.ID] {
				continue
			}
			violations := Violations(p, ins, active, usedBudget)
			if len(violations) == 0 {
				active[ins.ID] = true
				usedBudget += ins.BudgetCostMS
				schedule.Admitted = append(schedule.Admitted, ins.ID)
			} else {
				queue = append(queue, model.QueueItem{
					InstructionID: ins.ID, Sequence: ins.Sequence, SinceTick: tick,
					ConstraintID: violations[0], ViolatedConstraints: violations,
					Reason: "instruction exceeds deterministic scheduling guardrails",
				})
			}
		}

		sort.SliceStable(queue, func(i, j int) bool { return queue[i].Sequence < queue[j].Sequence })
		for i := range queue {
			queue[i].Position = i + 1
		}
		schedule.Queued = append(schedule.Queued, queue...)

		activeIDs := make([]string, 0, len(active))
		for id := range active {
			activeIDs = append(activeIDs, id)
		}
		sort.Strings(activeIDs)
		schedule.Active = activeIDs
		out = append(out, schedule)
	}
	return out
}

func Violations(p *model.Project, ins model.Instruction, active map[string]bool, usedBudget int64) []string {
	violations := []string{}
	targetService := TargetService(p, ins)
	serviceActive := 0
	globalActive := 0
	activeIDs := make([]string, 0, len(active))
	for id := range active {
		activeIDs = append(activeIDs, id)
	}
	sort.Strings(activeIDs)
	for _, id := range activeIDs {
		globalActive++
		other := FindInstruction(p, id)
		if other != nil && TargetService(p, *other) == targetService {
			serviceActive++
		}
	}
	if cap := p.ServiceConcurrency(targetService); serviceActive >= cap {
		violations = append(violations, fmt.Sprintf("CONCURRENCY-SERVICE:%s", targetService))
	}
	if globalActive >= p.Limits.GlobalConcurrent {
		violations = append(violations, "CONCURRENCY-GLOBAL")
	}
	if usedBudget+ins.BudgetCostMS > p.AdmissionTimeoutBudgetMS {
		violations = append(violations, "TIMEOUT-BUDGET-ADMISSION")
	}
	return violations
}

func TargetService(p *model.Project, ins model.Instruction) string {
	if ins.Selector.Kind == model.TargetService {
		return ins.Selector.Service
	}
	if ins.Selector.EdgeID != "" {
		if edge, ok := p.EdgeByID(ins.Selector.EdgeID); ok {
			return edge.To
		}
	}
	return ins.Selector.To
}

func FindInstruction(p *model.Project, id string) *model.Instruction {
	for i := range p.Instructions {
		if p.Instructions[i].ID == id {
			return &p.Instructions[i]
		}
	}
	return nil
}

func ActiveMap(schedules []model.TickSchedule) map[int]map[string]bool {
	out := map[int]map[string]bool{}
	for _, s := range schedules {
		m := map[string]bool{}
		for _, id := range s.Active {
			m[id] = true
		}
		out[s.Tick] = m
	}
	return out
}
