package measure

import (
	"fmt"
	"math"
	"strings"

	"volumelab/pkg/model"
	"volumelab/pkg/volume"
)

func NewAnnotation(v *volume.Volume, request model.AnnotationRequest, id string, sequence uint64) (*model.Annotation, *model.AppError) {
	if v == nil {
		return nil, model.NewError("E_STATE", "no volume is open")
	}
	if _, ok := model.PlaneFromString(string(request.Plane)); !ok {
		return nil, &model.AppError{Code: "E_ARG", Message: "unknown annotation plane", Field: "plane", Actual: request.Plane}
	}
	coordinateSystem := strings.ToLower(strings.TrimSpace(request.CoordinateSystem))
	if coordinateSystem == "" {
		coordinateSystem = "world"
	}
	var a, b model.PointReading
	var appErr *model.AppError
	switch coordinateSystem {
	case "world":
		a, appErr = ReadingFromWorld(v, request.A, 1)
		if appErr == nil {
			b, appErr = ReadingFromWorld(v, request.B, 2)
		}
	case "voxel":
		a, appErr = ReadingFromVoxel(v, request.A, 1)
		if appErr == nil {
			b, appErr = ReadingFromVoxel(v, request.B, 2)
		}
	default:
		return nil, &model.AppError{Code: "E_ARG", Message: "coordinateSystem must be world or voxel", Field: "coordinateSystem", Actual: request.CoordinateSystem}
	}
	if appErr != nil {
		return nil, appErr
	}
	worldLength, voxelLength := Lengths(v, a, b)
	annotation := &model.Annotation{
		ID:              id,
		Plane:           request.Plane,
		EndpointA:       a,
		EndpointB:       b,
		WorldLengthMM:   worldLength,
		VoxelLength:     voxelLength,
		CreatedAtSeq:    sequence,
		StructureID:     request.StructureID,
		MeasurementMode: request.MeasurementMode,
	}
	return annotation, nil
}

func ReadingFromWorld(v *volume.Volume, world model.Vec3, endpoint int) (model.PointReading, *model.AppError) {
	if err := v.ValidateWorldPoint(world); err != nil {
		err.Endpoint = endpoint
		err.Details = map[string]any{"outOfRangeAxes": outOfRangeWorldAxes(v, world)}
		return model.PointReading{}, err
	}
	voxel := v.VoxelFromWorld(world)
	return model.PointReading{World: model.NormalizeVec(world), Voxel: model.NormalizeVec(voxel)}, nil
}

func ReadingFromVoxel(v *volume.Volume, voxel model.Vec3, endpoint int) (model.PointReading, *model.AppError) {
	for axis := 0; axis < 3; axis++ {
		if voxel[axis] < -1e-9 || voxel[axis] > float64(v.Shape[axis]-1)+1e-9 {
			return model.PointReading{}, &model.AppError{
				Code:     "E_RANGE",
				Message:  "voxel point is outside the volume",
				Axis:     model.AxisName(axis),
				Endpoint: endpoint,
				Actual:   voxel[axis],
				Allowed:  [2]float64{0, float64(v.Shape[axis] - 1)},
				Details:  map[string]any{"coordinateSystem": "voxel", "outOfRangeAxes": outOfRangeVoxelAxes(v, voxel)},
			}
		}
	}
	world := v.WorldFromVoxel(voxel)
	return model.PointReading{World: model.NormalizeVec(world), Voxel: model.NormalizeVec(voxel)}, nil
}

func Lengths(v *volume.Volume, a, b model.PointReading) (worldLength, voxelLength float64) {
	var worldSum, voxelSum float64
	for axis := 0; axis < 3; axis++ {
		delta := b.Voxel[axis] - a.Voxel[axis]
		voxelSum += delta * delta
		scaled := delta * v.Spacing[axis]
		worldSum += scaled * scaled
	}
	return math.Sqrt(worldSum), math.Sqrt(voxelSum)
}

func ValidateStoredAnnotation(v *volume.Volume, annotation model.Annotation) *model.AppError {
	aWorldErr := v.ValidateWorldPoint(annotation.EndpointA.World)
	if aWorldErr != nil {
		aWorldErr.Endpoint = 1
		return aWorldErr
	}
	bWorldErr := v.ValidateWorldPoint(annotation.EndpointB.World)
	if bWorldErr != nil {
		bWorldErr.Endpoint = 2
		return bWorldErr
	}
	worldLength, voxelLength := Lengths(v, annotation.EndpointA, annotation.EndpointB)
	if math.Abs(worldLength-annotation.WorldLengthMM) > 1e-9 || math.Abs(voxelLength-annotation.VoxelLength) > 1e-9 {
		return &model.AppError{
			Code:    "E_STATE",
			Message: "stored annotation length readings are inconsistent",
			Details: map[string]any{
				"storedWorldLengthMM":  annotation.WorldLengthMM,
				"derivedWorldLengthMM": worldLength,
				"storedVoxelLength":    annotation.VoxelLength,
				"derivedVoxelLength":   voxelLength,
			},
		}
	}
	for axis := 0; axis < 3; axis++ {
		derivedWorld := annotation.EndpointA.Voxel[axis] * v.Spacing[axis]
		if math.Abs(derivedWorld-annotation.EndpointA.World[axis]) > 1e-8 {
			return &model.AppError{Code: "E_STATE", Message: "stored dual coordinates are inconsistent", Axis: model.AxisName(axis), Endpoint: 1}
		}
		derivedWorld = annotation.EndpointB.Voxel[axis] * v.Spacing[axis]
		if math.Abs(derivedWorld-annotation.EndpointB.World[axis]) > 1e-8 {
			return &model.AppError{Code: "E_STATE", Message: "stored dual coordinates are inconsistent", Axis: model.AxisName(axis), Endpoint: 2}
		}
	}
	return nil
}

func Compare(v *volume.Volume, annotation model.Annotation, structureID, mode string, tolerance float64) (*model.PrecisionReport, *model.AppError) {
	structure, structureErr := findStructure(v.Structures, structureID)
	if structureErr != nil {
		return nil, structureErr
	}
	mode = strings.ToLower(strings.TrimSpace(mode))
	if mode == "" {
		mode = DefaultMode(structure.Type)
	}
	truth, truthErr := truthLength(structure, mode)
	if truthErr != nil {
		return nil, truthErr
	}
	residual := boundaryResidual(structure, mode, annotation)
	if tolerance <= 0 {
		tolerance = v.ToleranceMM
	}
	if tolerance <= 0 {
		tolerance = 2.5
	}
	upperBound, formula, inputs := UpperBound(v, annotation)
	deviation := math.Abs(annotation.WorldLengthMM - truth)
	report := &model.PrecisionReport{
		Pass:               deviation <= tolerance+1e-12 && deviation <= upperBound+1e-12,
		AnnotationID:       annotation.ID,
		StructureID:        structure.ID,
		StructureType:      structure.Type,
		Mode:               mode,
		MeasuredLengthMM:   annotation.WorldLengthMM,
		TruthLengthMM:      truth,
		DeviationMM:        deviation,
		UpperBoundMM:       upperBound,
		ToleranceMM:        tolerance,
		BoundaryResidualMM: residual,
		Formula:            formula,
		Inputs:             inputs,
	}
	if report.Pass {
		return report, nil
	}
	message := "measured deviation exceeds the declared tolerance or the derived upper bound"
	return report, &model.AppError{
		Code:    "E_PREC",
		Message: message,
		Actual:  deviation,
		Allowed: tolerance,
		Details: map[string]any{
			"deviationMM":        deviation,
			"upperBoundMM":       upperBound,
			"toleranceMM":        tolerance,
			"boundaryResidualMM": residual,
		},
	}
}

func UpperBound(v *volume.Volume, annotation model.Annotation) (float64, string, []string) {
	var sum float64
	inputs := make([]string, 0, 6)
	for axis := 0; axis < 3; axis++ {
		maxLayer := int(math.Round(math.Max(math.Abs(annotation.EndpointA.Voxel[axis]), math.Abs(annotation.EndpointB.Voxel[axis]))))
		layerFraction := 0.0
		if v.Shape[axis] > 1 {
			layerFraction = float64(maxLayer) / float64(v.Shape[axis]-1)
		}
		factor := (0.5 + 0.5*float64(annotation.Interpolation)) * (1 + 0.25*layerFraction)
		term := v.Spacing[axis] * factor
		sum += term * term
		inputs = append(inputs, fmt.Sprintf("%s: spacing=%.9g mm, maxLayerIndex=%d, layers=%d, term=%.9g mm", model.AxisName(axis), v.Spacing[axis], maxLayer, v.Shape[axis], term))
	}
	bound := 2 * math.Sqrt(sum)
	formula := "2*sqrt(sum_axis((spacing_axis*(0.5+0.5*interpolationOrder)*(1+0.25*maxLayerIndex_axis/(layers_axis-1)))^2))"
	return bound, formula, inputs
}

func AutoAnnotation(v *volume.Volume, structureID, mode string) (*model.Annotation, *model.AppError) {
	structure, appErr := findStructure(v.Structures, structureID)
	if appErr != nil {
		return nil, appErr
	}
	mode = strings.ToLower(strings.TrimSpace(mode))
	if mode == "" {
		mode = DefaultMode(structure.Type)
	}
	var a, b model.Vec3
	switch mode {
	case "sphere_diameter":
		if structure.Center == nil || structure.Radius == nil {
			return nil, &model.AppError{Code: "E_TRUTH", Message: "sphere truth is incomplete", Field: "structure"}
		}
		radius := *structure.Radius
		a = structure.Center.Sub(model.Vec3{radius, 0, 0})
		b = structure.Center.Add(model.Vec3{radius, 0, 0})
	case "tube_axis":
		if structure.Start == nil || structure.End == nil {
			return nil, &model.AppError{Code: "E_TRUTH", Message: "tube truth is incomplete", Field: "structure"}
		}
		a, b = *structure.Start, *structure.End
	case "box_diagonal":
		if structure.Min == nil || structure.Max == nil {
			return nil, &model.AppError{Code: "E_TRUTH", Message: "box truth is incomplete", Field: "structure"}
		}
		a, b = *structure.Min, *structure.Max
	default:
		return nil, &model.AppError{Code: "E_ARG", Message: "unsupported measurement mode", Field: "mode", Actual: mode}
	}
	plane, planeErr := planeForSegment(a, b)
	if planeErr != nil {
		return nil, planeErr
	}
	annotation, appErr := NewAnnotation(v, model.AnnotationRequest{
		Plane:            plane,
		A:                a,
		B:                b,
		CoordinateSystem: "world",
		StructureID:      structureID,
		MeasurementMode:  mode,
	}, "auto-"+structureID, 0)
	if appErr != nil {
		return nil, appErr
	}
	annotation.Interpolation = 1
	return annotation, nil
}

func DefaultMode(structureType string) string {
	switch structureType {
	case "sphere":
		return "sphere_diameter"
	case "tube":
		return "tube_axis"
	case "box":
		return "box_diagonal"
	default:
		return ""
	}
}

func findStructure(structures []model.Structure, id string) (model.Structure, *model.AppError) {
	for _, structure := range structures {
		if structure.ID == id {
			return structure, nil
		}
	}
	return model.Structure{}, &model.AppError{Code: "E_NOT_FOUND", Message: "analytic structure is absent", Field: "structureId", Actual: id}
}

func truthLength(structure model.Structure, mode string) (float64, *model.AppError) {
	switch mode {
	case "sphere_diameter":
		if structure.Radius == nil {
			return 0, &model.AppError{Code: "E_TRUTH", Message: "sphere radius is absent"}
		}
		return 2 * *structure.Radius, nil
	case "tube_axis":
		if structure.Start == nil || structure.End == nil {
			return 0, &model.AppError{Code: "E_TRUTH", Message: "tube axis endpoints are absent"}
		}
		return structure.End.Sub(*structure.Start).Norm(), nil
	case "box_diagonal":
		if structure.Min == nil || structure.Max == nil {
			return 0, &model.AppError{Code: "E_TRUTH", Message: "box bounds are absent"}
		}
		return structure.Max.Sub(*structure.Min).Norm(), nil
	default:
		return 0, &model.AppError{Code: "E_ARG", Message: "unsupported measurement mode", Field: "mode", Actual: mode}
	}
}

func boundaryResidual(structure model.Structure, mode string, annotation model.Annotation) float64 {
	switch mode {
	case "sphere_diameter":
		if structure.Center == nil || structure.Radius == nil {
			return math.Inf(1)
		}
		da := math.Abs(annotation.EndpointA.World.Sub(*structure.Center).Norm() - *structure.Radius)
		db := math.Abs(annotation.EndpointB.World.Sub(*structure.Center).Norm() - *structure.Radius)
		midpointOff := annotation.EndpointA.World.Add(annotation.EndpointB.World).Mul(0.5).Sub(*structure.Center).Norm()
		return math.Max(math.Max(da, db), midpointOff)
	case "tube_axis":
		if structure.Start == nil || structure.End == nil {
			return math.Inf(1)
		}
		forward := math.Max(annotation.EndpointA.World.Sub(*structure.Start).Norm(), annotation.EndpointB.World.Sub(*structure.End).Norm())
		reverse := math.Max(annotation.EndpointA.World.Sub(*structure.End).Norm(), annotation.EndpointB.World.Sub(*structure.Start).Norm())
		return math.Min(forward, reverse)
	case "box_diagonal":
		if structure.Min == nil || structure.Max == nil {
			return math.Inf(1)
		}
		forward := math.Max(annotation.EndpointA.World.Sub(*structure.Min).Norm(), annotation.EndpointB.World.Sub(*structure.Max).Norm())
		reverse := math.Max(annotation.EndpointA.World.Sub(*structure.Max).Norm(), annotation.EndpointB.World.Sub(*structure.Min).Norm())
		return math.Min(forward, reverse)
	default:
		return math.Inf(1)
	}
}

func planeForSegment(a, b model.Vec3) (model.Plane, *model.AppError) {
	if math.Abs(a[2]-b[2]) < 1e-9 {
		return model.PlaneAxial, nil
	}
	if math.Abs(a[1]-b[1]) < 1e-9 {
		return model.PlaneCoronal, nil
	}
	if math.Abs(a[0]-b[0]) < 1e-9 {
		return model.PlaneSagittal, nil
	}
	return model.PlaneAxial, nil
}

func outOfRangeWorldAxes(v *volume.Volume, point model.Vec3) []string {
	var axes []string
	for axis := 0; axis < 3; axis++ {
		max := float64(v.Shape[axis]-1) * v.Spacing[axis]
		if point[axis] < 0 || point[axis] > max {
			axes = append(axes, model.AxisName(axis))
		}
	}
	return axes
}

func outOfRangeVoxelAxes(v *volume.Volume, point model.Vec3) []string {
	var axes []string
	for axis := 0; axis < 3; axis++ {
		if point[axis] < 0 || point[axis] > float64(v.Shape[axis]-1) {
			axes = append(axes, model.AxisName(axis))
		}
	}
	return axes
}
