package engine

import (
	"fmt"
	"sort"

	"storyboardstudio/internal/contenthash"
	"storyboardstudio/internal/model"
)

type State struct {
	Flags           map[string]bool              `json:"flags"`
	Characters      map[string]map[string]string `json:"characters"`
	EventOrder      []string                     `json:"event_order"`
	Arrived         map[string]bool              `json:"arrived"`
	ConsumedChoices int                          `json:"consumed_choices"`
	CursorMS        int64                        `json:"cursor_ms"`
	LastGlobalIndex int                          `json:"last_global_index"`
	LastNodeID      string                       `json:"last_node_id"`
	LastLocalIndex  int                          `json:"last_local_index"`
	Completed       bool                         `json:"completed"`
}

func NewState() *State {
	return &State{
		Flags:           map[string]bool{},
		Characters:      map[string]map[string]string{},
		Arrived:         map[string]bool{},
		LastGlobalIndex: -1,
		LastLocalIndex:  -1,
	}
}

func (s *State) Clone() *State {
	clone := &State{
		EventOrder:      append([]string(nil), s.EventOrder...),
		Arrived:         make(map[string]bool, len(s.Arrived)),
		Flags:           make(map[string]bool, len(s.Flags)),
		Characters:      make(map[string]map[string]string, len(s.Characters)),
		ConsumedChoices: s.ConsumedChoices,
		CursorMS:        s.CursorMS,
		LastGlobalIndex: s.LastGlobalIndex,
		LastNodeID:      s.LastNodeID,
		LastLocalIndex:  s.LastLocalIndex,
		Completed:       s.Completed,
	}
	for key, value := range s.Arrived {
		clone.Arrived[key] = value
	}
	for key, value := range s.Flags {
		clone.Flags[key] = value
	}
	for character, values := range s.Characters {
		clone.Characters[character] = make(map[string]string, len(values))
		for key, value := range values {
			clone.Characters[character][key] = value
		}
	}
	return clone
}

type ShotObserver func(shot model.PresentedShot, state *State) error

func ValidateBoundary(shot model.PresentedShot, state *State) error {
	if state == nil {
		return &model.Error{Code: model.CodeState, Message: "shot boundary state is nil"}
	}
	if shot.GlobalIndex < 0 || shot.ContentHash == "" || shot.NodeID == "" || shot.ShotID == "" {
		return &model.Error{Code: model.CodeState, ConflictShot: shot.ShotID, Message: "presented shot is not a valid boundary"}
	}
	if state.LastGlobalIndex != shot.GlobalIndex || state.LastNodeID != shot.NodeID || state.LastLocalIndex != shot.LocalIndex {
		return &model.Error{Code: model.CodeState, ConflictShot: shot.ShotID, Message: "shot boundary state does not match presented shot"}
	}
	if len(shot.Samples) == 0 {
		return &model.Error{Code: model.CodeState, ConflictShot: shot.ShotID, Message: "presented shot has no deterministic samples"}
	}
	return nil
}

func Run(project *model.Project) (*model.RunResult, error) {
	return RunWithObserver(project, nil)
}

func RunWithObserver(project *model.Project, observer ShotObserver) (*model.RunResult, error) {
	if project == nil {
		return nil, &model.Error{Code: model.CodeSchema, Message: "project is nil"}
	}
	order, err := model.TopologicalOrder(project.Nodes)
	if err != nil {
		return nil, err
	}
	byID := NodeMap(project.Nodes)
	state := NewState()
	for _, flag := range project.ObservableFlags {
		state.Flags[flag] = false
	}
	result := newResult(project)

	for _, nodeID := range order {
		node := byID[nodeID]
		if state.Arrived[nodeID] {
			continue
		}
		ok, reason := Ready(node, state, byID)
		if !ok {
			_ = reason
			continue
		}
		if err := arrive(project, node, state, result, observer); err != nil {
			return result, err
		}
	}
	if state.ConsumedChoices != len(project.ChoiceSequence.Items) {
		next := state.ConsumedChoices
		actual := "end of sequence"
		if next < len(project.ChoiceSequence.Items) {
			choice := project.ChoiceSequence.Items[next]
			actual = fmt.Sprintf("choice %d -> %s/%s was not consumed", next, choice.NodeID, choice.OptionID)
		}
		return nil, &model.Error{Code: model.CodeChoice, Expected: "all choices consumed by arrived decision nodes",
			Actual: actual, Message: "choice sequence does not match reachable narrative path"}
	}
	state.Completed = true
	result.ConsumedChoices = state.ConsumedChoices
	result.TerminalFlags = copyFlags(state.Flags)
	result.Characters = copyCharacters(state.Characters)
	result.EventOrder = append([]string(nil), state.EventOrder...)
	result.ReachableNodes = sortedTrue(state.Arrived)
	return result, nil
}

func ResumeWithObserver(project *model.Project, initial *State, observer ShotObserver) (*model.RunResult, error) {
	if project == nil || initial == nil {
		return nil, &model.Error{Code: model.CodeState, Message: "resume requires project and checkpoint state"}
	}
	order, err := model.TopologicalOrder(project.Nodes)
	if err != nil {
		return nil, err
	}
	byID := NodeMap(project.Nodes)
	state := initial.Clone()
	for _, flag := range project.ObservableFlags {
		if _, ok := state.Flags[flag]; !ok {
			state.Flags[flag] = false
		}
	}
	result := newResult(project)

	startOrderIndex := 0
	if state.LastNodeID != "" {
		found := false
		for i, id := range order {
			if id == state.LastNodeID {
				startOrderIndex = i
				found = true
				break
			}
		}
		if !found {
			return nil, &model.Error{Code: model.CodeRef, ConflictNode: state.LastNodeID, Message: "checkpoint node no longer exists"}
		}
		node := byID[state.LastNodeID]
		if !state.Arrived[node.ID] {
			return nil, &model.Error{Code: model.CodeState, ConflictNode: node.ID, Message: "checkpoint node was not arrived"}
		}
		if state.LastLocalIndex+1 < len(node.Shots) {
			shots, err := emitShotsFrom(project, node, state.LastLocalIndex+1, state, observer)
			result.Shots = append(result.Shots, shots...)
			if err != nil {
				return result, err
			}
		}
		startOrderIndex++
	}

	for idx := startOrderIndex; idx < len(order); idx++ {
		nodeID := order[idx]
		node := byID[nodeID]
		if state.Arrived[nodeID] {
			continue
		}
		ok, _ := Ready(node, state, byID)
		if !ok {
			continue
		}
		if err := arrive(project, node, state, result, observer); err != nil {
			return result, err
		}
	}
	if state.ConsumedChoices != len(project.ChoiceSequence.Items) {
		return nil, &model.Error{Code: model.CodeChoice,
			Message:  "resumed choice sequence does not match narrative path",
			Expected: fmt.Sprintf("%d choices", len(project.ChoiceSequence.Items)),
			Actual:   fmt.Sprintf("%d consumed", state.ConsumedChoices)}
	}
	state.Completed = true
	result.ConsumedChoices = state.ConsumedChoices
	result.TerminalFlags = copyFlags(state.Flags)
	result.Characters = copyCharacters(state.Characters)
	result.EventOrder = append([]string(nil), state.EventOrder...)
	result.ReachableNodes = sortedTrue(state.Arrived)
	return result, nil
}

func NodeMap(nodes []model.Node) map[string]*model.Node {
	result := make(map[string]*model.Node, len(nodes))
	for i := range nodes {
		result[nodes[i].ID] = &nodes[i]
	}
	return result
}

func Ready(node *model.Node, state *State, byID map[string]*model.Node) (bool, string) {
	for _, flag := range node.Preconditions.All {
		if !state.Flags[flag] {
			return false, "missing required flag " + flag
		}
	}
	if len(node.Preconditions.Any) > 0 {
		matched := false
		for _, flag := range node.Preconditions.Any {
			if state.Flags[flag] {
				matched = true
				break
			}
		}
		if !matched {
			return false, "none of any-flags is set"
		}
	}
	for _, flag := range node.Preconditions.None {
		if state.Flags[flag] {
			return false, "excluded flag is set " + flag
		}
	}

	if node.Convergence != nil {
		arrived := 0
		for _, id := range node.Convergence.Predecessors {
			if state.Arrived[id] {
				arrived++
			}
		}
		if node.Convergence.Policy == "all" && arrived != len(node.Convergence.Predecessors) {
			return false, "convergence all is incomplete"
		}
		if node.Convergence.Policy == "any" && arrived == 0 {
			return false, "convergence any has no arrived predecessor"
		}
	} else {
		for _, parentID := range node.Parents {
			if !state.Arrived[parentID] {
				return false, "parent has not arrived " + parentID
			}
		}
	}

	for _, otherID := range node.MutexWith {
		if state.Arrived[otherID] {
			return false, "mutually exclusive node already arrived " + otherID
		}
	}
	return true, ""
}

func arrive(project *model.Project, node *model.Node, state *State, result *model.RunResult, observer ShotObserver) error {
	state.Arrived[node.ID] = true
	state.EventOrder = append(state.EventOrder, node.ID)
	ApplyEffects(node.OnArrive, state)

	if len(node.Options) > 0 {
		if state.ConsumedChoices >= len(project.ChoiceSequence.Items) {
			return &model.Error{Code: model.CodeChoice, ConflictNode: node.ID,
				Expected: "choice for decision node", Actual: "choice sequence exhausted",
				Message: "missing player choice at decision node"}
		}
		choice := project.ChoiceSequence.Items[state.ConsumedChoices]
		if choice.NodeID != node.ID {
			return &model.Error{Code: model.CodeChoice, ConflictNode: node.ID,
				Path:     fmt.Sprintf("choice_sequence.items[%d]", state.ConsumedChoices),
				Expected: node.ID, Actual: choice.NodeID,
				Message: "next choice targets an unexpected node"}
		}
		var option *model.ChoiceOption
		for i := range node.Options {
			if node.Options[i].ID == choice.OptionID {
				option = &node.Options[i]
				break
			}
		}
		if option == nil {
			return &model.Error{Code: model.CodeChoice, ConflictNode: node.ID, Actual: choice.OptionID,
				Expected: "declared option id", Message: "unknown option in choice sequence"}
		}
		ApplyEffects(option.Effects, state)
		state.ConsumedChoices++
	}

	shots, err := emitShotsFrom(project, node, 0, state, observer)
	result.Shots = append(result.Shots, shots...)
	return err
}

func emitShotsFrom(project *model.Project, node *model.Node, startLocal int, state *State, observer ShotObserver) ([]model.PresentedShot, error) {
	emitted := make([]model.PresentedShot, 0, max(0, len(node.Shots)-startLocal))
	for localIndex := startLocal; localIndex < len(node.Shots); localIndex++ {
		shotSpec := node.Shots[localIndex]
		globalIndex := state.LastGlobalIndex + 1
		startMS := state.CursorMS
		endMS := startMS + shotSpec.DurationMS
		presented := model.PresentedShot{
			GlobalIndex: globalIndex,
			NodeID:      node.ID,
			LocalIndex:  localIndex,
			ShotID:      shotSpec.ID,
			DurationMS:  shotSpec.DurationMS,
			AspectPath:  shotSpec.AspectPath,
			Camera:      shotSpec.Camera,
			Motion:      shotSpec.Motion,
			Transition:  shotSpec.Transition,
			StartMS:     startMS,
			EndMS:       endMS,
		}
		presented.Samples = buildSamples(project, presented)
		presented.ContentHash = contenthash.Of(struct {
			ProjectID  string         `json:"project_id"`
			NodeID     string         `json:"node_id"`
			LocalIndex int            `json:"local_index"`
			ShotID     string         `json:"shot_id"`
			DurationMS int64          `json:"duration_ms"`
			AspectPath string         `json:"aspect_path"`
			Camera     string         `json:"camera"`
			Motion     string         `json:"motion"`
			Transition string         `json:"transition"`
			Samples    []model.Sample `json:"samples"`
		}{project.ID, node.ID, localIndex, shotSpec.ID, shotSpec.DurationMS, shotSpec.AspectPath,
			shotSpec.Camera, shotSpec.Motion, shotSpec.Transition, presented.Samples})

		state.LastGlobalIndex = globalIndex
		state.LastNodeID = node.ID
		state.LastLocalIndex = localIndex
		state.CursorMS = endMS
		emitted = append(emitted, presented)
		if observer != nil {
			if err := observer(presented, state.Clone()); err != nil {
				return emitted, err
			}
		}
	}
	return emitted, nil
}

func buildSamples(project *model.Project, shot model.PresentedShot) []model.Sample {
	interval := project.SampleIntervalMS
	if interval <= 0 {
		interval = 250
	}
	values := []int64{}
	if shot.DurationMS == 0 {
		values = []int64{0}
	} else {
		for at := int64(0); at < shot.DurationMS; at += interval {
			values = append(values, at)
		}
		values = append(values, shot.DurationMS)
	}
	samples := make([]model.Sample, 0, len(values))
	for _, at := range values {
		samples = append(samples, model.Sample{
			GlobalIndex: shot.GlobalIndex,
			ShotIndex:   shot.GlobalIndex,
			NodeID:      shot.NodeID,
			ShotID:      shot.ShotID,
			LocalMS:     at,
			GlobalMS:    shot.StartMS + at,
		})
	}
	return samples
}

func ApplyEffects(effects model.Effects, state *State) {
	for _, flag := range effects.SetFlags {
		state.Flags[flag] = true
	}
	for _, flag := range effects.ClearFlags {
		state.Flags[flag] = false
	}
	for _, change := range effects.Characters {
		if state.Characters[change.CharacterID] == nil {
			state.Characters[change.CharacterID] = map[string]string{}
		}
		state.Characters[change.CharacterID][change.Key] = change.Value
	}
}

func newResult(project *model.Project) *model.RunResult {
	return &model.RunResult{
		ProjectID:        project.ID,
		ContentHash:      contenthash.Of(project),
		AlgorithmVersion: model.AlgorithmVersion,
		StructureVersion: project.Schema,
		TerminalFlags:    map[string]bool{},
		Characters:       map[string]map[string]string{},
	}
}

func copyFlags(input map[string]bool) map[string]bool {
	output := make(map[string]bool, len(input))
	for key, value := range input {
		output[key] = value
	}
	return output
}

func copyCharacters(input map[string]map[string]string) map[string]map[string]string {
	output := make(map[string]map[string]string, len(input))
	for id, values := range input {
		output[id] = make(map[string]string, len(values))
		for key, value := range values {
			output[id][key] = value
		}
	}
	return output
}

func sortedTrue(input map[string]bool) []string {
	output := make([]string, 0, len(input))
	for key, value := range input {
		if value {
			output = append(output, key)
		}
	}
	sort.Strings(output)
	return output
}
