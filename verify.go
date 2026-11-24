package spectralrender

import (
	"fmt"
	"math"
	"sort"
)

type BoundModel struct {
	SamplingConstant     float64 `json:"sampling_constant"`
	QuantizationConstant float64 `json:"quantization_constant"`
	PeakScale            float64 `json:"peak_scale"`
	ChromaticityConstant float64 `json:"chromaticity_constant"`
}

func DefaultBoundModel() BoundModel {
	return BoundModel{
		SamplingConstant:     2.0,
		QuantizationConstant: 8.0,
		PeakScale:            2.0,
		ChromaticityConstant: 4.0,
	}
}

type ErrorBound struct {
	SampleCount      int     `json:"sample_count"`
	Integration      string  `json:"integration"`
	Bounces          int     `json:"bounces"`
	VertexUnit       int64   `json:"vertex_unit"`
	CoefficientUnit  int64   `json:"coefficient_unit"`
	SamplingTerm     float64 `json:"sampling_term"`
	QuantizationTerm float64 `json:"quantization_term"`
	LinearRGBAbs     float64 `json:"linear_rgb_abs"`
	PeakRadianceAbs  float64 `json:"peak_radiance_abs"`
	ChromaticityXY   float64 `json:"chromaticity_xy"`
}

func boundOrder(method IntegrationMethod) int {
	switch method {
	case IntegrationRectangle:
		return 1
	case IntegrationTrapezoid:
		return 2
	case IntegrationSimpson:
		return 4
	default:
		return 1
	}
}

// ComputeErrorBound derives a conservative per-pixel bound from the observed
// sampling grid, quadrature order, fixed-point units and bounce count.
func ComputeErrorBound(scene *Scene, params RenderParams, quant QuantizationReport, model BoundModel) ErrorBound {
	order := boundOrder(params.Integration)
	h := 1.0 / float64(params.SampleCount-1)
	sampling := model.SamplingConstant * math.Pow(h, float64(order))
	basisCount := 1
	for _, m := range scene.Materials {
		if len(m.Basis) > basisCount {
			basisCount = len(m.Basis)
		}
	}
	quantBase := 1.0/float64(quant.Effective.VertexUnit) +
		float64(basisCount)/float64(quant.Effective.CoefficientUnit)
	bounceFactor := 1.0
	term := 1.0
	for i := 0; i < params.Bounces; i++ {
		term *= params.IndirectWeight
		bounceFactor += term
	}
	quantTerm := model.QuantizationConstant * quantBase * bounceFactor
	linear := sampling + quantTerm
	return ErrorBound{
		SampleCount:      params.SampleCount,
		Integration:      string(params.Integration),
		Bounces:          params.Bounces,
		VertexUnit:       quant.Effective.VertexUnit,
		CoefficientUnit:  quant.Effective.CoefficientUnit,
		SamplingTerm:     sampling,
		QuantizationTerm: quantTerm,
		LinearRGBAbs:     linear,
		PeakRadianceAbs:  linear * model.PeakScale,
		ChromaticityXY:   model.ChromaticityConstant*linear + 1e-4,
	}
}

type TruthObservation struct {
	X                 int     `json:"x"`
	Y                 int     `json:"y"`
	LinearRGBError    float64 `json:"linear_rgb_error"`
	PeakError         float64 `json:"peak_radiance_error"`
	ChromaticityError float64 `json:"chromaticity_error"`
}

type BoundCheck struct {
	Model                   BoundModel         `json:"model"`
	Bound                   ErrorBound         `json:"bound"`
	Width                   int                `json:"width"`
	Height                  int                `json:"height"`
	PerPixelLinearRGBAbs    []float64          `json:"per_pixel_linear_rgb_abs"`
	PerPixelPeakRadianceAbs []float64          `json:"per_pixel_peak_radiance_abs"`
	PerPixelChromaticityXY  []float64          `json:"per_pixel_chromaticity_xy"`
	TruthObservedP95        float64            `json:"truth_observed_p95_linear_rgb"`
	TruthObservedMax        float64            `json:"truth_observed_max_linear_rgb"`
	PeakObservedP95         float64            `json:"truth_observed_p95_peak_radiance"`
	ChromaticityObservedP95 float64            `json:"truth_observed_p95_chromaticity"`
	Passed                  bool               `json:"passed"`
	Observations            []TruthObservation `json:"observations,omitempty"`
}

type VerifyMismatch struct {
	Tile        int     `json:"tile"`
	X           int     `json:"x"`
	Y           int     `json:"y"`
	Field       string  `json:"field"`
	Expected    float64 `json:"expected"`
	Actual      float64 `json:"actual"`
	Description string  `json:"description"`
}

type VerifyReport struct {
	OK               bool            `json:"ok"`
	SnapshotID       string          `json:"snapshot_id"`
	SceneHash        string          `json:"scene_hash"`
	ParamVersion     uint64          `json:"param_version"`
	SamplingVersion  string          `json:"sampling_version"`
	MaterialCacheKey string          `json:"material_cache_key"`
	TilesChecked     int             `json:"tiles_checked"`
	FirstMismatch    *VerifyMismatch `json:"first_mismatch,omitempty"`
	BoundCheck       *BoundCheck     `json:"bound_check,omitempty"`
}

func p95(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	s := append([]float64(nil), values...)
	sort.Float64s(s)
	idx := int(math.Ceil(0.95*float64(len(s)))) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(s) {
		idx = len(s) - 1
	}
	return s[idx]
}

func maxFloat(values []float64) float64 {
	m := 0.0
	for _, v := range values {
		if v > m {
			m = v
		}
	}
	return m
}

func firstFloatDiff(a, b []float64) (int, bool) {
	if len(a) != len(b) {
		return 0, true
	}
	for i := range a {
		if math.Float64bits(a[i]) != math.Float64bits(b[i]) {
			return i, true
		}
	}
	return 0, false
}

func makeMismatch(tile Tile, field string, idx int, expected, actual float64) *VerifyMismatch {
	x, y := tile.X, tile.Y
	switch field {
	case "linear_rgb", "xyz_accum":
		pixel := idx / 3
		x = tile.X + pixel%tile.Width
		y = tile.Y + pixel/tile.Width
		field = fmt.Sprintf("%s[%d]", field, idx%3)
	case "peak_radiance":
		x = tile.X + idx%tile.Width
		y = tile.Y + idx/tile.Width
	default:
		x, y = tile.X, tile.Y
	}
	return &VerifyMismatch{
		Tile: tile.Index, X: x, Y: y, Field: field,
		Expected: expected, Actual: actual,
		Description: fmt.Sprintf("%s differs at pixel (%d,%d)", field, x, y),
	}
}

func compareTile(tile Tile, stored, recomputed *TileResult) *VerifyMismatch {
	if stored.X != tile.X || stored.Y != tile.Y || stored.Width != tile.Width || stored.Height != tile.Height {
		return &VerifyMismatch{Tile: tile.Index, X: tile.X, Y: tile.Y, Field: "tile_geometry",
			Description: "stored tile geometry does not match the tile grid"}
	}
	if i, diff := firstFloatDiff(stored.LinearRGB, recomputed.LinearRGB); diff {
		return makeMismatch(tile, "linear_rgb", i, recomputed.LinearRGB[i], stored.LinearRGB[i])
	}
	if i, diff := firstFloatDiff(stored.XYZAccum, recomputed.XYZAccum); diff {
		return makeMismatch(tile, "xyz_accum", i, recomputed.XYZAccum[i], stored.XYZAccum[i])
	}
	if i, diff := firstFloatDiff(stored.PeakRadiance, recomputed.PeakRadiance); diff {
		return makeMismatch(tile, "peak_radiance", i, recomputed.PeakRadiance[i], stored.PeakRadiance[i])
	}
	if stored.Digest != recomputed.Digest {
		return &VerifyMismatch{Tile: tile.Index, X: tile.X, Y: tile.Y, Field: "digest",
			Description: "stored tile digest does not match the recomputed spectral accumulation"}
	}
	return nil
}

func compareFinalImage(stored *Frame, recomputed *Frame, tileSize int, tilesX int) *VerifyMismatch {
	if stored == nil {
		return &VerifyMismatch{Tile: -1, Field: "image", Description: "checkpoint is missing the committed final image"}
	}
	if stored.ParamVersion != recomputed.ParamVersion ||
		stored.SamplingVersion != recomputed.SamplingVersion ||
		stored.MaterialCacheKey != recomputed.MaterialCacheKey {
		return &VerifyMismatch{Tile: -1, Field: "image.version",
			Description: fmt.Sprintf("final image version metadata differs: image=%d/%s/%s recomputed=%d/%s/%s",
				stored.ParamVersion, stored.SamplingVersion, stored.MaterialCacheKey,
				recomputed.ParamVersion, recomputed.SamplingVersion, recomputed.MaterialCacheKey)}
	}
	if i, diff := firstFloatDiff(stored.LinearRGB, recomputed.LinearRGB); diff {
		pixel := i / 3
		x, y := pixel%recomputed.Width, pixel/recomputed.Width
		tile := (y/tileSize)*tilesX + x/tileSize
		return &VerifyMismatch{Tile: tile, X: x, Y: y, Field: fmt.Sprintf("image.linear_rgb[%d]", i%3),
			Expected: recomputed.LinearRGB[i], Actual: stored.LinearRGB[i],
			Description: "final image linear RGB differs from tile recomputation"}
	}
	if len(stored.EncodedRGB) != len(recomputed.EncodedRGB) {
		return &VerifyMismatch{Tile: -1, Field: "image.encoded_rgb", Description: "encoded image length mismatch"}
	}
	for i := range stored.EncodedRGB {
		if stored.EncodedRGB[i] != recomputed.EncodedRGB[i] {
			pixel := i / 3
			x, y := pixel%recomputed.Width, pixel/recomputed.Width
			tile := (y/tileSize)*tilesX + x/tileSize
			return &VerifyMismatch{Tile: tile, X: x, Y: y, Field: fmt.Sprintf("image.encoded_rgb[%d]", i%3),
				Expected: float64(recomputed.EncodedRGB[i]), Actual: float64(stored.EncodedRGB[i]),
				Description: "final encoded image differs from tone-mapped tile recomputation"}
		}
	}
	if stored.EncodedSHA256 != recomputed.EncodedSHA256 {
		return &VerifyMismatch{Tile: -1, Field: "image.encoded_sha256",
			Description: "encoded image digest differs"}
	}
	return nil
}

func VerifyCheckpoint(scene *Scene, sceneHash string, truth *TruthManifest, root string, model BoundModel) (*VerifyReport, error) {
	data, err := loadSnapshotForVerify(root)
	if err != nil {
		return nil, err
	}
	meta := data.Manifest.Meta
	report := &VerifyReport{
		SnapshotID:       data.Manifest.SnapshotID,
		SceneHash:        meta.SceneHash,
		ParamVersion:     meta.ParamVersion,
		SamplingVersion:  meta.SamplingVersion,
		MaterialCacheKey: meta.MaterialCacheKey,
	}
	if sceneHash == "" {
		sceneHash = HashScene(scene)
	}
	if meta.SceneHash != sceneHash {
		return report, &Error{Code: ErrState, Field: "snapshot.meta.scene_hash",
			Message: "snapshot scene hash does not match the supplied scene"}
	}
	if truth != nil {
		if terr := ValidateTruthForScene(truth, scene, sceneHash); terr != nil {
			return report, terr
		}
	}
	if data.Image == nil {
		return report, &Error{Code: ErrState, Field: "snapshot.image",
			Message: "snapshot is incomplete; it has no committed final image (expected after cancellation or forced termination)"}
	}
	params := meta.Params
	if err := params.Validate(); err != nil {
		return report, err
	}
	engine, engineErr := NewSpectralEngine(scene, params)
	if engineErr != nil {
		return report, engineErr
	}
	tiles, tileErr := BuildTiles(scene.Width, scene.Height, params.TileSize)
	if tileErr != nil {
		return report, tileErr
	}
	recomputed := make([]*TileResult, len(tiles))
	for _, tile := range tiles {
		stored, ok := data.Tiles[tile.Index]
		if !ok {
			return report, &Error{Code: ErrState, Tile: tile.Index, Message: "snapshot is missing a committed tile"}
		}
		got, rerr := renderTile(scene, engine, params, tile)
		if rerr != nil {
			return report, rerr
		}
		recomputed[tile.Index] = got
		report.TilesChecked++
		if report.FirstMismatch == nil {
			report.FirstMismatch = compareTile(tile, stored, got)
		}
	}
	if report.FirstMismatch != nil {
		return report, &Error{Code: ErrState, Tile: report.FirstMismatch.Tile,
			Field: report.FirstMismatch.Field, Pixel: &Pixel{report.FirstMismatch.X, report.FirstMismatch.Y},
			Message: report.FirstMismatch.Description}
	}
	linear, lerr := assembleLinearFrame(tiles, recomputed, scene.Width, scene.Height, params)
	if lerr != nil {
		return report, lerr
	}
	final, ferr := EncodeDisplay(linear, params)
	if ferr != nil {
		return report, ferr
	}
	report.FirstMismatch = compareFinalImage(data.Image, final, params.TileSize, meta.TilesX)
	if report.FirstMismatch != nil {
		return report, &Error{Code: ErrState, Tile: report.FirstMismatch.Tile,
			Field: report.FirstMismatch.Field, Pixel: &Pixel{report.FirstMismatch.X, report.FirstMismatch.Y},
			Message: report.FirstMismatch.Description}
	}

	bound := ComputeErrorBound(scene, params, engine.quantized.Report, model)
	bc := &BoundCheck{
		Model: model, Bound: bound, Width: scene.Width, Height: scene.Height,
		PerPixelLinearRGBAbs:    make([]float64, scene.Width*scene.Height),
		PerPixelPeakRadianceAbs: make([]float64, scene.Width*scene.Height),
		PerPixelChromaticityXY:  make([]float64, scene.Width*scene.Height),
	}
	for i := range bc.PerPixelLinearRGBAbs {
		bc.PerPixelLinearRGBAbs[i] = bound.LinearRGBAbs
		bc.PerPixelPeakRadianceAbs[i] = bound.PeakRadianceAbs
		bc.PerPixelChromaticityXY[i] = bound.ChromaticityXY
	}
	if truth != nil {
		var rgbErrors, peakErrors, chromaErrors []float64
		for _, px := range truth.Pixels {
			idx := px.Y*final.Width + px.X
			got := RGB{final.LinearRGB[idx*3], final.LinearRGB[idx*3+1], final.LinearRGB[idx*3+2]}
			obs := TruthObservation{X: px.X, Y: px.Y}
			obs.LinearRGBError = maxAbsDiff(got, px.LinearRGB)
			obs.PeakError = math.Abs(final.PeakRadiance[idx] - px.PeakRadiance)
			xyz := RGB{final.XYZ[idx*3], final.XYZ[idx*3+1], final.XYZ[idx*3+2]}
			xy := chromaticity([3]float64{xyz[0], xyz[1], xyz[2]})
			obs.ChromaticityError = math.Hypot(xy[0]-px.ChromaticityXY[0], xy[1]-px.ChromaticityXY[1])
			rgbErrors = append(rgbErrors, obs.LinearRGBError)
			peakErrors = append(peakErrors, obs.PeakError)
			chromaErrors = append(chromaErrors, obs.ChromaticityError)
			bc.Observations = append(bc.Observations, obs)
		}
		bc.TruthObservedP95 = p95(rgbErrors)
		bc.TruthObservedMax = maxFloat(rgbErrors)
		bc.PeakObservedP95 = p95(peakErrors)
		bc.ChromaticityObservedP95 = p95(chromaErrors)
		bc.Passed = bc.TruthObservedP95 <= bound.LinearRGBAbs &&
			bc.PeakObservedP95 <= bound.PeakRadianceAbs &&
			bc.ChromaticityObservedP95 <= bound.ChromaticityXY
		if !bc.Passed {
			report.BoundCheck = bc
			return report, &Error{Code: ErrBound, Field: "error_bound",
				Message: fmt.Sprintf("derived bound is below observed truth quantile: rgb(p95)=%.9g bound=%.9g, peak(p95)=%.9g bound=%.9g, chroma(p95)=%.9g bound=%.9g",
					bc.TruthObservedP95, bound.LinearRGBAbs, bc.PeakObservedP95, bound.PeakRadianceAbs,
					bc.ChromaticityObservedP95, bound.ChromaticityXY)}
		}
	} else {
		bc.Passed = true
	}
	report.BoundCheck = bc
	report.OK = true
	return report, nil
}
