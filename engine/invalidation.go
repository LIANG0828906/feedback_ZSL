package engine

import (
	"fmt"
	"sort"
	"strings"

	"storyworkbench/story"
)

type NodeEdit struct {
	NodeID   string             `json:"node_id"`
	Requires *[]story.Condition `json:"requires,omitempty"`
	Events   *[]story.Event     `json:"events,omitempty"`
}

type InvalidatedNode struct {
	NodeID string `json:"node_id"`
	Reason string `json:"reason"`
}

type InvalidatedShot struct {
	Sequence      int    `json:"sequence"`
	NodeID        string `json:"node_id"`
	NodeShotIndex int    `json:"node_shot_index"`
	Reason        string `json:"reason"`
}

type InvalidationReport struct {
	EditedNode              string            `json:"edited_node"`
	InvalidatedNodes        []InvalidatedNode `json:"invalidated_nodes"`
	InvalidatedShots        []InvalidatedShot `json:"invalidated_shots"`
	ReusedShotSequences     []int             `json:"reused_shot_sequences"`
	RecomputedShotSequences []int             `json:"recomputed_shot_sequences"`
	ReplayTopologicalOrder  []string          `json:"replay_topological_order"`
}

type ReplayOutcome struct {
	Project         *story.Project     `json:"project"`
	Run             *RunResult         `json:"run"`
	VerificationRun *RunResult         `json:"verification_run"`
	Invalidation    InvalidationReport `json:"invalidation"`
}

func ApplyEdit(project *story.Project, edit NodeEdit) (*story.Project, error) {
	if edit.Requires == nil && edit.Events == nil {
		return nil, story.NewError(story.CodeSchema, edit.NodeID, "edit", "at least one of requires or events must be provided")
	}
	clone, err := story.CloneProject(project)
	if err != nil {
		return nil, story.NewError(story.CodeSchema, edit.NodeID, "edit", err.Error())
	}
	found := false
	for i := range clone.Nodes {
		if clone.Nodes[i].ID != edit.NodeID {
			continue
		}
		found = true
		if edit.Requires != nil {
			clone.Nodes[i].Requires = append([]story.Condition(nil), (*edit.Requires)...)
		}
		if edit.Events != nil {
			clone.Nodes[i].Events = append([]story.Event(nil), (*edit.Events)...)
		}
	}
	if !found {
		return nil, story.NewError(story.CodeSchema, edit.NodeID, "node_id", "edited node does not exist")
	}
	if err := story.ValidateProject(clone); err != nil {
		return nil, err
	}
	return clone, nil
}

func ApplyEditSequence(project *story.Project, edits []NodeEdit) (*story.Project, error) {
	if len(edits) == 0 {
		return nil, story.NewError(story.CodeSchema, "", "edits", "at least one edit is required")
	}
	current := project
	for i, edit := range edits {
		next, err := ApplyEdit(current, edit)
		if err != nil {
			if appErr, ok := story.AsError(err); ok {
				appErr.Field = fmt.Sprintf("edits[%d].%s", i, appErr.Field)
			}
			return nil, err
		}
		current = next
	}
	return current, nil
}

func PlanInvalidation(oldProject, editedProject *story.Project, oldRun *RunResult, edit NodeEdit) (*InvalidationReport, error) {
	if oldRun == nil {
		return nil, story.NewError(story.CodeSchema, edit.NodeID, "run", "old run result is required")
	}
	if oldRun.ContentHash != story.ContentHash(oldProject) {
		return nil, story.NewError(story.CodeDiverge, edit.NodeID, "content_hash", "baseline run does not belong to the supplied project")
	}
	oldGraph, err := story.BuildDependencyGraph(oldProject)
	if err != nil {
		return nil, err
	}
	editedGraph, err := story.BuildDependencyGraph(editedProject)
	if err != nil {
		return nil, err
	}
	if _, ok := oldRun.NodeStates[edit.NodeID]; !ok {
		return nil, story.NewError(story.CodeSchema, edit.NodeID, "node_id", "edited node is absent from the baseline run")
	}
	reasons := map[string]string{edit.NodeID: "node_edited"}
	queue := []string{edit.NodeID}
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		next := append([]string(nil), oldGraph.Dependents[current]...)
		next = append(next, editedGraph.Dependents[current]...)
		sort.Strings(next)
		for _, dependent := range next {
			if _, seen := reasons[dependent]; seen {
				continue
			}
			reasons[dependent] = invalidationReason(oldGraph, editedGraph, current, dependent, reasons[current])
			queue = append(queue, dependent)
		}
	}
	topo := oldGraph.TopologicalOrder
	invalidNodes := make([]InvalidatedNode, 0, len(reasons))
	for _, nodeID := range topo {
		if reason, ok := reasons[nodeID]; ok {
			invalidNodes = append(invalidNodes, InvalidatedNode{NodeID: nodeID, Reason: reason})
		}
	}
	invalidSet := make(map[string]bool, len(reasons))
	for nodeID := range reasons {
		invalidSet[nodeID] = true
	}
	report := &InvalidationReport{
		EditedNode:              edit.NodeID,
		InvalidatedNodes:        invalidNodes,
		InvalidatedShots:        []InvalidatedShot{},
		ReusedShotSequences:     []int{},
		RecomputedShotSequences: []int{},
	}
	for _, nodeID := range topo {
		if invalidSet[nodeID] {
			report.ReplayTopologicalOrder = append(report.ReplayTopologicalOrder, nodeID)
		}
	}
	for _, shot := range oldRun.Shots {
		if invalidSet[shot.NodeID] {
			report.InvalidatedShots = append(report.InvalidatedShots, InvalidatedShot{
				Sequence:      shot.Sequence,
				NodeID:        shot.NodeID,
				NodeShotIndex: shot.NodeShotIndex,
				Reason:        reasons[shot.NodeID],
			})
		} else {
			report.ReusedShotSequences = append(report.ReusedShotSequences, shot.Sequence)
		}
	}
	return report, nil
}

func invalidationReason(oldGraph, editedGraph *story.DependencyGraph, from, to, inherited string) string {
	kind := ""
	field := ""
	for _, edge := range oldGraph.Edges {
		if edge.From == from && edge.To == to {
			kind, field = edge.Kind, edge.Field
			break
		}
	}
	if kind == "" {
		for _, edge := range editedGraph.Edges {
			if edge.From == from && edge.To == to {
				kind, field = edge.Kind, edge.Field
				break
			}
		}
	}
	switch kind {
	case "join":
		return "causal_downstream:" + from
	case "reads":
		return "reads_invalidated_field:" + strings.TrimPrefix(field, "flag:") + ":" + from
	case "mutex":
		return "mutex_peer:" + from
	default:
		return "downstream:" + from + ":" + inherited
	}
}

// ReplayEdited re-simulates the edited project and atomically assembles a new
// result. Unaffected presented shots are copied from the old result only after
// every semantic field matches the canonical replay; otherwise E_DIVERGE is
// returned and the old result remains untouched.
func ReplayEdited(oldProject *story.Project, choices []story.Choice, oldRun *RunResult, edit NodeEdit) (*ReplayOutcome, error) {
	editedProject, err := ApplyEdit(oldProject, edit)
	if err != nil {
		return nil, err
	}
	if choices == nil {
		choices = oldProject.Choices
	}
	report, err := PlanInvalidation(oldProject, editedProject, oldRun, edit)
	if err != nil {
		return nil, err
	}
	full, err := Play(editedProject, choices)
	if err != nil {
		return nil, err
	}
	invalid := make(map[string]string, len(report.InvalidatedNodes))
	for _, node := range report.InvalidatedNodes {
		invalid[node.NodeID] = node.Reason
	}
	oldByKey := map[string]ShotPresentation{}
	for _, shot := range oldRun.Shots {
		key := shotKey(shot.NodeID, shot.NodeShotIndex)
		oldByKey[key] = shot
	}
	assembled := cloneRun(full)
	for i := range assembled.Shots {
		current := &assembled.Shots[i]
		reason, invalidated := invalid[current.NodeID]
		if invalidated {
			current.Invalidated = true
			current.Recomputed = true
			report.RecomputedShotSequences = append(report.RecomputedShotSequences, current.Sequence)
			_ = reason
			continue
		}
		oldShot, ok := oldByKey[shotKey(current.NodeID, current.NodeShotIndex)]
		if !ok {
			return nil, story.DivergeError(i, current.NodeID, "reused_shot",
				"unaffected node produced a shot with no prior presented counterpart")
		}
		if err := compareShotSemantics(oldShot, *current, i); err != nil {
			return nil, err
		}
		originalSequence := oldShot.Sequence
		reused := oldShot
		reused.Sequence = current.Sequence
		if reused.ContentHash != current.ContentHash {
			reused.OriginalContentHash = reused.ContentHash
			reused.ContentHash = current.ContentHash
		}
		reused.Reused = true
		reused.Invalidated = false
		reused.Recomputed = false
		reused.OriginalSequence = &originalSequence
		*current = reused
	}
	for i := range assembled.Shots {
		if assembled.Shots[i].NodeID != "" {
			continue
		}
	}
	if err := CompareRuns(full, assembled); err != nil {
		return nil, err
	}
	return &ReplayOutcome{
		Project:         editedProject,
		Run:             assembled,
		VerificationRun: full,
		Invalidation:    *report,
	}, nil
}

// ReplayEditedSequence applies multiple node edits in order. Every intermediate
// run is verified before it becomes the baseline for the next edit, so no
// partial or silently rewritten state can escape.
func ReplayEditedSequence(oldProject *story.Project, choices []story.Choice, oldRun *RunResult, edits []NodeEdit) (*ReplayOutcome, error) {
	if len(edits) == 0 {
		return nil, story.NewError(story.CodeSchema, "", "edits", "at least one edit is required")
	}
	currentProject := oldProject
	currentRun := oldRun
	var final *ReplayOutcome
	aggregate := InvalidationReport{
		InvalidatedNodes:        []InvalidatedNode{},
		InvalidatedShots:        []InvalidatedShot{},
		ReusedShotSequences:     []int{},
		RecomputedShotSequences: []int{},
	}
	for _, edit := range edits {
		outcome, err := ReplayEdited(currentProject, choices, currentRun, edit)
		if err != nil {
			return nil, err
		}
		aggregate.EditedNode = edit.NodeID
		mergeInvalidatedNodes(&aggregate, outcome.Invalidation.InvalidatedNodes)
		aggregate.InvalidatedShots = append(aggregate.InvalidatedShots, outcome.Invalidation.InvalidatedShots...)
		aggregate.ReusedShotSequences = append([]int(nil), outcome.Invalidation.ReusedShotSequences...)
		aggregate.RecomputedShotSequences = append([]int(nil), outcome.Invalidation.RecomputedShotSequences...)
		aggregate.ReplayTopologicalOrder = append([]string(nil), outcome.Invalidation.ReplayTopologicalOrder...)
		final = outcome
		currentProject = outcome.Project
		currentRun = outcome.Run
	}
	final.Invalidation = aggregate
	return final, nil
}

func mergeInvalidatedNodes(report *InvalidationReport, nodes []InvalidatedNode) {
	index := map[string]int{}
	for i, node := range report.InvalidatedNodes {
		index[node.NodeID] = i
	}
	for _, node := range nodes {
		if position, ok := index[node.NodeID]; ok {
			if !strings.Contains(report.InvalidatedNodes[position].Reason, node.Reason) {
				report.InvalidatedNodes[position].Reason += " + " + node.Reason
			}
			continue
		}
		index[node.NodeID] = len(report.InvalidatedNodes)
		report.InvalidatedNodes = append(report.InvalidatedNodes, node)
	}
}

func shotKey(nodeID string, index int) string {
	return fmt.Sprintf("%s#%d", nodeID, index)
}

func cloneRun(in *RunResult) *RunResult {
	out := *in
	out.Shots = cloneShots(in.Shots)
	out.FinalFlags = story.CloneAnyMap(in.FinalFlags)
	out.FinalCharacters = story.CloneCharacterState(in.FinalCharacters)
	out.NodeStates = cloneNodeStates(in.NodeStates)
	out.ReachableNodes = append([]string{}, in.ReachableNodes...)
	out.EventOrder = append([]string{}, in.EventOrder...)
	out.BlockedChoices = append([]BlockedChoice{}, in.BlockedChoices...)
	out.DurationStats = append([]NodeDurationStat{}, in.DurationStats...)
	return &out
}

func compareShotSemantics(expected, actual ShotPresentation, sequence int) error {
	if expected.NodeID != actual.NodeID {
		return story.DivergeError(sequence, expected.NodeID, "node_id", fmt.Sprintf("expected %q, got %q", expected.NodeID, actual.NodeID))
	}
	if expected.NodeTitle != actual.NodeTitle {
		return story.DivergeError(sequence, expected.NodeID, "node_title", fmt.Sprintf("expected %q, got %q", expected.NodeTitle, actual.NodeTitle))
	}
	if expected.NodeShotIndex != actual.NodeShotIndex {
		return story.DivergeError(sequence, expected.NodeID, "node_shot_index", fmt.Sprintf("expected %d, got %d", expected.NodeShotIndex, actual.NodeShotIndex))
	}
	if expected.DurationMS != actual.DurationMS {
		return story.DivergeError(sequence, expected.NodeID, "duration_ms", fmt.Sprintf("expected %d, got %d", expected.DurationMS, actual.DurationMS))
	}
	if !sameStrings(expected.AspectPriority, actual.AspectPriority) {
		return story.DivergeError(sequence, expected.NodeID, "aspect_priority", fmt.Sprintf("expected %v, got %v", expected.AspectPriority, actual.AspectPriority))
	}
	if expected.Description != actual.Description {
		return story.DivergeError(sequence, expected.NodeID, "description", fmt.Sprintf("expected %q, got %q", expected.Description, actual.Description))
	}
	if expected.PresentedAtMS != actual.PresentedAtMS {
		return story.DivergeError(sequence, expected.NodeID, "presented_at_ms", fmt.Sprintf("expected %d, got %d", expected.PresentedAtMS, actual.PresentedAtMS))
	}
	if expected.ChoiceIndex != actual.ChoiceIndex {
		return story.DivergeError(sequence, expected.NodeID, "choice_index", fmt.Sprintf("expected %d, got %d", expected.ChoiceIndex, actual.ChoiceIndex))
	}
	if expected.ChoiceAtMS != actual.ChoiceAtMS {
		return story.DivergeError(sequence, expected.NodeID, "choice_at_ms", fmt.Sprintf("expected %d, got %d", expected.ChoiceAtMS, actual.ChoiceAtMS))
	}
	if expected.StateHash != actual.StateHash {
		return story.DivergeError(sequence, expected.NodeID, "state_hash", "state hash differs")
	}
	return nil
}
