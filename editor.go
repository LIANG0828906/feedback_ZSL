package vpf

import "fmt"

type Change struct {
	DependencyID string `json:"dependency_id"`
	Kind         string `json:"kind"`
	LayerID      string `json:"layer_id,omitempty"`
	GroupID      string `json:"group_id,omitempty"`
	SourceID     string `json:"source_id,omitempty"`
}

func (p *Project) BumpSnapshot() {
	p.StackSnapshotVersion++
}

// SetLayerOpacity edits opacity and advances the layer parameter version.
func SetLayerOpacity(p *Project, layerID string, opacityBP int) (Change, error) {
	if opacityBP < 0 || opacityBP > OpacityScale {
		return Change{}, schemaError(p.SourceVersion, "$.layers[].opacity_bp", "opacity must be 0..10000")
	}
	for i := range p.Layers {
		if p.Layers[i].ID != layerID {
			continue
		}
		p.Layers[i].OpacityBP = opacityBP
		p.Layers[i].ParamVersion++
		p.StackSnapshotVersion++
		if err := p.RecalculateStack(); err != nil {
			return Change{}, err
		}
		return Change{DependencyID: "param:" + layerID + ":opacity", Kind: "layer_opacity", LayerID: layerID}, nil
	}
	return Change{}, schemaError(p.SourceVersion, "$.layers[].id", "layer %q not found", layerID)
}

func FindNode(node Node, id string) (Node, bool) {
	if node.ID == id {
		return node, true
	}
	for _, child := range node.Children {
		if found, ok := FindNode(child, id); ok {
			return found, true
		}
	}
	return Node{}, false
}

func mutateNode(node *Node, id string, fn func(*Node)) bool {
	if node.ID == id {
		fn(node)
		return true
	}
	for i := range node.Children {
		if mutateNode(&node.Children[i], id, fn) {
			return true
		}
	}
	return false
}

// SetGroupVisibility toggles a group node. Ancestor/descendant visibility is
// resolved again by RecalculateStack.
func SetGroupVisibility(p *Project, groupID string, visible bool) (Change, error) {
	if _, ok := FindNode(p.Root, groupID); !ok {
		return Change{}, schemaError(p.SourceVersion, "$.layerTree", "group %q not found", groupID)
	}
	mutateNode(&p.Root, groupID, func(n *Node) { n.Visible = visible })
	p.StackSnapshotVersion++
	if err := p.RecalculateStack(); err != nil {
		return Change{}, err
	}
	return Change{DependencyID: "visibility:group:" + groupID, Kind: "group_visibility", GroupID: groupID}, nil
}

// EditRippleSource replaces center/radius/decay/amplitude when non-zero
// pointers are supplied and advances both content and ripple versions.
func EditRippleSource(p *Project, sourceID string, center *Point, radius, decayBP, amplitudeBP int) (Change, error) {
	for layerIndex := range p.Layers {
		layer := &p.Layers[layerIndex]
		if layer.Kind != KindRipple {
			continue
		}
		for sourceIndex := range layer.Ripples {
			source := &layer.Ripples[sourceIndex]
			if source.ID != sourceID {
				continue
			}
			if center != nil {
				source.Center = *center
			}
			if radius > 0 {
				source.Radius = radius
			}
			if decayBP > 0 {
				source.DecayBP = decayBP
			}
			if amplitudeBP > 0 {
				source.AmplitudeBP = amplitudeBP
			}
			source.ParamVersion++
			p.RippleSourceVersion++
			p.StackSnapshotVersion++
			if err := p.RecalculateStack(); err != nil {
				return Change{}, err
			}
			return Change{
				DependencyID: "ripple:" + layer.ID + ":" + sourceID,
				Kind:         "ripple_source", LayerID: layer.ID, SourceID: sourceID,
			}, nil
		}
	}
	return Change{}, schemaError(p.SourceVersion, "$.layers[].ripples[].source_id", "ripple source %q not found", sourceID)
}

func DescendantLayerIDs(node Node) []string {
	var ids []string
	var walk func(Node)
	walk = func(n Node) {
		if n.Kind == NodeLayer {
			ids = append(ids, n.LayerID)
		}
		for _, child := range n.Children {
			walk(child)
		}
	}
	for _, child := range node.Children {
		walk(child)
	}
	if node.Kind == NodeLayer {
		ids = append(ids, node.LayerID)
	}
	return ids
}

func MustChangeID(format string, args ...any) string {
	return fmt.Sprintf(format, args...)
}
