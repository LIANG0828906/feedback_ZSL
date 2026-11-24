package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"

	"cadlab/internal/deps"
	"cadlab/internal/engine"
	"cadlab/internal/model"
	"cadlab/internal/render"
	"cadlab/internal/sample"
	"cadlab/internal/sched"
)

type Response struct {
	OK    bool              `json:"ok"`
	Error *model.Diagnostic `json:"error,omitempty"`
	Data  json.RawMessage   `json:"data,omitempty"`
}

type App struct {
	engine *engine.Engine
	ctx    context.Context
	mu     sync.Mutex
	cancel context.CancelFunc
	active string
	token  uint64
}

type EditView struct {
	Frame        engine.FrameView   `json:"frame"`
	Reference    engine.FrameView   `json:"reference"`
	Invalidation deps.Report        `json:"invalidation"`
	Comparison   *engine.Comparison `json:"comparison"`
	Queue        sched.Metrics      `json:"queue"`
}

type ResumeView struct {
	Frame          *engine.FrameView `json:"frame,omitempty"`
	Diagnostic     *model.Diagnostic `json:"diagnostic,omitempty"`
	LoadedTiles    int               `json:"loadedTiles"`
	FullRerendered bool              `json:"fullRerendered"`
}

type PathView struct {
	Frame         engine.FrameView  `json:"frame"`
	SnapshotCheck *model.Diagnostic `json:"snapshotCheck,omitempty"`
}

type RetryView struct {
	Frame  engine.FrameView  `json:"frame"`
	Result *model.Diagnostic `json:"result,omitempty"`
}

func New(opts engine.Options, seed int64) (*App, error) {
	ds := sample.Generate(seed)
	eng, err := engine.New(ds, opts)
	if err != nil {
		return nil, err
	}
	return &App{engine: eng, ctx: context.Background()}, nil
}

func (a *App) Startup(ctx context.Context) {
	a.ctx = ctx
}

func (a *App) Shutdown(ctx context.Context) {
	_ = a.engine.Close()
}

func (a *App) Close() error {
	return a.engine.Close()
}

func (a *App) Call(action, payloadJSON string) (response Response) {
	defer func() {
		if r := recover(); r != nil {
			response = fail(model.Diag(model.CodeStage, fmt.Sprintf("API panic: %v", r)))
		}
	}()
	if payloadJSON == "" {
		payloadJSON = "{}"
	}
	ctx := a.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	if action == "cancel" {
		a.mu.Lock()
		cancel := a.cancel
		active := a.active
		a.mu.Unlock()
		if cancel != nil {
			cancel()
		}
		return ok(map[string]any{"cancelled": cancel != nil, "active": active})
	}
	if cancellableAction(action) {
		var cancel context.CancelFunc
		ctx, cancel = context.WithCancel(ctx)
		a.mu.Lock()
		a.token++
		myToken := a.token
		a.cancel, a.active = cancel, action
		a.mu.Unlock()
		defer func() {
			cancel()
			a.mu.Lock()
			if a.token == myToken {
				a.cancel, a.active = nil, ""
			}
			a.mu.Unlock()
		}()
	}
	switch action {
	case "bootstrap":
		return ok(a.engine.Bootstrap())
	case "preview":
		frame, err := a.engine.RenderPreview(ctx)
		if err != nil {
			return fail(diag(err))
		}
		view := PathView{Frame: a.engine.FrameView(frame)}
		if exact := a.engine.LatestExact(); exact != nil {
			view.SnapshotCheck = a.engine.ValidatePathSnapshot(frame, exact)
		}
		return ok(view)
	case "exact":
		frame, err := a.engine.RenderExact(ctx)
		if err != nil {
			return fail(diag(err))
		}
		view := PathView{Frame: a.engine.FrameView(frame)}
		if preview := a.engine.LatestPreview(); preview != nil {
			view.SnapshotCheck = a.engine.ValidatePathSnapshot(preview, frame)
		}
		return ok(view)
	case "applyControlPoint":
		var p struct {
			PatchID  int        `json:"patchId"`
			Row      int        `json:"row"`
			Col      int        `json:"col"`
			Position model.Vec3 `json:"position"`
		}
		if err := json.Unmarshal([]byte(payloadJSON), &p); err != nil {
			return fail(model.Diag(model.CodeSchema, err.Error()))
		}
		out, err := a.engine.ApplyControlPoint(ctx, p.PatchID, p.Row, p.Col, p.Position)
		return editResponse(out, err, a.engine)
	case "applyMaterial":
		var p struct {
			PatchID  int            `json:"patchId"`
			Material model.Material `json:"material"`
		}
		if err := json.Unmarshal([]byte(payloadJSON), &p); err != nil {
			return fail(model.Diag(model.CodeSchema, err.Error()))
		}
		out, err := a.engine.ApplyMaterial(ctx, p.PatchID, p.Material)
		return editResponse(out, err, a.engine)
	case "applyDensity":
		var p struct {
			PatchID int `json:"patchId"`
			Density int `json:"density"`
		}
		if err := json.Unmarshal([]byte(payloadJSON), &p); err != nil {
			return fail(model.Diag(model.CodeSchema, err.Error()))
		}
		out, err := a.engine.ApplyDensity(ctx, p.PatchID, p.Density)
		return editResponse(out, err, a.engine)
	case "compare":
		cmp, err := a.engine.ComparePaths()
		if err != nil {
			return fail(diag(err))
		}
		return ok(cmp)
	case "validatePaths":
		preview := a.engine.LatestPreview()
		exact := a.engine.LatestExact()
		if preview == nil || exact == nil {
			return fail(model.Diag(model.CodeVersion, "preview and exact frames are both required"))
		}
		if d := a.engine.ValidatePathSnapshot(preview, exact); d != nil {
			return fail(d)
		}
		return ok(map[string]any{
			"version": preview.Version, "previewPath": preview.Path, "exactPath": exact.Path,
			"previewLOD": preview.LOD, "exactLOD": exact.LOD,
		})
	case "resume":
		out, err := a.engine.Resume(ctx)
		if err != nil {
			return fail(diag(err))
		}
		v := ResumeView{Diagnostic: out.Diagnostic, LoadedTiles: out.LoadedTiles, FullRerendered: out.FullRerendered}
		if out.Frame != nil {
			fv := a.engine.FrameView(out.Frame)
			v.Frame = &fv
		}
		return ok(v)
	case "anomaly":
		var p struct {
			Kind   string `json:"kind"`
			TileID int    `json:"tileId"`
		}
		if err := json.Unmarshal([]byte(payloadJSON), &p); err != nil {
			return fail(model.Diag(model.CodeSchema, err.Error()))
		}
		res, err := a.engine.InjectAnomaly(ctx, p.Kind, p.TileID)
		if err != nil {
			return fail(diag(err))
		}
		return ok(res)
	case "retry":
		var p struct {
			TileID int `json:"tileId"`
		}
		if err := json.Unmarshal([]byte(payloadJSON), &p); err != nil {
			return fail(model.Diag(model.CodeSchema, err.Error()))
		}
		frame, err := a.engine.RetryTile(ctx, p.TileID)
		if err != nil {
			return fail(diag(err))
		}
		return ok(RetryView{Frame: a.engine.FrameView(frame)})
	case "profile":
		var p struct {
			Normal model.Vec3 `json:"normal"`
			Offset float64    `json:"offset"`
		}
		if err := json.Unmarshal([]byte(payloadJSON), &p); err != nil {
			return fail(model.Diag(model.CodeSchema, err.Error()))
		}
		res, err := a.engine.Profile(ctx, p.Normal, p.Offset)
		if err != nil {
			return fail(diag(err))
		}
		return ok(res)
	case "frozenProfile":
		var p struct {
			Version int64 `json:"version"`
		}
		if err := json.Unmarshal([]byte(payloadJSON), &p); err != nil {
			return fail(model.Diag(model.CodeSchema, err.Error()))
		}
		res, found := a.engine.FrozenProfile(p.Version)
		if !found {
			return fail(model.Diag(model.CodeVersion, "profile result is not frozen for that version"))
		}
		return ok(res)
	case "export":
		data, err := a.engine.ExportState()
		if err != nil {
			return fail(diag(err))
		}
		return ok(data)
	default:
		return fail(model.Diag(model.CodeSchema, "unknown action: "+action))
	}
}

func cancellableAction(action string) bool {
	switch action {
	case "preview", "exact", "applyControlPoint", "applyMaterial", "applyDensity",
		"resume", "retry", "profile":
		return true
	default:
		return false
	}
}

func editResponse(out *engine.EditOutcome, err error, eng *engine.Engine) Response {
	if err != nil {
		return fail(diag(err))
	}
	return ok(EditView{
		Frame: eng.FrameView(out.Frame), Reference: eng.FrameView(out.Reference),
		Invalidation: out.Invalidation, Comparison: out.Comparison, Queue: out.Queue,
	})
}

func ok(data any) Response {
	raw, err := json.Marshal(data)
	if err != nil {
		return fail(model.Diag(model.CodeStage, "encode response: "+err.Error()))
	}
	return Response{OK: true, Data: raw}
}

func fail(d *model.Diagnostic) Response {
	if d == nil {
		d = model.Diag(model.CodeStage, "unknown failure")
	}
	return Response{OK: false, Error: d}
}

func diag(err error) *model.Diagnostic {
	if err == nil {
		return nil
	}
	if d, ok := err.(*model.Diagnostic); ok {
		return d
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return model.Diag(model.CodeRecover, "operation cancelled; only committed checkpoints are resumable")
	}
	return model.Diag(model.CodeStage, err.Error())
}

func (a *App) RenderPathName() string { return render.AlgorithmVersion }
