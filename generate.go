package balance

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"os"
	"path/filepath"
)

// GenerateProject deterministically creates the local sample project. It does
// not read files or use a process-global random source.
func GenerateProject(seed int64) *Project {
	classes := []Class{
		{ID: "warrior", Name: "Warrior"},
		{ID: "mage", Name: "Mage"},
		{ID: "ranger", Name: "Ranger"},
	}

	skillSpecs := []struct {
		id       string
		classID  string
		base     Fixed
		growth   Fixed
		cooldown int
		cost     Fixed
		hitMin   int
		hitMax   int
	}{
		{"warrior.slash", "warrior", Fixed(118000 + seeded(seed, "slash.base", 9000)), Fixed(1050 + seeded(seed, "slash.growth", 250)), 0, Fixed(40 + seeded(seed, "slash.cost", 20)), -180, 220},
		{"warrior.cleave", "warrior", Fixed(171000 + seeded(seed, "cleave.base", 12000)), Fixed(1320 + seeded(seed, "cleave.growth", 300)), 2, Fixed(260 + seeded(seed, "cleave.cost", 40)), -350, 450},
		{"mage.bolt", "mage", Fixed(125000 + seeded(seed, "bolt.base", 8000)), Fixed(1150 + seeded(seed, "bolt.growth", 220)), 0, Fixed(55 + seeded(seed, "bolt.cost", 20)), -120, 300},
		{"mage.nova", "mage", Fixed(206000 + seeded(seed, "nova.base", 16000)), Fixed(1480 + seeded(seed, "nova.growth", 260)), 2, Fixed(340 + seeded(seed, "nova.cost", 50)), -400, 520},
		{"ranger.shot", "ranger", Fixed(143000 + seeded(seed, "shot.base", 9000)), Fixed(1210 + seeded(seed, "shot.growth", 260)), 1, Fixed(90 + seeded(seed, "shot.cost", 25)), -260, 380},
	}

	skills := make([]Skill, 0, len(skillSpecs))
	formulas := make([]Formula, 0, len(skillSpecs)*5)
	for _, spec := range skillSpecs {
		skill := Skill{
			ID:                spec.id,
			ClassID:           spec.classID,
			FormulaID:         spec.id + ".root",
			BaseDamage:        spec.base,
			GrowthCoefficient: spec.growth,
			Cooldown:          spec.cooldown,
			ResourceCost:      spec.cost,
			HitVarianceMinBPS: spec.hitMin,
			HitVarianceMaxBPS: spec.hitMax,
		}
		skills = append(skills, skill)
		formulas = append(formulas, formulaForSkill(skill.ID)...)
	}

	builds := []Build{
		{
			ID: "duel_warrior", ClassID: "warrior",
			MaxHP: 730000, MaxResource: 650, ResourceRegen: 170,
			Attributes: map[string]Fixed{"attack": 81000 + Fixed(seeded(seed, "dw.atk", 4000))},
			Rotation:   []string{"warrior.slash", "warrior.cleave"},
		},
		{
			ID: "duel_mage", ClassID: "mage",
			MaxHP: 760000, MaxResource: 720, ResourceRegen: 205,
			Attributes: map[string]Fixed{"attack": 88000 + Fixed(seeded(seed, "dm.atk", 4000))},
			Rotation:   []string{"mage.bolt", "mage.nova"},
		},
		{
			ID: "squad_guard", ClassID: "warrior",
			MaxHP: 715000, MaxResource: 680, ResourceRegen: 175,
			Attributes: map[string]Fixed{"attack": 76000 + Fixed(seeded(seed, "sg.atk", 4000))},
			Rotation:   []string{"warrior.slash", "warrior.cleave"},
		},
		{
			ID: "squad_ranger_a", ClassID: "ranger",
			MaxHP: 715000, MaxResource: 600, ResourceRegen: 185,
			Attributes: map[string]Fixed{"attack": 86000 + Fixed(seeded(seed, "sra.atk", 4000))},
			Rotation:   []string{"ranger.shot", "mage.bolt"},
		},
		{
			ID: "squad_mage", ClassID: "mage",
			MaxHP: 780000, MaxResource: 760, ResourceRegen: 210,
			Attributes: map[string]Fixed{"attack": 90000 + Fixed(seeded(seed, "sm.atk", 4000))},
			Rotation:   []string{"mage.bolt", "mage.nova"},
		},
		{
			ID: "squad_ranger_b", ClassID: "ranger",
			MaxHP: 820000, MaxResource: 610, ResourceRegen: 190,
			Attributes: map[string]Fixed{"attack": 84000 + Fixed(seeded(seed, "srb.atk", 4000))},
			Rotation:   []string{"ranger.shot", "warrior.slash"},
		},
	}

	scenarios := []Scenario{
		{
			ID: "duel", Kind: ScenarioDuel,
			LeftBuilds: []string{"duel_warrior"}, RightBuilds: []string{"duel_mage"},
			MaxRounds: 10, ReviveRounds: 0,
		},
		{
			ID: "squad", Kind: ScenarioSquad,
			LeftBuilds:  []string{"squad_guard", "squad_ranger_a"},
			RightBuilds: []string{"squad_mage", "squad_ranger_b"},
			MaxRounds:   12, ReviveRounds: 1,
		},
	}

	plans := []Plan{
		{
			ID: "baseline", Priority: 100,
			TargetScenarios: []string{"duel", "squad"},
			Adjustments:     []SkillAdjustment{},
		},
		{
			ID: "melee_growth", Priority: 80,
			TargetScenarios: []string{"duel", "squad"},
			Adjustments: []SkillAdjustment{
				{
					SkillID:           "warrior.slash",
					BaseDamageDelta:   Fixed(4500 + seeded(seed, "plan.melee.base", 2000)),
					CooldownDelta:     0,
					ResourceCostDelta: -Fixed(5),
				},
			},
		},
		{
			ID: "burst_control", Priority: 60,
			TargetScenarios: []string{"duel", "squad"},
			Adjustments: []SkillAdjustment{
				{
					SkillID:           "mage.nova",
					BaseDamageDelta:   -Fixed(14000 + seeded(seed, "plan.burst.base", 4000)),
					CooldownDelta:     1,
					ResourceCostDelta: 20,
				},
			},
		},
	}

	thresholds := []Threshold{
		{ID: "baseline-balanced", PlanID: "baseline", Metric: "win_rate_ppm", Min: 350000, Max: 650000},
		{ID: "baseline-damage-floor", PlanID: "baseline", Metric: "avg_damage", Min: 100000, Max: int64(MaxFixed)},
		{ID: "all-ci-visible", Metric: "max_ci_width_ppm", Min: 0, Max: 1000000},
	}

	return &Project{
		SchemaVersion:    SchemaVersion,
		Seed:             seed,
		AlgorithmVersion: AlgorithmVersion,
		BudgetVersion:    BudgetVersion,
		Classes:          classes,
		Skills:           skills,
		Formulas:         formulas,
		Builds:           builds,
		Scenarios:        scenarios,
		Plans:            plans,
		Thresholds:       thresholds,
		Budget: Budget{
			Slots:                 4,
			MaxMatchesPerRound:    6,
			MemoryBuffer:          2,
			MaxRounds:             10,
			TargetMatchesPerCombo: 8,
			MaxTotalMatches:       36,
			PriorityRule:          PriorityFirst,
		},
	}
}

func formulaForSkill(skillID string) []Formula {
	prefix := skillID + "."
	return []Formula{
		{ID: prefix + "base", Op: "skill_base"},
		{ID: prefix + "growth", Op: "skill_growth"},
		{ID: prefix + "attack", Op: "attr", Ref: "attack"},
		{ID: prefix + "scaled", Op: "mul", Inputs: []string{prefix + "growth", prefix + "attack"}},
		{ID: prefix + "root", Op: "add", Inputs: []string{prefix + "base", prefix + "scaled"}},
	}
}

func seeded(seed int64, salt string, span int64) int64 {
	if span <= 0 {
		return 0
	}
	h := sha256.Sum256([]byte(salt + "\x00" + formatInt(seed)))
	return int64(binary.BigEndian.Uint64(h[:8]) % uint64(span))
}

func formatInt(v int64) string {
	const digits = "0123456789"
	if v == 0 {
		return "0"
	}
	neg := v < 0
	var buf [20]byte
	i := len(buf)
	u := uint64(v)
	if neg {
		u = uint64(-v)
	}
	for u > 0 {
		i--
		buf[i] = digits[u%10]
		u /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

func MarshalProject(project *Project) ([]byte, error) {
	data, err := json.MarshalIndent(project, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

func SaveProject(path string, project *Project) error {
	data, err := MarshalProject(project)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}
