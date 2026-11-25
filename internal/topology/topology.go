package topology

import (
	"sort"

	"sre-timeline/internal/bizerr"
	"sre-timeline/internal/fixedpoint"
	"sre-timeline/internal/model"
)

const DefaultDecayBP = 5000

type Topology struct {
	Services []string
	Out      map[string][]string // caller -> callees
	In       map[string][]string // callee -> callers/dependents
	edgeSet  map[model.Edge]bool
}

func New(services []model.Service, edges []model.Edge) (*Topology, error) {
	t := &Topology{
		Out:     map[string][]string{},
		In:      map[string][]string{},
		edgeSet: map[model.Edge]bool{},
	}
	known := map[string]bool{}
	for _, svc := range services {
		if svc.ID == "" {
			return nil, bizerr.New(bizerr.CodeSchemaField, "services[].id", "service id is empty")
		}
		if known[svc.ID] {
			return nil, bizerr.New(bizerr.CodeSchemaField, "services[].id", "duplicate service id %q", svc.ID).With("service", svc.ID)
		}
		known[svc.ID] = true
		t.Services = append(t.Services, svc.ID)
		t.Out[svc.ID] = []string{}
		t.In[svc.ID] = []string{}
	}
	sort.Strings(t.Services)
	for i, edge := range edges {
		path := "edges[" + itoa(i) + "]"
		if edge.Caller == "" || edge.Callee == "" {
			return nil, bizerr.New(bizerr.CodeSchemaField, path, "edge endpoint is empty")
		}
		if !known[edge.Caller] {
			return nil, bizerr.New(bizerr.CodeEditTarget, path+".caller", "unknown caller %q", edge.Caller).With("service", edge.Caller)
		}
		if !known[edge.Callee] {
			return nil, bizerr.New(bizerr.CodeEditTarget, path+".callee", "unknown callee %q", edge.Callee).With("service", edge.Callee)
		}
		key := model.Edge{Caller: edge.Caller, Callee: edge.Callee}
		if t.edgeSet[key] {
			continue
		}
		t.edgeSet[key] = true
		t.Out[edge.Caller] = append(t.Out[edge.Caller], edge.Callee)
		t.In[edge.Callee] = append(t.In[edge.Callee], edge.Caller)
	}
	for k := range t.Out {
		sort.Strings(t.Out[k])
	}
	for k := range t.In {
		sort.Strings(t.In[k])
	}
	return t, nil
}

func (t *Topology) Edges() []model.Edge {
	out := make([]model.Edge, 0, len(t.edgeSet))
	for e := range t.edgeSet {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Caller != out[j].Caller {
			return out[i].Caller < out[j].Caller
		}
		return out[i].Callee < out[j].Callee
	})
	return out
}

// Distances returns shortest outgoing dependency generation from caller.
func (t *Topology) Distances(from string) map[string]int {
	dist := map[string]int{from: 0}
	queue := []string{from}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, next := range t.Out[cur] {
			if _, seen := dist[next]; !seen {
				dist[next] = dist[cur] + 1
				queue = append(queue, next)
			}
		}
	}
	return dist
}

// ReverseClosure returns services that depend on source, including source.
func (t *Topology) ReverseClosure(source string) []string {
	seen := map[string]bool{source: true}
	queue := []string{source}
	for len(queue) > 0 {
		cur := queue[0]
		queue = queue[1:]
		for _, caller := range t.In[cur] {
			if !seen[caller] {
				seen[caller] = true
				queue = append(queue, caller)
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

func (t *Topology) Reachability() map[string]map[string]int {
	out := map[string]map[string]int{}
	for _, svc := range t.Services {
		out[svc] = t.Distances(svc)
	}
	return out
}

func (t *Topology) ValidateAcyclic() error {
	state := map[string]int{} // 0 unvisited, 1 active, 2 done
	stack := []string{}
	var visit func(string) error
	visit = func(node string) error {
		state[node] = 1
		stack = append(stack, node)
		for _, next := range t.Out[node] {
			if state[next] == 0 {
				if err := visit(next); err != nil {
					return err
				}
			} else if state[next] == 1 {
				cycle := append([]string{}, stack...)
				cycle = append(cycle, next)
				return bizerr.New(bizerr.CodeTopologyCycle, "edges", "dependency cycle detected").
					With("cycle", cycle).With("edge", node+" -> "+next)
			}
		}
		stack = stack[:len(stack)-1]
		state[node] = 2
		return nil
	}
	for _, svc := range t.Services {
		if state[svc] == 0 {
			if err := visit(svc); err != nil {
				return err
			}
		}
	}
	return nil
}

type Impact struct {
	SourceService string `json:"source_service"`
	TargetService string `json:"target_service"`
	Generation    int    `json:"generation"`
	ImpactRaw     int64  `json:"impact_raw"`
	Saturated     bool   `json:"saturated,omitempty"`
}

type PropagationResult struct {
	SourceService    string   `json:"source_service"`
	MaxGeneration    int      `json:"max_generation"`
	Impacts          []Impact `json:"impacts"`
	Closure          []string `json:"closure"`
	CycleDetected    bool     `json:"cycle_detected"`
	GenerationCapped bool     `json:"generation_capped"`
}

// Propagate walks reverse edges. Impact decays by decayBP/10000 each generation
// and contributions are叠加 when multiple paths/cycles reach a target.
func (t *Topology) Propagate(source string, maxGeneration, decayBP int) (PropagationResult, error) {
	if _, ok := t.In[source]; !ok {
		return PropagationResult{}, bizerr.New(bizerr.CodeNotFound, "propagate.source", "unknown service %q", source).With("service", source)
	}
	if maxGeneration < 0 {
		return PropagationResult{}, bizerr.New(bizerr.CodeInvalidInput, "propagate.generation", "generation must be >= 0").With("value", maxGeneration)
	}
	if decayBP <= 0 {
		decayBP = DefaultDecayBP
	}
	cycleErr := t.ValidateAcyclic()
	cycleDetected := cycleErr != nil
	decay := fixedpoint.FromBP(decayBP)
	aggregate := map[string]fixedpoint.Value{source: {Raw: fixedpoint.Scale}}
	firstGen := map[string]int{source: 0}
	current := map[string]fixedpoint.Value{source: {Raw: fixedpoint.Scale}}
	capped := false
	if maxGeneration == 0 && len(t.In[source]) > 0 {
		capped = true
	}

	for gen := 1; gen <= maxGeneration; gen++ {
		next := map[string]fixedpoint.Value{}
		for node, value := range current {
			for _, caller := range t.In[node] {
				contribution := fixedpoint.MulSaturating(value, decay)
				nextValue := fixedpoint.AddSaturating(next[caller], contribution)
				if contribution.Saturated || nextValue.Saturated {
					nextValue.Saturated = true
				}
				next[caller] = nextValue
				if _, seen := firstGen[caller]; !seen {
					firstGen[caller] = gen
				}
			}
		}
		if len(next) == 0 {
			break
		}
		for svc, value := range next {
			aggregate[svc] = fixedpoint.AddSaturating(aggregate[svc], value)
		}
		current = next
		if gen == maxGeneration && len(next) > 0 {
			// There is at least one edge that would be considered at gen+1.
			for node := range next {
				if len(t.In[node]) > 0 {
					capped = true
					break
				}
			}
		}
	}

	targets := make([]string, 0, len(aggregate))
	for svc := range aggregate {
		targets = append(targets, svc)
	}
	sort.Strings(targets)
	result := PropagationResult{SourceService: source, MaxGeneration: maxGeneration, CycleDetected: cycleDetected, GenerationCapped: capped}
	for _, target := range targets {
		value := aggregate[target]
		result.Impacts = append(result.Impacts, Impact{
			SourceService: source,
			TargetService: target,
			Generation:    firstGen[target],
			ImpactRaw:     value.Raw,
			Saturated:     value.Saturated,
		})
		result.Closure = append(result.Closure, target)
	}
	sort.Strings(result.Closure)
	return result, nil
}

func itoa(i int) string {
	if i == 0 {
		return "0"
	}
	neg := i < 0
	if neg {
		i = -i
	}
	var b [20]byte
	pos := len(b)
	for i > 0 {
		pos--
		b[pos] = byte('0' + i%10)
		i /= 10
	}
	if neg {
		pos--
		b[pos] = '-'
	}
	return string(b[pos:])
}
