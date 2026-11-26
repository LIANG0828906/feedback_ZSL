package budget

import (
	"fmt"
	"math/rand"
)

const DefaultSeed int64 = 42

type Fixture struct {
	Project *Project         `json:"project"`
	Truth   *RunReport       `json:"truth"`
	Flat    FlatRuleTable    `json:"flat"`
	Trees   map[string]*Expr `json:"trees"`
}

// GenerateFixture builds the deterministic local sample. The seed is retained in
// the project identity, and math/rand is only used to choose among bounded,
// scale-compatible sample variations.
func GenerateFixture(seed int64) (*Fixture, error) {
	rng := rand.New(rand.NewSource(seed))
	variation := rng.Intn(3)
	regen := BP(900 + variation*100)
	initial := BP(3000 + variation*200)
	fireCDR := int64(2500 + variation*250) // 25%, 27.5%, 30%; floor keeps CD at 3

	regenTree := &Expr{
		Kind: "add",
		Node: "regen:mana",
		Args: []*Expr{
			{Kind: "lit", Value: 600 + int64(variation)*100},
			{Kind: "lit", Value: 300},
		},
	}
	fireCDRTree := &Expr{
		Kind: "min",
		Node: "cdr:fireball",
		Args: []*Expr{
			{Kind: "lit", Value: 3000},
			{
				Kind: "add",
				Args: []*Expr{
					{Kind: "lit", Value: 2000},
					{Kind: "var", Name: "attunement", Node: "attunement"},
				},
			},
		},
		Vars: map[string]int64{"attunement": fireCDR - 2000},
	}

	project := &Project{
		ID:            fmt.Sprintf("seed-%d-arena", seed),
		Seed:          seed,
		TickMS:        100,
		TickCount:     12,
		SourceVersion: SourceCanonical,
		Resource: ResourceRule{
			ID:        "mana",
			Max:       10000,
			Initial:   initial,
			RegenNode: "regen:mana",
			RegenExpr: regenTree,
			RegenFlat: regen,
		},
		Skills: []Skill{
			{
				ID: "fireball", Priority: 10, Cost: 2500, CooldownTicks: 4,
				CDRNode: "cdr:fireball", CDRExpr: fireCDRTree, CDRFlat: fireCDR,
				GroupID: "gcd",
			},
			{
				ID: "frost_nova", Priority: 20, Cost: 1500, CooldownTicks: 2,
				CDRNode: "cdr:frost_nova", CDRExpr: litExpr("cdr:frost_nova", 500), CDRFlat: 500,
				GroupID: "gcd",
			},
			{
				ID: "arcane_barrage", Priority: 30, Cost: 2000, CooldownTicks: 2,
				CDRNode: "cdr:arcane_barrage", CDRExpr: litExpr("cdr:arcane_barrage", 0), CDRFlat: 0,
			},
			{
				ID: "magic_slash", Priority: 40, Cost: 800, CooldownTicks: 1,
				CDRNode: "cdr:magic_slash", CDRExpr: litExpr("cdr:magic_slash", 0), CDRFlat: 0,
			},
			{
				ID: "unused_ward", Priority: 50, Cost: 1200, CooldownTicks: 6,
				CDRNode: "cdr:unused_ward", CDRExpr: litExpr("cdr:unused_ward", 1000), CDRFlat: 1000,
			},
		},
		Groups: []CooldownGroup{
			{ID: "gcd", CooldownTicks: 1, Members: []string{"fireball", "frost_nova"}},
		},
		Intents: []Intent{
			{IntentID: "i-00", Tick: 0, SkillID: "magic_slash"},
			{IntentID: "i-01", Tick: 1, SkillID: "fireball"},
			{IntentID: "i-02", Tick: 1, SkillID: "frost_nova"},
			{IntentID: "i-03", Tick: 1, SkillID: "magic_slash"},
			{IntentID: "i-04", Tick: 1, SkillID: "arcane_barrage"},
			{IntentID: "i-05", Tick: 2, SkillID: "fireball"},
			{IntentID: "i-06", Tick: 2, SkillID: "frost_nova"},
			{IntentID: "i-07", Tick: 3, SkillID: "arcane_barrage"},
			{IntentID: "i-08", Tick: 4, SkillID: "fireball"},
			{IntentID: "i-09", Tick: 5, SkillID: "magic_slash"},
			{IntentID: "i-10", Tick: 6, SkillID: "frost_nova"},
			{IntentID: "i-11", Tick: 8, SkillID: "arcane_barrage"},
		},
	}

	// Give same-tick source order a seeded deterministic permutation while
	// preserving nondecreasing ticks. Priority must still determine outcomes.
	shuffleSameTickIntents(project.Intents, rng)
	for i := range project.Intents {
		project.Intents[i].Ordinal = i
	}

	flat := FlatRuleTable{
		project.Resource.RegenNode: int64(project.Resource.RegenFlat),
	}
	trees := map[string]*Expr{project.Resource.RegenNode: project.Resource.RegenExpr}
	for _, skill := range project.Skills {
		flat[skill.CDRNode] = skill.CDRFlat
		trees[skill.CDRNode] = skill.CDRExpr
	}
	if mismatch, err := CompareFlatAndTree(flat, trees, map[string]int64{"attunement": fireCDR - 2000}); err != nil {
		return nil, err
	} else if mismatch != nil {
		return nil, &Error{Code: ECodeDiff, Message: "seeded flat/tree mismatch", Node: mismatch.FirstNode}
	}

	truth, err := ReferenceSchedule(project, DefaultParams())
	if err != nil {
		return nil, err
	}
	project.ContentHashMust()
	truth.ContentHash = project.ContentHash

	return &Fixture{Project: project, Truth: truth, Flat: flat, Trees: trees}, nil
}

func litExpr(node string, value int64) *Expr {
	return &Expr{Kind: "lit", Node: node, Value: value}
}

func shuffleSameTickIntents(intents []Intent, rng *rand.Rand) {
	for start := 0; start < len(intents); {
		end := start + 1
		for end < len(intents) && intents[end].Tick == intents[start].Tick {
			end++
		}
		if end-start > 1 {
			rng.Shuffle(end-start, func(i, j int) {
				intents[start+i], intents[start+j] = intents[start+j], intents[start+i]
			})
		}
		start = end
	}
}
