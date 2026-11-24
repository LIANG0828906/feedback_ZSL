package engine

import (
	"encoding/json"
	"fmt"
	"sort"

	"virtualstudio/model"
)

type WindowDependency struct {
	Index      int      `json:"index"`
	StartFrame int      `json:"start_frame"`
	EndFrame   int      `json:"end_frame"`
	LayerIDs   []string `json:"layer_ids"`
}

type ExportDependency struct {
	Name          string   `json:"name"`
	WindowIndexes []int    `json:"window_indexes"`
	LayerIDs      []string `json:"layer_ids"`
}

type DependencyGraph struct {
	LayerNodes  []string           `json:"layer_nodes"`
	WindowNodes []WindowDependency `json:"window_nodes"`
	ExportNodes []ExportDependency `json:"export_nodes"`
}

type WindowInvalidation struct {
	Window   int      `json:"window"`
	LayerIDs []string `json:"layer_ids"`
}

type Invalidation struct {
	Windows  []WindowInvalidation `json:"windows"`
	LayerIDs []string             `json:"layer_ids"`
	Exports  []string             `json:"exports"`
	Reason   string               `json:"reason"`
}

func (i *Invalidation) Empty() bool {
	return i == nil || (len(i.Windows) == 0 && len(i.LayerIDs) == 0 && len(i.Exports) == 0)
}

func BuildDependencyGraph(scene *model.Scene, commands *model.CommandSet, windowFrames int) *DependencyGraph {
	graph := &DependencyGraph{}
	for _, layer := range scene.Layers {
		graph.LayerNodes = append(graph.LayerNodes, layer.ID)
	}
	windowCount := (scene.FrameCount + windowFrames - 1) / windowFrames
	for index := 0; index < windowCount; index++ {
		start := index * windowFrames
		end := start + windowFrames - 1
		if end >= scene.FrameCount {
			end = scene.FrameCount - 1
		}
		node := WindowDependency{Index: index, StartFrame: start, EndFrame: end}
		for _, layer := range scene.Layers {
			active := false
			for frame := start; frame <= end; frame++ {
				eff := model.EffectiveLayerState(scene, commandAt(commands, frame), layer)
				if eff.Visible && eff.Opacity > 0 {
					active = true
					break
				}
			}
			if active {
				node.LayerIDs = append(node.LayerIDs, layer.ID)
			}
		}
		graph.WindowNodes = append(graph.WindowNodes, node)
	}
	for _, target := range []string{"v1", "v2", "v3"} {
		node := ExportDependency{Name: "export:" + target, LayerIDs: append([]string(nil), graph.LayerNodes...)}
		for i := range graph.WindowNodes {
			node.WindowIndexes = append(node.WindowIndexes, i)
		}
		graph.ExportNodes = append(graph.ExportNodes, node)
	}
	return graph
}

func (g *DependencyGraph) Diff(oldScene *model.Scene, oldCommands *model.CommandSet, newScene *model.Scene, newCommands *model.CommandSet, targetChanged bool) *Invalidation {
	inv := &Invalidation{Reason: "layer parameter, visibility, timeline, command or target edit"}
	windowFrames := oldCommands.WindowFrames
	if windowFrames <= 0 {
		windowFrames = 1
	}
	windowCount := (oldScene.FrameCount + windowFrames - 1) / windowFrames
	layerSet := map[string]bool{}
	sceneChanged := !sceneEditableEqual(oldScene, newScene)
	commandsChanged := !commandsEqual(oldCommands, newCommands)
	for index := 0; index < windowCount; index++ {
		start := index * windowFrames
		end := start + windowFrames - 1
		if end >= oldScene.FrameCount {
			end = oldScene.FrameCount - 1
		}
		oldSnap := model.BuildWindowSnapshot(oldScene, oldCommands, index, start, end)
		newSnap := model.BuildWindowSnapshot(newScene, newCommands, index, start, end)
		if oldSnap.ContentHash == newSnap.ContentHash && oldSnap.ParamVersion == newSnap.ParamVersion {
			continue
		}
		affected := changedLayerIDs(oldSnap, newSnap, oldScene.FrameDurationNS != newScene.FrameDurationNS)
		if len(affected) == 0 {
			affected = []string{"@timeline"}
		}
		inv.Windows = append(inv.Windows, WindowInvalidation{Window: index, LayerIDs: affected})
		for _, id := range affected {
			layerSet[id] = true
		}
	}
	for _, id := range sceneChangedLayerIDs(oldScene, newScene) {
		layerSet[id] = true
	}
	for id := range layerSet {
		inv.LayerIDs = append(inv.LayerIDs, id)
	}
	sort.Strings(inv.LayerIDs)
	if sceneChanged || commandsChanged || targetChanged {
		for _, node := range g.ExportNodes {
			inv.Exports = append(inv.Exports, node.Name)
		}
	}
	return inv
}

func sceneChangedLayerIDs(oldScene, newScene *model.Scene) []string {
	if oldScene == nil || newScene == nil {
		return nil
	}
	oldLayers := map[string]model.Layer{}
	newLayers := map[string]model.Layer{}
	for _, layer := range oldScene.Layers {
		oldLayers[layer.ID] = layer
	}
	for _, layer := range newScene.Layers {
		newLayers[layer.ID] = layer
	}
	set := map[string]bool{}
	for id, oldLayer := range oldLayers {
		newLayer, ok := newLayers[id]
		if !ok || oldLayer.Name != newLayer.Name || oldLayer.Visible != newLayer.Visible ||
			oldLayer.Opacity != newLayer.Opacity || oldLayer.SourceAlphaMode != newLayer.SourceAlphaMode ||
			model.HashBytes(oldLayer.Pixels) != model.HashBytes(newLayer.Pixels) {
			set[id] = true
		}
	}
	for id := range newLayers {
		if _, ok := oldLayers[id]; !ok {
			set[id] = true
		}
	}
	out := make([]string, 0, len(set))
	for id := range set {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

func changedLayerIDs(oldSnap, newSnap *model.WindowSnapshot, timebaseChanged bool) []string {
	oldLayers := map[string]model.LayerSnapshot{}
	newLayers := map[string]model.LayerSnapshot{}
	for _, layer := range oldSnap.Layers {
		oldLayers[layer.ID] = layer
	}
	for _, layer := range newSnap.Layers {
		newLayers[layer.ID] = layer
	}
	set := map[string]bool{}
	for id, oldLayer := range oldLayers {
		newLayer, ok := newLayers[id]
		if !ok {
			set[id] = true
			continue
		}
		active := false
		for _, state := range oldLayer.FrameStates {
			if state.Visible && state.Opacity > 0 {
				active = true
				break
			}
		}
		for _, state := range newLayer.FrameStates {
			if state.Visible && state.Opacity > 0 {
				active = true
				break
			}
		}
		if oldLayer.PixelsHash != newLayer.PixelsHash ||
			oldLayer.BaseVisible != newLayer.BaseVisible ||
			oldLayer.BaseOpacity != newLayer.BaseOpacity ||
			!equalFrameStates(oldLayer.FrameStates, newLayer.FrameStates) {
			if active || timebaseChanged {
				set[id] = true
			}
		} else if timebaseChanged && active {
			set[id] = true
		}
	}
	for id := range newLayers {
		if _, ok := oldLayers[id]; !ok {
			set[id] = true
		}
	}
	out := make([]string, 0, len(set))
	for id := range set {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

func equalFrameStates(a, b []model.EffectiveLayer) bool {
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

func sceneEditableEqual(a, b *model.Scene) bool {
	if a == nil || b == nil {
		return a == b
	}
	if a.Width != b.Width || a.Height != b.Height || a.Channels != b.Channels ||
		a.FrameCount != b.FrameCount || a.FrameDurationNS != b.FrameDurationNS || a.SourceVersion != b.SourceVersion ||
		a.OrderExpr != b.OrderExpr || len(a.Layers) != len(b.Layers) {
		return false
	}
	for i := range a.Layers {
		al, bl := a.Layers[i], b.Layers[i]
		if al.ID != bl.ID || al.Name != bl.Name || al.Visible != bl.Visible || al.Opacity != bl.Opacity ||
			al.SourceAlphaMode != bl.SourceAlphaMode || model.HashBytes(al.Pixels) != model.HashBytes(bl.Pixels) {
			return false
		}
	}
	return true
}

func commandsEqual(a, b *model.CommandSet) bool {
	aj, _ := json.Marshal(a)
	bj, _ := json.Marshal(b)
	return string(aj) == string(bj)
}

func commandAt(commands *model.CommandSet, frame int) *model.FrameCommand {
	if commands == nil {
		return nil
	}
	if frame >= 0 && frame < len(commands.Frames) && commands.Frames[frame].Frame == frame {
		return &commands.Frames[frame]
	}
	for i := range commands.Frames {
		if commands.Frames[i].Frame == frame {
			return &commands.Frames[i]
		}
	}
	return nil
}

func (g *DependencyGraph) String() string {
	return fmt.Sprintf("layers=%d windows=%d exports=%d", len(g.LayerNodes), len(g.WindowNodes), len(g.ExportNodes))
}
