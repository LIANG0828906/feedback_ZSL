package model

import (
	"encoding/json"
	"regexp"
	"sort"
	"strings"
)

var aspectPathRE = regexp.MustCompile(`^aspect://[1-9][0-9]*x[1-9][0-9]*$`)

func LoadProject(data []byte) (*Project, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return nil, schemaError("$", "invalid JSON: %v", err)
	}
	required := []string{"schema", "id", "seed", "sample_interval_ms", "tolerance_ms", "nodes", "choice_sequence", "truth"}
	for _, key := range required {
		raw, ok := fields[key]
		if !ok || string(raw) == "null" {
			return nil, schemaError("$."+key, "required field is missing")
		}
	}

	var project Project
	if err := json.Unmarshal(data, &project); err != nil {
		if typeError, ok := err.(*json.UnmarshalTypeError); ok {
			return nil, schemaError(typeError.Field, "invalid field type: expected %s", typeError.Type.String())
		}
		return nil, schemaError("$", "invalid project payload: %v", err)
	}
	if project.Schema != ProjectSchemaV1 {
		return nil, &Error{Code: CodeVersion, Path: "$.schema", Expected: ProjectSchemaV1, Actual: project.Schema,
			Message: "unsupported narrative project schema version"}
	}
	if err := ValidateProject(&project); err != nil {
		return nil, err
	}
	if err := ValidateChoiceReferences(&project); err != nil {
		return nil, err
	}
	if err := ValidateTruthReferences(&project); err != nil {
		return nil, err
	}
	return &project, nil
}

func ValidateProject(project *Project) error {
	if project == nil {
		return schemaError("$", "project is nil")
	}
	if strings.TrimSpace(project.ID) == "" {
		return schemaError("$.id", "project id is required")
	}
	if project.SampleIntervalMS <= 0 {
		return schemaError("$.sample_interval_ms", "sample_interval_ms must be a positive integer")
	}
	if project.ToleranceMS < 0 {
		return schemaError("$.tolerance_ms", "tolerance_ms must be non-negative")
	}

	byID := make(map[string]*Node, len(project.Nodes))
	order := make([]string, 0, len(project.Nodes))
	for i := range project.Nodes {
		node := &project.Nodes[i]
		path := "$.nodes[" + itoa(i) + "]"
		if strings.TrimSpace(node.ID) == "" {
			return schemaError(path+".id", "node id is required")
		}
		if _, exists := byID[node.ID]; exists {
			return schemaError(path+".id", "duplicate node id %q", node.ID)
		}
		if !node.ShotsPresent && node.Shots == nil {
			return schemaError(path+".shots", "shots field is required; use an explicit empty array for zero shots")
		}
		byID[node.ID] = node
		order = append(order, node.ID)

		for j, shot := range node.Shots {
			shotPath := path + ".shots[" + itoa(j) + "]"
			if strings.TrimSpace(shot.ID) == "" {
				return schemaError(shotPath+".id", "shot id is required")
			}
			if !shot.DurationPresent || shot.DurationMS < 0 {
				return schemaError(shotPath+".duration_ms", "duration_ms must be a present non-negative fixed-point integer")
			}
			if !aspectPathRE.MatchString(shot.AspectPath) {
				return schemaError(shotPath+".aspect_path", "aspect_path %q is not a legal canonical path", shot.AspectPath)
			}
		}
	}

	for _, id := range order {
		node := byID[id]
		for _, parentID := range node.Parents {
			if _, ok := byID[parentID]; !ok {
				return refError(id, "parent reference %q does not exist", parentID)
			}
			if parentID == id {
				return refError(id, "node directly references itself")
			}
		}
		for _, otherID := range node.MutexWith {
			if _, ok := byID[otherID]; !ok {
				return refError(id, "mutex reference %q does not exist", otherID)
			}
		}
		if node.Convergence != nil {
			policy := node.Convergence.Policy
			if policy != "all" && policy != "any" {
				return schemaError("$.nodes."+id+".convergence.policy", "convergence policy must be all or any")
			}
			if len(node.Convergence.Predecessors) == 0 {
				return schemaError("$.nodes."+id+".convergence.predecessors", "at least one predecessor is required")
			}
			for _, predID := range node.Convergence.Predecessors {
				if _, ok := byID[predID]; !ok {
					return refError(id, "convergence predecessor %q does not exist", predID)
				}
			}
		}
		for _, option := range node.Options {
			if strings.TrimSpace(option.ID) == "" {
				return schemaError("$.nodes."+id+".options", "choice option id is required")
			}
		}
	}

	if cyclic := cyclicNodes(project.Nodes, byID); len(cyclic) > 0 {
		first := ""
		for _, id := range order {
			if cyclic[id] {
				first = id
				break
			}
		}
		return refError(first, "causal graph contains a directed cycle")
	}
	return nil
}

func ValidateChoiceReferences(project *Project) error {
	nodeByID := make(map[string]*Node, len(project.Nodes))
	for i := range project.Nodes {
		nodeByID[project.Nodes[i].ID] = &project.Nodes[i]
	}
	for i, choice := range project.ChoiceSequence.Items {
		node, ok := nodeByID[choice.NodeID]
		if !ok {
			return &Error{Code: CodeChoice, Path: "$.choice_sequence.items[" + itoa(i) + "].node_id",
				ConflictNode: choice.NodeID, Message: "choice references unknown node"}
		}
		valid := false
		for _, option := range node.Options {
			if option.ID == choice.OptionID {
				valid = true
				break
			}
		}
		if !valid {
			return &Error{Code: CodeChoice, Path: "$.choice_sequence.items[" + itoa(i) + "].option_id",
				ConflictNode: node.ID, Expected: "one of declared option ids", Actual: choice.OptionID,
				Message: "choice references unknown option"}
		}
	}
	return nil
}

func ValidateTruthReferences(project *Project) error {
	nodeByID := make(map[string]*Node, len(project.Nodes))
	shotByGlobal := make(map[int]bool)
	global := 0
	for i := range project.Nodes {
		node := &project.Nodes[i]
		nodeByID[node.ID] = node
		for j := range node.Shots {
			shotByGlobal[global] = true
			global++
			_ = j
		}
	}
	if project.Truth.TerminalFlags == nil {
		return schemaError("$.truth.terminal_flags", "truth terminal_flags is required")
	}
	if project.Truth.EventOrder == nil {
		return schemaError("$.truth.event_order", "truth event_order is required")
	}
	if project.Truth.ShotSamples == nil {
		return schemaError("$.truth.shot_samples", "truth shot_samples is required")
	}
	for _, nodeID := range project.Truth.EventOrder {
		if _, ok := nodeByID[nodeID]; !ok {
			return refError(nodeID, "truth event order references unknown node")
		}
	}
	for _, nodeID := range project.Truth.ReachableNodes {
		if _, ok := nodeByID[nodeID]; !ok {
			return refError(nodeID, "truth reachable node %q does not exist", nodeID)
		}
	}
	for _, shotTruth := range project.Truth.ShotSamples {
		node, ok := nodeByID[shotTruth.NodeID]
		if !ok {
			return refError(shotTruth.NodeID, "truth shot sample references unknown node")
		}
		found := false
		for _, shot := range node.Shots {
			if shot.ID == shotTruth.ShotID {
				found = true
				break
			}
		}
		if !found {
			return &Error{Code: CodeRef, ConflictNode: node.ID, ConflictShot: shotTruth.ShotID,
				Message: "truth shot sample references unknown shot"}
		}
	}
	return nil
}

func TopologicalOrder(nodes []Node) ([]string, error) {
	byID := make(map[string]*Node, len(nodes))
	indegree := make(map[string]int, len(nodes))
	children := make(map[string][]string, len(nodes))
	for i := range nodes {
		node := &nodes[i]
		byID[node.ID] = node
		indegree[node.ID] = len(node.Parents)
		for _, parent := range node.Parents {
			children[parent] = append(children[parent], node.ID)
		}
	}
	ready := make([]string, 0)
	for id, degree := range indegree {
		if degree == 0 {
			ready = append(ready, id)
		}
	}
	sort.Strings(ready)
	order := make([]string, 0, len(nodes))
	for len(ready) > 0 {
		id := ready[0]
		ready = ready[1:]
		order = append(order, id)
		next := append([]string(nil), children[id]...)
		sort.Strings(next)
		for _, child := range next {
			indegree[child]--
			if indegree[child] == 0 {
				ready = append(ready, child)
				sort.Strings(ready)
			}
		}
	}
	if len(order) != len(nodes) {
		cyclic := cyclicNodes(nodes, byID)
		for i := range nodes {
			if cyclic[nodes[i].ID] {
				return nil, refError(nodes[i].ID, "cannot topologically order cyclic graph")
			}
		}
		return nil, refError("", "cannot topologically order graph")
	}
	return order, nil
}

func cyclicNodes(nodes []Node, byID map[string]*Node) map[string]bool {
	adjacency := make(map[string][]string, len(nodes))
	for i := range nodes {
		id := nodes[i].ID
		for _, other := range nodes {
			for _, parent := range other.Parents {
				if parent == id {
					adjacency[id] = append(adjacency[id], other.ID)
				}
			}
		}
		sort.Strings(adjacency[id])
	}

	index := 0
	indices := make(map[string]int, len(nodes))
	low := make(map[string]int, len(nodes))
	onStack := make(map[string]bool, len(nodes))
	stack := make([]string, 0, len(nodes))
	cyclic := make(map[string]bool)
	var strongConnect func(string)
	strongConnect = func(v string) {
		indices[v] = index
		low[v] = index
		index++
		stack = append(stack, v)
		onStack[v] = true
		for _, w := range adjacency[v] {
			if _, seen := indices[w]; !seen {
				strongConnect(w)
				if low[w] < low[v] {
					low[v] = low[w]
				}
			} else if onStack[w] && indices[w] < low[v] {
				low[v] = indices[w]
			}
		}
		if low[v] == indices[v] {
			component := []string{}
			for {
				w := stack[len(stack)-1]
				stack = stack[:len(stack)-1]
				onStack[w] = false
				component = append(component, w)
				if w == v {
					break
				}
			}
			if len(component) > 1 {
				for _, id := range component {
					cyclic[id] = true
				}
			} else if len(component) == 1 {
				for _, child := range adjacency[v] {
					if child == v {
						cyclic[v] = true
					}
				}
			}
		}
	}
	ids := make([]string, 0, len(nodes))
	for _, node := range nodes {
		ids = append(ids, node.ID)
	}
	sort.Strings(ids)
	for _, id := range ids {
		if _, seen := indices[id]; !seen {
			strongConnect(id)
		}
	}
	return cyclic
}

func itoa(value int) string {
	if value == 0 {
		return "0"
	}
	negative := value < 0
	if negative {
		value = -value
	}
	var buffer [20]byte
	pos := len(buffer)
	for value > 0 {
		pos--
		buffer[pos] = byte('0' + value%10)
		value /= 10
	}
	if negative {
		pos--
		buffer[pos] = '-'
	}
	return string(buffer[pos:])
}
