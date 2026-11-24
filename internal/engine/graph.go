package engine

import "integritykernel/internal/core"

type Node struct {
	Frame int    `json:"frame"`
	Type  string `json:"type"`
}

func (n Node) ID() string { return NodeID(n.Frame, n.Type) }

var dependencyEdges = map[string][]string{
	core.EvidenceWatermark: {},
	core.EvidenceResidual:  {},
	core.EvidenceNoise:     {},
	core.EvidenceVerdict: {
		core.EvidenceWatermark,
		core.EvidenceResidual,
		core.EvidenceNoise,
	},
}

func Dependencies(frame int, evidenceType string) []Node {
	upstreamTypes := dependencyEdges[evidenceType]
	nodes := make([]Node, 0, len(upstreamTypes))
	for _, upstreamType := range upstreamTypes {
		nodes = append(nodes, Node{Frame: frame, Type: upstreamType})
	}
	return nodes
}

func Dependents(frame int, evidenceType string, frameCount int) []Node {
	var out []Node
	for candidateFrame := 0; candidateFrame < frameCount; candidateFrame++ {
		for candidateType, upstreamTypes := range dependencyEdges {
			for _, upstreamType := range upstreamTypes {
				if upstreamType == evidenceType &&
					(candidateType != core.EvidenceVerdict || candidateFrame == frame) {
					out = append(out, Node{Frame: candidateFrame, Type: candidateType})
				}
			}
		}
	}
	return out
}

func TopologicalOrder(frameCount int) []Node {
	order := make([]Node, 0, frameCount*len(core.EvidenceTypes))
	for _, item := range core.CanonicalEvidenceOrder(frameCount) {
		order = append(order, Node{Frame: item.Frame, Type: item.Type})
	}
	return order
}
