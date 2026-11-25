package lipsync

import (
	"fmt"
	"sort"
)

type dsu struct {
	parent []int
	size   []int
}

func newDSU(n int) *dsu {
	d := &dsu{parent: make([]int, n), size: make([]int, n)}
	for i := range d.parent {
		d.parent[i] = i
		d.size[i] = 1
	}
	return d
}

func (d *dsu) find(x int) int {
	for d.parent[x] != x {
		d.parent[x] = d.parent[d.parent[x]]
		x = d.parent[x]
	}
	return x
}

func (d *dsu) union(a, b int) bool {
	ra, rb := d.find(a), d.find(b)
	if ra == rb {
		return false
	}
	if d.size[ra] < d.size[rb] {
		ra, rb = rb, ra
	}
	d.parent[rb] = ra
	d.size[ra] += d.size[rb]
	return true
}

func (d *dsu) componentSize(x int) int {
	return d.size[d.find(x)]
}

type mergeEdge struct {
	index      int
	affinity   int
	startFrame int
}

func PackDialogue(d *Dialogue, qps []QuantizedPhoneme, recipe Recipe) ([]Pack, error) {
	if d == nil {
		return nil, SchemaError("dialogue", "dialogue is nil")
	}
	if len(qps) != len(d.Phonemes) {
		return nil, DialogueError(ECStage, d, "phonemes",
			"quantized phoneme count %d does not match annotations %d", len(qps), len(d.Phonemes))
	}
	n := len(qps)
	decisions := make([]BoundaryDecision, max(0, n-1))
	eligibleEdges := make([]mergeEdge, 0, max(0, n-1))

	for i := 0; i < n-1; i++ {
		affinity := 0
		if qps[i].AffinityToNext != nil {
			affinity = *qps[i].AffinityToNext
		}
		decision := BoundaryDecision{
			BoundaryIndex:  i,
			AfterPhonemeID: qps[i].ID,
			Affinity:       affinity,
			StartFrame:     qps[i].StartFrame,
			Rule:           "affinity_desc,start_frame_asc,boundary_index_asc",
		}
		switch {
		case qps[i].Class == ClassSilence || qps[i+1].Class == ClassSilence:
			decision.Eligible = false
			decision.Reason = "silence_boundary_must_split"
		case affinity < recipe.MergeAffinityThreshold:
			decision.Eligible = false
			decision.Reason = fmt.Sprintf("affinity_below_threshold_%d", recipe.MergeAffinityThreshold)
		default:
			decision.Eligible = true
			decision.Reason = "eligible"
			eligibleEdges = append(eligibleEdges, mergeEdge{
				index:      i,
				affinity:   affinity,
				startFrame: qps[i].StartFrame,
			})
		}
		decisions[i] = decision
	}

	sort.SliceStable(eligibleEdges, func(a, b int) bool {
		if eligibleEdges[a].affinity != eligibleEdges[b].affinity {
			return eligibleEdges[a].affinity > eligibleEdges[b].affinity
		}
		if eligibleEdges[a].startFrame != eligibleEdges[b].startFrame {
			return eligibleEdges[a].startFrame < eligibleEdges[b].startFrame
		}
		return eligibleEdges[a].index < eligibleEdges[b].index
	})

	for _, edge := range eligibleEdges {
		group := make([]int, 0)
		for _, other := range eligibleEdges {
			if other.affinity == edge.affinity {
				group = append(group, other.startFrame)
			}
		}
		if len(group) > 1 {
			sort.Ints(group)
			decisions[edge.index].TieBreakApplied = true
			decisions[edge.index].TieBreakRule = "equal_affinity: earlier_phoneme_start_frame_wins"
			decisions[edge.index].GroupStartFrames = group
		}
	}

	ds := newDSU(n)
	for _, edge := range eligibleEdges {
		i := edge.index
		leftRootSize := ds.componentSize(i)
		rightRootSize := ds.componentSize(i + 1)
		if leftRootSize+rightRootSize > recipe.MaxPhonemesPerPack {
			decisions[i].Selected = false
			decisions[i].Reason = fmt.Sprintf("max_phonemes_per_pack_%d", recipe.MaxPhonemesPerPack)
			continue
		}
		ds.union(i, i+1)
		decisions[i].Selected = true
		decisions[i].Reason = "merged_by_affinity"
	}

	groups := collectGroups(ds, n)
	repairLogs := make([][]RepairDecision, len(groups))
	groups, repairLogs, err := repairMinimumVisibility(d, qps, recipe, groups, repairLogs)
	if err != nil {
		return nil, err
	}

	packs := make([]Pack, 0, len(groups))
	for gi, indices := range groups {
		first, last := indices[0], indices[len(indices)-1]
		startFrame := qps[first].StartFrame
		endFrame := qps[last].EndFrame
		pack := Pack{
			DialogueID:    d.ID,
			ShardIndex:    d.ShardIndex,
			Index:         gi,
			PackID:        fmt.Sprintf("%s-pack-%03d", d.ID, gi),
			PhonemeIDs:    idsFromIndices(qps, indices),
			StartFrame:    startFrame,
			EndFrame:      endFrame,
			LastFrame:     endFrame - 1,
			VisibleFrames: endFrame - startFrame,
			Decision: PackDecision{
				Rule:         "affinity_desc,start_frame_asc,boundary_index_asc; silence hard split; max phonemes cap; min visibility repair",
				TieBreakRule: "equal_affinity: start_frame_asc,boundary_index_asc",
				Repairs:      repairLogs[gi],
			},
		}
		bestWeight := -1
		bestIndex := indices[0]
		for _, phonemeIndex := range indices {
			if qps[phonemeIndex].WeightPermyriad > bestWeight {
				bestWeight = qps[phonemeIndex].WeightPermyriad
				bestIndex = phonemeIndex
			}
		}
		for _, phonemeIndex := range indices {
			qp := qps[phonemeIndex]
			pack.PhonemeDetails = append(pack.PhonemeDetails, PackPhonemeDetail{
				PhonemeID:       qp.ID,
				StartFrame:      qp.StartFrame,
				EndFrame:        qp.EndFrame,
				WeightPermyriad: qp.WeightPermyriad,
				WeightPermille:  qp.WeightPermille,
				Dominant:        phonemeIndex == bestIndex,
			})
		}

		for bi := first; bi < last; bi++ {
			bd := decisions[bi]
			if !bd.Selected {
				bd.Selected = true
				bd.Reason = "selected_for_min_visibility_repair"
			}
			pack.Decision.Boundaries = append(pack.Decision.Boundaries, bd)
		}
		if last < n-1 {
			pack.Decision.Boundaries = append(pack.Decision.Boundaries, decisions[last])
		}
		pack.Decision.Rationale = fmt.Sprintf(
			"pack %03d contains %d phoneme(s), frames [%d,%d), visible=%d, selected_boundaries=%d, repairs=%d",
			gi, len(indices), startFrame, endFrame, pack.VisibleFrames,
			countSelected(pack.Decision.Boundaries), len(pack.Decision.Repairs),
		)
		pack.SummaryHash = PackSummaryHash(&pack)
		packs = append(packs, pack)
	}
	return packs, nil
}

func repairMinimumVisibility(
	d *Dialogue,
	qps []QuantizedPhoneme,
	recipe Recipe,
	groups [][]int,
	logs [][]RepairDecision,
) ([][]int, [][]RepairDecision, error) {
	for g := 0; g < len(groups); g++ {
		for groupVisible(qps, groups[g]) < recipe.MinVisibleFrames {
			currentVisible := groupVisible(qps, groups[g])
			type candidate struct {
				side       string
				groupIndex int
				boundary   int
				affinity   int
				startFrame int
				tieApplied bool
			}
			candidates := make([]candidate, 0, 2)
			first, last := groups[g][0], groups[g][len(groups[g])-1]
			if g > 0 {
				left := groups[g-1]
				boundary := left[len(left)-1]
				if !groupHasSilence(qps, groups[g]) && !groupHasSilence(qps, groups[g-1]) &&
					len(groups[g])+len(groups[g-1]) <= recipe.MaxPhonemesPerPack {
					affinity := affinityAt(qps, boundary)
					candidates = append(candidates, candidate{
						side: "left", groupIndex: g - 1, boundary: boundary,
						affinity: affinity, startFrame: qps[boundary].StartFrame,
					})
				}
			}
			if g+1 < len(groups) {
				boundary := last
				if !groupHasSilence(qps, groups[g]) && !groupHasSilence(qps, groups[g+1]) &&
					len(groups[g])+len(groups[g+1]) <= recipe.MaxPhonemesPerPack {
					affinity := affinityAt(qps, boundary)
					candidates = append(candidates, candidate{
						side: "right", groupIndex: g + 1, boundary: boundary,
						affinity: affinity, startFrame: qps[boundary].StartFrame,
					})
				}
			}
			if len(candidates) == 2 && candidates[0].affinity == candidates[1].affinity {
				candidates[0].tieApplied = true
				candidates[1].tieApplied = true
			}
			sort.SliceStable(candidates, func(a, b int) bool {
				if candidates[a].affinity != candidates[b].affinity {
					return candidates[a].affinity > candidates[b].affinity
				}
				if candidates[a].startFrame != candidates[b].startFrame {
					return candidates[a].startFrame < candidates[b].startFrame
				}
				return candidates[a].boundary < candidates[b].boundary
			})
			if len(candidates) == 0 {
				return nil, nil, DialogueError(ECVis, d,
					fmt.Sprintf("packs[%d].visibleFrames", g),
					"pack at frames [%d,%d) has %d visible frames but minimum is %d and no legal merge exists",
					qps[first].StartFrame, qps[last].EndFrame, currentVisible, recipe.MinVisibleFrames)
			}
			chosen := candidates[0]
			repair := RepairDecision{
				PackIndex:       g,
				WithPackIndex:   chosen.groupIndex,
				BoundaryIndex:   chosen.boundary,
				Affinity:        chosen.affinity,
				StartFrame:      chosen.startFrame,
				VisibleFrames:   currentVisible,
				TieBreakApplied: chosen.tieApplied,
				Reason:          "merge_to_satisfy_min_visible_frames",
			}
			if chosen.affinity < recipe.MergeAffinityThreshold {
				repair.Reason = "visibility_override_low_affinity"
			}
			if chosen.side == "right" {
				groups[g] = append(groups[g], groups[g+1]...)
				logs[g] = append(logs[g], repair)
				groups = append(groups[:g+1], groups[g+2:]...)
				logs = append(logs[:g+1], logs[g+2:]...)
			} else {
				target := g - 1
				repair.PackIndex = target
				groups[target] = append(groups[target], groups[g]...)
				logs[target] = append(logs[target], repair)
				groups = append(groups[:g], groups[g+1:]...)
				logs = append(logs[:g], logs[g+1:]...)
				g = max(0, target-1)
			}
		}
	}
	for gi, indices := range groups {
		if groupVisible(qps, indices) < recipe.MinVisibleFrames {
			first, last := indices[0], indices[len(indices)-1]
			return nil, nil, DialogueError(ECVis, d,
				fmt.Sprintf("packs[%d].visibleFrames", gi),
				"pack at frames [%d,%d) has %d visible frames but minimum is %d",
				qps[first].StartFrame, qps[last].EndFrame,
				groupVisible(qps, indices), recipe.MinVisibleFrames)
		}
	}
	return groups, logs, nil
}

func collectGroups(ds *dsu, n int) [][]int {
	byRoot := map[int][]int{}
	roots := make([]int, n)
	for i := 0; i < n; i++ {
		root := ds.find(i)
		roots[i] = root
		byRoot[root] = append(byRoot[root], i)
	}
	groups := make([][]int, 0)
	seenRoot := map[int]bool{}
	for i := 0; i < n; i++ {
		root := roots[i]
		if seenRoot[root] {
			continue
		}
		seenRoot[root] = true
		groups = append(groups, byRoot[root])
	}
	sort.Slice(groups, func(i, j int) bool { return groups[i][0] < groups[j][0] })
	return groups
}

func groupVisible(qps []QuantizedPhoneme, indices []int) int {
	return qps[indices[len(indices)-1]].EndFrame - qps[indices[0]].StartFrame
}

func groupHasSilence(qps []QuantizedPhoneme, indices []int) bool {
	for _, i := range indices {
		if qps[i].Class == ClassSilence {
			return true
		}
	}
	return false
}

func affinityAt(qps []QuantizedPhoneme, boundary int) int {
	if boundary < 0 || boundary >= len(qps)-1 || qps[boundary].AffinityToNext == nil {
		return 0
	}
	return *qps[boundary].AffinityToNext
}

func idsFromIndices(qps []QuantizedPhoneme, indices []int) []string {
	ids := make([]string, 0, len(indices))
	for _, i := range indices {
		ids = append(ids, qps[i].ID)
	}
	return ids
}

func countSelected(decisions []BoundaryDecision) int {
	count := 0
	for _, decision := range decisions {
		if decision.Selected {
			count++
		}
	}
	return count
}
