package macrokit

import (
	"math/rand"
)

const FixedSampleSeed int64 = 20261125

type SampleProject struct {
	Slug    string
	Project *Project
	Events  EventStream
	Truth   *Truth
}

type SampleSet struct {
	Seed     int64
	Projects []*SampleProject
}

func GenerateSamples(seed int64) *SampleSet {
	if seed == 0 {
		seed = FixedSampleSeed
	}
	return &SampleSet{
		Seed:     seed,
		Projects: []*SampleProject{basicTimingSample(seed), priorityConflictSample(seed), dependencyChainSample(seed)},
	}
}

func baseProject(seed int64, id, name string) *Project {
	return &Project{
		FormatNo:      FormatFlatFrameV1,
		FormatName:    "internal",
		FormatVersion: FormatVersionV1,
		ID:            id,
		Name:          name,
		Seed:          seed,
		FrameRate:     60,
		TimePolicy:    TimePolicy{FrameToMS: RoundFloor, MSToFrame: RoundCeil},
		Flags:         map[string]bool{"advanced": true},
	}
}

func keyPressedCondition(key string) *Condition {
	return &Condition{Kind: CondKey, Key: key, KeyState: KeyPressed}
}

func allCondition(children ...*Condition) *Condition {
	return &Condition{Kind: CondAll, Children: children}
}

func branchCondition(id string) *Condition {
	return &Condition{Kind: CondBranch, BranchID: id, Outcome: "completed"}
}

func sampleStep(id, action, lane string, gap, hold int64) *Step {
	return &Step{ID: id, Action: action, Lane: lane, GapFrames: gap, HoldFrames: hold}
}

func shuffleBranches(seed int64, salt int, branches []*Branch) {
	rng := rand.New(rand.NewSource(seed + int64(salt)*1009))
	rng.Shuffle(len(branches), func(i, j int) { branches[i], branches[j] = branches[j], branches[i] })
}

func basicTimingSample(seed int64) *SampleProject {
	p := baseProject(seed, "basic-timing", "Basic timing, cancellation and predecessor")

	basic := &Branch{
		ID: "b_basic_q", MacroID: "m_attack", Name: "Q attack",
		DelayFrames: 2, TriggerWindowFrames: 4, Condition: keyPressedCondition("KeyQ"),
		Steps: []*Step{
			sampleStep("s_draw", "draw_weapon", "attack-lane", 0, 0),
			sampleStep("s_strike", "strike", "attack-lane", 1, 0),
			sampleStep("s_recover", "recover", "attack-lane", 2, 0),
		},
	}
	follow := &Branch{
		ID: "b_followup", MacroID: "m_combo", Name: "After Q follow-up",
		DelayFrames: 1, TriggerWindowFrames: 4,
		Condition: allCondition(keyPressedCondition("KeyF"), branchCondition("b_basic_q")),
		Steps: []*Step{
			sampleStep("s_dash", "dash", "combo-lane", 0, 0),
			sampleStep("s_heavy", "heavy_strike", "combo-lane", 2, 0),
		},
	}
	flag := &Branch{
		ID: "b_flag_e", MacroID: "m_magic", Name: "Flag plus E",
		DelayFrames: 1, TriggerWindowFrames: 4,
		Condition: allCondition(keyPressedCondition("KeyE"), &Condition{Kind: CondFlag, Flag: "advanced"}),
		Steps: []*Step{
			sampleStep("s_scan", "scan", "magic-lane", 0, 0),
			sampleStep("s_blast", "blast", "magic-lane", 2, 0),
		},
	}
	cancel := &Branch{
		ID: "b_cancel_r", MacroID: "m_cancel_demo", Name: "Cancelable R",
		DelayFrames: 4, TriggerWindowFrames: 4, Condition: keyPressedCondition("KeyR"),
		CancelKeys: []string{"Escape"},
		Steps: []*Step{
			sampleStep("s_chant", "chant", "cancel-lane", 0, 1),
			sampleStep("s_fire", "fire", "cancel-lane", 3, 0),
		},
	}
	p.Branches = []*Branch{basic, follow, flag, cancel}
	shuffleBranches(seed, 1, p.Branches)
	p.Index()

	events := EventStream{FrameCount: 16, Events: []KeyEvent{
		{Frame: 2, Key: "KeyQ", Kind: "down"},
		{Frame: 3, Key: "KeyQ", Kind: "up"},
		{Frame: 5, Key: "KeyE", Kind: "down"},
		{Frame: 6, Key: "KeyE", Kind: "up"},
		{Frame: 6, Key: "KeyR", Kind: "down"},
		{Frame: 7, Key: "KeyR", Kind: "up"},
		{Frame: 8, Key: "Escape", Kind: "down"},
		{Frame: 9, Key: "Escape", Kind: "up"},
		{Frame: 10, Key: "KeyF", Kind: "down"},
		{Frame: 11, Key: "KeyF", Kind: "up"},
	}}

	truth := &Truth{Seed: seed, ProjectID: p.ID, FrameCount: events.FrameCount, Branches: []ExpectedBranch{
		{ID: basic.ID, MacroID: basic.MacroID, Triggered: true, TriggerFrame: 2, DelayFrames: 2, Steps: []string{"s_draw", "s_strike", "s_recover"}, StepFrames: []int64{4, 5, 7}, StepGaps: []int64{1, 2}, Completed: true},
		{ID: flag.ID, MacroID: flag.MacroID, Triggered: true, TriggerFrame: 5, DelayFrames: 1, Steps: []string{"s_scan", "s_blast"}, StepFrames: []int64{6, 8}, StepGaps: []int64{2}, Completed: true},
		{ID: cancel.ID, MacroID: cancel.MacroID, Triggered: true, TriggerFrame: 6, DelayFrames: 4, Steps: nil, Completed: false, Canceled: true, CancelFrame: 8, CancelReason: "cancel_key:Escape"},
		{ID: follow.ID, MacroID: follow.MacroID, Triggered: true, TriggerFrame: 10, DelayFrames: 1, Steps: []string{"s_dash", "s_heavy"}, StepFrames: []int64{11, 13}, StepGaps: []int64{2}, Completed: true},
	}}
	return &SampleProject{Slug: "basic-timing", Project: p, Events: events, Truth: truth}
}

func priorityConflictSample(seed int64) *SampleProject {
	p := baseProject(seed, "priority-conflict", "Priority arbitration for same-frame lane conflicts")
	p.Flags = map[string]bool{}

	high := &Branch{
		ID: "p_high", MacroID: "m_high", HasPriority: true, Priority: 20,
		DelayFrames: 1, TriggerWindowFrames: 1, Condition: keyPressedCondition("Space"),
		Steps: []*Step{sampleStep("s_high_slash", "slash", "shared-combat-lane", 0, 2)},
	}
	low := &Branch{
		ID: "p_low", MacroID: "m_low", HasPriority: true, Priority: 5,
		DelayFrames: 1, TriggerWindowFrames: 1, Condition: keyPressedCondition("Space"),
		Steps: []*Step{sampleStep("s_low_slash", "slash", "shared-combat-lane", 0, 2)},
	}
	other := &Branch{
		ID: "p_other", MacroID: "m_movement", HasPriority: true, Priority: 1,
		DelayFrames: 0, TriggerWindowFrames: 1, Condition: keyPressedCondition("Space"),
		Steps: []*Step{sampleStep("s_jump", "jump", "movement-lane", 0, 0)},
	}
	p.Branches = []*Branch{low, other, high}
	shuffleBranches(seed, 2, p.Branches)
	p.Index()

	events := EventStream{FrameCount: 8, Events: []KeyEvent{
		{Frame: 3, Key: "Space", Kind: "down"},
		{Frame: 4, Key: "Space", Kind: "up"},
	}}
	truth := &Truth{Seed: seed, ProjectID: p.ID, FrameCount: events.FrameCount, Branches: []ExpectedBranch{
		{ID: high.ID, MacroID: high.MacroID, Triggered: true, TriggerFrame: 3, DelayFrames: 1, Steps: []string{"s_high_slash"}, StepFrames: []int64{4}, Completed: true},
		{ID: low.ID, MacroID: low.MacroID, Triggered: true, TriggerFrame: 3, SuppressedBy: "p_high"},
		{ID: other.ID, MacroID: other.MacroID, Triggered: true, TriggerFrame: 3, DelayFrames: 0, Steps: []string{"s_jump"}, StepFrames: []int64{3}, Completed: true},
	}}
	return &SampleProject{Slug: "priority-conflict", Project: p, Events: events, Truth: truth}
}

func dependencyChainSample(seed int64) *SampleProject {
	p := baseProject(seed, "dependency-chain", "Branch dependency graph and incremental invalidation")
	p.Flags = map[string]bool{}

	a := &Branch{
		ID: "d_a", MacroID: "m_chain_a", DelayFrames: 1, TriggerWindowFrames: 1,
		Condition: keyPressedCondition("KeyZ"),
		Steps: []*Step{
			sampleStep("s_a1", "a1", "lane-a", 0, 0),
			sampleStep("s_a2", "a2", "lane-a", 1, 0),
		},
	}
	b := &Branch{
		ID: "d_b", MacroID: "m_chain_b", DelayFrames: 2, TriggerWindowFrames: 1,
		Condition: allCondition(keyPressedCondition("KeyX"), branchCondition("d_a")),
		Steps: []*Step{
			sampleStep("s_b1", "b1", "lane-b", 0, 0),
			sampleStep("s_b2", "b2", "lane-b", 1, 0),
		},
	}
	c := &Branch{
		ID: "d_c", MacroID: "m_chain_c", DelayFrames: 0, TriggerWindowFrames: 1,
		Condition: branchCondition("d_b"),
		Steps:     []*Step{sampleStep("s_c1", "c1", "lane-c", 0, 0)},
	}
	ind := &Branch{
		ID: "d_ind", MacroID: "m_independent", DelayFrames: 0, TriggerWindowFrames: 1,
		Condition: keyPressedCondition("KeyC"),
		Steps:     []*Step{sampleStep("s_ind1", "ind1", "lane-ind", 0, 0)},
	}
	p.Branches = []*Branch{c, ind, a, b}
	shuffleBranches(seed, 3, p.Branches)
	p.Index()

	events := EventStream{FrameCount: 12, Events: []KeyEvent{
		{Frame: 1, Key: "KeyC", Kind: "down"},
		{Frame: 2, Key: "KeyZ", Kind: "down"},
		{Frame: 3, Key: "KeyZ", Kind: "up"},
		{Frame: 6, Key: "KeyX", Kind: "down"},
		{Frame: 7, Key: "KeyX", Kind: "up"},
	}}
	truth := &Truth{Seed: seed, ProjectID: p.ID, FrameCount: events.FrameCount, Branches: []ExpectedBranch{
		{ID: ind.ID, MacroID: ind.MacroID, Triggered: true, TriggerFrame: 1, DelayFrames: 0, Steps: []string{"s_ind1"}, StepFrames: []int64{1}, Completed: true},
		{ID: a.ID, MacroID: a.MacroID, Triggered: true, TriggerFrame: 2, DelayFrames: 1, Steps: []string{"s_a1", "s_a2"}, StepFrames: []int64{3, 4}, StepGaps: []int64{1}, Completed: true},
		{ID: b.ID, MacroID: b.MacroID, Triggered: true, TriggerFrame: 6, DelayFrames: 2, Steps: []string{"s_b1", "s_b2"}, StepFrames: []int64{8, 9}, StepGaps: []int64{1}, Completed: true},
		{ID: c.ID, MacroID: c.MacroID, Triggered: true, TriggerFrame: 10, DelayFrames: 0, Steps: []string{"s_c1"}, StepFrames: []int64{10}, Completed: true},
	}}
	return &SampleProject{Slug: "dependency-chain", Project: p, Events: events, Truth: truth}
}
