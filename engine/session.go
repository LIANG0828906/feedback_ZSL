package engine

import (
	"fmt"
	"math"
	"sort"
	"strings"

	"storyworkbench/story"
)

type runtimeState struct {
	Flags               map[string]any
	Characters          map[string]map[string]any
	NodeStates          map[string]NodeRuntime
	EventOrder          []string
	BlockedChoices      []BlockedChoice
	Shots               []ShotPresentation
	ChoiceOffset        int
	ClockMS             int64
	CurrentNodeID       string
	CurrentNodeNextShot int
	CurrentChoiceIndex  int
	CurrentChoiceAtMS   int64
	ActivationCounter   int
}

type Session struct {
	project     *story.Project
	choices     []story.Choice
	graph       *story.DependencyGraph
	contentHash string
	mutexPeers  map[string][]string
	state       runtimeState
	finished    bool
}

func NewSession(project *story.Project, choices []story.Choice) (*Session, error) {
	if err := story.ValidateProject(project); err != nil {
		return nil, err
	}
	if choices == nil {
		choices = project.Choices
	}
	if err := validateChoiceView(project, choices); err != nil {
		return nil, err
	}
	graph, err := story.BuildDependencyGraph(project)
	if err != nil {
		return nil, err
	}
	flags := story.CloneAnyMap(project.InitialFlags)
	chars := map[string]map[string]any{}
	for id, ch := range project.Characters {
		chars[id] = story.CloneAnyMap(ch.State)
	}
	nodeStates := make(map[string]NodeRuntime, len(project.Nodes))
	for _, n := range project.Nodes {
		nodeStates[n.ID] = NodeRuntime{}
	}
	peers := make(map[string][]string, len(project.Nodes))
	for _, n := range project.Nodes {
		for _, m := range n.Mutex {
			peers[n.ID] = appendUnique(peers[n.ID], m)
			peers[m] = appendUnique(peers[m], n.ID)
		}
	}
	for id := range peers {
		sort.Strings(peers[id])
	}
	return &Session{
		project:     project,
		choices:     choices,
		graph:       graph,
		contentHash: story.ContentHash(project),
		mutexPeers:  peers,
		state: runtimeState{
			Flags:      flags,
			Characters: chars,
			NodeStates: nodeStates,
			EventOrder: []string{},
			Shots:      []ShotPresentation{},
			ClockMS:    0,
		},
	}, nil
}

func validateChoiceView(project *story.Project, choices []story.Choice) error {
	if len(choices) == 0 {
		return story.NewError(story.CodeSchema, "", "choices", "at least one choice is required")
	}
	targets := make(map[string]bool, len(project.Nodes))
	for _, n := range project.Nodes {
		targets[n.ID] = true
	}
	var previous int64 = -1
	for i, c := range choices {
		if c.AtMS < 0 || c.AtMS < previous {
			return story.NewError(story.CodePara, c.Target, fmt.Sprintf("choices[%d].at_ms", i), "logical timestamps must be monotonic and non-negative")
		}
		if !targets[c.Target] {
			return story.NewError(story.CodeSchema, c.Target, fmt.Sprintf("choices[%d].target", i), "choice target node does not exist")
		}
		previous = c.AtMS
	}
	return nil
}

func appendUnique(in []string, value string) []string {
	for _, current := range in {
		if current == value {
			return in
		}
	}
	return append(in, value)
}

func (s *Session) Project() *story.Project { return s.project }
func (s *Session) ContentHash() string     { return s.contentHash }
func (s *Session) IsDone() bool            { return s.finished }

// NextShot advances to the next validated shot boundary. Blocked choices are
// consumed deterministically and reported rather than retried implicitly.
func (s *Session) NextShot() (ShotPresentation, bool, error) {
	if s.finished {
		return ShotPresentation{}, false, nil
	}
	for {
		if s.state.CurrentNodeID != "" {
			node := s.nodeByID(s.state.CurrentNodeID)
			if node == nil {
				return ShotPresentation{}, false, story.NewError(story.CodeSave, s.state.CurrentNodeID, "current_node_id", "restored current node does not exist")
			}
			if s.state.CurrentNodeNextShot >= len(node.Shots) {
				s.completeCurrentNode()
				continue
			}
			idx := s.state.CurrentNodeNextShot
			def := node.Shots[idx]
			aspect := append([]string(nil), def.AspectPriority...)
			rec := ShotPresentation{
				Sequence:       len(s.state.Shots),
				NodeID:         node.ID,
				NodeTitle:      node.Title,
				NodeShotIndex:  idx,
				DurationMS:     def.DurationMS,
				AspectPriority: aspect,
				Description:    def.Description,
				PresentedAtMS:  s.state.ClockMS,
				ChoiceIndex:    s.state.CurrentChoiceIndex,
				ChoiceAtMS:     s.state.CurrentChoiceAtMS,
				ContentHash:    s.contentHash,
				StateHash:      s.stateHash(),
			}
			s.state.Shots = append(s.state.Shots, rec)
			s.state.ClockMS += def.DurationMS
			s.state.CurrentNodeNextShot++
			if s.state.CurrentNodeNextShot >= len(node.Shots) {
				s.completeCurrentNode()
			}
			return rec, true, nil
		}
		if s.state.ChoiceOffset >= len(s.choices) {
			s.finished = true
			return ShotPresentation{}, false, nil
		}
		choiceIndex := s.state.ChoiceOffset
		choice := s.choices[choiceIndex]
		s.state.ChoiceOffset++
		node := s.nodeByID(choice.Target)
		if node == nil {
			s.state.BlockedChoices = append(s.state.BlockedChoices, BlockedChoice{
				ChoiceIndex: choiceIndex,
				AtMS:        choice.AtMS,
				Target:      choice.Target,
				Reason:      "target_missing",
			})
			continue
		}
		if reason := s.ineligibleReason(node); reason != "" {
			s.state.BlockedChoices = append(s.state.BlockedChoices, BlockedChoice{
				ChoiceIndex: choiceIndex,
				AtMS:        choice.AtMS,
				Target:      choice.Target,
				Reason:      reason,
			})
			continue
		}
		if err := s.activate(node, choiceIndex, choice.AtMS); err != nil {
			return ShotPresentation{}, false, err
		}
	}
}

func (s *Session) completeCurrentNode() {
	if s.state.CurrentNodeID == "" {
		return
	}
	runtime := s.state.NodeStates[s.state.CurrentNodeID]
	runtime.Completed = true
	s.state.NodeStates[s.state.CurrentNodeID] = runtime
	s.state.CurrentNodeID = ""
	s.state.CurrentNodeNextShot = 0
	s.state.CurrentChoiceIndex = 0
	s.state.CurrentChoiceAtMS = 0
}

func (s *Session) nodeByID(id string) *story.Node {
	for i := range s.project.Nodes {
		if s.project.Nodes[i].ID == id {
			return &s.project.Nodes[i]
		}
	}
	return nil
}

func (s *Session) ineligibleReason(node *story.Node) string {
	if runtime := s.state.NodeStates[node.ID]; runtime.Activated {
		return "already_activated"
	}
	for _, dep := range node.Join {
		if !s.state.NodeStates[dep].Completed {
			return "join_missing:" + dep
		}
	}
	for _, peer := range s.mutexPeers[node.ID] {
		if s.state.NodeStates[peer].Activated {
			return "mutex_conflict:" + peer
		}
	}
	for i, cond := range node.Requires {
		if !s.evalCondition(cond) {
			return fmt.Sprintf("condition_failed:requires[%d]:%s", i, conditionField(cond))
		}
	}
	return ""
}

func (s *Session) evalCondition(cond story.Condition) bool {
	var value any
	var exists bool
	if cond.Scope == "character" {
		state, ok := s.state.Characters[cond.Character]
		if ok {
			value, exists = state[cond.Key]
		}
	} else {
		value, exists = s.state.Flags[cond.Key]
	}
	switch cond.Op {
	case "exists":
		return exists
	case "not_exists":
		return !exists
	}
	if !exists {
		return false
	}
	switch cond.Op {
	case "eq":
		return story.ScalarEqual(value, cond.Value)
	case "ne":
		return !story.ScalarEqual(value, cond.Value)
	case "gt", "gte", "lt", "lte":
		left, lok := number(value)
		right, rok := number(cond.Value)
		if !lok || !rok {
			return false
		}
		switch cond.Op {
		case "gt":
			return left > right
		case "gte":
			return left >= right
		case "lt":
			return left < right
		case "lte":
			return left <= right
		}
	}
	return false
}

func (s *Session) activate(node *story.Node, choiceIndex int, choiceAtMS int64) error {
	s.state.ActivationCounter++
	s.state.NodeStates[node.ID] = NodeRuntime{
		Activated:       true,
		ActivationOrder: s.state.ActivationCounter,
		ChoiceIndex:     choiceIndex,
	}
	for _, event := range node.Events {
		if err := s.applyEvent(event); err != nil {
			return err
		}
		s.state.EventOrder = append(s.state.EventOrder, event.ID)
	}
	s.state.CurrentNodeID = node.ID
	s.state.CurrentNodeNextShot = 0
	s.state.CurrentChoiceIndex = choiceIndex
	s.state.CurrentChoiceAtMS = choiceAtMS
	if s.state.ClockMS < choiceAtMS {
		s.state.ClockMS = choiceAtMS
	}
	return nil
}

func (s *Session) applyEvent(event story.Event) error {
	switch event.Kind {
	case "set_flag":
		s.state.Flags[event.Key] = story.NormalizeValue(event.Value)
	case "inc_flag":
		delta, ok := number(event.Value)
		if !ok {
			return story.NewError(story.CodeSchema, "", "event.value", "increment must be numeric")
		}
		current, _ := number(s.state.Flags[event.Key])
		s.state.Flags[event.Key] = normalizeNumber(current + delta)
	case "set_character":
		state := s.state.Characters[event.Character]
		if state == nil {
			return story.NewError(story.CodeSchema, event.Character, "event.character", "character state does not exist")
		}
		state[event.Key] = story.NormalizeValue(event.Value)
	case "inc_character":
		delta, ok := number(event.Value)
		if !ok {
			return story.NewError(story.CodeSchema, event.Character, "event.value", "increment must be numeric")
		}
		state := s.state.Characters[event.Character]
		if state == nil {
			return story.NewError(story.CodeSchema, event.Character, "event.character", "character state does not exist")
		}
		current, _ := number(state[event.Key])
		state[event.Key] = normalizeNumber(current + delta)
	default:
		return story.NewError(story.CodeSchema, "", "event.kind", "unsupported event kind")
	}
	return nil
}

func normalizeNumber(v float64) any {
	if math.Trunc(v) == v && v >= math.MinInt64 && v <= math.MaxInt64 {
		return int64(v)
	}
	return v
}

func number(v any) (float64, bool) {
	switch x := story.NormalizeValue(v).(type) {
	case int:
		return float64(x), true
	case int64:
		return float64(x), true
	case float64:
		return x, true
	default:
		return 0, false
	}
}

func conditionField(cond story.Condition) string {
	if cond.Scope == "character" {
		return "char:" + cond.Character + "." + cond.Key
	}
	return "flag:" + cond.Key
}

func (s *Session) stateHash() string {
	return story.HashJSON(map[string]any{
		"flags":       s.state.Flags,
		"characters":  s.state.Characters,
		"event_order": s.state.EventOrder,
	})
}

func (s *Session) ResolvedShots() []ShotPresentation {
	return append([]ShotPresentation(nil), s.state.Shots...)
}

func (s *Session) Result() *RunResult {
	shots := append([]ShotPresentation(nil), s.state.Shots...)
	nodeStates := make(map[string]NodeRuntime, len(s.state.NodeStates))
	for k, v := range s.state.NodeStates {
		nodeStates[k] = v
	}
	reachable := make([]string, 0)
	activated := make([]string, 0)
	for id, rt := range s.state.NodeStates {
		if rt.Activated {
			activated = append(activated, id)
		}
	}
	sort.Slice(activated, func(i, j int) bool {
		a := s.state.NodeStates[activated[i]]
		b := s.state.NodeStates[activated[j]]
		if a.ActivationOrder != b.ActivationOrder {
			return a.ActivationOrder < b.ActivationOrder
		}
		return activated[i] < activated[j]
	})
	reachable = append(reachable, activated...)
	statsMap := map[string]*NodeDurationStat{}
	var total int64
	for _, shot := range shots {
		stat := statsMap[shot.NodeID]
		if stat == nil {
			stat = &NodeDurationStat{NodeID: shot.NodeID}
			statsMap[shot.NodeID] = stat
		}
		stat.ShotCount++
		stat.DurationMS += shot.DurationMS
		total += shot.DurationMS
	}
	stats := make([]NodeDurationStat, 0, len(statsMap))
	for _, nodeID := range reachable {
		if stat := statsMap[nodeID]; stat != nil {
			stats = append(stats, *stat)
		}
	}
	eventOrder := append([]string(nil), s.state.EventOrder...)
	return &RunResult{
		ProjectID:        s.project.ID,
		ProjectTitle:     s.project.Title,
		SchemaVersion:    story.ProjectSchemaVersion,
		AlgorithmVersion: AlgorithmVersion,
		ContentHash:      s.contentHash,
		Shots:            shots,
		FinalFlags:       story.CloneAnyMap(s.state.Flags),
		FinalCharacters:  story.CloneCharacterState(s.state.Characters),
		NodeStates:       nodeStates,
		ReachableNodes:   reachable,
		EventOrder:       eventOrder,
		BlockedChoices:   append([]BlockedChoice(nil), s.state.BlockedChoices...),
		ChoiceOffset:     s.state.ChoiceOffset,
		ShotCount:        len(shots),
		TotalDurationMS:  total,
		DurationStats:    stats,
		FinalStateHash:   s.stateHash(),
		Done:             s.finished,
	}
}

func Play(project *story.Project, choices []story.Choice) (*RunResult, error) {
	session, err := NewSession(project, choices)
	if err != nil {
		return nil, err
	}
	for {
		_, ok, err := session.NextShot()
		if err != nil {
			return nil, err
		}
		if !ok {
			break
		}
	}
	return session.Result(), nil
}

func PlayToBoundary(project *story.Project, choices []story.Choice, shotBoundary int) (*Session, error) {
	session, err := NewSession(project, choices)
	if err != nil {
		return nil, err
	}
	if shotBoundary < 0 {
		return nil, story.NewError(story.CodePara, "", "shot_boundary", "shot boundary must be non-negative")
	}
	for i := 0; i < shotBoundary; i++ {
		_, ok, err := session.NextShot()
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, story.NewError(story.CodePara, "", "shot_boundary",
				fmt.Sprintf("shot boundary %d exceeds available story shots", shotBoundary))
		}
	}
	return session, nil
}

func (r *RunResult) NodeDurationSummary() string {
	parts := make([]string, 0, len(r.DurationStats))
	for _, stat := range r.DurationStats {
		parts = append(parts, fmt.Sprintf("%s:%dms", stat.NodeID, stat.DurationMS))
	}
	return strings.Join(parts, ",")
}
