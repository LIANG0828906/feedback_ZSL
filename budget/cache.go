package budget

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

type CacheEntry struct {
	Key    string     `json:"key"`
	NodeID string     `json:"node_id"`
	Tick   int        `json:"tick"`
	Result TickResult `json:"result"`
}

type ResultCache struct {
	ParamVersion string                `json:"param_version"`
	Entries      map[string]CacheEntry `json:"entries"`
}

func NewResultCache(paramVersion string) *ResultCache {
	if paramVersion == "" {
		paramVersion = DefaultParamVersion
	}
	return &ResultCache{ParamVersion: paramVersion, Entries: map[string]CacheEntry{}}
}

func WriteAtomicJSON(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return &Error{Code: ECodeIO, Message: "cannot encode JSON", Field: path, Cause: err.Error()}
	}
	return WriteAtomicBytes(path, data)
}

func WriteAtomicBytes(path string, data []byte) error {
	atomicWriteMu.Lock()
	defer atomicWriteMu.Unlock()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return &Error{Code: ECodeIO, Message: "cannot create directory for atomic file", Field: path, Cause: err.Error()}
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".json-*.tmp")
	if err != nil {
		return &Error{Code: ECodeIO, Message: "cannot create temp JSON", Field: path, Cause: err.Error()}
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return &Error{Code: ECodeIO, Message: "cannot write JSON bytes", Field: path, Cause: err.Error()}
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return &Error{Code: ECodeIO, Message: "cannot sync JSON", Field: path, Cause: err.Error()}
	}
	if err := tmp.Close(); err != nil {
		return &Error{Code: ECodeIO, Message: "cannot close JSON temp", Field: path, Cause: err.Error()}
	}
	if err := os.Rename(tmpName, path); err != nil {
		return &Error{Code: ECodeIO, Message: "cannot atomically rename JSON", Field: path, Cause: err.Error()}
	}
	return nil
}

func LoadResultCache(path string) (*ResultCache, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, &Error{Code: ECodeIO, Message: "cannot read result cache", Field: path, Cause: err.Error()}
	}
	var cache ResultCache
	if err := json.Unmarshal(data, &cache); err != nil {
		return nil, &Error{Code: ECodeDecode, Message: "corrupt result cache", Field: path, Cause: err.Error()}
	}
	if cache.Entries == nil {
		cache.Entries = map[string]CacheEntry{}
	}
	return &cache, nil
}

func (c *ResultCache) Save(path string) error {
	return WriteAtomicJSON(path, c)
}

func BuildBaselineCache(project *Project, report *RunReport, params Params, path string) (*Graph, *ResultCache, error) {
	graph, err := BuildDependencyGraph(project, report)
	if err != nil {
		return nil, nil, err
	}
	keys, err := graph.ComputeKeys(params.ParamVersion)
	if err != nil {
		return nil, nil, err
	}
	cache := NewResultCache(params.ParamVersion)
	for i, state := range report.Ticks {
		nodeID := fmt.Sprintf("tick:%d", state.Tick)
		key := keys[nodeID]
		var arb []ArbitrationRecord
		if i < len(report.Arbitration) {
			arb = report.Arbitration[i]
		}
		state.CacheKey = key
		cache.Entries[key] = CacheEntry{
			Key: key, NodeID: nodeID, Tick: state.Tick,
			Result: TickResult{State: state, Arbitration: arb},
		}
	}
	if err := cache.Save(path); err != nil {
		return nil, nil, err
	}
	return graph, cache, nil
}

type IncrementalReport struct {
	Report          *RunReport        `json:"report"`
	Plan            *InvalidationPlan `json:"plan"`
	ReusedTicks     []int             `json:"reused_ticks"`
	RecomputedTicks []int             `json:"recomputed_ticks"`
}

// RerunWithPersistentCache reloads cache from disk and reuses every contiguous
// unaffected prefix. The first invalidated tick restores serializable state and
// all later ticks recompute because tick results form an upstream key chain.
func RerunWithPersistentCache(
	oldProject *Project,
	oldReport *RunReport,
	newProject *Project,
	params Params,
	cachePath string,
	changedNodes []string,
) (*IncrementalReport, error) {
	oldGraph, err := BuildDependencyGraph(oldProject, oldReport)
	if err != nil {
		return nil, err
	}
	oldKeys, err := oldGraph.ComputeKeys(params.ParamVersion)
	if err != nil {
		return nil, err
	}
	plan, err := PlanInvalidation(oldProject, oldGraph, changedNodes)
	if err != nil {
		return nil, err
	}
	cache, err := LoadResultCache(cachePath)
	if err != nil {
		return nil, err
	}
	newStaticGraph, err := BuildDependencyGraph(newProject, nil)
	if err != nil {
		return nil, err
	}
	newKeys, err := newStaticGraph.ComputeKeys(params.ParamVersion)
	if err != nil {
		return nil, err
	}

	firstInvalid := FirstInvalidTick(plan)
	allStates := []TickState{}
	allArb := [][]ArbitrationRecord{}
	reused := []int{}
	recomputed := []int{}
	prevKey := ""

	reuseUntil := newProject.TickCount
	if firstInvalid >= 0 {
		reuseUntil = firstInvalid
	}
	for tick := 0; tick < reuseUntil; tick++ {
		nodeID := fmt.Sprintf("tick:%d", tick)
		node := oldGraph.Nodes[nodeID]
		key, err := plannedTickKey(node, params.ParamVersion, newKeys, prevKey)
		if err != nil {
			return nil, err
		}
		if oldKeys[nodeID] != key {
			return nil, &Error{Code: ECodeDiff, Message: "reusable tick key unexpectedly changed", Tick: &tick, Node: nodeID, Expected: oldKeys[nodeID], Actual: key}
		}
		entry, ok := cache.Entries[key]
		if !ok {
			return nil, &Error{Code: ECodeStage, Message: "persistent cache miss for reusable tick", Tick: &tick, Node: nodeID}
		}
		entry.Result.State.CacheKey = key
		allStates = append(allStates, entry.Result.State)
		allArb = append(allArb, entry.Result.Arbitration)
		reused = append(reused, tick)
		prevKey = key
	}

	var sim *Simulation
	if firstInvalid >= 0 {
		if firstInvalid == 0 {
			sim, err = NewSimulation(newProject, params)
			if err != nil {
				return nil, err
			}
		} else {
			last := allStates[len(allStates)-1]
			sim, err = restoreFromState(newProject, params, last)
			if err != nil {
				return nil, err
			}
		}

		for tick := firstInvalid; tick < newProject.TickCount; tick++ {
			state, records, err := sim.Step()
			if err != nil {
				return nil, err
			}
			content := tickContentHash(newProject, tick)
			key, err := plannedTickKey(&Node{
				ID: fmt.Sprintf("tick:%d", tick), ContentHash: content, Deps: state.Dependencies,
			}, params.ParamVersion, newKeys, prevKey)
			if err != nil {
				return nil, err
			}
			state.CacheKey = key
			allStates = append(allStates, state)
			allArb = append(allArb, records)
			recomputed = append(recomputed, tick)
			prevKey = key
			cache.Entries[key] = CacheEntry{
				Key: key, NodeID: fmt.Sprintf("tick:%d", tick), Tick: tick,
				Result: TickResult{State: state, Arbitration: records},
			}
		}
		if err := cache.Save(cachePath); err != nil {
			return nil, err
		}
	}

	hash := newProject.ContentHash
	if hash == "" {
		hash, err = newProject.SemanticHash()
		if err != nil {
			return nil, err
		}
	}
	report := &RunReport{
		SourceVersion: newProject.SourceVersion, ContentHash: hash,
		AlgorithmVersion: AlgorithmVersion, ParamVersion: params.ParamVersion,
		Ticks: allStates, Arbitration: allArb,
	}
	return &IncrementalReport{Report: report, Plan: plan, ReusedTicks: reused, RecomputedTicks: recomputed}, nil
}

func restoreFromState(project *Project, params Params, last TickState) (*Simulation, error) {
	sim, err := NewSimulation(project, params)
	if err != nil {
		return nil, err
	}
	sim.nextTick = last.Tick + 1
	sim.resource = last.ResourceCurrent
	sim.intentOffset = last.ConsumedIntentOffset
	for _, state := range last.Skills {
		if _, ok := sim.cooldowns[state.SkillID]; ok {
			sim.cooldowns[state.SkillID] = state.CooldownRemaining
		}
	}
	for _, state := range last.Groups {
		if _, ok := sim.groupCD[state.GroupID]; ok {
			sim.groupCD[state.GroupID] = state.CooldownRemaining
		}
	}
	return sim, nil
}

func plannedTickKey(node *Node, paramVersion string, staticKeys map[string]string, previousTickKey string) (string, error) {
	depKeys := map[string]string{}
	for _, dep := range node.Deps {
		if strings.HasPrefix(dep, "tick:") {
			depKeys[dep] = previousTickKey
			continue
		}
		key, ok := staticKeys[dep]
		if !ok {
			return "", &Error{Code: ECodeStage, Message: "missing static cache key", Node: node.ID, Field: dep}
		}
		depKeys[dep] = key
	}
	upstream := make([]string, 0, len(node.Deps))
	for _, dep := range node.Deps {
		upstream = append(upstream, dep+"="+depKeys[dep])
	}
	sort.Strings(upstream)
	parts := append([]string{"cache-key", paramVersion, node.ID, node.ContentHash}, upstream...)
	return hashStrings(parts...), nil
}

func tickContentHash(project *Project, tick int) string {
	ids := []string{"tick", strconv.Itoa(tick)}
	for _, intent := range project.Intents {
		if intent.Tick == tick {
			ids = append(ids, intent.IntentID+":"+intent.SkillID)
		}
	}
	return hashStrings(ids...)
}

func EditSkillCooldown(project *Project, skillID string, newTicks int64) (*Project, []string, error) {
	clone, err := CloneProject(project)
	if err != nil {
		return nil, nil, err
	}
	found := false
	changed := []string{}
	for i := range clone.Skills {
		if clone.Skills[i].ID == skillID {
			clone.Skills[i].CooldownTicks = newTicks
			found = true
			changed = append(changed, "skill:"+skillID+":cooldown")
		}
	}
	if !found {
		return nil, nil, &Error{Code: ECodeSchema, Message: "unknown skill for cooldown edit", SkillID: skillID}
	}
	clone.ContentHash = clone.ContentHashMust()
	return clone, changed, nil
}

func EditRegen(project *Project, newRegen BP) (*Project, []string, error) {
	clone, err := CloneProject(project)
	if err != nil {
		return nil, nil, err
	}
	if newRegen < 0 {
		return nil, nil, ValidateRegenBoundary(clone.Resource.RegenNode, newRegen, map[string]int64{"regen": int64(newRegen)})
	}
	clone.Resource.RegenFlat = newRegen
	clone.Resource.RegenExpr = &Expr{Kind: "lit", Node: clone.Resource.RegenNode, Value: int64(newRegen)}
	clone.ContentHash = clone.ContentHashMust()
	return clone, []string{clone.Resource.RegenNode}, nil
}

func CloneProject(project *Project) (*Project, error) {
	data, err := json.Marshal(project)
	if err != nil {
		return nil, &Error{Code: ECodeIO, Message: "cannot clone project", Cause: err.Error()}
	}
	var clone Project
	if err := json.Unmarshal(data, &clone); err != nil {
		return nil, &Error{Code: ECodeIO, Message: "cannot clone project", Cause: err.Error()}
	}
	return &clone, nil
}
