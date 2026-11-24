package generator

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"sort"

	"saveinterop/internal/core"
)

type Archive struct {
	FormatVersion  int            `json:"formatVersion"`
	Branch         string         `json:"branch"`
	MappingVersion string         `json:"mappingVersion"`
	Save           map[string]any `json:"save"`
}

type SampleTruth struct {
	ID       string                        `json:"id"`
	Branches map[string]core.SemanticModel `json:"branches"`
}

type TruthCatalog struct {
	Seed    int64         `json:"seed"`
	Samples []SampleTruth `json:"samples"`
}

type Catalog struct {
	Seed        int64             `json:"seed"`
	RuleVersion string            `json:"ruleVersion"`
	Branches    []string          `json:"branches"`
	Mappings    core.MappingTable `json:"mappings"`
	Samples     []SampleTruth     `json:"samples"`
	TruthPath   string            `json:"truthPath"`
	MappingPath string            `json:"mappingPath"`
}

type sampleSeed struct {
	model    core.SemanticModel
	bonus    int64
	bonusExp int64
	active   bool
}

func Generate(outDir string, seed int64, count int) (Catalog, error) {
	if count <= 0 {
		return Catalog{}, fmt.Errorf("sample count must be positive")
	}
	table := core.DemoMappingTable()
	for _, branch := range table.Branches {
		if err := branch.Validate(); err != nil {
			return Catalog{}, err
		}
	}
	branchIDs := make([]string, 0, len(table.Branches))
	for _, branch := range table.Branches {
		branchIDs = append(branchIDs, branch.ID)
	}
	sort.Strings(branchIDs)

	rng := rand.New(rand.NewSource(seed))
	catalog := Catalog{
		Seed: seed, RuleVersion: table.Version, Branches: branchIDs, Mappings: table,
		TruthPath: "truth.json", MappingPath: "mappings.json",
	}
	for sampleIndex := 0; sampleIndex < count; sampleIndex++ {
		seedData := makeSample(rng, sampleIndex)
		id := fmt.Sprintf("sample-%02d", sampleIndex+1)
		truth := SampleTruth{ID: id, Branches: make(map[string]core.SemanticModel)}
		for _, branch := range table.Branches {
			archive, err := encodeArchive(table.Version, branch, seedData)
			if err != nil {
				return Catalog{}, fmt.Errorf("%s/%s: %w", id, branch.ID, err)
			}
			branchDir := filepath.Join(outDir, branch.ID)
			if err := os.MkdirAll(branchDir, 0o755); err != nil {
				return Catalog{}, err
			}
			bytes, err := json.MarshalIndent(archive, "", "  ")
			if err != nil {
				return Catalog{}, err
			}
			bytes = append(bytes, '\n')
			if err := os.WriteFile(filepath.Join(branchDir, id+".json"), bytes, 0o644); err != nil {
				return Catalog{}, err
			}
			expected := seedData.model.Clone()
			expected.Conditionals = nil
			expected.Flags = nil
			truth.Branches[branch.ID] = expected
		}
		catalog.Samples = append(catalog.Samples, truth)
	}

	if err := writeJSON(filepath.Join(outDir, "truth.json"), TruthCatalog{Seed: seed, Samples: catalog.Samples}); err != nil {
		return Catalog{}, err
	}
	if err := writeJSON(filepath.Join(outDir, "mappings.json"), table); err != nil {
		return Catalog{}, err
	}
	if err := writeJSON(filepath.Join(outDir, "catalog.json"), catalog); err != nil {
		return Catalog{}, err
	}
	return catalog, nil
}

func makeSample(rng *rand.Rand, index int) sampleSeed {
	baseCoins := int64(1_200 + index*173 + rng.Intn(400))
	baseExperience := int64(310 + index*89 + rng.Intn(250))
	bonusCoins := int64(41 + index*3 + rng.Intn(17))
	bonusExperience := int64(19 + index*2 + rng.Intn(11))
	active := index%3 != 2
	coins, experience := baseCoins, baseExperience
	if active {
		coins += bonusCoins
		experience += bonusExperience
	}
	items := []string{
		fmt.Sprintf("potion:%02d", index%4),
		fmt.Sprintf("key:%02d", index%3),
		fmt.Sprintf("relic:%02d", index%5),
	}
	quests := []string{
		fmt.Sprintf("quest:main:%02d", index+1),
		fmt.Sprintf("quest:side:%02d", index%6),
	}
	// Whole seconds are representable by all three tick scales.
	timeNanos := (int64(1_730_000_000) + int64(index)*3_600 + int64(rng.Intn(120))) * 1_000_000_000
	return sampleSeed{
		model: core.SemanticModel{
			Level:          int64(4 + index%9),
			Coins:          coins,
			Experience:     experience,
			Items:          items,
			Quests:         quests,
			Coordinate:     core.Coordinate{X: int64(index*17 - 40), Y: int64(85 + index*23)},
			TimeNanos:      timeNanos,
			TimeScaleNanos: core.InternalTimeScaleNS,
		},
		bonus:    bonusCoins,
		bonusExp: bonusExperience,
		active:   active,
	}
}

func encodeArchive(version string, branch core.BranchSpec, sample sampleSeed) (Archive, error) {
	save := make(map[string]any)
	baseValues := numericValues(sample)

	for _, field := range branch.Fields {
		value, ok := baseValues[field.Role]
		if !ok {
			return Archive{}, fmt.Errorf("no generated value for role %s", field.Role)
		}
		core.Set(save, field.Path, value)
	}
	for _, list := range branch.Lists {
		switch list.Role {
		case "items":
			core.Set(save, list.Path, append([]string(nil), sample.model.Items...))
		case "quests":
			core.Set(save, list.Path, append([]string(nil), sample.model.Quests...))
		default:
			return Archive{}, fmt.Errorf("no generated list for role %s", list.Role)
		}
	}
	flagValues := map[string]bool{
		"event_enabled":  sample.active,
		"legacy_enabled": sample.active,
	}
	for _, flag := range branch.Flags {
		value, ok := flagValues[flag.Semantic]
		if !ok {
			return Archive{}, fmt.Errorf("no generated flag for semantic %s", flag.Semantic)
		}
		core.Set(save, flag.Path, value != flag.Invert)
	}
	for _, conditional := range branch.Conditionals {
		switch conditional.Role {
		case "coins":
			core.Set(save, conditional.Path, sample.bonus)
		case "experience":
			core.Set(save, conditional.Path, sample.bonusExp)
		default:
			return Archive{}, fmt.Errorf("unsupported conditional role %s", conditional.Role)
		}
	}
	ticks, err := core.UnixNanosToTicks(sample.model.TimeNanos, branch.Time.EpochSeconds, branch.Time.TickNanos, branch.Time.Rounding)
	if err != nil {
		return Archive{}, fmt.Errorf("encode time for %s: %w", branch.ID, err)
	}
	core.Set(save, branch.Time.Path, ticks)
	return Archive{
		FormatVersion: 1, Branch: branch.ID, MappingVersion: version, Save: save,
	}, nil
}

func numericValues(sample sampleSeed) map[string]int64 {
	baseCoins, baseExperience := sample.model.Coins, sample.model.Experience
	if sample.active {
		baseCoins -= sample.bonus
		baseExperience -= sample.bonusExp
	}
	return map[string]int64{
		"level":        sample.model.Level,
		"coins":        baseCoins,
		"experience":   baseExperience,
		"coordinate.x": sample.model.Coordinate.X,
		"coordinate.y": sample.model.Coordinate.Y,
	}
}

func writeJSON(path string, value any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	bytes, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	bytes = append(bytes, '\n')
	return os.WriteFile(path, bytes, 0o644)
}
