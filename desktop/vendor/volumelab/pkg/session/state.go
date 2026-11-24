package session

import (
	"math"

	"volumelab/pkg/model"
	"volumelab/pkg/render"
	"volumelab/pkg/volume"
)

const (
	SnapshotFormatVersion = 1
	AlgorithmVersion      = "resample-nearest-linear-v1"
	SnapshotFileFormat    = "volumelab.session/v1"
)

type State struct {
	VolumeID           string             `json:"volumeId"`
	ContentHash        string             `json:"contentHash"`
	ManifestHash       string             `json:"manifestHash"`
	Window             model.Window       `json:"window"`
	Crosshair          model.Vec3         `json:"crosshair"`
	ThicknessMM        map[string]float64 `json:"thicknessMM"`
	InterpolationOrder int                `json:"interpolationOrder"`
	Annotations        []model.Annotation `json:"annotations"`
	NextAnnotation     uint64             `json:"nextAnnotation"`
	Sequence           uint64             `json:"sequence"`
	ImageHashes        map[string]string  `json:"imageHashes"`
}

func (s State) Clone() State {
	out := s
	out.ThicknessMM = make(map[string]float64, len(s.ThicknessMM))
	for key, value := range s.ThicknessMM {
		out.ThicknessMM[key] = value
	}
	out.Annotations = append([]model.Annotation(nil), s.Annotations...)
	out.ImageHashes = make(map[string]string, len(s.ImageHashes))
	for key, value := range s.ImageHashes {
		out.ImageHashes[key] = value
	}
	return out
}

func DefaultState(v *volume.Volume) State {
	width := (v.GrayRange[1] - v.GrayRange[0]) / 2
	level := (v.GrayRange[0] + v.GrayRange[1]) / 2
	if level < v.GrayRange[0] || level > v.GrayRange[1] {
		level = v.GrayRange[0]
	}
	if width <= 0 {
		width = 1
	}
	center := model.Vec3{
		float64(v.Shape[0]-1) * v.Spacing[0] / 2,
		float64(v.Shape[1]-1) * v.Spacing[1] / 2,
		float64(v.Shape[2]-1) * v.Spacing[2] / 2,
	}
	thickness := map[string]float64{}
	for _, plane := range model.AllPlanes {
		info, _ := model.PlaneGeometry(plane, v.Shape, v.Spacing)
		thickness[string(plane)] = v.Spacing[info.Normal]
	}
	return State{
		VolumeID:           v.ID,
		ContentHash:        v.ContentHash,
		ManifestHash:       v.ManifestHash,
		Window:             model.Window{Width: width, Level: level},
		Crosshair:          center,
		ThicknessMM:        thickness,
		InterpolationOrder: 1,
		Annotations:        []model.Annotation{},
		NextAnnotation:     1,
		Sequence:           1,
		ImageHashes:        map[string]string{},
	}
}

func (s State) RenderRequest() render.Request {
	thickness := make(map[string]float64, len(s.ThicknessMM))
	for key, value := range s.ThicknessMM {
		thickness[key] = value
	}
	return render.Request{
		Crosshair:          s.Crosshair,
		Window:             s.Window,
		ThicknessMM:        thickness,
		InterpolationOrder: s.InterpolationOrder,
	}
}

func ValidateState(v *volume.Volume, state State) *model.AppError {
	if v == nil {
		return model.NewError("E_STATE", "no volume is open")
	}
	if state.VolumeID != v.ID {
		return &model.AppError{Code: "E_VERSION", Message: "snapshot volume id does not match", Field: "volumeId", Actual: state.VolumeID, Allowed: v.ID}
	}
	if state.ContentHash != v.ContentHash {
		return &model.AppError{Code: "E_VERSION", Message: "snapshot content hash does not match", Field: "contentHash", Actual: state.ContentHash, Allowed: v.ContentHash}
	}
	if state.ManifestHash != v.ManifestHash {
		return &model.AppError{Code: "E_VERSION", Message: "snapshot manifest hash does not match", Field: "manifestHash", Actual: state.ManifestHash, Allowed: v.ManifestHash}
	}
	if state.Sequence == 0 {
		return &model.AppError{Code: "E_STATE", Message: "snapshot sequence must be positive", Field: "sequence"}
	}
	if state.NextAnnotation == 0 {
		return &model.AppError{Code: "E_STATE", Message: "next annotation serial must be positive", Field: "nextAnnotation"}
	}
	if !render.InterpolationSupported(state.InterpolationOrder) {
		return &model.AppError{Code: "E_STATE", Message: "snapshot interpolation order is invalid", Field: "interpolationOrder", Actual: state.InterpolationOrder}
	}
	if err := render.ValidateWindow(state.Window, v.GrayRange); err != nil {
		return err
	}
	if err := v.ValidateWorldPoint(state.Crosshair); err != nil {
		return err
	}
	if len(state.ThicknessMM) != 3 {
		return &model.AppError{Code: "E_STATE", Message: "snapshot must contain thickness for all three planes", Field: "thicknessMM"}
	}
	for _, plane := range model.AllPlanes {
		if _, ok := state.ThicknessMM[string(plane)]; !ok {
			return &model.AppError{Code: "E_STATE", Message: "snapshot thickness is missing a plane", Field: "thicknessMM." + string(plane)}
		}
	}
	if _, appErr := render.ValidateRequest(v, state.RenderRequest()); appErr != nil {
		return appErr
	}
	ids := map[string]bool{}
	for _, annotation := range state.Annotations {
		if annotation.ID == "" || ids[annotation.ID] {
			return &model.AppError{Code: "E_STATE", Message: "snapshot has a missing or duplicate annotation id", Field: "annotations", Actual: annotation.ID}
		}
		ids[annotation.ID] = true
		if !render.InterpolationSupported(annotation.Interpolation) {
			return &model.AppError{Code: "E_STATE", Message: "snapshot annotation interpolation is invalid", Field: "annotations", Actual: annotation.Interpolation}
		}
		if appErr := measureValidate(v, annotation); appErr != nil {
			return appErr
		}
	}
	if len(state.ImageHashes) != 3 {
		return &model.AppError{Code: "E_STATE", Message: "snapshot must contain all three image hashes", Field: "imageHashes"}
	}
	for _, plane := range model.AllPlanes {
		if state.ImageHashes[string(plane)] == "" {
			return &model.AppError{Code: "E_STATE", Message: "snapshot image hash is missing", Field: "imageHashes." + string(plane)}
		}
	}
	return nil
}

func CompareImageHashes(state State, triple *render.Triple) *model.AppError {
	for _, image := range triple.Images {
		want := state.ImageHashes[string(image.Plane)]
		if want == "" || want != image.PixelHash {
			return &model.AppError{
				Code:    "E_VERSION",
				Message: "snapshot image hash does not match deterministic reconstruction",
				Field:   "imageHashes." + string(image.Plane),
				Actual:  image.PixelHash,
				Allowed: want,
			}
		}
	}
	return nil
}

func FillImageHashes(state *State, triple *render.Triple) {
	state.ImageHashes = make(map[string]string, len(triple.Images))
	for _, image := range triple.Images {
		state.ImageHashes[string(image.Plane)] = image.PixelHash
	}
}

func WidthRange(state State) (float64, float64) {
	return state.Window.Level - state.Window.Width/2, state.Window.Level + state.Window.Width/2
}

func finite(v float64) bool {
	return !math.IsNaN(v) && !math.IsInf(v, 0)
}
