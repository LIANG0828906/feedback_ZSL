package vpf

import (
	"fmt"
	"strings"
)

// ResolveTree recursively expands group nodes. Document order is declared to
// be bottom-to-top compositing order; depth-first pre-order therefore gives
// the fixed internal stack order.
func ResolveTree(root Node, lookup func(layerID string) (Layer, bool)) ([]ResolvedLayer, []Layer, error) {
	var resolved []ResolvedLayer
	var stack []Layer
	seen := map[string]bool{}

	var walk func(node Node, parentVisible bool, depth int, path string) error
	walk = func(node Node, parentVisible bool, depth int, path string) error {
		if node.ID == "" {
			return schemaError(0, path, "node id is required")
		}
		currentPath := path + "/" + node.ID
		effectiveVisible := parentVisible && node.Visible

		switch node.Kind {
		case NodeGroup:
			for i := range node.Children {
				if err := walk(node.Children[i], effectiveVisible, depth+1, currentPath+"/children"); err != nil {
					return err
				}
			}
			return nil
		case NodeLayer:
			if node.LayerID == "" {
				return schemaError(0, currentPath, "layer node missing layer_id")
			}
			if seen[node.LayerID] {
				return schemaError(0, currentPath, "layer %q appears more than once", node.LayerID)
			}
			seen[node.LayerID] = true
			layer, ok := lookup(node.LayerID)
			if !ok {
				return schemaError(0, currentPath, "layer_id %q has no layer definition", node.LayerID)
			}
			layer.SourcePath = currentPath
			layer.Visible = effectiveVisible
			item := ResolvedLayer{
				Layer:            layer,
				SourcePath:       layer.SourcePath,
				TreePath:         currentPath,
				Depth:            depth,
				EffectiveVisible: effectiveVisible,
			}
			resolved = append(resolved, item)
			if effectiveVisible {
				stack = append(stack, layer)
			}
			return nil
		default:
			return schemaError(0, currentPath, "unknown node kind %q", node.Kind)
		}
	}

	if err := walk(root, root.Visible, 0, ""); err != nil {
		return nil, nil, err
	}
	return resolved, stack, nil
}

// RecalculateStack rebuilds resolved and active stack from Root and Layers.
func (p *Project) RecalculateStack() error {
	byID := make(map[string]Layer, len(p.Layers))
	for _, layer := range p.Layers {
		byID[layer.ID] = layer
	}
	resolved, stack, err := ResolveTree(p.Root, func(id string) (Layer, bool) {
		layer, ok := byID[id]
		return layer, ok
	})
	if err != nil {
		return err
	}
	p.Resolved = resolved
	p.Stack = stack
	for i := range p.Layers {
		for _, item := range resolved {
			if p.Layers[i].ID == item.Layer.ID {
				p.Layers[i].Visible = item.EffectiveVisible
				p.Layers[i].SourcePath = item.SourcePath
				break
			}
		}
	}
	return nil
}

// StackDifference describes the first non-equivalent layer expression.
type StackDifference struct {
	Index        int    `json:"index"`
	LeftLayer    string `json:"left_layer"`
	RightLayer   string `json:"right_layer"`
	LeftVisible  bool   `json:"left_visible"`
	RightVisible bool   `json:"right_visible"`
	Reason       string `json:"reason"`
}

// CompareExpressions verifies that flat and nested encodings resolve to the
// same per-layer effective visibility and active compositing order.
func CompareExpressions(left, right *Project) (*StackDifference, error) {
	maxLen := len(left.Resolved)
	if len(right.Resolved) > maxLen {
		maxLen = len(right.Resolved)
	}
	for i := 0; i < maxLen; i++ {
		if i >= len(left.Resolved) {
			diff := &StackDifference{Index: i, RightLayer: right.Resolved[i].Layer.ID, RightVisible: right.Resolved[i].EffectiveVisible, Reason: "layer missing from left expression"}
			return diff, treeError(diff)
		}
		if i >= len(right.Resolved) {
			diff := &StackDifference{Index: i, LeftLayer: left.Resolved[i].Layer.ID, LeftVisible: left.Resolved[i].EffectiveVisible, Reason: "layer missing from right expression"}
			return diff, treeError(diff)
		}
		a := left.Resolved[i]
		b := right.Resolved[i]
		if a.Layer.ID != b.Layer.ID {
			diff := &StackDifference{Index: i, LeftLayer: a.Layer.ID, RightLayer: b.Layer.ID, Reason: "layer order differs"}
			return diff, treeError(diff)
		}
		if a.EffectiveVisible != b.EffectiveVisible {
			diff := &StackDifference{Index: i, LeftLayer: a.Layer.ID, RightLayer: b.Layer.ID, LeftVisible: a.EffectiveVisible, RightVisible: b.EffectiveVisible, Reason: "effective visibility differs"}
			return diff, treeError(diff)
		}
	}

	if len(left.Stack) != len(right.Stack) {
		diff := &StackDifference{Index: minInt(len(left.Stack), len(right.Stack)), Reason: "active stack length differs"}
		if diff.Index < len(left.Stack) {
			diff.LeftLayer = left.Stack[diff.Index].ID
		}
		if diff.Index < len(right.Stack) {
			diff.RightLayer = right.Stack[diff.Index].ID
		}
		return diff, treeError(diff)
	}
	for i := range left.Stack {
		if left.Stack[i].ID != right.Stack[i].ID {
			diff := &StackDifference{Index: i, LeftLayer: left.Stack[i].ID, RightLayer: right.Stack[i].ID, Reason: "active compositing order differs"}
			return diff, treeError(diff)
		}
	}
	return nil, nil
}

func treeError(diff *StackDifference) *Error {
	field := fmt.Sprintf("$.resolved[%d]", diff.Index)
	return &Error{
		Code:      CodeTree,
		Message:   "flat and nested layer expressions are not equivalent",
		FieldPath: field,
		Left:      diff.LeftLayer,
		Right:     diff.RightLayer,
		Details:   diff.Reason,
	}
}

// ActiveLayerIDs is a diagnostic helper.
func ActiveLayerIDs(p *Project) []string {
	ids := make([]string, 0, len(p.Stack))
	for _, layer := range p.Stack {
		ids = append(ids, layer.ID)
	}
	return ids
}

// TreeOutline renders a small text outline used by CLI tests/diagnostics.
func TreeOutline(p *Project) string {
	var b strings.Builder
	var walk func(Node, int)
	walk = func(n Node, depth int) {
		for i := 0; i < depth; i++ {
			b.WriteString("  ")
		}
		state := "visible"
		if !n.Visible {
			state = "hidden"
		}
		if n.Kind == NodeGroup {
			fmt.Fprintf(&b, "▸ %s [%s group]\n", n.ID, state)
			for _, child := range n.Children {
				walk(child, depth+1)
			}
		} else {
			fmt.Fprintf(&b, "• %s [%s layer]\n", n.ID, state)
		}
	}
	walk(p.Root, 0)
	return b.String()
}
