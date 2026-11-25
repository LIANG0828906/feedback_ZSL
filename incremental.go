package lipsync

import (
	"fmt"
	"sort"
	"strconv"
)

type PackInvalidation struct {
	DialogueID string   `json:"dialogueId"`
	ShardIndex int      `json:"shardIndex"`
	PackIndex  int      `json:"packIndex"`
	PackID     string   `json:"packId"`
	Reasons    []string `json:"reasons"`
	OldKey     string   `json:"oldKey,omitempty"`
	NewKey     string   `json:"newKey,omitempty"`
}

type FrameInvalidation struct {
	Scope      string `json:"scope"`
	Frame      int    `json:"frame"`
	DialogueID string `json:"dialogueId,omitempty"`
	PackID     string `json:"packId,omitempty"`
	Field      string `json:"field"`
	Reason     string `json:"reason"`
}

type InvalidationManifest struct {
	ProjectID        string              `json:"projectId"`
	Revision         int                 `json:"revision"`
	Edit             Edit                `json:"edit"`
	StartedNodes     []string            `json:"startedNodes"`
	CandidateNodes   []string            `json:"candidateNodes"`
	PropagationOrder []string            `json:"propagationOrder"`
	Packs            []PackInvalidation  `json:"packs"`
	DownstreamFrames []FrameInvalidation `json:"downstreamFrames"`
	DialogueFrames   []FrameInvalidation `json:"dialogueFrames"`
	DispatchFrames   []FrameInvalidation `json:"dispatchFrames"`
	ReusedPacks      []string            `json:"reusedPacks"`
	ReusedPhonemes   []string            `json:"reusedPhonemes"`
	CacheStats       CacheStats          `json:"cacheStats"`
}

type Snapshot struct {
	Revision    int
	Project     *Project
	Result      *FullResult
	Graph       *DependencyGraph
	PhonemeKeys map[string]string
	PackKeys    map[string]string
	FrameKeys   map[string]string
}

type Engine struct {
	Project  *Project
	Snapshot *Snapshot
	Cache    *Cache
	Store    *Store
}

func phonemeMapKey(dialogueID string, index int) string {
	return dialogueID + "#phoneme#" + strconv.Itoa(index)
}

func packMapKey(dialogueID string, index int) string {
	return dialogueID + "#pack#" + strconv.Itoa(index)
}

func NewEngine(project *Project, store *Store) (*Engine, error) {
	if err := ValidateProject(project); err != nil {
		return nil, err
	}
	cache := NewCache(store)
	if store != nil {
		if err := cache.LoadPersistent(project.ID); err != nil {
			return nil, err
		}
	}
	result, err := BuildFullResult(project)
	if err != nil {
		return nil, err
	}
	keys, err := warmCaches(project, result, cache, 0)
	if err != nil {
		return nil, err
	}
	graph := BuildDependencyGraph(result)
	engine := &Engine{
		Project: project,
		Cache:   cache,
		Store:   store,
		Snapshot: &Snapshot{
			Revision:    0,
			Project:     project,
			Result:      result,
			Graph:       graph,
			PhonemeKeys: keys.phoneme,
			PackKeys:    keys.pack,
			FrameKeys:   keys.frame,
		},
	}
	return engine, nil
}

type keyMaps struct {
	phoneme map[string]string
	pack    map[string]string
	frame   map[string]string
}

func warmCaches(project *Project, result *FullResult, cache *Cache, revision int) (keyMaps, error) {
	keys := keyMaps{
		phoneme: map[string]string{},
		pack:    map[string]string{},
		frame:   map[string]string{},
	}
	packKeyByID := map[string]string{}
	for _, dr := range result.Dialogues {
		d := dr.Dialogue
		pcmUpstream := CacheKey("pcm-v1", d.PCM.SHA256,
			strconv.Itoa(d.PCM.SampleRate), strconv.Itoa(d.PCM.SampleCount))
		for i, qp := range dr.Quantized {
			key := PhonemeKey(d.ContentHash, qp.ID, project.Recipe.RecipeVersion, pcmUpstream)
			keys.phoneme[phonemeMapKey(d.ID, i)] = key
			entry, err := MakeCacheEntry(project.ID, key, "quantized_phoneme", revision,
				d, project.Recipe.RecipeVersion, []string{pcmUpstream}, qp)
			if err != nil {
				return keys, err
			}
			if err := cache.Put(entry); err != nil {
				return keys, err
			}
		}
		idToIndex := map[string]int{}
		for i, qp := range dr.Quantized {
			idToIndex[qp.ID] = i
		}
		for _, pack := range dr.Packs {
			upstream := make([]string, 0, len(pack.PhonemeIDs))
			for _, id := range pack.PhonemeIDs {
				idx := idToIndex[id]
				upstream = append(upstream, keys.phoneme[phonemeMapKey(d.ID, idx)])
			}
			key := PackKey(d.ContentHash, pack.PackID, project.Recipe.RecipeVersion, pack.PhonemeIDs, upstream)
			keys.pack[packMapKey(d.ID, pack.Index)] = key
			packKeyByID[pack.PackID] = key
			entry, err := MakeCacheEntry(project.ID, key, "mouth_pack", revision,
				d, project.Recipe.RecipeVersion, upstream, pack)
			if err != nil {
				return keys, err
			}
			if err := cache.Put(entry); err != nil {
				return keys, err
			}
		}
	}
	if result.Dispatch != nil {
		for _, frame := range result.Dispatch.Frames {
			upstream := []string{}
			for _, assignment := range frame.Dispatched {
				if key, ok := packKeyByID[assignment.PackID]; ok {
					upstream = append(upstream, key)
				}
			}
			for _, channel := range frame.Channels {
				for _, packID := range channel.PackIDs {
					if key, ok := packKeyByID[packID]; ok {
						upstream = append(upstream, key)
					}
				}
			}
			sort.Strings(upstream)
			key := CacheKey("dispatch-frame-v1", project.ID, project.Recipe.RecipeVersion,
				strconv.Itoa(frame.Frame), HashCanonical("frame", frame), stringsJoin(upstream))
			keys.frame[strconv.Itoa(frame.Frame)] = key
			entry := CacheEntry{
				Key: key, ProjectID: project.ID, Kind: "dispatch_frame",
				RecipeVersion: project.Recipe.RecipeVersion, UpstreamKeys: upstream,
				PayloadJSON: string(CanonicalJSON(frame)), Revision: revision,
			}
			if err := cache.Put(entry); err != nil {
				return keys, err
			}
		}
	}
	return keys, nil
}

func (e *Engine) ApplyEdit(edit Edit) (*FullResult, *InvalidationManifest, error) {
	newProject, err := EditProject(e.Project, edit)
	if err != nil {
		return nil, nil, err
	}
	old := e.Snapshot
	revision := old.Revision + 1
	manifest := &InvalidationManifest{
		ProjectID: newProject.ID,
		Revision:  revision,
		Edit:      edit,
	}
	manifest.StartedNodes = editStartNodes(old.Graph, edit)
	manifest.CandidateNodes = old.Graph.Propagate(manifest.StartedNodes...)
	manifest.PropagationOrder, err = TopologicalNodeOrder(old.Graph, manifest.CandidateNodes)
	if err != nil {
		return nil, nil, err
	}

	recipeEdit := edit.DialogueID == "" || len(edit.Field) >= 7 && edit.Field[:7] == "recipe."
	editReasons := []string{edit.Field + "_changed"}
	newResult := &FullResult{
		ProjectID: newProject.ID,
		Recipe:    newProject.Recipe,
		Dialogues: []*DialogueResult{},
	}
	newKeys := keyMaps{
		phoneme: map[string]string{},
		pack:    map[string]string{},
		frame:   map[string]string{},
	}

	for _, newD := range sortedDialogues(newProject.Dialogues) {
		oldDR := findDialogueResult(old.Result, newD.ID)
		if !recipeEdit && oldDR != nil && oldDR.Dialogue.ContentHash == newD.ContentHash {
			newResult.Dialogues = append(newResult.Dialogues, oldDR)
			copyDialogueKeys(newKeys, old, newD.ID, len(newD.Phonemes), len(oldDR.Packs))
			continue
		}
		dr, inv, reusedPacks, reusedPhonemes, dKeys, frameChanges, err := e.recomputeDialogue(
			newProject, newD, oldDR, old, revision, editReasons,
		)
		if err != nil {
			return nil, manifest, err
		}
		newResult.Dialogues = append(newResult.Dialogues, dr)
		manifest.Packs = append(manifest.Packs, inv...)
		manifest.ReusedPacks = append(manifest.ReusedPacks, reusedPacks...)
		manifest.ReusedPhonemes = append(manifest.ReusedPhonemes, reusedPhonemes...)
		manifest.DialogueFrames = append(manifest.DialogueFrames, frameChanges...)
		for k, v := range dKeys.phoneme {
			newKeys.phoneme[k] = v
		}
		for k, v := range dKeys.pack {
			newKeys.pack[k] = v
		}
	}

	allPacks := []Pack{}
	for _, dr := range newResult.Dialogues {
		allPacks = append(allPacks, dr.Packs...)
		newResult.Truncations = append(newResult.Truncations, dr.Truncations...)
	}
	newResult.Dispatch = DispatchPacks(allPacks, newProject.Recipe)
	manifest.DispatchFrames = diffDispatchFrames(old.Result.Dispatch, newResult.Dispatch)
	manifest.DownstreamFrames = downstreamFramesForInvalidations(old.Graph, manifest.Packs)

	warmed, err := warmCaches(newProject, newResult, e.Cache, revision)
	if err != nil {
		return nil, manifest, err
	}
	mergeKeys(newKeys, warmed)
	newGraph := BuildDependencyGraph(newResult)
	e.Project = newProject
	e.Snapshot = &Snapshot{
		Revision:    revision,
		Project:     newProject,
		Result:      newResult,
		Graph:       newGraph,
		PhonemeKeys: newKeys.phoneme,
		PackKeys:    newKeys.pack,
		FrameKeys:   newKeys.frame,
	}
	manifest.CacheStats = e.Cache.Stats()
	sortInvalidationManifest(manifest)
	return newResult, manifest, nil
}

func (e *Engine) recomputeDialogue(
	project *Project,
	d *Dialogue,
	oldDR *DialogueResult,
	old *Snapshot,
	revision int,
	reasons []string,
) (*DialogueResult, []PackInvalidation, []string, []string, keyMaps, []FrameInvalidation, error) {
	qps, truncations, err := QuantizeDialogue(d, project.Recipe)
	if err != nil {
		return nil, nil, nil, nil, keyMaps{}, nil, err
	}
	packs, err := PackDialogue(d, qps, project.Recipe)
	if err != nil {
		return nil, nil, nil, nil, keyMaps{}, nil, err
	}
	attribution := BuildAttribution(d, qps, packs)
	dr := &DialogueResult{
		Dialogue: d, Quantized: qps, Packs: packs,
		Attribution: attribution, Truncations: truncations,
	}
	keys := keyMaps{phoneme: map[string]string{}, pack: map[string]string{}}
	pcmUpstream := CacheKey("pcm-v1", d.PCM.SHA256,
		strconv.Itoa(d.PCM.SampleRate), strconv.Itoa(d.PCM.SampleCount))
	reusedPhonemes := []string{}
	for i, qp := range qps {
		newKey := PhonemeKey(d.ContentHash, qp.ID, project.Recipe.RecipeVersion, pcmUpstream)
		keys.phoneme[phonemeMapKey(d.ID, i)] = newKey
		oldKey := old.PhonemeKeys[phonemeMapKey(d.ID, i)]
		if oldDR != nil && i < len(oldDR.Quantized) && oldKey != "" &&
			HashCanonical("qp", oldDR.Quantized[i]) == HashCanonical("qp", qp) {
			if _, ok, err := e.Cache.Reuse(oldKey, newKey, revision); err != nil {
				return nil, nil, nil, nil, keys, nil, err
			} else if ok {
				reusedPhonemes = append(reusedPhonemes, qp.ID)
				continue
			}
		}
		entry, err := MakeCacheEntry(project.ID, newKey, "quantized_phoneme", revision,
			d, project.Recipe.RecipeVersion, []string{pcmUpstream}, qp)
		if err != nil {
			return nil, nil, nil, nil, keys, nil, err
		}
		if err := e.Cache.Put(entry); err != nil {
			return nil, nil, nil, nil, keys, nil, err
		}
	}

	invalidations := []PackInvalidation{}
	reusedPacks := []string{}
	idToIndex := map[string]int{}
	for i, qp := range qps {
		idToIndex[qp.ID] = i
	}
	for _, pack := range packs {
		upstream := []string{}
		for _, id := range pack.PhonemeIDs {
			idx := idToIndex[id]
			upstream = append(upstream, keys.phoneme[phonemeMapKey(d.ID, idx)])
		}
		newKey := PackKey(d.ContentHash, pack.PackID, project.Recipe.RecipeVersion, pack.PhonemeIDs, upstream)
		keys.pack[packMapKey(d.ID, pack.Index)] = newKey
		oldKey := old.PackKeys[packMapKey(d.ID, pack.Index)]
		var oldPack *Pack
		if oldDR != nil && pack.Index < len(oldDR.Packs) {
			oldPack = &oldDR.Packs[pack.Index]
		}
		if oldPack != nil && oldKey != "" &&
			HashCanonical("pack", *oldPack) == HashCanonical("pack", pack) {
			if _, ok, err := e.Cache.Reuse(oldKey, newKey, revision); err != nil {
				return nil, invalidations, nil, nil, keys, nil, err
			} else if ok {
				reusedPacks = append(reusedPacks, pack.PackID)
				continue
			}
		}
		entry, err := MakeCacheEntry(project.ID, newKey, "mouth_pack", revision,
			d, project.Recipe.RecipeVersion, upstream, pack)
		if err != nil {
			return nil, invalidations, nil, nil, keys, nil, err
		}
		if err := e.Cache.Put(entry); err != nil {
			return nil, invalidations, nil, nil, keys, nil, err
		}
		oldKeyForReport := oldKey
		invalidations = append(invalidations, PackInvalidation{
			DialogueID: d.ID, ShardIndex: d.ShardIndex, PackIndex: pack.Index,
			PackID: pack.PackID, Reasons: append([]string(nil), reasons...),
			OldKey: oldKeyForReport, NewKey: newKey,
		})
	}
	if oldDR != nil {
		for j := len(packs); j < len(oldDR.Packs); j++ {
			oldPack := oldDR.Packs[j]
			invalidations = append(invalidations, PackInvalidation{
				DialogueID: d.ID, ShardIndex: d.ShardIndex, PackIndex: j,
				PackID: oldPack.PackID, Reasons: []string{"pack_removed_by_partition_change"},
				OldKey: old.PackKeys[packMapKey(d.ID, j)],
			})
		}
	}

	frameChanges := []FrameInvalidation{}
	if oldDR != nil {
		maxFrames := max(len(oldDR.Attribution), len(attribution))
		for f := 0; f < maxFrames; f++ {
			var oldID, newID string
			if f < len(oldDR.Attribution) {
				oldID = oldDR.Attribution[f].PackID
			}
			if f < len(attribution) {
				newID = attribution[f].PackID
			}
			if oldID != newID {
				frameChanges = append(frameChanges, FrameInvalidation{
					Scope: "dialogue_attribution", Frame: f, DialogueID: d.ID,
					PackID: newID, Field: "attribution.packId",
					Reason: fmt.Sprintf("pack attribution %q -> %q", oldID, newID),
				})
			}
		}
	}
	return dr, invalidations, reusedPacks, reusedPhonemes, keys, frameChanges, nil
}

func editStartNodes(graph *DependencyGraph, edit Edit) []string {
	if graph == nil {
		return nil
	}
	if edit.DialogueID == "" || len(edit.Field) >= 7 && edit.Field[:7] == "recipe." {
		ids := []string{}
		for id, node := range graph.Nodes {
			if node.Kind == NodePack {
				ids = append(ids, id)
			}
		}
		sort.Strings(ids)
		return ids
	}
	switch edit.Field {
	case "weight":
		return []string{WeightNodeID(edit.DialogueID, edit.PhonemeIndex)}
	case "affinityToNext":
		idx := edit.PhonemeIndex
		if edit.BoundaryIndex != 0 {
			idx = edit.BoundaryIndex
		}
		return []string{AffinityNodeID(edit.DialogueID, idx)}
	case "sharedBoundary":
		b := edit.BoundaryIndex
		if b == 0 && edit.PhonemeIndex != 0 {
			b = edit.PhonemeIndex
		}
		return []string{
			BoundaryNodeID(edit.DialogueID, b),
			BoundaryNodeID(edit.DialogueID, b+1),
		}
	case "startSample", "endSample":
		return []string{BoundaryNodeID(edit.DialogueID, edit.PhonemeIndex)}
	default:
		return nil
	}
}

func downstreamFramesForInvalidations(graph *DependencyGraph, invalidations []PackInvalidation) []FrameInvalidation {
	if graph == nil {
		return []FrameInvalidation{}
	}
	seen := map[string]bool{}
	out := []FrameInvalidation{}
	for _, invalidation := range invalidations {
		packNode := PackNodeID(invalidation.DialogueID, invalidation.PackIndex)
		for _, nodeID := range graph.Propagate(packNode) {
			node, ok := graph.Nodes[nodeID]
			if !ok {
				continue
			}
			var scope, field string
			switch node.Kind {
			case NodeDialogueFrame:
				scope, field = "downstream_dialogue_frame", "dialogue.frames"
			case NodeDispatchFrame:
				scope, field = "downstream_dispatch_frame", "dispatch.frames"
			default:
				continue
			}
			key := fmt.Sprintf("%s|%s|%d", scope, node.DialogueID, node.Frame)
			if seen[key] {
				continue
			}
			seen[key] = true
			out = append(out, FrameInvalidation{
				Scope: scope, Frame: node.Frame, DialogueID: node.DialogueID,
				PackID: invalidation.PackID, Field: field,
				Reason: "upstream_pack_invalidated:" + stringsJoin(invalidation.Reasons),
			})
		}
	}
	return out
}

func findDialogueResult(result *FullResult, id string) *DialogueResult {
	if result == nil {
		return nil
	}
	for _, dr := range result.Dialogues {
		if dr.Dialogue.ID == id {
			return dr
		}
	}
	return nil
}

func copyDialogueKeys(keys keyMaps, old *Snapshot, dialogueID string, phonemeCount, packCount int) {
	for i := 0; i < phonemeCount; i++ {
		mapKey := phonemeMapKey(dialogueID, i)
		keys.phoneme[mapKey] = old.PhonemeKeys[mapKey]
	}
	for i := 0; i < packCount; i++ {
		mapKey := packMapKey(dialogueID, i)
		keys.pack[mapKey] = old.PackKeys[mapKey]
	}
}

func mergeKeys(target, extra keyMaps) {
	for k, v := range extra.phoneme {
		target.phoneme[k] = v
	}
	for k, v := range extra.pack {
		target.pack[k] = v
	}
	for k, v := range extra.frame {
		target.frame[k] = v
	}
}

func diffDispatchFrames(oldDispatch, newDispatch *DispatchResult) []FrameInvalidation {
	changes := []FrameInvalidation{}
	if oldDispatch == nil || newDispatch == nil {
		return changes
	}
	maxFrames := max(len(oldDispatch.Frames), len(newDispatch.Frames))
	for f := 0; f < maxFrames; f++ {
		if f >= len(oldDispatch.Frames) || f >= len(newDispatch.Frames) {
			changes = append(changes, FrameInvalidation{
				Scope: "dispatch", Frame: f, Field: "dispatch.frameCount",
				Reason: fmt.Sprintf("frame presence changed old=%v new=%v",
					f < len(oldDispatch.Frames), f < len(newDispatch.Frames)),
			})
			continue
		}
		oldFrame := oldDispatch.Frames[f]
		newFrame := newDispatch.Frames[f]
		reason := dispatchFrameDifference(oldFrame, newFrame)
		if reason != "" {
			dialogueID := firstDialogueInFrame(newFrame)
			packID := ""
			if len(newFrame.Dispatched) > 0 {
				packID = newFrame.Dispatched[0].PackID
			}
			changes = append(changes, FrameInvalidation{
				Scope: "dispatch", Frame: f, DialogueID: dialogueID, PackID: packID,
				Field: "dispatch.frame", Reason: reason,
			})
		}
	}
	return changes
}

func dispatchFrameDifference(a, b FrameDispatch) string {
	if a.BudgetUsed != b.BudgetUsed {
		return fmt.Sprintf("budgetUsed %d -> %d", a.BudgetUsed, b.BudgetUsed)
	}
	if a.ActivePackCount != b.ActivePackCount {
		return fmt.Sprintf("activePackCount %d -> %d", a.ActivePackCount, b.ActivePackCount)
	}
	if frameAssignmentSet(a) != frameAssignmentSet(b) {
		return fmt.Sprintf("dispatched set %s -> %s", frameAssignmentSet(a), frameAssignmentSet(b))
	}
	if channelSet(a) != channelSet(b) {
		return fmt.Sprintf("channel set %s -> %s", channelSet(a), channelSet(b))
	}
	if deferralSet(a.DeferredThisFrame) != deferralSet(b.DeferredThisFrame) {
		return "deferral set changed"
	}
	return ""
}

func sortInvalidationManifest(m *InvalidationManifest) {
	sort.SliceStable(m.Packs, func(i, j int) bool {
		if m.Packs[i].ShardIndex != m.Packs[j].ShardIndex {
			return m.Packs[i].ShardIndex < m.Packs[j].ShardIndex
		}
		return m.Packs[i].PackIndex < m.Packs[j].PackIndex
	})
	sort.SliceStable(m.DialogueFrames, func(i, j int) bool {
		if m.DialogueFrames[i].DialogueID != m.DialogueFrames[j].DialogueID {
			return m.DialogueFrames[i].DialogueID < m.DialogueFrames[j].DialogueID
		}
		return m.DialogueFrames[i].Frame < m.DialogueFrames[j].Frame
	})
	sort.SliceStable(m.DispatchFrames, func(i, j int) bool {
		return m.DispatchFrames[i].Frame < m.DispatchFrames[j].Frame
	})
	sort.SliceStable(m.DownstreamFrames, func(i, j int) bool {
		if m.DownstreamFrames[i].Scope != m.DownstreamFrames[j].Scope {
			return m.DownstreamFrames[i].Scope < m.DownstreamFrames[j].Scope
		}
		if m.DownstreamFrames[i].DialogueID != m.DownstreamFrames[j].DialogueID {
			return m.DownstreamFrames[i].DialogueID < m.DownstreamFrames[j].DialogueID
		}
		return m.DownstreamFrames[i].Frame < m.DownstreamFrames[j].Frame
	})
	sort.Strings(m.ReusedPacks)
	sort.Strings(m.ReusedPhonemes)
}
