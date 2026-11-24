package syncgame

import "testing"

func TestAuthoritativeSerialParallelEqual(t *testing.T) {
	sample := testFixture(t)
	serial, err := RunAuthoritative(sample, false, 1)
	if err != nil {
		t.Fatal(err)
	}
	parallel, err := RunAuthoritative(sample, true, 4)
	if err != nil {
		t.Fatal(err)
	}
	if mismatch := CompareAuthority(serial, parallel); mismatch != nil {
		t.Fatalf("serial/parallel mismatch: %+v", mismatch)
	}
	last := serial.States[len(serial.States)-1]
	if !last.Finished || last.Winner == "" || last.LockedTick != last.Tick {
		t.Fatalf("sample did not lock deterministically: %+v", last)
	}
	if serial.Settlement.LockedTick != last.LockedTick || serial.Settlement.Winner != last.Winner {
		t.Fatalf("settlement mismatch: %+v", serial.Settlement)
	}
}

func TestSkillDamageDeathAndRespawn(t *testing.T) {
	sample := &Sample{
		SchemaVersion:    SchemaVersion,
		AlgorithmVersion: AlgorithmVersion,
		Seed:             77,
		TickRate:         30,
		RenderFPS:        30,
		MaxTicks:         8,
		Arena:            Arena{Width: 100, Height: 100},
		ResourceGoal:     10000,
		LatencyTiers: []LatencyTier{
			{ID: "direct", ObservationDelayFrames: 0, MaxInputLatencyFrames: 2, MaxCorrectionFrames: 2, MaxConsecutiveRollbacks: 4, MaxRollbackDepth: 4},
		},
		Clients: []ClientSeed{
			{ID: "c1", PlayerID: "p1", LatencyTier: "direct"},
			{ID: "c2", PlayerID: "p2", LatencyTier: "direct"},
			{ID: "c3", PlayerID: "p3", LatencyTier: "direct"},
		},
		Units: []UnitSeed{
			{PlayerID: "p1", SpawnX: 10, SpawnY: 10, Health: 40, MaxHealth: 40, Resource: 30, MoveSpeed: 3, RespawnTicks: 2, Skills: []SkillDef{{ID: "strike", Cost: 5, CooldownTicks: 3, Range: 1000, Damage: 100}}},
			{PlayerID: "p2", SpawnX: 50, SpawnY: 50, Health: 20, MaxHealth: 20, Resource: 20, MoveSpeed: 3, RespawnTicks: 2},
			{PlayerID: "p3", SpawnX: 80, SpawnY: 80, Health: 30, MaxHealth: 30, Resource: 20, MoveSpeed: 3, RespawnTicks: 2},
		},
		Intents: make([]TickIntent, 8),
	}
	for tick := 0; tick < sample.MaxTicks; tick++ {
		sample.Intents[tick] = TickIntent{Tick: tick, Inputs: []Intent{{PlayerID: "p1", Kind: IntentGather}}}
	}
	sample.Intents[0].Inputs = []Intent{{PlayerID: "p1", Kind: IntentSkill, Skill: "strike", TargetID: "p2"}}
	auth, err := RunAuthoritative(sample, false, 1)
	if err != nil {
		t.Fatal(err)
	}
	p2Tick1, _ := findUnit(auth.States[1], "p2")
	if p2Tick1.Alive || p2Tick1.Health != 0 || p2Tick1.RespawnAt != 3 {
		t.Fatalf("expected p2 death at tick 1: %+v", p2Tick1)
	}
	p2Tick3, _ := findUnit(auth.States[3], "p2")
	if p2Tick3.Alive {
		t.Fatalf("p2 revived too early: %+v", p2Tick3)
	}
	p2Tick4, _ := findUnit(auth.States[4], "p2")
	if !p2Tick4.Alive || p2Tick4.Health != 10 {
		t.Fatalf("expected p2 respawn at tick 4: %+v", p2Tick4)
	}
	p1, _ := findUnit(auth.States[1], "p1")
	// 30 starting resource - 5 skill cost + 12 kill bounty.
	if p1.Resource != 37 || p1.SkillCooldown["strike"] != 3 {
		t.Fatalf("skill cost/cooldown not applied: %+v", p1)
	}
	p1Tick2, _ := findUnit(auth.States[2], "p1")
	if p1Tick2.SkillCooldown["strike"] != 2 {
		t.Fatalf("cooldown did not tick down: %+v", p1Tick2)
	}
}
