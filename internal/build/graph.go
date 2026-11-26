package build

import (
	"fmt"
	"sort"
	"strings"

	"layerforge/internal/apperr"
	"layerforge/internal/model"
)

type LayerNode struct {
	ID           string
	ProducerStep int
	SizeBytes    int64
	DependsOn    []string
	Consumers    []int
}

type Graph struct {
	Project    model.Project
	BaseLayers map[string]model.BaseLayer
	Steps      map[int]model.Step
	Layers     map[string]LayerNode
	Topo       []int
}

func BuildGraph(project model.Project) (*Graph, error) {
	g := &Graph{
		Project:    project,
		BaseLayers: make(map[string]model.BaseLayer, len(project.Bases)),
		Steps:      make(map[int]model.Step, len(project.Steps)),
		Layers:     make(map[string]LayerNode, len(project.Bases)+len(project.Steps)),
	}
	for _, base := range project.Bases {
		if base.ID == "" {
			return nil, apperr.New(apperr.GraphDuplicate, "validate-graph", "baseLayers", "empty base layer id")
		}
		if _, exists := g.BaseLayers[base.ID]; exists {
			return nil, apperr.WithLayers(apperr.New(apperr.GraphDuplicate, "validate-graph", "baseLayers", "duplicate base layer"), base.ID)
		}
		if _, exists := g.Layers[base.ID]; exists {
			return nil, apperr.WithLayers(apperr.New(apperr.GraphDuplicate, "validate-graph", "baseLayers", "base layer collides with step output"), base.ID)
		}
		if base.SizeBytes <= 0 || base.Digest == "" {
			return nil, apperr.WithLayers(apperr.New(apperr.LayerMetadata, "validate-graph", "baseLayers", "base layer requires positive size and digest"), base.ID)
		}
		g.BaseLayers[base.ID] = base
		g.Layers[base.ID] = LayerNode{
			ID:        base.ID,
			SizeBytes: base.SizeBytes,
		}
	}

	for _, step := range project.Steps {
		if step.Number <= 0 {
			return nil, apperr.WithSteps(apperr.New(apperr.GraphDuplicate, "validate-graph", "steps", "step numbers must be positive"), step.Number)
		}
		if _, exists := g.Steps[step.Number]; exists {
			return nil, apperr.WithSteps(apperr.New(apperr.GraphDuplicate, "validate-graph", "steps", "duplicate step number"), step.Number)
		}
		if step.Name == "" || step.CommandFingerprint == "" {
			return nil, apperr.WithSteps(apperr.New(apperr.SchemaRequired, "validate-graph", "steps", "step name and command fingerprint are required"), step.Number)
		}
		if step.Output.ID == "" || step.Output.SizeBytes <= 0 {
			return nil, apperr.WithSteps(apperr.New(apperr.LayerMetadata, "validate-graph", fmt.Sprintf("steps[%d].output", step.Number), "output layer requires id and positive size"), step.Number)
		}
		if _, exists := g.Layers[step.Output.ID]; exists {
			return nil, apperr.WithLayers(apperr.WithSteps(apperr.New(apperr.GraphDuplicate, "validate-graph", "steps.output", "duplicate output layer id"), step.Number), step.Output.ID)
		}
		if err := validateUniqueInputs(step); err != nil {
			return nil, err
		}
		if err := validateUniqueDeps(step); err != nil {
			return nil, err
		}
		node := LayerNode{
			ID:           step.Output.ID,
			ProducerStep: step.Number,
			SizeBytes:    step.Output.SizeBytes,
			DependsOn:    append([]string(nil), step.DependsOn...),
		}
		g.Layers[step.Output.ID] = node
		g.Steps[step.Number] = step
	}

	for _, step := range project.Steps {
		for _, dependency := range step.DependsOn {
			if _, ok := g.Layers[dependency]; !ok {
				return nil, apperr.WithLayers(
					apperr.WithSteps(apperr.New(apperr.GraphMissingRef, "validate-graph", "steps.dependsOn", "dependency layer does not exist"), step.Number),
					dependency,
				)
			}
			dependencyNode := g.Layers[dependency]
			dependencyNode.Consumers = appendUniqueInt(dependencyNode.Consumers, step.Number)
			g.Layers[dependency] = dependencyNode
		}
	}

	if err := validateShares(g); err != nil {
		return nil, err
	}
	topo, err := topologicalSteps(g)
	if err != nil {
		return nil, err
	}
	g.Topo = topo
	return g, nil
}

func validateUniqueInputs(step model.Step) error {
	seen := make(map[string]string, len(step.Inputs))
	for _, input := range step.Inputs {
		if input.Path == "" || input.Digest == "" {
			return apperr.WithSteps(apperr.New(apperr.SchemaRequired, "validate-graph", "steps.inputs", "input path and digest are required"), step.Number)
		}
		if previous, exists := seen[input.Path]; exists {
			detail := "duplicate input path"
			if previous != input.Digest {
				detail = "duplicate input path with conflicting digests"
			}
			return apperr.WithSteps(apperr.New(apperr.InputConflict, "validate-graph", "steps.inputs", detail), step.Number)
		}
		seen[input.Path] = input.Digest
	}
	return nil
}

func validateUniqueDeps(step model.Step) error {
	seen := make(map[string]bool, len(step.DependsOn))
	for _, dependency := range step.DependsOn {
		if dependency == "" {
			return apperr.WithSteps(apperr.New(apperr.SchemaRequired, "validate-graph", "steps.dependsOn", "empty dependency"), step.Number)
		}
		if seen[dependency] {
			return apperr.WithSteps(apperr.New(apperr.GraphDuplicate, "validate-graph", "steps.dependsOn", "duplicate dependency"), step.Number)
		}
		seen[dependency] = true
	}
	return nil
}

func validateShares(g *Graph) error {
	declared := make(map[string][]int, len(g.Project.Shares))
	for _, share := range g.Project.Shares {
		if _, exists := declared[share.LayerID]; exists {
			return apperr.WithLayers(apperr.New(apperr.GraphDuplicate, "validate-shares", "shares", "duplicate share declaration"), share.LayerID)
		}
		if _, exists := g.Layers[share.LayerID]; !exists {
			return apperr.WithLayers(apperr.New(apperr.GraphMissingRef, "validate-shares", "shares.layer", "shared layer does not exist"), share.LayerID)
		}
		declared[share.LayerID] = appendUniqueInts(nil, share.Consumers)
		sort.Ints(declared[share.LayerID])
	}

	for layerID, node := range g.Layers {
		actual := appendUniqueInts(nil, node.Consumers)
		sort.Ints(actual)
		shareConsumers, isDeclared := declared[layerID]
		if isDeclared {
			if len(actual) < 2 {
				return apperr.WithLayers(
					apperr.WithSteps(apperr.New(apperr.GraphShareConflict, "validate-shares", "shares", "shared layer has fewer than two consumers"), actual...),
					layerID,
				)
			}
			if !equalInts(actual, shareConsumers) {
				steps := symmetricDifference(actual, shareConsumers)
				return apperr.WithLayers(
					apperr.WithSteps(apperr.New(apperr.GraphShareConflict, "validate-shares", "shares", "declared consumers do not match steps that use the layer"), steps...),
					layerID,
				)
			}
			delete(declared, layerID)
			continue
		}
		if len(actual) > 1 {
			return apperr.WithLayers(
				apperr.WithSteps(apperr.New(apperr.GraphShareMissing, "validate-shares", "shares", "layer reused by multiple steps has no share declaration"), actual...),
				layerID,
			)
		}
	}
	for layerID := range declared {
		return apperr.WithLayers(apperr.New(apperr.GraphShareConflict, "validate-shares", "shares", "share declaration could not be matched"), layerID)
	}
	return nil
}

func topologicalSteps(g *Graph) ([]int, error) {
	indegree := make(map[int]int, len(g.Steps))
	next := make(map[int][]int, len(g.Steps))
	for number := range g.Steps {
		indegree[number] = 0
	}
	for _, step := range g.Steps {
		seen := make(map[int]bool)
		for _, dependency := range step.DependsOn {
			producer := g.Layers[dependency].ProducerStep
			if producer == 0 || seen[producer] {
				continue
			}
			seen[producer] = true
			indegree[step.Number]++
			next[producer] = append(next[producer], step.Number)
		}
	}

	ready := make([]int, 0, len(g.Steps))
	for number, degree := range indegree {
		if degree == 0 {
			ready = append(ready, number)
		}
	}
	sort.Ints(ready)
	var order []int
	for len(ready) > 0 {
		number := ready[0]
		ready = ready[1:]
		order = append(order, number)
		children := append([]int(nil), next[number]...)
		sort.Ints(children)
		for _, child := range children {
			indegree[child]--
			if indegree[child] == 0 {
				ready = append(ready, child)
				sort.Ints(ready)
			}
		}
	}
	if len(order) != len(g.Steps) {
		cycle := findCycle(g)
		return nil, apperr.WithSteps(apperr.New(apperr.GraphCycle, "topological-sort", "steps", "step dependency cycle"), cycle...)
	}
	return order, nil
}

func findCycle(g *Graph) []int {
	state := make(map[int]int, len(g.Steps))
	stack := make([]int, 0, len(g.Steps))
	var cycle []int
	var visit func(int) bool
	visit = func(stepNumber int) bool {
		state[stepNumber] = 1
		stack = append(stack, stepNumber)
		deps := append([]string(nil), g.Steps[stepNumber].DependsOn...)
		sort.Strings(deps)
		for _, dependency := range deps {
			producer := g.Layers[dependency].ProducerStep
			if producer == 0 {
				continue
			}
			switch state[producer] {
			case 1:
				start := 0
				for i, number := range stack {
					if number == producer {
						start = i
						break
					}
				}
				cycle = append([]int(nil), stack[start:]...)
				return true
			case 0:
				if visit(producer) {
					return true
				}
			}
		}
		stack = stack[:len(stack)-1]
		state[stepNumber] = 2
		return false
	}
	for _, number := range sortedStepNumbers(g.Steps) {
		if state[number] == 0 && visit(number) {
			break
		}
	}
	sort.Ints(cycle)
	return cycle
}

func sortedStepNumbers(steps map[int]model.Step) []int {
	out := make([]int, 0, len(steps))
	for number := range steps {
		out = append(out, number)
	}
	sort.Ints(out)
	return out
}

func appendUniqueInt(values []int, value int) []int {
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}

func appendUniqueInts(dst, src []int) []int {
	for _, value := range src {
		dst = appendUniqueInt(dst, value)
	}
	return dst
}

func equalInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func symmetricDifference(a, b []int) []int {
	seen := make(map[int]bool)
	for _, value := range a {
		seen[value] = !seen[value]
	}
	for _, value := range b {
		seen[value] = !seen[value]
	}
	out := make([]int, 0)
	for value, differs := range seen {
		if differs {
			out = append(out, value)
		}
	}
	sort.Ints(out)
	return out
}

func (g *Graph) LayerConsumers(layerID string) []int {
	node, ok := g.Layers[layerID]
	if !ok {
		return nil
	}
	return append([]int(nil), node.Consumers...)
}

func (g *Graph) Downstream(stepNumber int) []int {
	producerLayer := ""
	for layerID, node := range g.Layers {
		if node.ProducerStep == stepNumber {
			producerLayer = layerID
			break
		}
	}
	if producerLayer == "" {
		return nil
	}
	seen := map[int]bool{stepNumber: true}
	queue := []string{producerLayer}
	for len(queue) > 0 {
		layerID := queue[0]
		queue = queue[1:]
		for _, consumer := range g.Layers[layerID].Consumers {
			if seen[consumer] {
				continue
			}
			seen[consumer] = true
			for nextLayerID, node := range g.Layers {
				if node.ProducerStep == consumer {
					queue = append(queue, nextLayerID)
					break
				}
			}
		}
	}
	out := make([]int, 0, len(seen)-1)
	for number := range seen {
		if number != stepNumber {
			out = append(out, number)
		}
	}
	sort.Ints(out)
	return out
}

func (g *Graph) Describe() string {
	var b strings.Builder
	fmt.Fprintf(&b, "topo=%v layers=%d", g.Topo, len(g.Layers))
	return b.String()
}
