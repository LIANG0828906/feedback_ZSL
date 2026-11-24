package service

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"volumelab/pkg/model"
	"volumelab/pkg/pool"
	"volumelab/pkg/render"
	"volumelab/pkg/samplegen"
	"volumelab/pkg/session"
	"volumelab/pkg/volume"
)

type Options struct {
	ManifestPath    string
	VolumeID        string
	SessionDir      string
	SnapshotPath    string
	MaxParallelism  int
	MaxBufferPixels int
}

type Service struct {
	opts Options

	opMu     sync.Mutex
	stateMu  sync.RWMutex
	session  *session.Session
	volume   *volume.Volume
	manifest *volume.Manifest
	lastView *SessionView
	executor *pool.Executor
}

type APIResult struct {
	OK    bool            `json:"ok"`
	Data  any             `json:"data,omitempty"`
	Error *model.AppError `json:"error,omitempty"`
}

type SessionView struct {
	Volume           model.VolumeSummary  `json:"volume"`
	ManifestPath     string               `json:"manifestPath"`
	SnapshotPath     string               `json:"snapshotPath"`
	State            session.State        `json:"state"`
	Planes           *render.Triple       `json:"planes"`
	Snapshot         session.SnapshotInfo `json:"snapshot"`
	Recovered        bool                 `json:"recovered"`
	InvalidReason    string               `json:"invalidReason,omitempty"`
	ThicknessRanges  map[string]any       `json:"thicknessRanges"`
	AvailableVolumes []ManifestVolumeInfo `json:"availableVolumes"`
	ParallelStats    pool.Stats           `json:"parallelStats"`
	Limits           map[string]int       `json:"limits"`
	WorldBounds      [2]model.Vec3        `json:"worldBounds"`
}

type ManifestVolumeInfo struct {
	ID         string            `json:"id"`
	AxisOrder  string            `json:"axisOrder"`
	Dimensions [3]int            `json:"dimensions"`
	Spacing    [3]float64        `json:"spacing"`
	GrayRange  [2]float64        `json:"grayRange"`
	Structures []model.Structure `json:"structures"`
}

func New(opts Options) *Service {
	if opts.MaxParallelism <= 0 {
		opts.MaxParallelism = 3
	}
	if opts.MaxBufferPixels <= 0 {
		opts.MaxBufferPixels = 16384
	}
	return &Service{opts: opts}
}

func (s *Service) Invoke(ctx context.Context, action, payload string) APIResult {
	switch action {
	case "status":
		return s.ok(s.status())
	case "generate":
		s.opMu.Lock()
		defer s.opMu.Unlock()
		return s.generate(payload)
	case "open":
		s.opMu.Lock()
		defer s.opMu.Unlock()
		return s.open(ctx, payload)
	case "state":
		s.opMu.Lock()
		defer s.opMu.Unlock()
		view := s.currentView()
		if view == nil {
			return fail(model.NewError("E_STATE", "no volume is open"))
		}
		return s.ok(view)
	case "pause":
		s.opMu.Lock()
		defer s.opMu.Unlock()
		view := s.currentView()
		if view == nil {
			return fail(model.NewError("E_STATE", "no volume is open"))
		}
		return s.ok(map[string]any{
			"paused":   true,
			"sequence": view.State.Sequence,
			"snapshot": view.Snapshot,
		})
	case "set_window":
		var req windowRequest
		return s.mutate(ctx, payload, &req, func(sess *session.Session) (*session.View, *model.AppError) {
			return sess.SetWindow(ctx, model.Window{Width: req.Width, Level: req.Level})
		})
	case "set_crosshair":
		var req crosshairRequest
		return s.mutate(ctx, payload, &req, func(sess *session.Session) (*session.View, *model.AppError) {
			return sess.SetCrosshair(ctx, model.Vec3{req.X, req.Y, req.Z})
		})
	case "set_plane_position":
		var req planePositionRequest
		return s.mutate(ctx, payload, &req, func(sess *session.Session) (*session.View, *model.AppError) {
			plane, ok := model.PlaneFromString(req.Plane)
			if !ok {
				return nil, &model.AppError{Code: "E_ARG", Message: "unknown plane", Field: "plane", Actual: req.Plane}
			}
			return sess.SetPlanePosition(ctx, plane, req.Position)
		})
	case "set_thickness":
		var req thicknessRequest
		return s.mutate(ctx, payload, &req, func(sess *session.Session) (*session.View, *model.AppError) {
			plane, ok := model.PlaneFromString(req.Plane)
			if !ok {
				return nil, &model.AppError{Code: "E_ARG", Message: "unknown plane", Field: "plane", Actual: req.Plane}
			}
			return sess.SetThickness(ctx, plane, req.ThicknessMM)
		})
	case "set_interpolation":
		var req interpolationRequest
		return s.mutate(ctx, payload, &req, func(sess *session.Session) (*session.View, *model.AppError) {
			return sess.SetInterpolation(ctx, req.Order)
		})
	case "add_annotation":
		var req annotationRequest
		return s.mutate(ctx, payload, &req, func(sess *session.Session) (*session.View, *model.AppError) {
			plane, ok := model.PlaneFromString(req.Plane)
			if !ok {
				return nil, &model.AppError{Code: "E_ARG", Message: "unknown plane", Field: "plane", Actual: req.Plane}
			}
			return sess.AddAnnotation(ctx, model.AnnotationRequest{
				Plane:            plane,
				A:                model.Vec3{req.A.X, req.A.Y, req.A.Z},
				B:                model.Vec3{req.B.X, req.B.Y, req.B.Z},
				CoordinateSystem: req.CoordinateSystem,
				StructureID:      req.StructureID,
				MeasurementMode:  req.MeasurementMode,
			})
		})
	case "auto_annotation":
		var req autoAnnotationRequest
		return s.mutate(ctx, payload, &req, func(sess *session.Session) (*session.View, *model.AppError) {
			return sess.AddAutoAnnotation(ctx, req.StructureID, req.Mode)
		})
	case "delete_annotation":
		var req deleteAnnotationRequest
		return s.mutate(ctx, payload, &req, func(sess *session.Session) (*session.View, *model.AppError) {
			return sess.DeleteAnnotation(ctx, req.AnnotationID)
		})
	case "precision":
		return s.precision(payload)
	case "cancel":
		sess := s.currentSession()
		if sess != nil {
			sess.Cancel()
		}
		return s.ok(map[string]any{"canceled": sess != nil})
	case "close":
		s.opMu.Lock()
		defer s.opMu.Unlock()
		sess := s.currentSession()
		if sess != nil {
			sess.Close()
		}
		s.stateMu.Lock()
		s.session = nil
		s.lastView = nil
		s.stateMu.Unlock()
		return s.ok(map[string]any{"closed": true})
	default:
		return fail(&model.AppError{Code: "E_ACTION", Message: "unknown action", Field: "action", Actual: action})
	}
}

func (s *Service) generate(raw string) APIResult {
	var req generateRequest
	if err := decode(raw, &req); err != nil {
		return fail(err)
	}
	if req.OutputDir == "" {
		req.OutputDir = "samples"
	}
	result, appErr := samplegen.Generate(samplegen.Options{
		OutputDir:    req.OutputDir,
		Seed:         req.Seed,
		ManifestName: req.ManifestName,
	})
	if appErr != nil {
		return fail(model.AsAppError(appErr))
	}
	s.stateMu.Lock()
	s.opts.ManifestPath = result.ManifestPath
	s.stateMu.Unlock()
	return s.ok(result)
}

func (s *Service) open(ctx context.Context, raw string) APIResult {
	var req openRequest
	if err := decode(raw, &req); err != nil {
		return fail(err)
	}
	if req.ManifestPath == "" {
		req.ManifestPath = s.opts.ManifestPath
	}
	if req.ManifestPath == "" {
		return fail(&model.AppError{Code: "E_ARG", Message: "manifest path is required", Field: "manifestPath"})
	}
	manifest, vol, appErr := volume.LoadFromManifest(req.ManifestPath, req.VolumeID)
	if appErr != nil {
		return fail(model.AsAppError(appErr))
	}
	maxWorkers := req.MaxParallelism
	if maxWorkers <= 0 {
		maxWorkers = s.opts.MaxParallelism
	}
	maxPixels := req.MaxBufferPixels
	if maxPixels <= 0 {
		maxPixels = s.opts.MaxBufferPixels
	}
	executor, err := pool.New(maxWorkers, maxPixels)
	if err != nil {
		return fail(model.AsAppError(err))
	}
	snapshotPath := req.SnapshotPath
	if snapshotPath == "" {
		if s.opts.SnapshotPath != "" {
			snapshotPath = s.opts.SnapshotPath
		} else {
			sessionDir := req.SessionDir
			if sessionDir == "" {
				sessionDir = s.opts.SessionDir
			}
			if sessionDir == "" {
				sessionDir = filepath.Join(filepath.Dir(manifest.ManifestPath), "sessions")
			}
			snapshotPath = filepath.Join(sessionDir, safeName(vol.ID)+".session.json")
		}
	}
	store := session.NewStore(snapshotPath)
	sess, opened, sessionErr := session.Open(ctx, vol, store, executor)
	if sessionErr != nil {
		return fail(sessionErr)
	}
	view := s.buildView(manifest, vol, sess, opened.View, opened.Recovered, opened.InvalidReason, executor)
	s.stateMu.Lock()
	s.session = sess
	s.volume = vol
	s.manifest = manifest
	s.executor = executor
	s.lastView = &view
	s.opts.ManifestPath = manifest.ManifestPath
	s.opts.VolumeID = vol.ID
	s.stateMu.Unlock()
	return s.ok(view)
}

func (s *Service) mutate(ctx context.Context, raw string, request any, run func(*session.Session) (*session.View, *model.AppError)) APIResult {
	s.opMu.Lock()
	defer s.opMu.Unlock()
	if err := decode(raw, request); err != nil {
		return fail(err)
	}
	sess := s.currentSession()
	if sess == nil {
		return fail(model.NewError("E_STATE", "no volume is open"))
	}
	view, appErr := run(sess)
	if appErr != nil {
		return fail(appErr)
	}
	manifest, vol, executor := s.currentContext()
	full := s.buildView(manifest, vol, sess, *view, false, "", executor)
	s.stateMu.Lock()
	s.lastView = &full
	s.stateMu.Unlock()
	return s.ok(full)
}

func (s *Service) precision(raw string) APIResult {
	s.opMu.Lock()
	defer s.opMu.Unlock()
	sess := s.currentSession()
	if sess == nil {
		return fail(model.NewError("E_STATE", "no volume is open"))
	}
	var req precisionRequest
	if err := decode(raw, &req); err != nil {
		return fail(err)
	}
	report, appErr := sess.Precision(req.AnnotationID, req.StructureID, req.Mode, req.ToleranceMM)
	if appErr != nil {
		result := fail(appErr)
		if report != nil {
			result.Data = report
		}
		return result
	}
	return s.ok(report)
}

func (s *Service) buildView(manifest *volume.Manifest, vol *volume.Volume, sess *session.Session, view session.View, recovered bool, invalidReason string, executor *pool.Executor) SessionView {
	ranges := map[string]any{}
	for _, plane := range model.AllPlanes {
		r, _ := render.ThicknessRange(vol, plane)
		ranges[string(plane)] = r
	}
	stats := pool.Stats{}
	if executor != nil {
		stats = executor.Stats()
	}
	return SessionView{
		Volume:           vol.Summary(),
		ManifestPath:     manifest.ManifestPath,
		SnapshotPath:     sess.Store.Path,
		State:            view.State,
		Planes:           view.Triple,
		Snapshot:         view.Snapshot,
		Recovered:        recovered,
		InvalidReason:    invalidReason,
		ThicknessRanges:  ranges,
		AvailableVolumes: manifestVolumeInfos(manifest),
		ParallelStats:    stats,
		Limits: map[string]int{
			"maxParallelism":         executor.MaxWorkers(),
			"maxBufferPixelsPerTask": executor.MaxPixelsPerTask(),
		},
		WorldBounds: vol.WorldBounds(),
	}
}

func (s *Service) status() map[string]any {
	s.stateMu.RLock()
	defer s.stateMu.RUnlock()
	status := map[string]any{
		"manifestPath": s.opts.ManifestPath,
		"sessionDir":   s.opts.SessionDir,
		"limits": map[string]int{
			"maxParallelism":         s.opts.MaxParallelism,
			"maxBufferPixelsPerTask": s.opts.MaxBufferPixels,
		},
	}
	if s.lastView != nil {
		status["open"] = true
		status["volumeId"] = s.lastView.Volume.ID
		status["snapshotPath"] = s.lastView.SnapshotPath
	} else {
		status["open"] = false
	}
	return status
}

func (s *Service) currentView() *SessionView {
	s.stateMu.RLock()
	defer s.stateMu.RUnlock()
	return s.lastView
}

func (s *Service) currentSession() *session.Session {
	s.stateMu.RLock()
	defer s.stateMu.RUnlock()
	return s.session
}

func (s *Service) currentContext() (*volume.Manifest, *volume.Volume, *pool.Executor) {
	s.stateMu.RLock()
	defer s.stateMu.RUnlock()
	return s.manifest, s.volume, s.executor
}

func (s *Service) ok(data any) APIResult {
	return APIResult{OK: true, Data: data}
}

func fail(err *model.AppError) APIResult {
	return APIResult{OK: false, Error: err}
}

func decode(raw string, dst any) *model.AppError {
	if strings.TrimSpace(raw) == "" || strings.TrimSpace(raw) == "null" {
		return nil
	}
	if err := json.Unmarshal([]byte(raw), dst); err != nil {
		return &model.AppError{Code: "E_ARG", Message: "invalid JSON payload", Field: "payload", Details: map[string]any{"cause": err.Error()}}
	}
	return nil
}

func manifestVolumeInfos(manifest *volume.Manifest) []ManifestVolumeInfo {
	var infos []ManifestVolumeInfo
	for _, decl := range manifest.Volumes {
		info := ManifestVolumeInfo{ID: decl.ID, AxisOrder: decl.AxisOrder, Structures: decl.Structures}
		for i := 0; i < 3 && i < len(decl.Dimensions); i++ {
			info.Dimensions[i] = decl.Dimensions[i]
		}
		for i := 0; i < 3 && i < len(decl.Spacing); i++ {
			if decl.Spacing[i] != nil {
				info.Spacing[i] = *decl.Spacing[i]
			}
		}
		for i := 0; i < 2 && i < len(decl.GrayRange); i++ {
			if decl.GrayRange[i] != nil {
				info.GrayRange[i] = *decl.GrayRange[i]
			}
		}
		infos = append(infos, info)
	}
	return infos
}

func safeName(name string) string {
	var builder strings.Builder
	for _, r := range name {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.' {
			builder.WriteRune(r)
		} else {
			builder.WriteByte('_')
		}
	}
	if builder.Len() == 0 {
		return "volume-" + strconv.FormatUint(uint64(len(name)), 10)
	}
	return builder.String()
}

type generateRequest struct {
	OutputDir    string `json:"outputDir"`
	Seed         int64  `json:"seed"`
	ManifestName string `json:"manifestName"`
}

type openRequest struct {
	ManifestPath    string `json:"manifestPath"`
	VolumeID        string `json:"volumeId"`
	SessionDir      string `json:"sessionDir"`
	SnapshotPath    string `json:"snapshotPath"`
	MaxParallelism  int    `json:"maxParallelism"`
	MaxBufferPixels int    `json:"maxBufferPixels"`
}

type windowRequest struct {
	Width float64 `json:"width"`
	Level float64 `json:"level"`
}

type crosshairRequest struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
	Z float64 `json:"z"`
}

type planePositionRequest struct {
	Plane    string  `json:"plane"`
	Position float64 `json:"position"`
}

type thicknessRequest struct {
	Plane       string  `json:"plane"`
	ThicknessMM float64 `json:"thicknessMM"`
}

type interpolationRequest struct {
	Order int `json:"order"`
}

type annotationRequest struct {
	Plane            string `json:"plane"`
	A                point  `json:"a"`
	B                point  `json:"b"`
	CoordinateSystem string `json:"coordinateSystem"`
	StructureID      string `json:"structureId"`
	MeasurementMode  string `json:"measurementMode"`
}

type point struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
	Z float64 `json:"z"`
}

type autoAnnotationRequest struct {
	StructureID string `json:"structureId"`
	Mode        string `json:"mode"`
}

type deleteAnnotationRequest struct {
	AnnotationID string `json:"annotationId"`
}

type precisionRequest struct {
	AnnotationID string  `json:"annotationId"`
	StructureID  string  `json:"structureId"`
	Mode         string  `json:"mode"`
	ToleranceMM  float64 `json:"toleranceMM"`
}
