package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"virtualstudio/formats"
	"virtualstudio/model"
	"virtualstudio/store"
)

type WorkspaceOptions struct {
	Store        *store.Store
	ProjectID    string
	Scene        *model.Scene
	Commands     *model.CommandSet
	WindowFrames int
	TargetFormat string
}

type Workspace struct {
	store            *store.Store
	projectID        string
	scene            *model.Scene
	commands         *model.CommandSet
	windowFrames     int
	targetFormat     string
	projectVersion   int64
	complete         bool
	windows          *WindowSet
	graph            *DependencyGraph
	LastInvalidation *Invalidation
}

func OpenWorkspace(ctx context.Context, opts WorkspaceOptions) (*Workspace, error) {
	if opts.Store == nil {
		return nil, model.NewError(model.CodeParam, "store", "store is required")
	}
	if opts.ProjectID == "" {
		return nil, model.NewError(model.CodeParam, "project_id", "must not be empty")
	}
	if err := model.ValidateScene(opts.Scene); err != nil {
		return nil, err
	}
	if err := model.ValidateCommands(opts.Scene, opts.Commands); err != nil {
		return nil, err
	}
	windowFrames := opts.WindowFrames
	if windowFrames == 0 {
		windowFrames = opts.Commands.WindowFrames
	}
	if windowFrames <= 0 {
		return nil, model.NewError(model.CodeParam, "window_frames", "must be > 0")
	}
	target := opts.TargetFormat
	if target != "" && !supportedTarget(target) {
		return nil, model.NewError(model.CodeParam, "target_format", fmt.Sprintf("unsupported target %q", target))
	}
	ws := &Workspace{
		store:          opts.Store,
		projectID:      opts.ProjectID,
		scene:          opts.Scene.Clone(),
		commands:       opts.Commands.Clone(),
		windowFrames:   windowFrames,
		targetFormat:   target,
		projectVersion: 1,
		graph:          BuildDependencyGraph(opts.Scene, opts.Commands, windowFrames),
	}
	existing, err := opts.Store.LoadProject(ctx, opts.ProjectID)
	if err != nil && err != store.ErrNotFound {
		return nil, err
	}
	if existing != nil {
		if !projectCompatible(existing, ws.scene, windowFrames) {
			if err := opts.Store.Purge(ctx, opts.ProjectID); err != nil {
				return nil, err
			}
			existing = nil
		} else {
			oldScene, oldCommands, decodeErr := decodeProject(existing)
			if decodeErr != nil {
				if err := opts.Store.Purge(ctx, opts.ProjectID); err != nil {
					return nil, err
				}
				existing = nil
			} else {
				ws.projectVersion = existing.ProjectVersion
				ws.complete = existing.Complete
				if ws.targetFormat == "" {
					ws.targetFormat = "v3"
					if supportedTarget(existing.TargetFormat) {
						ws.targetFormat = existing.TargetFormat
					}
				}
				targetChanged := existing.TargetFormat != ws.targetFormat
				if !sceneEditableEqual(oldScene, ws.scene) || !commandsEqual(oldCommands, ws.commands) || targetChanged {
					oldGraph := BuildDependencyGraph(oldScene, oldCommands, windowFrames)
					inv := oldGraph.Diff(oldScene, oldCommands, ws.scene, ws.commands, targetChanged)
					ws.projectVersion++
					ws.complete = false
					if err := ws.persistProject(ctx); err != nil {
						return nil, err
					}
					if err := ws.deleteInvalidatedWindows(ctx, inv); err != nil {
						return nil, err
					}
					ws.LastInvalidation = inv
				}
			}
		}
	}
	if existing == nil {
		ws.projectVersion = 1
		ws.complete = false
		if err := ws.persistProject(ctx); err != nil {
			return nil, err
		}
	}
	if ws.targetFormat == "" {
		ws.targetFormat = "v3"
	}
	ws.windows, err = NewWindowSet(ws.scene.FrameCount, windowFrames)
	if err != nil {
		return nil, err
	}
	if err := ws.persistProject(ctx); err != nil {
		return nil, err
	}
	return ws, nil
}

func projectCompatible(p *store.Project, scene *model.Scene, windowFrames int) bool {
	return p.AlgorithmVersion == model.AlgorithmVersion &&
		p.SourceVersion == scene.SourceVersion &&
		p.BaseHash == scene.BaseHash() &&
		p.WindowSize == windowFrames &&
		p.FrameCount == scene.FrameCount
}

func decodeProject(p *store.Project) (*model.Scene, *model.CommandSet, error) {
	var scene model.Scene
	if err := json.Unmarshal(p.SceneJSON, &scene); err != nil {
		return nil, nil, err
	}
	var commands model.CommandSet
	if err := json.Unmarshal(p.CommandsJSON, &commands); err != nil {
		return nil, nil, err
	}
	if err := model.ValidateScene(&scene); err != nil {
		return nil, nil, err
	}
	if err := model.ValidateCommands(&scene, &commands); err != nil {
		return nil, nil, err
	}
	return &scene, &commands, nil
}

func (w *Workspace) persistProject(ctx context.Context) error {
	scene := w.scene.Clone()
	scene.SourcePath = ""
	sceneJSON, err := json.Marshal(scene)
	if err != nil {
		return model.WrapError(model.CodeIO, "project.scene", "marshal scene", err)
	}
	commandsJSON, err := json.Marshal(w.commands)
	if err != nil {
		return model.WrapError(model.CodeIO, "project.commands", "marshal commands", err)
	}
	return w.store.SaveProject(ctx, store.Project{
		ID:               w.projectID,
		AlgorithmVersion: model.AlgorithmVersion,
		SourceVersion:    w.scene.SourceVersion,
		BaseHash:         w.scene.BaseHash(),
		WindowSize:       w.windowFrames,
		FrameCount:       w.scene.FrameCount,
		ProjectVersion:   w.projectVersion,
		TargetFormat:     w.targetFormat,
		SceneJSON:        sceneJSON,
		CommandsJSON:     commandsJSON,
		Complete:         w.complete,
	})
}

func (w *Workspace) deleteInvalidatedWindows(ctx context.Context, inv *Invalidation) error {
	if inv == nil {
		return nil
	}
	if len(inv.Windows) > 0 {
		indexes := make([]int, 0, len(inv.Windows))
		for _, item := range inv.Windows {
			indexes = append(indexes, item.Window)
		}
		sort.Ints(indexes)
		if err := w.store.DeleteWindows(ctx, w.projectID, indexes); err != nil {
			return err
		}
	}
	for _, name := range inv.Exports {
		target := strings.TrimPrefix(name, "export:")
		if supportedTarget(target) {
			if err := w.store.SetExportStatus(ctx, w.projectID, name, false, target); err != nil {
				return err
			}
		}
	}
	return nil
}

func (w *Workspace) ProjectID() string       { return w.projectID }
func (w *Workspace) Store() *store.Store     { return w.store }
func (w *Workspace) WindowFrames() int       { return w.windowFrames }
func (w *Workspace) TargetFormat() string    { return w.targetFormat }
func (w *Workspace) ProjectVersion() int64   { return w.projectVersion }
func (w *Workspace) Graph() *DependencyGraph { return w.graph }

func (w *Workspace) Scene() *model.Scene {
	return w.scene.Clone()
}

func (w *Workspace) Commands() *model.CommandSet {
	return w.commands.Clone()
}

func (w *Workspace) Windows() []*Window {
	return w.windows.Windows()
}

func (w *Workspace) EditLayer(id string, visible *bool, opacity *float64) (*Invalidation, error) {
	if _, _, ok := w.scene.LayerByID(id); !ok {
		return nil, model.NewError(model.CodeVersion, "layer_id", fmt.Sprintf("unknown layer %q", id))
	}
	oldScene := w.scene.Clone()
	oldCommands := w.commands.Clone()
	newScene := w.scene.Clone()
	layer, _, _ := newScene.LayerByID(id)
	if visible != nil {
		layer.Visible = *visible
	}
	if opacity != nil {
		layer.Opacity = *opacity
	}
	if err := model.ValidateScene(newScene); err != nil {
		return nil, err
	}
	return w.applySceneEdit(oldScene, oldCommands, newScene, w.commands.Clone(), false)
}

func (w *Workspace) SetFrameDurationNS(ns int64) (*Invalidation, error) {
	if ns <= 0 {
		return nil, model.NewError(model.CodeParam, "frame_duration_ns", "must be > 0")
	}
	oldScene := w.scene.Clone()
	oldCommands := w.commands.Clone()
	newScene := w.scene.Clone()
	newScene.FrameDurationNS = ns
	return w.applySceneEdit(oldScene, oldCommands, newScene, w.commands.Clone(), false)
}

func (w *Workspace) SetTargetFormat(target string) (*Invalidation, error) {
	if !supportedTarget(target) {
		return nil, model.NewError(model.CodeParam, "target_format", fmt.Sprintf("unsupported target %q", target))
	}
	if target == w.targetFormat {
		return &Invalidation{}, nil
	}
	oldScene := w.scene.Clone()
	oldCommands := w.commands.Clone()
	oldTarget := w.targetFormat
	w.targetFormat = target
	inv := w.graph.Diff(oldScene, oldCommands, w.scene, w.commands, oldTarget != target)
	w.projectVersion++
	if err := w.persistProject(context.Background()); err != nil {
		w.targetFormat = oldTarget
		return nil, err
	}
	if err := w.deleteInvalidatedWindows(context.Background(), inv); err != nil {
		return nil, err
	}
	w.LastInvalidation = inv
	return inv, nil
}

func (w *Workspace) applySceneEdit(oldScene *model.Scene, oldCommands *model.CommandSet, newScene *model.Scene, newCommands *model.CommandSet, targetChanged bool) (*Invalidation, error) {
	if sceneEditableEqual(oldScene, newScene) && commandsEqual(oldCommands, newCommands) && !targetChanged {
		return &Invalidation{}, nil
	}
	inv := w.graph.Diff(oldScene, oldCommands, newScene, newCommands, targetChanged)
	oldGraph := w.graph
	w.scene = newScene
	w.commands = newCommands
	w.graph = BuildDependencyGraph(newScene, newCommands, w.windowFrames)
	w.projectVersion++
	w.complete = false
	if err := w.persistProject(context.Background()); err != nil {
		w.scene = oldScene
		w.commands = oldCommands
		w.graph = oldGraph
		w.projectVersion--
		return nil, err
	}
	if err := w.deleteInvalidatedWindows(context.Background(), inv); err != nil {
		return nil, err
	}
	for _, item := range inv.Windows {
		if win, ok := w.windows.Window(item.Window); ok {
			_ = win.Invalidate(item.LayerIDs, inv.Reason)
		}
	}
	w.LastInvalidation = inv
	return inv, nil
}

func (w *Workspace) MarkExportValid(ctx context.Context, target string, valid bool) error {
	if !supportedTarget(target) {
		return model.NewError(model.CodeParam, "target_format", "unsupported target")
	}
	return w.store.SetExportStatus(ctx, w.projectID, "export:"+target, valid, target)
}

func (w *Workspace) ExportStatus(ctx context.Context) (map[string]bool, error) {
	return w.store.ExportStatus(ctx, w.projectID)
}

func (w *Workspace) Export(ctx context.Context, dir, target string, frames []model.Frame) (*formats.BundleManifest, error) {
	if !supportedTarget(target) {
		return nil, model.NewError(model.CodeParam, "target_format", "unsupported target")
	}
	manifest, err := formats.WriteBundle(dir, w.scene, frames, target)
	if err != nil {
		_ = w.MarkExportValid(ctx, target, false)
		return nil, err
	}
	if err := w.MarkExportValid(ctx, target, true); err != nil {
		return nil, err
	}
	return manifest, nil
}

func supportedTarget(target string) bool {
	for _, v := range formats.SupportedVersions {
		if v == target {
			return true
		}
	}
	return false
}
