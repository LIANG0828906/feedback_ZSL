package samples

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"economycore/internal/core"
	"economycore/internal/det"
)

const GeneratorVersion = "samplegen/v1"

type Manifest struct {
	Version   int             `json:"version"`
	Seed      uint64          `json:"seed"`
	Generator string          `json:"generator"`
	Samples   []ManifestEntry `json:"samples"`
}

type ManifestEntry struct {
	ID          string `json:"id"`
	File        string `json:"file"`
	Seed        uint64 `json:"seed"`
	NodeCount   int    `json:"node_count"`
	IntentCount int    `json:"intent_count"`
	FinalTick   int64  `json:"final_tick"`
	SceneHash   string `json:"scene_hash"`
}

type Profile struct {
	ID           string
	Intents      int
	Horizon      int64
	TruthSamples int
}

func DefaultProfiles() []Profile {
	return []Profile{
		{ID: "frontier_small", Intents: 240, Horizon: 720, TruthSamples: 10},
		{ID: "river_medium", Intents: 3500, Horizon: 1440, TruthSamples: 16},
		{ID: "industry_large", Intents: 24000, Horizon: 2400, TruthSamples: 20},
	}
}

func Generate(outDir string, seed uint64, profiles []Profile) (*Manifest, error) {
	if len(profiles) == 0 {
		profiles = DefaultProfiles()
	}
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return nil, core.NewError(core.CodeIO, "failed to create sample directory").At(outDir).Wrap(err)
	}
	manifest := &Manifest{
		Version:   1,
		Seed:      seed,
		Generator: GeneratorVersion,
		Samples:   make([]ManifestEntry, 0, len(profiles)),
	}
	for _, profile := range profiles {
		scenario, err := Build(profile, seed)
		if err != nil {
			return nil, err
		}
		name := profile.ID + ".json"
		path := filepath.Join(outDir, name)
		data, err := json.MarshalIndent(scenario, "", "  ")
		if err != nil {
			return nil, core.NewError(core.CodeState, "failed to encode generated sample").Wrap(err)
		}
		data = append(data, '\n')
		if err := os.WriteFile(path, data, 0o644); err != nil {
			return nil, core.NewError(core.CodeIO, "failed to write generated sample").At(path).Wrap(err)
		}
		manifest.Samples = append(manifest.Samples, ManifestEntry{
			ID:          profile.ID,
			File:        name,
			Seed:        scenario.Seed,
			NodeCount:   len(scenario.Nodes),
			IntentCount: len(scenario.Intents),
			FinalTick:   scenario.FinalTick,
			SceneHash:   scenario.SceneHash,
		})
	}
	data, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return nil, core.NewError(core.CodeState, "failed to encode sample manifest").Wrap(err)
	}
	data = append(data, '\n')
	if err := os.WriteFile(filepath.Join(outDir, "manifest.json"), data, 0o644); err != nil {
		return nil, core.NewError(core.CodeIO, "failed to write sample manifest").Wrap(err)
	}
	return manifest, nil
}

func LoadManifest(dir string) (*Manifest, error) {
	data, err := os.ReadFile(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return nil, core.NewError(core.CodeIO, "failed to read sample manifest").At(dir).Wrap(err)
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, core.NewError(core.CodeDecode, "sample manifest is not valid JSON").Wrap(err)
	}
	if m.Version != 1 || len(m.Samples) == 0 {
		return nil, core.NewError(core.CodeSchema, "sample manifest is empty or has an unsupported version").
			At("manifest.json")
	}
	for i := range m.Samples {
		if m.Samples[i].ID == "" || m.Samples[i].File == "" {
			return nil, core.NewError(core.CodeSchema, "sample manifest entry is incomplete").
				At(fmt.Sprintf("samples[%d]", i))
		}
	}
	return &m, nil
}

func Load(dir, id string) (*core.Scenario, error) {
	m, err := LoadManifest(dir)
	if err != nil {
		return nil, err
	}
	for _, e := range m.Samples {
		if e.ID != id {
			continue
		}
		scenario, err := core.LoadScenario(filepath.Join(dir, e.File))
		if err != nil {
			return nil, err
		}
		if e.SceneHash != "" && scenario.SceneHash != e.SceneHash {
			return nil, core.NewError(core.CodeSchema, "sample hash does not match manifest").
				At("manifest.json").WithValues(e.SceneHash, scenario.SceneHash)
		}
		return scenario, nil
	}
	return nil, core.NewError(core.CodeSchema, "sample id is not present in manifest").At("sample_id")
}

func Ensure(dir string, seed uint64) (*Manifest, error) {
	m, err := LoadManifest(dir)
	if err == nil {
		ok := true
		for _, e := range m.Samples {
			if _, statErr := os.Stat(filepath.Join(dir, e.File)); statErr != nil {
				ok = false
				break
			}
		}
		if ok {
			return m, nil
		}
	}
	return Generate(dir, seed, DefaultProfiles())
}

func Build(profile Profile, seed uint64) (*core.Scenario, error) {
	scenarioSeed := det.Seed(seed, profile.ID, "scenario")
	s := &core.Scenario{
		SchemaVersion: core.SchemaVersion,
		ID:            profile.ID,
		Seed:          scenarioSeed,
		TickUnitMS:    100,
		ParamsVersion: "params/" + profile.ID + "/v1",
		FinalTick:     profile.Horizon,
	}
	s.Nodes = buildNodes(scenarioSeed)
	s.Intents = buildIntents(s.Nodes, scenarioSeed, profile.Intents, profile.Horizon)
	if err := core.ValidateScenario(s); err != nil {
		return nil, err
	}
	hash, err := core.ComputeSceneHash(s)
	if err != nil {
		return nil, err
	}
	s.SceneHash = hash
	s.Truth = buildTruth(s, profile.TruthSamples)
	return s, nil
}

func buildNodes(seed uint64) []core.Node {
	mk := func(id string, typ core.NodeType, parent string, active bool, outputs, inputs, capacity map[string]int64, required int, levels int) core.Node {
		rng := det.New(seed, id, "node-params")
		if outputs == nil {
			outputs = map[string]int64{}
		}
		if inputs == nil {
			inputs = map[string]int64{}
		}
		if capacity == nil {
			capacity = map[string]int64{}
		}
		outputs = jitterMap(rng, outputs, 8)
		capacity = jitterMap(rng, capacity, 5)
		n := core.Node{
			ID:              id,
			Type:            typ,
			Name:            id,
			ParentID:        parent,
			Active:          active,
			Level:           0,
			Workers:         required + rng.Intn(2),
			MaxWorkers:      required + 4,
			Outputs:         outputs,
			Inputs:          inputs,
			Capacity:        capacity,
			WorkersRequired: required,
			InitialStock:    make(map[string]int64),
			Upgrades:        make([]core.Upgrade, 0, levels),
		}
		for level := 1; level <= levels; level++ {
			mult := int64(100 + 15*level)
			upOut := scaleMap(outputs, mult)
			upIn := scaleMap(inputs, int64(100+5*level))
			upCap := scaleMap(capacity, int64(100+20*level))
			n.Upgrades = append(n.Upgrades, core.Upgrade{
				Level:           level,
				Outputs:         upOut,
				Inputs:          upIn,
				Capacity:        upCap,
				WorkersRequired: required + level,
			})
		}
		return n
	}

	nodes := []core.Node{
		mk("depot_root", core.NodeWarehouse, "", true, nil,
			map[string]int64{"grain": 0, "ore": 0, "metal": 0, "food": 0, "tools": 0},
			map[string]int64{"grain": 900000, "ore": 900000, "metal": 900000, "food": 900000, "tools": 900000},
			0, 1),
		mk("farm_west", core.NodeFarm, "depot_root", true,
			map[string]int64{"grain": 2100}, nil,
			map[string]int64{"grain": 120000}, 2, 3),
		mk("farm_east", core.NodeFarm, "depot_root", true,
			map[string]int64{"grain": 1800}, nil,
			map[string]int64{"grain": 110000}, 2, 3),
		mk("mine_north", core.NodeMine, "depot_root", true,
			map[string]int64{"ore": 1600}, nil,
			map[string]int64{"ore": 100000}, 3, 3),
		mk("mine_south", core.NodeMine, "depot_root", false,
			map[string]int64{"ore": 1400}, nil,
			map[string]int64{"ore": 95000}, 3, 3),
		mk("mill_west", core.NodeFactory, "farm_west", true,
			map[string]int64{"food": 850}, map[string]int64{"grain": 1050},
			map[string]int64{"food": 80000, "grain": 40000}, 2, 3),
		mk("mill_east", core.NodeFactory, "farm_east", true,
			map[string]int64{"food": 720}, map[string]int64{"grain": 900},
			map[string]int64{"food": 70000, "grain": 35000}, 2, 3),
		mk("smelter_alpha", core.NodeFactory, "mine_north", true,
			map[string]int64{"metal": 700}, map[string]int64{"ore": 1100},
			map[string]int64{"metal": 70000, "ore": 30000}, 2, 3),
		mk("tools_factory", core.NodeFactory, "smelter_alpha", false,
			map[string]int64{"tools": 420}, map[string]int64{"metal": 650},
			map[string]int64{"tools": 60000, "metal": 25000}, 3, 3),
		mk("warehouse_east", core.NodeWarehouse, "tools_factory", true,
			map[string]int64{"food": 0, "tools": 0},
			nil,
			map[string]int64{"food": 300000, "tools": 250000}, 0, 1),
	}
	nodes[0].InitialStock = map[string]int64{
		"grain": 26000, "ore": 22000, "metal": 9000, "food": 12000, "tools": 5000,
	}
	nodes[1].InitialStock = map[string]int64{"grain": 8000}
	nodes[2].InitialStock = map[string]int64{"grain": 7000}
	nodes[3].InitialStock = map[string]int64{"ore": 6500}
	nodes[4].InitialStock = map[string]int64{"ore": 5000}
	nodes[5].InitialStock = map[string]int64{"grain": 12000}
	nodes[6].InitialStock = map[string]int64{"grain": 9000}
	nodes[7].InitialStock = map[string]int64{"ore": 10000}
	nodes[8].InitialStock = map[string]int64{"metal": 7000}
	nodes[9].InitialStock = map[string]int64{"food": 2000, "tools": 1000}
	return nodes
}

func jitterMap(rng *det.RNG, src map[string]int64, pct int64) map[string]int64 {
	if src == nil {
		return nil
	}
	out := make(map[string]int64, len(src))
	for _, k := range resourceKeys(src) {
		v := src[k]
		if v == 0 {
			out[k] = 0
			continue
		}
		out[k] = rng.Jitter(v, pct)
	}
	return out
}

func scaleMap(src map[string]int64, pct int64) map[string]int64 {
	if src == nil {
		return nil
	}
	out := make(map[string]int64, len(src))
	for _, k := range resourceKeys(src) {
		out[k] = src[k] * pct / 100
	}
	return out
}

func buildIntents(nodes []core.Node, seed uint64, count int, horizon int64) []core.Intent {
	if count <= 0 {
		count = 1
	}
	byID := make(map[string]*core.Node, len(nodes))
	for i := range nodes {
		byID[nodes[i].ID] = &nodes[i]
	}
	levels := make(map[string]int, len(nodes))
	workers := make(map[string]int, len(nodes))
	active := make(map[string]bool, len(nodes))
	for _, n := range nodes {
		levels[n.ID] = n.Level
		workers[n.ID] = n.Workers
		active[n.ID] = n.Active
	}
	ids := make([]string, 0, len(nodes))
	for id := range byID {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	intents := make([]core.Intent, 0, count)
	var prevTimestamp int64
	for i := 0; i < count; i++ {
		timestamp := int64(i) * horizon / int64(count)
		if timestamp < prevTimestamp {
			timestamp = prevTimestamp
		} else if i%11 == 5 && timestamp > prevTimestamp {
			timestamp = prevTimestamp
		}
		prevTimestamp = timestamp
		selector := det.New(seed, ids[i%len(ids)], fmt.Sprintf("intent-select-%d", i))
		nodeID := ids[selector.Intn(len(ids))]
		node := byID[nodeID]
		rng := det.New(seed, nodeID, fmt.Sprintf("intent-%d", i))
		choices := validIntentTypes(node, levels[nodeID], workers[nodeID], active[nodeID])
		if len(choices) == 0 {
			choices = []core.IntentType{core.IntentAssignWorker}
		}
		typ := choices[rng.Intn(len(choices))]
		in := core.Intent{Seq: int64(i), Timestamp: timestamp, Type: typ, NodeID: nodeID}
		switch typ {
		case core.IntentBuild:
			active[nodeID] = true
			in.NodeID = nodeID
		case core.IntentUpgrade:
			next := levels[nodeID] + 1
			levels[nodeID] = next
			in.Level = &next
		case core.IntentDemolish:
			active[nodeID] = false
		case core.IntentAssignWorker:
			w := rng.Intn(node.MaxWorkers + 1)
			if w < node.WorkersRequired && rng.Intn(3) != 0 {
				w = node.WorkersRequired
			}
			if w > node.MaxWorkers {
				w = node.MaxWorkers
			}
			workers[nodeID] = w
			in.Workers = &w
		case core.IntentRecipe:
			recipe := currentParams(*node, levels[nodeID])
			recipe = mutateRecipe(rng, recipe)
			in.Recipe = &recipe
		}
		intents = append(intents, in)
	}
	return intents
}

func validIntentTypes(node *core.Node, level, workers int, active bool) []core.IntentType {
	out := make([]core.IntentType, 0, 5)
	if !active {
		out = append(out, core.IntentBuild)
	} else {
		out = append(out, core.IntentAssignWorker, core.IntentRecipe)
		if level < len(node.Upgrades) {
			out = append(out, core.IntentUpgrade)
		}
		if len(out) > 1 {
			out = append(out, core.IntentDemolish)
		}
	}
	return out
}

func currentParams(node core.Node, level int) core.Params {
	if level == 0 {
		return node.BaseParams()
	}
	if up, ok := node.UpgradeFor(level); ok {
		return up.Params()
	}
	return node.BaseParams()
}

func mutateRecipe(rng *det.RNG, p core.Params) core.Params {
	out := p.Clone()
	for _, r := range resourceKeys(out.Outputs) {
		out.Outputs[r] = rng.Jitter(out.Outputs[r], 10)
	}
	for _, r := range resourceKeys(out.Inputs) {
		out.Inputs[r] = rng.Jitter(out.Inputs[r], 8)
	}
	for _, r := range resourceKeys(out.Capacity) {
		out.Capacity[r] = rng.Jitter(out.Capacity[r], 6)
	}
	return out
}

func resourceKeys(m map[string]int64) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func buildTruth(s *core.Scenario, count int) core.Truth {
	if count < 2 {
		count = 2
	}
	ticks := make([]int64, 0, count+1)
	for i := 0; i < count; i++ {
		ticks = append(ticks, int64(i)*s.FinalTick/int64(count))
	}
	ticks = append(ticks, s.FinalTick)
	ticks = uniqueSorted(ticks)
	engine, err := core.NewEngine(s)
	if err != nil {
		panic(err)
	}
	truth := core.Truth{SampleTicks: append([]int64(nil), ticks...)}
	for _, tick := range ticks {
		if err := engine.AdvanceTo(tick); err != nil {
			panic(err)
		}
		truth.Snapshots = append(truth.Snapshots, core.TruthFromSnapshot(engine.Snapshot()))
	}
	return truth
}

func uniqueSorted(in []int64) []int64 {
	seen := make(map[int64]struct{}, len(in))
	out := make([]int64, 0, len(in))
	for _, v := range in {
		if _, ok := seen[v]; ok {
			continue
		}
		seen[v] = struct{}{}
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}
