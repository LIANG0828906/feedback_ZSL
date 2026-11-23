package story

import (
	"fmt"
	"sort"
	"strings"
)

var validOps = map[string]bool{
	"eq": true, "ne": true, "gt": true, "gte": true,
	"lt": true, "lte": true, "exists": true, "not_exists": true,
}

var validEventKinds = map[string]bool{
	"set_flag": true, "inc_flag": true,
	"set_character": true, "inc_character": true,
}

// ValidateProject performs semantic validation. The first issue is returned
// with the stable error code and the affected node/field.
func ValidateProject(p *Project) error {
	if p == nil {
		return NewError(CodeSchema, "", "project", "project is nil")
	}
	if p.SchemaVersion != ProjectSchemaVersion {
		return NewError(CodeSchema, "", "schema_version", fmt.Sprintf("unsupported schema version %d", p.SchemaVersion))
	}
	if strings.TrimSpace(p.ID) == "" {
		return NewError(CodeSchema, "", "id", "project id is required")
	}
	if strings.TrimSpace(p.Title) == "" {
		return NewError(CodeSchema, "", "title", "project title is required")
	}
	if p.InitialFlags == nil {
		return NewError(CodeSchema, "", "initial_flags", "must be an object")
	}
	if p.Characters == nil {
		return NewError(CodeSchema, "", "characters", "must be an object")
	}
	if len(p.Nodes) == 0 {
		return NewError(CodeSchema, "", "nodes", "at least one node is required")
	}
	if len(p.Choices) == 0 {
		return NewError(CodeSchema, "", "choices", "at least one choice is required")
	}

	for key, value := range p.InitialFlags {
		if !validScalar(value) {
			return NewError(CodeSchema, "", "initial_flags."+key, "flag value must be bool, string or number")
		}
	}
	for charID, char := range p.Characters {
		if strings.TrimSpace(charID) == "" {
			return NewError(CodeSchema, "", "characters", "character id must not be empty")
		}
		if strings.TrimSpace(char.Name) == "" {
			return NewError(CodeSchema, "", "characters."+charID+".name", "character name is required")
		}
		if char.State == nil {
			return NewError(CodeSchema, "", "characters."+charID+".state", "must be an object")
		}
		for key, value := range char.State {
			if !validScalar(value) {
				return NewError(CodeSchema, "", "characters."+charID+".state."+key, "state value must be bool, string or number")
			}
		}
	}

	nodeIndex := make(map[string]int, len(p.Nodes))
	eventIDs := map[string]string{}
	for i := range p.Nodes {
		n := &p.Nodes[i]
		if strings.TrimSpace(n.ID) == "" {
			return NewError(CodeSchema, "", fmt.Sprintf("nodes[%d].id", i), "node id is required")
		}
		if _, exists := nodeIndex[n.ID]; exists {
			return NewError(CodeSchema, n.ID, "id", "duplicate node id")
		}
		nodeIndex[n.ID] = i
		if strings.TrimSpace(n.Title) == "" {
			return NewError(CodeSchema, n.ID, "title", "node title is required")
		}
		if len(n.Shots) == 0 {
			return NewError(CodeSchema, n.ID, "shots", "at least one shot is required")
		}
		if len(n.Shots) > MaxShotsPerNode {
			return NewError(CodePara, n.ID, "shots", fmt.Sprintf("shot count exceeds %d", MaxShotsPerNode))
		}
		for j, shot := range n.Shots {
			field := fmt.Sprintf("shots[%d]", j)
			if shot.DurationMS <= 0 || shot.DurationMS > MaxShotDurationMS {
				return NewError(CodePara, n.ID, field+".duration_ms",
					fmt.Sprintf("duration must be an integer in [1,%d] ms", MaxShotDurationMS))
			}
			if len(shot.AspectPriority) == 0 {
				return NewError(CodeSchema, n.ID, field+".aspect_priority", "at least one aspect priority is required")
			}
			if len(shot.AspectPriority) > MaxAspectPriorities {
				return NewError(CodePara, n.ID, field+".aspect_priority",
					fmt.Sprintf("aspect priority count exceeds %d", MaxAspectPriorities))
			}
			seenAspect := map[string]bool{}
			for k, aspect := range shot.AspectPriority {
				aspect = strings.TrimSpace(aspect)
				if aspect == "" {
					return NewError(CodeSchema, n.ID, fmt.Sprintf("%s.aspect_priority[%d]", field, k), "aspect must not be empty")
				}
				if seenAspect[aspect] {
					return NewError(CodeSchema, n.ID, fmt.Sprintf("%s.aspect_priority[%d]", field, k), "duplicate aspect")
				}
				seenAspect[aspect] = true
			}
		}
		seenJoin := map[string]bool{}
		for j, dep := range n.Join {
			if dep == n.ID {
				return NewError(CodeCycle, n.ID, fmt.Sprintf("join[%d]", j), "node cannot join itself")
			}
			if seenJoin[dep] {
				return NewError(CodeSchema, n.ID, fmt.Sprintf("join[%d]", j), "duplicate join reference")
			}
			seenJoin[dep] = true
		}
		seenMutex := map[string]bool{}
		for j, other := range n.Mutex {
			if other == n.ID {
				return NewError(CodeSchema, n.ID, fmt.Sprintf("mutex[%d]", j), "node cannot be mutually exclusive with itself")
			}
			if seenMutex[other] {
				return NewError(CodeSchema, n.ID, fmt.Sprintf("mutex[%d]", j), "duplicate mutex reference")
			}
			seenMutex[other] = true
		}
		for j, cond := range n.Requires {
			if err := validateCondition(p, n.ID, fmt.Sprintf("requires[%d]", j), cond); err != nil {
				return err
			}
		}
		for j, event := range n.Events {
			if err := validateEvent(p, n.ID, fmt.Sprintf("events[%d]", j), event, eventIDs); err != nil {
				return err
			}
		}
	}

	for i := range p.Nodes {
		n := &p.Nodes[i]
		for j, dep := range n.Join {
			if _, ok := nodeIndex[dep]; !ok {
				return NewError(CodeSchema, n.ID, fmt.Sprintf("join[%d]", j), "referenced node does not exist: "+dep)
			}
		}
		for j, other := range n.Mutex {
			if _, ok := nodeIndex[other]; !ok {
				return NewError(CodeSchema, n.ID, fmt.Sprintf("mutex[%d]", j), "referenced node does not exist: "+other)
			}
		}
	}

	var previousAt int64 = -1
	for i, choice := range p.Choices {
		field := fmt.Sprintf("choices[%d]", i)
		if choice.AtMS < 0 {
			return NewError(CodePara, choice.Target, field+".at_ms", "logical timestamp must be non-negative")
		}
		if choice.AtMS < previousAt {
			return NewError(CodePara, choice.Target, field+".at_ms",
				fmt.Sprintf("logical timestamps must be monotonic (%d after %d)", choice.AtMS, previousAt))
		}
		previousAt = choice.AtMS
		if strings.TrimSpace(choice.Target) == "" {
			return NewError(CodeSchema, "", field+".target", "choice target is required")
		}
		if _, ok := nodeIndex[choice.Target]; !ok {
			return NewError(CodeSchema, choice.Target, field+".target", "choice target node does not exist")
		}
		if strings.TrimSpace(choice.Label) == "" {
			return NewError(CodeSchema, choice.Target, field+".label", "choice label is required")
		}
	}

	if _, err := TopologicalOrder(p); err != nil {
		return err
	}
	return nil
}

func validateCondition(p *Project, nodeID, field string, cond Condition) error {
	if cond.Scope != "flag" && cond.Scope != "character" {
		return NewError(CodeSchema, nodeID, field+".scope", "scope must be flag or character")
	}
	if strings.TrimSpace(cond.Key) == "" {
		return NewError(CodeSchema, nodeID, field+".key", "condition key is required")
	}
	if !validOps[cond.Op] {
		return NewError(CodeSchema, nodeID, field+".op", "unsupported comparison operator")
	}
	if cond.Scope == "character" {
		if strings.TrimSpace(cond.Character) == "" {
			return NewError(CodeSchema, nodeID, field+".character", "character reference is required")
		}
		if _, ok := p.Characters[cond.Character]; !ok {
			return NewError(CodeSchema, nodeID, field+".character", "referenced character does not exist")
		}
	}
	if cond.Op != "exists" && cond.Op != "not_exists" {
		if !validScalar(cond.Value) {
			return NewError(CodeSchema, nodeID, field+".value", "comparison value must be bool, string or number")
		}
		if isOrderingOp(cond.Op) && !isNumeric(cond.Value) {
			return NewError(CodeSchema, nodeID, field+".value", "ordering comparison requires a numeric value")
		}
	}
	return nil
}

func validateEvent(p *Project, nodeID, field string, event Event, seen map[string]string) error {
	if strings.TrimSpace(event.ID) == "" {
		return NewError(CodeSchema, nodeID, field+".id", "event id is required")
	}
	if previous, exists := seen[event.ID]; exists {
		return NewError(CodeSchema, nodeID, field+".id", "duplicate event id already used by "+previous)
	}
	seen[event.ID] = nodeID
	if !validEventKinds[event.Kind] {
		return NewError(CodeSchema, nodeID, field+".kind", "unsupported event kind")
	}
	if strings.TrimSpace(event.Key) == "" {
		return NewError(CodeSchema, nodeID, field+".key", "event key is required")
	}
	if !validScalar(event.Value) {
		return NewError(CodeSchema, nodeID, field+".value", "event value must be bool, string or number")
	}
	switch event.Kind {
	case "inc_flag":
		if !isNumeric(event.Value) {
			return NewError(CodeSchema, nodeID, field+".value", "numeric increment requires a numeric value")
		}
	case "set_character", "inc_character":
		if strings.TrimSpace(event.Character) == "" {
			return NewError(CodeSchema, nodeID, field+".character", "character reference is required")
		}
		if _, ok := p.Characters[event.Character]; !ok {
			return NewError(CodeSchema, nodeID, field+".character", "referenced character does not exist")
		}
		if event.Kind == "inc_character" && !isNumeric(event.Value) {
			return NewError(CodeSchema, nodeID, field+".value", "numeric increment requires a numeric value")
		}
	}
	return nil
}

func validScalar(v any) bool {
	switch x := NormalizeValue(v).(type) {
	case nil:
		return false
	case bool, string, int, int32, int64, float32, float64:
		return true
	default:
		_ = x
		return false
	}
}

func isNumeric(v any) bool {
	_, ok := numeric(v)
	return ok
}

func isOrderingOp(op string) bool {
	return op == "gt" || op == "gte" || op == "lt" || op == "lte"
}

// TopologicalOrder returns a stable topological order using project node order
// as the deterministic tie-breaker.
func TopologicalOrder(p *Project) ([]string, error) {
	index := make(map[string]int, len(p.Nodes))
	indegree := make(map[string]int, len(p.Nodes))
	downstream := make(map[string][]int, len(p.Nodes))
	for i, n := range p.Nodes {
		index[n.ID] = i
		indegree[n.ID] = 0
	}
	for i, n := range p.Nodes {
		for _, dep := range n.Join {
			depIndex, ok := index[dep]
			if !ok {
				return nil, NewError(CodeSchema, n.ID, "join", "referenced node does not exist: "+dep)
			}
			indegree[n.ID]++
			downstream[p.Nodes[depIndex].ID] = append(downstream[p.Nodes[depIndex].ID], i)
		}
	}
	ready := make([]int, 0, len(p.Nodes))
	for i, n := range p.Nodes {
		if indegree[n.ID] == 0 {
			ready = append(ready, i)
		}
	}
	sort.Ints(ready)
	order := make([]string, 0, len(p.Nodes))
	for len(ready) > 0 {
		i := ready[0]
		ready = ready[1:]
		id := p.Nodes[i].ID
		order = append(order, id)
		for _, child := range downstream[id] {
			childID := p.Nodes[child].ID
			indegree[childID]--
			if indegree[childID] == 0 {
				ready = append(ready, child)
				sort.Ints(ready)
			}
		}
	}
	if len(order) != len(p.Nodes) {
		for _, n := range p.Nodes {
			if indegree[n.ID] > 0 {
				return nil, NewError(CodeCycle, n.ID, "join", "causal graph contains a cycle")
			}
		}
		return nil, NewError(CodeCycle, "", "join", "causal graph contains a cycle")
	}
	return order, nil
}
