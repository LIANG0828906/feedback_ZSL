package budget

import "testing"

func TestCooldownAndResourceBounds(t *testing.T) {
	fixture := testFixture(t)
	for _, tick := range fixture.Truth.Ticks {
		if tick.ResourceCurrent < 0 || tick.ResourceCurrent > tick.ResourceMax {
			t.Fatalf("tick %d resource %d outside [0,%d]", tick.Tick, tick.ResourceCurrent, tick.ResourceMax)
		}
		for _, state := range tick.Skills {
			skill, ok := fixture.Project.skillByID(state.SkillID)
			if !ok {
				t.Fatalf("missing skill %s", state.SkillID)
			}
			if state.CooldownRemaining < 0 || state.CooldownRemaining > skill.CooldownTicks {
				t.Fatalf("skill %s cooldown %d outside [0,%d]", state.SkillID, state.CooldownRemaining, skill.CooldownTicks)
			}
		}
		for _, state := range tick.Groups {
			group, ok := fixture.Project.groupByID(state.GroupID)
			if !ok {
				t.Fatalf("missing group %s", state.GroupID)
			}
			if state.CooldownRemaining < 0 || state.CooldownRemaining > group.CooldownTicks {
				t.Fatalf("group %s cooldown %d outside [0,%d]", state.GroupID, state.CooldownRemaining, group.CooldownTicks)
			}
		}
	}
}

func TestSeedIsDeterministic(t *testing.T) {
	a, err := GenerateFixture(DefaultSeed)
	if err != nil {
		t.Fatal(err)
	}
	b, err := GenerateFixture(DefaultSeed)
	if err != nil {
		t.Fatal(err)
	}
	if a.Project.ContentHash != b.Project.ContentHash {
		t.Fatalf("hashes %s != %s", a.Project.ContentHash, b.Project.ContentHash)
	}
}
