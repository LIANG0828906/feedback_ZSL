package story

import "sort"

// DependencyEdge makes an otherwise implicit relationship explicit. Kinds are
// join, reads and mutex. Field is populated for data (reads) dependencies.
type DependencyEdge struct {
	From  string `json:"from"`
	To    string `json:"to"`
	Kind  string `json:"kind"`
	Field string `json:"field,omitempty"`
}

type DependencyGraph struct {
	TopologicalOrder []string            `json:"topological_order"`
	Edges            []DependencyEdge    `json:"edges"`
	Dependents       map[string][]string `json:"dependents"`
	Prerequisites    map[string][]string `json:"prerequisites"`
	FieldWriters     map[string][]string `json:"field_writers"`
	FieldReaders     map[string][]string `json:"field_readers"`
}

func BuildDependencyGraph(p *Project) (*DependencyGraph, error) {
	topo, err := TopologicalOrder(p)
	if err != nil {
		return nil, err
	}
	g := &DependencyGraph{
		TopologicalOrder: topo,
		Dependents:       map[string][]string{},
		Prerequisites:    map[string][]string{},
		FieldWriters:     map[string][]string{},
		FieldReaders:     map[string][]string{},
	}
	add := func(from, to, kind, field string) {
		if from == "" || to == "" || from == to {
			return
		}
		g.Edges = append(g.Edges, DependencyEdge{From: from, To: to, Kind: kind, Field: field})
		g.Dependents[from] = appendUnique(g.Dependents[from], to)
		g.Prerequisites[to] = appendUnique(g.Prerequisites[to], from)
	}
	for _, n := range p.Nodes {
		for _, dep := range n.Join {
			add(dep, n.ID, "join", "")
		}
		for _, cond := range n.Requires {
			field := conditionField(cond)
			g.FieldReaders[field] = appendUnique(g.FieldReaders[field], n.ID)
		}
		for _, mutex := range n.Mutex {
			add(n.ID, mutex, "mutex", "")
			add(mutex, n.ID, "mutex", "")
		}
		for _, event := range n.Events {
			field := eventField(event)
			g.FieldWriters[field] = appendUnique(g.FieldWriters[field], n.ID)
		}
	}
	for field, writers := range g.FieldWriters {
		readers := g.FieldReaders[field]
		for _, writer := range writers {
			for _, reader := range readers {
				add(writer, reader, "reads", field)
			}
		}
	}
	sort.SliceStable(g.Edges, func(i, j int) bool {
		if g.Edges[i].From != g.Edges[j].From {
			return g.Edges[i].From < g.Edges[j].From
		}
		if g.Edges[i].To != g.Edges[j].To {
			return g.Edges[i].To < g.Edges[j].To
		}
		if g.Edges[i].Kind != g.Edges[j].Kind {
			return g.Edges[i].Kind < g.Edges[j].Kind
		}
		return g.Edges[i].Field < g.Edges[j].Field
	})
	for key := range g.Dependents {
		sort.Strings(g.Dependents[key])
	}
	for key := range g.Prerequisites {
		sort.Strings(g.Prerequisites[key])
	}
	for key := range g.FieldWriters {
		sort.Strings(g.FieldWriters[key])
	}
	for key := range g.FieldReaders {
		sort.Strings(g.FieldReaders[key])
	}
	return g, nil
}

func conditionField(c Condition) string {
	if c.Scope == "character" {
		return "char:" + c.Character + "." + c.Key
	}
	return "flag:" + c.Key
}

func eventField(e Event) string {
	if e.Kind == "set_character" || e.Kind == "inc_character" {
		return "char:" + e.Character + "." + e.Key
	}
	return "flag:" + e.Key
}

func appendUnique(in []string, value string) []string {
	for _, current := range in {
		if current == value {
			return in
		}
	}
	return append(in, value)
}
