package model

import "strings"

type HopTarget struct {
	Index  int
	EdgeID string
	From   string
	To     string
	Path   []string
}

func (p *Project) EdgeBetween(from, to string) (Edge, bool) {
	for _, edge := range p.Edges {
		if edge.From == from && edge.To == to {
			return edge, true
		}
	}
	return Edge{}, false
}

func (p *Project) EdgeByID(id string) (Edge, bool) {
	for _, edge := range p.Edges {
		if edge.ID == id {
			return edge, true
		}
	}
	return Edge{}, false
}

func SelectorMatches(sel Selector, hop HopTarget) bool {
	switch sel.Kind {
	case TargetService:
		if sel.Service != hop.To {
			return false
		}
	case TargetEdge:
		if sel.Service != "" && sel.Service != hop.To {
			return false
		}
		if sel.EdgeID != "" && sel.EdgeID != hop.EdgeID {
			return false
		}
		if sel.From != "" && sel.From != hop.From {
			return false
		}
		if sel.To != "" && sel.To != hop.To {
			return false
		}
	default:
		return false
	}
	if sel.PathContains != "" {
		joined := strings.Join(hop.Path, ">")
		if !strings.Contains(joined, sel.PathContains) {
			return false
		}
	}
	return true
}

func (p *Project) HopTarget(req Request, hopIndex int) (HopTarget, bool) {
	if hopIndex < 0 || hopIndex+1 >= len(req.Path) {
		return HopTarget{}, false
	}
	from := req.Path[hopIndex]
	to := req.Path[hopIndex+1]
	edge, ok := p.EdgeBetween(from, to)
	edgeID := from + "-" + to
	if ok {
		edgeID = edge.ID
	}
	return HopTarget{
		Index:  hopIndex,
		EdgeID: edgeID,
		From:   from,
		To:     to,
		Path:   req.Path,
	}, true
}
