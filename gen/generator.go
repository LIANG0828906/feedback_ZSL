package gen

import (
	"chaosdiag/model"
)

const (
	Seed           int64 = 42
	ProjectID            = "chaosdiag-demo"
	ProjectVersion       = "sample-seed-42-rev-1"
)

type Bundle struct {
	Seed      int64
	Canonical *model.Project
	Docs      map[string][]byte
	Truth     *Truth
}

func Generate() (*Bundle, error) {
	p := BuildProject()
	truth := BuildTruth(p)
	docs, err := RenderAll(p)
	if err != nil {
		return nil, err
	}
	return &Bundle{Seed: Seed, Canonical: p, Docs: docs, Truth: truth}, nil
}

func BuildProject() *model.Project {
	services := []model.Service{
		{Name: "gateway", LatencyMS: 4},
		{Name: "auth", LatencyMS: 6},
		{Name: "order", LatencyMS: 8},
		{Name: "inventory", LatencyMS: 10},
		{Name: "payment", LatencyMS: 12},
		{Name: "risk", LatencyMS: 5, SelfErrorCode: "E_RISK_PANIC", SelfErrorMessage: "deterministic service self failure"},
	}
	edges := []model.Edge{
		{ID: "edge-gateway-auth", From: "gateway", To: "auth", LatencyMS: 8},
		{ID: "edge-gateway-order", From: "gateway", To: "order", LatencyMS: 10},
		{ID: "edge-order-inventory", From: "order", To: "inventory", LatencyMS: 12},
		{ID: "edge-order-payment", From: "order", To: "payment", LatencyMS: 14},
		{ID: "edge-gateway-risk", From: "gateway", To: "risk", LatencyMS: 7},
	}

	instructions := []model.Instruction{
		edgeInstruction("I-DELAY-INV", 0, 3, model.KindDelay, 10, "edge-order-inventory", 3, 3, func(i *model.Instruction) {
			i.DelayMS = 60
			i.BudgetCostMS = 60
		}),
		edgeInstruction("I-FAIL-PAY", 1, 4, model.KindFailure, 20, "edge-order-payment", 4, 5, func(i *model.Instruction) {
			i.FailureCode = "CHAOS_INJECTED_PAYMENT_FAILURE"
			i.BudgetCostMS = 20
		}),
		serviceInstruction("I-HOP-TIMEOUT-AUTH", 2, 4, model.KindTimeout, 30, "auth", 4, 4, func(i *model.Instruction) {
			i.TimeoutMS = 10
			i.BudgetCostMS = 10
		}),
		edgeInstruction("I-BUDGET-TIMEOUT-INV", 3, 5, model.KindDelay, 30, "edge-order-inventory", 5, 6, func(i *model.Instruction) {
			i.DelayMS = 260
			i.BudgetCostMS = 260
		}),
		serviceInstruction("I-EXEMPT-FAIL-PAY", 4, 6, model.KindFailure, 40, "payment", 6, 7, func(i *model.Instruction) {
			i.FailureCode = "CHAOS_INJECTED_EXEMPT_FAILURE"
			i.BudgetCostMS = 20
		}),
		edgeInstruction("I-DUP-PAY", 5, 9, model.KindDuplicate, 15, "edge-order-payment", 9, 9, func(i *model.Instruction) {
			i.RepeatCount = 2
			i.RepeatDelayMS = 10
			i.BudgetCostMS = 30
		}),
		edgeInstruction("I-REORDER-INV", 6, 9, model.KindReorder, 5, "edge-order-inventory", 9, 9, func(i *model.Instruction) {
			i.ReorderDepth = 1
			i.BudgetCostMS = 10
		}),
		serviceInstruction("I-Q1-PAY", 7, 10, model.KindDelay, 10, "payment", 10, 10, func(i *model.Instruction) {
			i.DelayMS = 180
			i.BudgetCostMS = 180
		}),
		serviceInstruction("I-Q2-AUTH", 8, 10, model.KindDelay, 10, "auth", 10, 10, func(i *model.Instruction) {
			i.DelayMS = 160
			i.BudgetCostMS = 160
		}),
		serviceInstruction("I-Q3-PAY", 9, 10, model.KindDelay, 5, "payment", 10, 12, func(i *model.Instruction) {
			i.DelayMS = 80
			i.BudgetCostMS = 80
		}),
		serviceInstruction("I-Q4-INV", 10, 10, model.KindDelay, 5, "inventory", 10, 12, func(i *model.Instruction) {
			i.DelayMS = 200
			i.BudgetCostMS = 200
		}),
		edgeInstruction("I-DELAY-OVERLAP-LOW", 11, 3, model.KindDelay, 1, "edge-order-inventory", 3, 3, func(i *model.Instruction) {
			i.DelayMS = 999
			i.BudgetCostMS = 20
		}),
	}

	exemptions := []model.Exemption{{
		ID: "EX-PAY-CB", Source: "breaker:payment-circuit", Reason: "deterministic circuit-breaker open exemption",
		InstructionID: "I-EXEMPT-FAIL-PAY",
		Selector:      model.Selector{ID: "sel-ex-pay", Kind: model.TargetService, Service: "payment"},
	}}

	requests := []model.Request{
		{ID: "R-01-BASE-AUTH", Sequence: 0, Tick: 1, Entry: "gateway", Path: []string{"gateway", "auth"}, Expected: model.ExpectedResult{Status: model.StatusCompleted}},
		{ID: "R-02-BASE-INV", Sequence: 1, Tick: 2, Entry: "gateway", Path: []string{"gateway", "order", "inventory"}, Expected: model.ExpectedResult{Status: model.StatusCompleted}},
		{ID: "R-03-LATENCY-SLO", Sequence: 2, Tick: 3, Entry: "gateway", Path: []string{"gateway", "order", "inventory"}, Expected: model.ExpectedResult{Status: model.StatusCompleted, MaxTotalMS: 70}},
		{ID: "R-04-INJECTED-FAIL", Sequence: 3, Tick: 4, Entry: "gateway", Path: []string{"gateway", "order", "payment"}, Expected: model.ExpectedResult{Status: model.StatusCompleted}},
		{ID: "R-05-HOP-TIMEOUT", Sequence: 4, Tick: 4, Entry: "gateway", Path: []string{"gateway", "auth"}, Expected: model.ExpectedResult{Status: model.StatusCompleted}},
		{ID: "R-06-BUDGET-TIMEOUT", Sequence: 5, Tick: 5, Entry: "gateway", Path: []string{"gateway", "order", "inventory"}, Expected: model.ExpectedResult{Status: model.StatusCompleted}},
		{ID: "R-07-EXEMPT-CB", Sequence: 6, Tick: 6, Entry: "gateway", Path: []string{"gateway", "order", "payment"}, Expected: model.ExpectedResult{Status: model.StatusCompleted}},
		{ID: "R-08-SERVICE-SELF", Sequence: 7, Tick: 7, Entry: "gateway", Path: []string{"gateway", "risk"}, Expected: model.ExpectedResult{Status: model.StatusCompleted}},
		{ID: "R-09-BAD-EXPECTATION", Sequence: 8, Tick: 8, Entry: "gateway", Path: []string{"gateway", "auth"}, Expected: model.ExpectedResult{Status: model.StatusTimeout, Code: "EXPECTED_BUT_NOT_OBSERVED"}},
		{ID: "R-10-DUPLICATE", Sequence: 9, Tick: 9, Entry: "gateway", Path: []string{"gateway", "order", "payment"}, Expected: model.ExpectedResult{Status: model.StatusCompleted, MaxRepeats: 0}},
		{ID: "R-11-REORDER", Sequence: 10, Tick: 9, Entry: "gateway", Path: []string{"gateway", "order", "inventory"}, Expected: model.ExpectedResult{Status: model.StatusCompleted}},
		{ID: "R-12-QUEUED-PAY-FIRST", Sequence: 11, Tick: 10, Entry: "gateway", Path: []string{"gateway", "order", "payment"}, Expected: model.ExpectedResult{Status: model.StatusCompleted}},
		{ID: "R-13-RETRY-PAY", Sequence: 12, Tick: 11, Entry: "gateway", Path: []string{"gateway", "order", "payment"}, Expected: model.ExpectedResult{Status: model.StatusCompleted}},
		{ID: "R-14-RETRY-INV", Sequence: 13, Tick: 11, Entry: "gateway", Path: []string{"gateway", "order", "inventory"}, Expected: model.ExpectedResult{Status: model.StatusCompleted}},
	}

	selectors := make([]model.Selector, 0, len(instructions)+len(exemptions))
	for _, ins := range instructions {
		selectors = append(selectors, ins.Selector)
	}
	for _, ex := range exemptions {
		selectors = append(selectors, ex.Selector)
	}

	return &model.Project{
		Schema: model.SchemaV1, ProjectID: ProjectID, ProjectVersion: ProjectVersion, Seed: Seed,
		TickMS: 10, RequestTimeoutBudgetMS: 250, AdmissionTimeoutBudgetMS: 500,
		Limits:   model.Limits{GlobalConcurrent: 2, ServiceConcurrentDefault: 2, ServiceConcurrent: map[string]int{"payment": 1}},
		Services: services, Edges: edges, Selectors: selectors, Instructions: instructions,
		Exemptions: exemptions, Requests: requests, ConstraintCatalog: []model.Constraint{
			{ID: "TIMEOUT-BUDGET-REQUEST", Level: "request", Description: "integer-ms end-to-end logical deadline"},
			{ID: "TIMEOUT-BUDGET-ADMISSION", Level: "global", Description: "releasable integer-ms amplitude reservation for active commands"},
			{ID: "CONCURRENCY-GLOBAL", Level: "global", Description: "maximum active fault commands in the project"},
			{ID: "CONCURRENCY-SERVICE:payment", Level: "service:payment", Description: "maximum active fault commands targeting payment"},
		},
	}
}

func edgeInstruction(id string, seq, submitted int, kind string, priority int, edgeID string, start, end int, mutate func(*model.Instruction)) model.Instruction {
	ins := model.Instruction{
		ID: id, Sequence: seq, SubmittedTick: submitted, Kind: kind, Priority: priority,
		SelectorID: "sel-" + id, StartTick: start, EndTick: end,
		Selector: model.Selector{ID: "sel-" + id, Kind: model.TargetEdge, EdgeID: edgeID},
	}
	if mutate != nil {
		mutate(&ins)
	}
	return ins
}

func serviceInstruction(id string, seq, submitted int, kind string, priority int, service string, start, end int, mutate func(*model.Instruction)) model.Instruction {
	ins := model.Instruction{
		ID: id, Sequence: seq, SubmittedTick: submitted, Kind: kind, Priority: priority,
		SelectorID: "sel-" + id, StartTick: start, EndTick: end,
		Selector: model.Selector{ID: "sel-" + id, Kind: model.TargetService, Service: service},
	}
	if mutate != nil {
		mutate(&ins)
	}
	return ins
}
