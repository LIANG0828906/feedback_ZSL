package render

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"math"
	"sort"

	"volumelab/pkg/model"
	"volumelab/pkg/pool"
	"volumelab/pkg/volume"
)

type Request struct {
	Crosshair          model.Vec3         `json:"crosshair"`
	Window             model.Window       `json:"window"`
	ThicknessMM        map[string]float64 `json:"thicknessMM"`
	InterpolationOrder int                `json:"interpolationOrder"`
}

type PlaneImage struct {
	Plane              model.Plane   `json:"plane"`
	Width              int           `json:"width"`
	Height             int           `json:"height"`
	Pixels             []uint8       `json:"pixels"`
	PixelHash          string        `json:"pixelHash"`
	PositionMM         model.Vec3    `json:"positionMM"`
	NormalAxis         string        `json:"normalAxis"`
	UAxis              string        `json:"uAxis"`
	VAxis              string        `json:"vAxis"`
	U_spacingMM        float64       `json:"uSpacingMM"`
	V_spacingMM        float64       `json:"vSpacingMM"`
	Window             model.Window  `json:"window"`
	ThicknessMM        float64       `json:"thicknessMM"`
	InterpolationOrder int           `json:"interpolationOrder"`
	LayerIndices       []int         `json:"layerIndices"`
	LayerWeights       []float64     `json:"layerWeights"`
	CrosshairPixel     [2]int        `json:"crosshairPixel"`
	WorldBounds        [2]model.Vec3 `json:"worldBounds"`
}

type Triple struct {
	Images    []PlaneImage       `json:"images"`
	Crosshair model.Vec3         `json:"crosshair"`
	Window    model.Window       `json:"window"`
	Thickness map[string]float64 `json:"thicknessMM"`
	Order     int                `json:"interpolationOrder"`
}

type thicknessChoice struct {
	indices []int
	weights []float64
}

func InterpolationSupported(order int) bool {
	return order == 0 || order == 1
}

func ValidateWindow(w model.Window, declared [2]float64) *model.AppError {
	if math.IsNaN(w.Width) || math.IsInf(w.Width, 0) || w.Width <= 0 {
		return &model.AppError{
			Code:    "E_WIN",
			Message: "window width must be finite and positive",
			Field:   "width",
			Actual:  w.Width,
			Allowed: "> 0",
		}
	}
	if math.IsNaN(w.Level) || math.IsInf(w.Level, 0) || w.Level < declared[0] || w.Level > declared[1] {
		return &model.AppError{
			Code:    "E_WIN",
			Message: "window level is outside the declared calibrated gray range",
			Field:   "level",
			Actual:  w.Level,
			Allowed: [2]float64{declared[0], declared[1]},
		}
	}
	return nil
}

func ValidateRequest(v *volume.Volume, request Request) (map[model.Plane]thicknessChoice, *model.AppError) {
	if v == nil {
		return nil, model.NewError("E_STATE", "no volume is open")
	}
	if !InterpolationSupported(request.InterpolationOrder) {
		return nil, &model.AppError{Code: "E_ARG", Message: "interpolation order must be 0 (nearest) or 1 (linear)", Field: "interpolationOrder", Actual: request.InterpolationOrder, Allowed: []int{0, 1}}
	}
	if err := ValidateWindow(request.Window, v.GrayRange); err != nil {
		return nil, err
	}
	if err := v.ValidateWorldPoint(request.Crosshair); err != nil {
		return nil, err
	}
	choices := make(map[model.Plane]thicknessChoice, 3)
	for _, plane := range model.AllPlanes {
		info, _ := model.PlaneGeometry(plane, v.Shape, v.Spacing)
		thickness := request.ThicknessMM[string(plane)]
		maxThickness := float64(v.Shape[info.Normal]-1) * v.Spacing[info.Normal]
		if math.IsNaN(thickness) || math.IsInf(thickness, 0) || thickness < 0 || thickness > maxThickness+1e-9 {
			return nil, &model.AppError{
				Code:    "E_THICK",
				Message: "layer thickness is outside the available range for this plane",
				Field:   "thicknessMM." + string(plane),
				Actual:  thickness,
				Allowed: map[string]any{"min": 0.0, "max": maxThickness, "layers": v.Shape[info.Normal], "spacing": v.Spacing[info.Normal]},
				Details: map[string]any{"plane": plane},
			}
		}
		choice := equivalentLayers(request.Crosshair[info.Normal], thickness, v.Spacing[info.Normal], v.Shape[info.Normal])
		choices[plane] = choice
	}
	return choices, nil
}

func RenderTriple(ctx context.Context, v *volume.Volume, request Request, executor *pool.Executor) (*Triple, *model.AppError) {
	choices, appErr := ValidateRequest(v, request)
	if appErr != nil {
		return nil, appErr
	}
	triple := &Triple{
		Crosshair: request.Crosshair,
		Window:    request.Window,
		Thickness: map[string]float64{},
		Order:     request.InterpolationOrder,
		Images:    make([]PlaneImage, len(model.AllPlanes)),
	}
	for _, plane := range model.AllPlanes {
		triple.Thickness[string(plane)] = request.ThicknessMM[string(plane)]
	}
	type planeWork struct {
		image  *PlaneImage
		info   model.PlaneInfo
		choice thicknessChoice
	}
	works := make([]planeWork, 0, 3)
	for i, plane := range model.AllPlanes {
		info, _ := model.PlaneGeometry(plane, v.Shape, v.Spacing)
		choice := choices[plane]
		img := PlaneImage{
			Plane:              plane,
			Width:              info.Width,
			Height:             info.Height,
			Pixels:             make([]uint8, info.Width*info.Height),
			PositionMM:         request.Crosshair,
			NormalAxis:         model.AxisName(info.Normal),
			UAxis:              model.AxisName(info.U),
			VAxis:              model.AxisName(info.V),
			U_spacingMM:        info.UMM,
			V_spacingMM:        info.VMM,
			Window:             request.Window,
			ThicknessMM:        request.ThicknessMM[string(plane)],
			InterpolationOrder: request.InterpolationOrder,
			LayerIndices:       append([]int(nil), choice.indices...),
			LayerWeights:       append([]float64(nil), choice.weights...),
			CrosshairPixel: [2]int{
				clampInt(int(math.Round(request.Crosshair[info.U]/info.UMM)), 0, info.Width-1),
				clampInt(int(math.Round(request.Crosshair[info.V]/info.VMM)), 0, info.Height-1),
			},
			WorldBounds: v.WorldBounds(),
		}
		triple.Images[i] = img
		works = append(works, planeWork{image: &triple.Images[i], info: info, choice: choice})
	}

	var tasks []pool.Task
	for i := range works {
		work := works[i]
		if executor == nil {
			renderPixels(ctx, v, request, work.image, work.info, work.choice, 0, work.info.Width*work.info.Height)
			continue
		}
		chunks := pool.SplitPixels(work.info.Width*work.info.Height, executor.MaxPixelsPerTask())
		for _, chunk := range chunks {
			start, end := chunk[0], chunk[1]
			tasks = append(tasks, pool.Task{
				ID:     string(work.info.Plane),
				Pixels: end - start,
				Run: func(ctx context.Context) error {
					renderPixels(ctx, v, request, work.image, work.info, work.choice, start, end)
					return nil
				},
			})
		}
	}
	if executor != nil {
		if err := executor.Run(ctx, tasks); err != nil {
			return nil, model.AsAppError(err)
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, &model.AppError{Code: "E_CANCEL", Message: "resampling canceled", Details: map[string]any{"cause": err.Error()}}
	}
	for i := range triple.Images {
		triple.Images[i].PixelHash = PixelHash(triple.Images[i].Pixels)
	}
	return triple, nil
}

func renderPixels(ctx context.Context, v *volume.Volume, request Request, img *PlaneImage, info model.PlaneInfo, choice thicknessChoice, start, end int) {
	for index := start; index < end; index++ {
		if index&63 == 0 && ctx.Err() != nil {
			return
		}
		row := index / info.Width
		col := index % info.Width
		point := model.Vec3{}
		point[info.Normal] = request.Crosshair[info.Normal]
		point[info.U] = float64(col) * v.Spacing[info.U]
		point[info.V] = float64(row) * v.Spacing[info.V]
		var calibrated float64
		if len(choice.weights) == 0 {
			calibrated = v.Calibrated(interpolateRaw(v, point, request.InterpolationOrder))
		} else {
			for i, layer := range choice.indices {
				sample := point
				sample[info.Normal] = float64(layer) * v.Spacing[info.Normal]
				calibrated += choice.weights[i] * v.Calibrated(interpolateRaw(v, sample, request.InterpolationOrder))
			}
		}
		img.Pixels[index] = mapDisplay(calibrated, request.Window)
	}
}

func interpolateRaw(v *volume.Volume, world model.Vec3, order int) int16 {
	voxel := v.VoxelFromWorld(world)
	if order == 0 {
		index := model.IVec3{
			clampInt(int(math.Round(voxel[0])), 0, v.Shape[0]-1),
			clampInt(int(math.Round(voxel[1])), 0, v.Shape[1]-1),
			clampInt(int(math.Round(voxel[2])), 0, v.Shape[2]-1),
		}
		raw, _ := v.RawAt(index)
		return raw
	}
	x0 := int(math.Floor(voxel[0]))
	y0 := int(math.Floor(voxel[1]))
	z0 := int(math.Floor(voxel[2]))
	fx := voxel[0] - float64(x0)
	fy := voxel[1] - float64(y0)
	fz := voxel[2] - float64(z0)
	var result float64
	for dz := 0; dz <= 1; dz++ {
		wz := 1 - fz
		if dz == 1 {
			wz = fz
		}
		z := clampInt(z0+dz, 0, v.Shape[2]-1)
		for dy := 0; dy <= 1; dy++ {
			wy := 1 - fy
			if dy == 1 {
				wy = fy
			}
			y := clampInt(y0+dy, 0, v.Shape[1]-1)
			for dx := 0; dx <= 1; dx++ {
				wx := 1 - fx
				if dx == 1 {
					wx = fx
				}
				x := clampInt(x0+dx, 0, v.Shape[0]-1)
				raw, _ := v.RawAt(model.IVec3{x, y, z})
				result += wx * wy * wz * float64(raw)
			}
		}
	}
	return int16(math.Round(result))
}

func mapDisplay(calibrated float64, w model.Window) uint8 {
	min := w.Level - w.Width/2
	value := (calibrated - min) / w.Width * 255.0
	if value <= 0 {
		return 0
	}
	if value >= 255 {
		return 255
	}
	return uint8(math.Round(value))
}

func equivalentLayers(position, thickness, spacing float64, layers int) thicknessChoice {
	if thickness <= 0 {
		return thicknessChoice{}
	}
	slabMin := position - thickness/2
	slabMax := position + thickness/2
	var indices []int
	var weights []float64
	sum := 0.0
	for layer := 0; layer < layers; layer++ {
		center := float64(layer) * spacing
		layerMin := center - spacing/2
		layerMax := center + spacing/2
		overlap := math.Min(slabMax, layerMax) - math.Max(slabMin, layerMin)
		if overlap <= 1e-12 {
			continue
		}
		indices = append(indices, layer)
		weights = append(weights, overlap)
		sum += overlap
	}
	if sum <= 0 {
		layer := clampInt(int(math.Round(position/spacing)), 0, layers-1)
		return thicknessChoice{indices: []int{layer}, weights: []float64{1}}
	}
	for i := range weights {
		weights[i] /= sum
	}
	return thicknessChoice{indices: indices, weights: weights}
}

func EquivalentLayers(position, thickness, spacing float64, layers int) ([]int, []float64) {
	choice := equivalentLayers(position, thickness, spacing, layers)
	return append([]int(nil), choice.indices...), append([]float64(nil), choice.weights...)
}

func PixelHash(pixels []uint8) string {
	h := sha256.New()
	_ = binary.Write(h, binary.LittleEndian, uint64(len(pixels)))
	_, _ = h.Write(pixels)
	return hex.EncodeToString(h.Sum(nil))
}

func ThicknessRange(v *volume.Volume, plane model.Plane) (map[string]any, *model.AppError) {
	info, ok := model.PlaneGeometry(plane, v.Shape, v.Spacing)
	if !ok {
		return nil, &model.AppError{Code: "E_ARG", Message: "unknown plane", Field: "plane", Actual: plane}
	}
	max := float64(v.Shape[info.Normal]-1) * v.Spacing[info.Normal]
	return map[string]any{
		"min":     0.0,
		"max":     max,
		"layers":  v.Shape[info.Normal],
		"spacing": v.Spacing[info.Normal],
		"plane":   plane,
	}, nil
}

func SortedPlaneImages(images []PlaneImage) []PlaneImage {
	out := append([]PlaneImage(nil), images...)
	sort.Slice(out, func(i, j int) bool { return out[i].Plane < out[j].Plane })
	return out
}

func clampInt(v, min, max int) int {
	if v < min {
		return min
	}
	if v > max {
		return max
	}
	return v
}
