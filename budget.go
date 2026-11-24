package balance

import (
	"fmt"
	"sort"
	"strings"
)

type ComboID struct {
	PlanID     string `json:"planId"`
	ScenarioID string `json:"scenarioId"`
}

func (c ComboID) Key() string {
	return c.PlanID + "\x1f" + c.ScenarioID
}

type ReductionRecord struct {
	PlanID      string `json:"planId"`
	ScenarioID  string `json:"scenarioId"`
	Round       int    `json:"round"`
	Rule        string `json:"rule"`
	PolicyRule  string `json:"policyRule"`
	Explanation string `json:"explanation"`
	Requested   int    `json:"requested"`
	Allocated   int    `json:"allocated"`
	Constraint  string `json:"constraint"`
}

type BudgetCombo struct {
	ComboID
	Priority    int
	Requested   int
	Allocated   int
	Reduction   *ReductionRecord
	RoundCounts []int
}

type BudgetPlan struct {
	Combos         []*BudgetCombo
	ByKey          map[string]*BudgetCombo
	RoundCapacity  int
	Capacity       int64
	RequestedTotal int64
	Slots          int
	MemoryBuffer   int
	MaxRounds      int
	Policy         string
}

func BuildBudgetPlan(project *Project) (*BudgetPlan, error) {
	if project == nil {
		return nil, newError(CodeSchema, "project", "project is nil")
	}
	budget := project.Budget
	if budget.Slots <= 0 || budget.MaxMatchesPerRound <= 0 || budget.MemoryBuffer <= 0 ||
		budget.MaxRounds <= 0 || budget.TargetMatchesPerCombo <= 0 {
		return nil, &AppError{
			Code:   CodeBudget,
			Object: "budget",
			Field:  "capacity",
			Detail: "slots, matches per round, memory buffer, rounds, and target samples must all be positive",
		}
	}

	planIndex := make(map[string]int, len(project.Plans))
	for index, plan := range project.Plans {
		planIndex[plan.ID] = index
	}
	scenarioIndex := make(map[string]int, len(project.Scenarios))
	for index, scenario := range project.Scenarios {
		scenarioIndex[scenario.ID] = index
	}

	combos := make([]*BudgetCombo, 0, len(project.Plans)*len(project.Scenarios))
	for _, plan := range project.Plans {
		for _, scenarioID := range plan.TargetScenarios {
			combos = append(combos, &BudgetCombo{
				ComboID:   ComboID{PlanID: plan.ID, ScenarioID: scenarioID},
				Priority:  plan.Priority,
				Requested: budget.TargetMatchesPerCombo,
			})
		}
	}
	if len(combos) == 0 {
		return nil, &AppError{Code: CodeBudget, Object: "budget", Detail: "no plan/scenario combinations to simulate"}
	}
	sort.SliceStable(combos, func(i, j int) bool {
		if combos[i].Priority != combos[j].Priority {
			return combos[i].Priority > combos[j].Priority
		}
		if planIndex[combos[i].PlanID] != planIndex[combos[j].PlanID] {
			return planIndex[combos[i].PlanID] < planIndex[combos[j].PlanID]
		}
		return scenarioIndex[combos[i].ScenarioID] < scenarioIndex[combos[j].ScenarioID]
	})

	perRoundByMemory, ok := safeMulInt64(int64(budget.Slots), int64(budget.MemoryBuffer))
	if !ok {
		return nil, &AppError{Code: CodeBudget, Object: "budget", Field: "memoryBuffer", Detail: "slot*memory capacity overflows"}
	}
	roundCapacity64 := int64(budget.MaxMatchesPerRound)
	if perRoundByMemory < roundCapacity64 {
		roundCapacity64 = perRoundByMemory
	}
	if roundCapacity64 <= 0 {
		return nil, &AppError{Code: CodeBudget, Object: "budget", Field: "capacity", Detail: "effective per-round capacity is zero"}
	}

	capacity, ok := safeMulInt64(int64(budget.MaxRounds), roundCapacity64)
	if !ok {
		return nil, &AppError{Code: CodeBudget, Object: "budget", Field: "maxRounds", Detail: "round capacity overflows"}
	}
	requested, ok := safeMulInt64(int64(len(combos)), int64(budget.TargetMatchesPerCombo))
	if !ok {
		return nil, &AppError{Code: CodeBudget, Object: "budget", Field: "targetMatchesPerCombo", Detail: "requested samples overflow"}
	}
	constraint := "round_capacity"
	if budget.MaxTotalMatches > 0 && int64(budget.MaxTotalMatches) < capacity {
		capacity = int64(budget.MaxTotalMatches)
		constraint = "max_total_matches"
	}
	if capacity > requested {
		capacity = requested
	}
	if capacity < int64(len(combos)) {
		ids := make([]string, 0, len(combos))
		for _, combo := range combos {
			ids = append(ids, combo.PlanID+"/"+combo.ScenarioID)
		}
		return nil, &AppError{
			Code:         CodeBudget,
			Object:       "budget.coverage",
			ConflictRule: budget.PriorityRule,
			Detail:       fmt.Sprintf("capacity %d cannot cover minimum one sample for combinations: %s", capacity, strings.Join(ids, ", ")),
		}
	}

	for _, combo := range combos {
		combo.Allocated = 1
	}
	remaining := capacity - int64(len(combos))
	if budget.PriorityRule == PriorityFirst {
		for _, combo := range combos {
			if remaining == 0 {
				break
			}
			room := int64(combo.Requested - combo.Allocated)
			if room <= 0 {
				continue
			}
			take := room
			if take > remaining {
				take = remaining
			}
			combo.Allocated += int(take)
			remaining -= take
		}
	} else {
		for remaining > 0 {
			progress := false
			for _, combo := range combos {
				if remaining == 0 {
					break
				}
				if combo.Allocated >= combo.Requested {
					continue
				}
				combo.Allocated++
				remaining--
				progress = true
			}
			if !progress {
				break
			}
		}
	}
	if remaining != 0 {
		return nil, &AppError{Code: CodeBudget, Object: "budget", Detail: "allocator left capacity unassigned"}
	}

	plan := &BudgetPlan{
		Combos:         combos,
		ByKey:          make(map[string]*BudgetCombo, len(combos)),
		RoundCapacity:  int(roundCapacity64),
		Capacity:       capacity,
		RequestedTotal: requested,
		Slots:          budget.Slots,
		MemoryBuffer:   budget.MemoryBuffer,
		MaxRounds:      budget.MaxRounds,
		Policy:         budget.PriorityRule,
	}
	reductions := make([]ReductionRecord, 0)
	for _, combo := range combos {
		plan.ByKey[combo.Key()] = combo
		if combo.Allocated >= combo.Requested {
			continue
		}
		reason := "capacity shortfall: higher-priority combinations receive expanded sampling first"
		if budget.PriorityRule == PriorityEqual {
			reason = "capacity shortfall: samples are reduced evenly to preserve combination coverage"
		}
		record := ReductionRecord{
			PlanID:      combo.PlanID,
			ScenarioID:  combo.ScenarioID,
			Round:       0,
			Rule:        budget.PriorityRule,
			PolicyRule:  budget.PriorityRule,
			Explanation: reason,
			Requested:   combo.Requested,
			Allocated:   combo.Allocated,
			Constraint:  constraint,
		}
		combo.Reduction = &record
		reductions = append(reductions, record)
	}
	if err := ValidateBudgetReductions(reductions); err != nil {
		return nil, err
	}
	if err := assignRoundCounts(plan); err != nil {
		return nil, err
	}
	return plan, nil
}

func assignRoundCounts(plan *BudgetPlan) error {
	remaining := make(map[string]int, len(plan.Combos))
	totalRemaining := 0
	for _, combo := range plan.Combos {
		remaining[combo.Key()] = combo.Allocated
		totalRemaining += combo.Allocated
		combo.RoundCounts = make([]int, 0, plan.MaxRounds)
	}
	for round := 0; round < plan.MaxRounds && totalRemaining > 0; round++ {
		addedThisRound := 0
		for pass := 0; pass < plan.RoundCapacity && addedThisRound < plan.RoundCapacity; pass++ {
			progress := false
			for _, combo := range plan.Combos {
				if addedThisRound >= plan.RoundCapacity {
					break
				}
				key := combo.Key()
				if remaining[key] <= 0 {
					continue
				}
				remaining[key]--
				totalRemaining--
				addedThisRound++
				progress = true
			}
			if !progress {
				break
			}
		}
	}
	if totalRemaining != 0 {
		return &AppError{
			Code:         CodeBudget,
			Object:       "budget.schedule",
			ConflictRule: plan.Policy,
			Detail:       fmt.Sprintf("%d allocated samples could not be scheduled within %d rounds", totalRemaining, plan.MaxRounds),
		}
	}

	// The loop above only verified feasibility. Build deterministic per-combo
	// round counts with the same round-robin order used by the scheduler.
	left := make(map[string]int, len(plan.Combos))
	for _, combo := range plan.Combos {
		left[combo.Key()] = combo.Allocated
	}
	for round := 0; round < plan.MaxRounds; round++ {
		addedThisRound := 0
		added := make(map[string]int, len(plan.Combos))
		for addedThisRound < plan.RoundCapacity {
			progress := false
			for _, combo := range plan.Combos {
				if addedThisRound >= plan.RoundCapacity {
					break
				}
				key := combo.Key()
				if left[key] <= 0 {
					continue
				}
				left[key]--
				addedThisRound++
				added[key]++
				progress = true
			}
			if !progress {
				break
			}
		}
		for _, combo := range plan.Combos {
			combo.RoundCounts = append(combo.RoundCounts, added[combo.Key()])
		}
	}
	for _, combo := range plan.Combos {
		total := 0
		for _, count := range combo.RoundCounts {
			total += count
		}
		if total != combo.Allocated {
			return &AppError{Code: CodeBudget, Object: combo.Key(), Field: "schedule", Detail: "round partition does not match allocation"}
		}
	}
	return nil
}

func ValidateBudgetReductions(records []ReductionRecord) error {
	grouped := make(map[string][]ReductionRecord)
	conflicts := make([]string, 0)
	for _, record := range records {
		if record.PlanID == "" || record.ScenarioID == "" || record.Rule == "" || record.Explanation == "" {
			return &AppError{
				Code:         CodeBudget,
				Object:       record.PlanID + "/" + record.ScenarioID,
				PlanID:       record.PlanID,
				ScenarioID:   record.ScenarioID,
				ConflictRule: record.Rule,
				Field:        "reduction.rule",
				Detail:       "a reduction must include an explicit priority rule and explanation",
			}
		}
		key := ComboID{PlanID: record.PlanID, ScenarioID: record.ScenarioID}.Key()
		grouped[key] = append(grouped[key], record)
	}
	keys := make([]string, 0, len(grouped))
	for key := range grouped {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		items := grouped[key]
		sort.Slice(items, func(i, j int) bool { return items[i].Round < items[j].Round })
		for index := 1; index < len(items); index++ {
			previous := items[index-1]
			current := items[index]
			if current.PolicyRule == previous.PolicyRule && current.Rule == previous.Rule &&
				current.Explanation == previous.Explanation {
				conflicts = append(conflicts, current.PlanID+"/"+current.ScenarioID+"@"+current.Rule)
				return &AppError{
					Code:         CodeBudget,
					Object:       current.PlanID + "/" + current.ScenarioID,
					PlanID:       current.PlanID,
					ScenarioID:   current.ScenarioID,
					ConflictRule: current.Rule,
					Field:        "reduction.round",
					Detail:       "combination was reduced again without a new priority-rule explanation: " + strings.Join(conflicts, ", "),
				}
			}
		}
	}
	return nil
}

func (p *BudgetPlan) ComboList() []string {
	keys := make([]string, 0, len(p.Combos))
	for _, combo := range p.Combos {
		keys = append(keys, combo.Key())
	}
	sort.Strings(keys)
	return keys
}
