package session

import (
	"context"
	"fmt"
	"sync"

	"volumelab/pkg/measure"
	"volumelab/pkg/model"
	"volumelab/pkg/pool"
	"volumelab/pkg/render"
	"volumelab/pkg/volume"
)

type View struct {
	State    State          `json:"state"`
	Triple   *render.Triple `json:"planes"`
	Snapshot SnapshotInfo   `json:"snapshot"`
}

type OpenResult struct {
	View
	Recovered     bool   `json:"recovered"`
	InvalidReason string `json:"invalidReason,omitempty"`
}

type Session struct {
	Volume   *volume.Volume
	Store    *Store
	Executor *pool.Executor

	mu    sync.Mutex
	state State
	info  SnapshotInfo

	cancelMu      sync.Mutex
	cancelSeq     uint64
	currentCancel context.CancelFunc
}

func Open(ctx context.Context, v *volume.Volume, store *Store, executor *pool.Executor) (*Session, *OpenResult, *model.AppError) {
	if v == nil {
		return nil, nil, model.NewError("E_STATE", "no volume is open")
	}
	s := &Session{Volume: v, Store: store, Executor: executor}
	loaded := store.Load()
	invalidReason := loaded.InvalidReason
	if loaded.Recovered && loaded.Payload != nil {
		payload := loaded.Payload
		stateCheck := &model.AppError{}
		switch {
		case payload.VolumeID != v.ID:
			stateCheck = &model.AppError{Code: "E_VERSION", Message: "snapshot volume id does not match", Field: "volumeId", Actual: payload.VolumeID, Allowed: v.ID}
		case payload.ContentHash != v.ContentHash:
			stateCheck = &model.AppError{Code: "E_VERSION", Message: "snapshot content hash does not match", Field: "contentHash", Actual: payload.ContentHash, Allowed: v.ContentHash}
		case payload.ManifestHash != v.ManifestHash:
			stateCheck = &model.AppError{Code: "E_VERSION", Message: "snapshot manifest hash does not match", Field: "manifestHash", Actual: payload.ManifestHash, Allowed: v.ManifestHash}
		default:
			stateCheck = ValidateState(v, payload.State)
		}
		if stateCheck == nil {
			triple, renderErr := render.RenderTriple(ctx, v, payload.State.RenderRequest(), executor)
			if renderErr == nil {
				stateCheck = CompareImageHashes(payload.State, triple)
			} else {
				stateCheck = renderErr
			}
			if stateCheck == nil {
				s.state = payload.State.Clone()
				s.info = loaded.Info
				return s, &OpenResult{
					View:      View{State: s.state.Clone(), Triple: triple, Snapshot: s.info},
					Recovered: true,
				}, nil
			}
		}
		invalidReason = stateCheck.Message
	}
	if loaded.Info.Exists || invalidReason != "" {
		_ = store.Invalidate()
	}
	state := DefaultState(v)
	triple, appErr := render.RenderTriple(ctx, v, state.RenderRequest(), executor)
	if appErr != nil {
		return nil, nil, appErr
	}
	FillImageHashes(&state, triple)
	if appErr := ValidateState(v, state); appErr != nil {
		return nil, nil, appErr
	}
	info, appErr := store.Commit(ctx, payloadFor(v, state))
	if appErr != nil {
		return nil, nil, appErr
	}
	s.state = state
	s.info = info
	return s, &OpenResult{
		View:          View{State: state.Clone(), Triple: triple, Snapshot: info},
		Recovered:     false,
		InvalidReason: invalidReason,
	}, nil
}

func (s *Session) Update(ctx context.Context, mutate func(*State) error) (*View, *model.AppError) {
	s.mu.Lock()
	defer s.mu.Unlock()
	opCtx, cleanup := s.beginOperation(ctx)
	defer cleanup()
	if err := opCtx.Err(); err != nil {
		return nil, &model.AppError{Code: "E_CANCEL", Message: "operation canceled before mutation", Details: map[string]any{"cause": err.Error()}}
	}
	draft := s.state.Clone()
	draft.Sequence = s.state.Sequence + 1
	if err := mutate(&draft); err != nil {
		return nil, model.AsAppError(err)
	}
	triple, renderErr := render.RenderTriple(opCtx, s.Volume, draft.RenderRequest(), s.Executor)
	if renderErr != nil {
		return nil, renderErr
	}
	if err := opCtx.Err(); err != nil {
		return nil, &model.AppError{Code: "E_CANCEL", Message: "operation canceled before commit", Details: map[string]any{"cause": err.Error()}}
	}
	FillImageHashes(&draft, triple)
	if err := ValidateState(s.Volume, draft); err != nil {
		return nil, err
	}
	info, appErr := s.Store.Commit(opCtx, payloadFor(s.Volume, draft))
	if appErr != nil {
		return nil, appErr
	}
	s.state = draft
	s.info = info
	return &View{State: draft.Clone(), Triple: triple, Snapshot: info}, nil
}

func (s *Session) SetWindow(ctx context.Context, window model.Window) (*View, *model.AppError) {
	return s.Update(ctx, func(draft *State) error {
		draft.Window = window
		return nil
	})
}

func (s *Session) SetCrosshair(ctx context.Context, point model.Vec3) (*View, *model.AppError) {
	return s.Update(ctx, func(draft *State) error {
		draft.Crosshair = model.NormalizeVec(point)
		return nil
	})
}

func (s *Session) SetPlanePosition(ctx context.Context, plane model.Plane, position float64) (*View, *model.AppError) {
	info, ok := model.PlaneGeometry(plane, s.Volume.Shape, s.Volume.Spacing)
	if !ok {
		return nil, &model.AppError{Code: "E_ARG", Message: "unknown plane", Field: "plane", Actual: plane}
	}
	return s.Update(ctx, func(draft *State) error {
		draft.Crosshair[info.Normal] = position
		return nil
	})
}

func (s *Session) SetThickness(ctx context.Context, plane model.Plane, thickness float64) (*View, *model.AppError) {
	if _, ok := model.PlaneFromString(string(plane)); !ok {
		return nil, &model.AppError{Code: "E_ARG", Message: "unknown plane", Field: "plane", Actual: plane}
	}
	return s.Update(ctx, func(draft *State) error {
		draft.ThicknessMM[string(plane)] = thickness
		return nil
	})
}

func (s *Session) SetInterpolation(ctx context.Context, order int) (*View, *model.AppError) {
	return s.Update(ctx, func(draft *State) error {
		draft.InterpolationOrder = order
		for i := range draft.Annotations {
			draft.Annotations[i].Interpolation = order
		}
		return nil
	})
}

func (s *Session) AddAnnotation(ctx context.Context, request model.AnnotationRequest) (*View, *model.AppError) {
	return s.Update(ctx, func(draft *State) error {
		id := annotationID(draft.NextAnnotation)
		annotation, appErr := measure.NewAnnotation(s.Volume, request, id, draft.Sequence)
		if appErr != nil {
			return appErr
		}
		annotation.Interpolation = draft.InterpolationOrder
		draft.Annotations = append(draft.Annotations, *annotation)
		draft.NextAnnotation++
		return nil
	})
}

func (s *Session) DeleteAnnotation(ctx context.Context, id string) (*View, *model.AppError) {
	return s.Update(ctx, func(draft *State) error {
		for i := range draft.Annotations {
			if draft.Annotations[i].ID == id {
				draft.Annotations = append(draft.Annotations[:i], draft.Annotations[i+1:]...)
				return nil
			}
		}
		return &model.AppError{Code: "E_NOT_FOUND", Message: "annotation is absent", Field: "annotationId", Actual: id}
	})
}

func (s *Session) AddAutoAnnotation(ctx context.Context, structureID, mode string) (*View, *model.AppError) {
	return s.Update(ctx, func(draft *State) error {
		annotation, appErr := measure.AutoAnnotation(s.Volume, structureID, mode)
		if appErr != nil {
			return appErr
		}
		annotation.ID = annotationID(draft.NextAnnotation)
		annotation.CreatedAtSeq = draft.Sequence
		annotation.Interpolation = draft.InterpolationOrder
		draft.Annotations = append(draft.Annotations, *annotation)
		draft.NextAnnotation++
		return nil
	})
}

func (s *Session) Precision(annotationID, structureID, mode string, tolerance float64) (*model.PrecisionReport, *model.AppError) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, annotation := range s.state.Annotations {
		if annotation.ID == annotationID {
			if structureID == "" {
				structureID = annotation.StructureID
			}
			if mode == "" {
				mode = annotation.MeasurementMode
			}
			return measure.Compare(s.Volume, annotation, structureID, mode, tolerance)
		}
	}
	return nil, &model.AppError{Code: "E_NOT_FOUND", Message: "annotation is absent", Field: "annotationId", Actual: annotationID}
}

func (s *Session) State() State {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state.Clone()
}

func (s *Session) SnapshotInfo() SnapshotInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.info
}

func (s *Session) Cancel() {
	s.cancelMu.Lock()
	cancel := s.currentCancel
	s.cancelMu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (s *Session) Close() {
	s.Cancel()
}

func (s *Session) beginOperation(parent context.Context) (context.Context, func()) {
	ctx, cancel := context.WithCancel(parent)
	s.cancelMu.Lock()
	s.cancelSeq++
	generation := s.cancelSeq
	s.currentCancel = cancel
	s.cancelMu.Unlock()
	return ctx, func() {
		cancel()
		s.cancelMu.Lock()
		if s.cancelSeq == generation {
			s.currentCancel = nil
		}
		s.cancelMu.Unlock()
	}
}

func payloadFor(v *volume.Volume, state State) SnapshotPayload {
	return SnapshotPayload{
		Version:          SnapshotFormatVersion,
		AlgorithmVersion: AlgorithmVersion,
		VolumeID:         v.ID,
		ContentHash:      v.ContentHash,
		ManifestHash:     v.ManifestHash,
		State:            state.Clone(),
	}
}

func annotationID(serial uint64) string {
	return fmt.Sprintf("ann-%04d", serial)
}
