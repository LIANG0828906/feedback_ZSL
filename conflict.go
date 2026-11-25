package macrokit

import (
	"fmt"
	"sort"
	"strings"
)

type stepInterval struct {
	BranchID string
	StepID   string
	Lane     string
	Start    int64
	End      int64
}

func branchIntervals(branch *Branch, triggerFrame int64) []stepInterval {
	out := make([]stepInterval, 0, len(branch.Steps))
	start := triggerFrame + branch.DelayFrames
	for i, step := range branch.Steps {
		lane := step.Lane
		if lane == "" {
			lane = "action:" + step.Action
		}
		out = append(out, stepInterval{
			BranchID: branch.ID, StepID: step.ID, Lane: lane,
			Start: start, End: start + 1 + step.HoldFrames,
		})
		if i+1 < len(branch.Steps) {
			start += branch.Steps[i+1].GapFrames
		}
	}
	return out
}

func intervalsConflict(a, b []stepInterval) bool {
	for _, x := range a {
		for _, y := range b {
			if x.Lane != y.Lane {
				continue
			}
			if x.Start < y.End && y.Start < x.End {
				return true
			}
		}
	}
	return false
}

func conditionSize(c *Condition) int {
	if c == nil {
		return 0
	}
	size := 1
	for _, child := range c.Children {
		size += conditionSize(child)
	}
	return size
}

type conflictEdge struct{ a, b string }

func arbitrate(project *Project, candidates []string, frame int64, policy ArbitrationPolicy) ([]string, map[string]string, []Adjudication, error) {
	ids := append([]string(nil), candidates...)
	sort.Strings(ids)
	if policy == "" {
		policy = ArbitrationDeterministic
	}

	intervals := make(map[string][]stepInterval, len(ids))
	priority := make(map[string]int, len(ids))
	specificity := make(map[string]int, len(ids))
	for _, id := range ids {
		b := project.BranchByID[id]
		intervals[id] = branchIntervals(b, frame)
		priority[id] = b.EffectivePriority()
		specificity[id] = conditionSize(b.Condition)
	}

	conflictsWith := make(map[string][]string)
	edges := make([]conflictEdge, 0)
	for i := 0; i < len(ids); i++ {
		for j := i + 1; j < len(ids); j++ {
			if intervalsConflict(intervals[ids[i]], intervals[ids[j]]) {
				edges = append(edges, conflictEdge{ids[i], ids[j]})
				conflictsWith[ids[i]] = append(conflictsWith[ids[i]], ids[j])
				conflictsWith[ids[j]] = append(conflictsWith[ids[j]], ids[i])
			}
		}
	}

	if policy == ArbitrationStrict {
		for _, edge := range edges {
			if priority[edge.a] == priority[edge.b] {
				set := conflictComponent(conflictsWith, edge.a)
				return nil, nil, nil, &Error{
					Code: EConflict, Frame: frame, ConflictSet: set,
					Message: fmt.Sprintf("ambiguous equal-priority step conflict at frame %d", frame),
				}
			}
		}
	}

	ranked := append([]string(nil), ids...)
	sort.SliceStable(ranked, func(i, j int) bool {
		a, b := ranked[i], ranked[j]
		if priority[a] != priority[b] {
			return priority[a] > priority[b]
		}
		if specificity[a] != specificity[b] {
			return specificity[a] > specificity[b]
		}
		return a < b
	})

	accepted := make([]string, 0, len(ranked))
	suppressed := make(map[string]string)
	adjudications := make([]Adjudication, 0)
	for _, id := range ranked {
		var winner string
		for _, candidateWinner := range accepted {
			if slicesContains(conflictsWith[id], candidateWinner) {
				winner = candidateWinner
				break
			}
		}
		if winner == "" {
			accepted = append(accepted, id)
			continue
		}
		suppressed[id] = winner
		winnerBranch := project.BranchByID[winner]
		loserBranch := project.BranchByID[id]
		basis := arbitrationBasis(winner, id, priority, specificity)
		adjudications = append(adjudications, Adjudication{
			Frame:    frame,
			WinnerID: winner, WinnerMacroID: winnerBranch.MacroID,
			SuppressedID: id, SuppressedMacroID: loserBranch.MacroID,
			Basis: basis, ConflictSet: []string{winner, id},
		})
	}

	sort.Strings(accepted)
	sort.Slice(adjudications, func(i, j int) bool { return adjudications[i].SuppressedID < adjudications[j].SuppressedID })
	return accepted, suppressed, adjudications, nil
}

func arbitrationBasis(winner, loser string, priority, specificity map[string]int) string {
	if priority[winner] != priority[loser] {
		return fmt.Sprintf("explicit_priority:%d>%d", priority[winner], priority[loser])
	}
	if specificity[winner] != specificity[loser] {
		return fmt.Sprintf("condition_specificity:%d>%d", specificity[winner], specificity[loser])
	}
	return fmt.Sprintf("stable_id:%s<%s", winner, loser)
}

func conflictComponent(adj map[string][]string, start string) []string {
	seen := map[string]bool{start: true}
	queue := []string{start}
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		for _, next := range adj[id] {
			if !seen[next] {
				seen[next] = true
				queue = append(queue, next)
			}
		}
	}
	out := make([]string, 0, len(seen))
	for id := range seen {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

func slicesContains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

func formatConflictSet(set []string) string {
	return "[" + strings.Join(set, ",") + "]"
}
