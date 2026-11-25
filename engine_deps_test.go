package skillcore_test

import (
	"errors"
	"testing"

	"skillbudget"
	"skillbudget/samplegen"
)

func TestDeterministicEngineIgnoresIntentOrderAndWorkerCount(t *testing.T) {
	bundle, err := samplegen.Generate(20261125)
	if err != nil {
		t.Fatal(err)
	}
	project, err := skillcore.LoadVersion(bundle.Versions[3], 3)
	if err != nil {
		t.Fatal(err)
	}
	engine, err := skillcore.NewEngine(project)
	if err != nil {
		t.Fatal(err)
	}
	first, err := engine.Run(nil, bundle.Intents, bundle.TotalTicks, skillcore.SimOptions{
		SampleEvery: 1, Workers: 1, BufferLimit: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	reversed := make([]skillcore.Intent, len(bundle.Intents))
	for i := range bundle.Intents {
		reversed[len(bundle.Intents)-1-i] = bundle.Intents[i]
	}
	second, err := engine.Run(nil, reversed, bundle.TotalTicks, skillcore.SimOptions{
		SampleEvery: 1, Workers: 8, BufferLimit: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if mismatch := skillcore.CompareSnapshots(first.Ticks, second.Ticks); mismatch != nil {
		t.Fatal(mismatch)
	}
	if mismatch := skillcore.CompareSnapshots(bundle.Truth, second.Ticks); mismatch != nil {
		t.Fatal(mismatch)
	}
}

func TestStrictPriorityConflictReturnsTickAndSkillSet(t *testing.T) {
	project := minimalProject()
	project.ConflictPolicy = skillcore.ConflictStrictPriority
	project.Skills = append(project.Skills, skillcore.Skill{
		ID: "spark", PoolID: "mana", Cost: 100, Cooldown: skillcore.Const(3),
		CastTicks: 0, CDRBp: 0, Priority: project.Skills[0].Priority,
		SharedGroup: "shared", Enabled: true,
	})
	project.Skills[0].SharedGroup = "shared"
	if err := project.Normalize(); err != nil {
		t.Fatal(err)
	}
	engine, err := skillcore.NewEngine(project)
	if err != nil {
		t.Fatal(err)
	}
	state := engine.InitialState()
	_, err = engine.Run(state, []skillcore.Intent{
		{Tick: 0, SkillID: "bolt", Sequence: 1},
		{Tick: 0, SkillID: "spark", Sequence: 2},
	}, 1, skillcore.SimOptions{SampleEvery: 1, Workers: 1, BufferLimit: 1})
	if !skillcore.IsCode(err, skillcore.CodeConflict) {
		t.Fatalf("expected E_CONFLICT, got %v", err)
	}
	domain := new(skillcore.Error)
	if !errors.As(err, &domain) || domain.Tick != 0 || len(domain.Skills) != 2 ||
		domain.Skills[0] != "bolt" || domain.Skills[1] != "spark" {
		t.Fatalf("conflict details incomplete: %#v", err)
	}
	if state.NextTick != 0 || state.Pools["mana"] != 500 {
		t.Fatalf("failed settlement mutated caller state: %#v", state)
	}
}

func TestDependencyInvalidationStartsAtEditTick(t *testing.T) {
	bundle, err := samplegen.Generate(42)
	if err != nil {
		t.Fatal(err)
	}
	project, err := skillcore.LoadVersion(bundle.Versions[3], 3)
	if err != nil {
		t.Fatal(err)
	}
	graph, err := skillcore.BuildDependencyGraph(project, bundle.Intents, bundle.TotalTicks)
	if err != nil {
		t.Fatal(err)
	}
	deps := graph.Dependencies("skill:arc_nova:cooldown")
	if !contains(deps, "skill:arc_nova:cdr") {
		t.Fatalf("cdr -> cooldown dependency missing: %v", deps)
	}
	plan, err := graph.PlanInvalidation(7, []skillcore.Edit{{
		AtTick: 14, Kind: skillcore.EditCooldown, SkillID: "arc_nova",
		Reason: "cooldown value edited",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if !containsInt(plan.ReusableWindows, 0) || !containsInt(plan.ReusableWindows, 1) {
		t.Fatalf("windows before edit were not reusable: %#v", plan)
	}
	found := false
	for _, item := range plan.Invalidations {
		if item.SkillID == "arc_nova" && item.TickStart == 14 && item.TickEnd == 27 {
			found = true
		}
		if item.TickStart < 14 {
			t.Fatalf("invalidation reached before edit tick: %#v", item)
		}
	}
	if !found {
		t.Fatalf("expected arc_nova 14..27 invalidation: %#v", plan.Invalidations)
	}
	regenPlan, err := graph.PlanInvalidation(7, []skillcore.Edit{{
		AtTick: 21, Kind: skillcore.EditRegen, PoolID: "mana",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if !containsInt(regenPlan.ReusableWindows, 0) || !containsInt(regenPlan.ReusableWindows, 1) ||
		!containsInt(regenPlan.ReusableWindows, 2) {
		t.Fatalf("windows before regen edit not reusable: %#v", regenPlan)
	}
}

func TestWindowStateMachineRejectsIllegalTransition(t *testing.T) {
	fsm, err := skillcore.NewWindowFSM(1)
	if err != nil {
		t.Fatal(err)
	}
	if err := fsm.Transition(0, skillcore.WindowSettling); err != nil {
		t.Fatal(err)
	}
	if err := fsm.Transition(0, skillcore.WindowCommitted); err != nil {
		t.Fatal(err)
	}
	err = fsm.Transition(0, skillcore.WindowSettling)
	if !skillcore.IsCode(err, skillcore.CodeState) {
		t.Fatalf("expected E_STATE, got %v", err)
	}
	state, err := fsm.State(0)
	if err != nil || state != skillcore.WindowCommitted {
		t.Fatalf("state changed after illegal transition: %s err=%v", state, err)
	}
}

func TestCompareReportsFirstTickSkillAndField(t *testing.T) {
	bundle, err := samplegen.Generate(42)
	if err != nil {
		t.Fatal(err)
	}
	project, _ := skillcore.LoadVersion(bundle.Versions[3], 3)
	engine, _ := skillcore.NewEngine(project)
	result, err := engine.Run(nil, bundle.Intents, bundle.TotalTicks, skillcore.SimOptions{
		SampleEvery: 1, Workers: 1, BufferLimit: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	actual := append([]skillcore.TickSnapshot(nil), result.Ticks...)
	for i := range actual {
		actual[i].Skills = append([]skillcore.SkillSnapshot(nil), actual[i].Skills...)
		actual[i].Pools = append([]skillcore.PoolSnapshot(nil), actual[i].Pools...)
		actual[i].Decisions = append([]skillcore.Decision(nil), actual[i].Decisions...)
	}
	actual[5].Skills[0].CooldownTicks++
	mismatch := skillcore.CompareSnapshots(result.Ticks, actual)
	if mismatch == nil {
		t.Fatal("expected mismatch")
	}
	if mismatch.Tick != 5 || mismatch.Field != "cooldownTicks" || mismatch.SkillID == "" {
		t.Fatalf("mismatch location incomplete: %#v", mismatch)
	}
}

func contains(values []string, needle string) bool {
	for _, value := range values {
		if value == needle {
			return true
		}
	}
	return false
}

func containsInt(values []int, needle int) bool {
	for _, value := range values {
		if value == needle {
			return true
		}
	}
	return false
}
