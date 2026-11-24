package geometry

import (
	"fmt"
	"math/big"
	"sort"

	"occlusionlab/internal/fixed"
	"occlusionlab/internal/model"
)

const (
	StatusVisible   = "visible"
	StatusOccluded  = "occluded"
	StatusUncertain = "uncertain"

	dotProductTerms     = 3
	projectionLadderOps = 4
	planeLadderOps      = 4
	rasterCellAllowance = 1
)

type ScreenPoint struct {
	XQ    int64 `json:"x_q"`
	YQ    int64 `json:"y_q"`
	Depth int64 `json:"depth_q"`
}

type ScreenRect struct {
	MinXQ int64 `json:"min_x_q"`
	MinYQ int64 `json:"min_y_q"`
	MaxXQ int64 `json:"max_x_q"`
	MaxYQ int64 `json:"max_y_q"`
}

type ErrorBreakdown struct {
	ProjectionResidualQ     int64 `json:"projection_residual_q"`
	ProjectionErrorQ        int64 `json:"projection_error_q"`
	FrustumPlaneErrorWorldQ int64 `json:"frustum_plane_error_world_q"`
	OcclusionCoverageErrorQ int64 `json:"occlusion_coverage_error_q"`
	DepthCompareErrorWorldQ int64 `json:"depth_compare_error_world_q"`
	VisibilityUpperBoundQ   int64 `json:"visibility_upper_bound_q"`
	CellSizeQ               int64 `json:"cell_size_q"`
	WorldStepQ              int64 `json:"world_step_q"`
	BasisStepQ              int64 `json:"basis_step_q"`
	RasterResolution        int   `json:"raster_resolution"`
	CameraDistanceQ         int64 `json:"camera_distance_q"`
	BBoxSpanQ               int64 `json:"bbox_span_q"`
	DotProductTerms         int   `json:"dot_product_terms"`
	ProjectionLadderOps     int   `json:"projection_ladder_ops"`
	PlaneLadderOps          int   `json:"plane_ladder_ops"`
	RasterCellAllowance     int   `json:"raster_cell_allowance"`
}

type FrustumResult struct {
	Status         string   `json:"status"`
	AnchorStatus   string   `json:"anchor_status"`
	PlaneMargins   [6]int64 `json:"plane_margins_world_q"`
	BBoxMinMargins [6]int64 `json:"bbox_min_margins_world_q"`
	PlaneErrors    [6]int64 `json:"plane_errors_world_q"`
	AnchorInside   bool     `json:"anchor_inside"`
	BoundaryWidthQ int64    `json:"boundary_width_q"`
}

type ChunkVerdict struct {
	Chunk               int            `json:"chunk"`
	Label               string         `json:"label"`
	Status              string         `json:"status"`
	Reason              string         `json:"reason"`
	ReferenceOccluder   string         `json:"reference_occluder,omitempty"`
	Anchor              ScreenPoint    `json:"anchor"`
	ScreenBounds        ScreenRect     `json:"screen_bounds"`
	Frustum             FrustumResult  `json:"frustum"`
	Error               ErrorBreakdown `json:"error"`
	Trace               fixed.Trace    `json:"trace"`
	VisibilityLowerQ    int64          `json:"visibility_lower_q16"`
	VisibilityUpperQ    int64          `json:"visibility_upper_q16"`
	DepthLowerQ         int64          `json:"depth_lower_q"`
	DepthUpperQ         int64          `json:"depth_upper_q"`
	ProjectedCorners    int            `json:"projected_corners"`
	CandidateCells      int            `json:"candidate_cells"`
	CoveredCandidates   int            `json:"covered_candidates"`
	DepthUncertainCells int            `json:"depth_uncertain_cells"`
	CandidateOccluders  []string       `json:"candidate_occluders,omitempty"`
}

type SampleResult struct {
	Window              int            `json:"window"`
	Time                int64          `json:"time"`
	Visible             []int          `json:"visible"`
	Occluded            []int          `json:"occluded"`
	Uncertain           []int          `json:"uncertain"`
	Chunks              []ChunkVerdict `json:"chunks"`
	MaxProjectionErrorQ int64          `json:"max_projection_error_q"`
	MaxCumulativeErrorQ int64          `json:"max_cumulative_error_q"`
	ProjectedCorners    int            `json:"projected_corners"`
	RasterCellsWork     int            `json:"raster_cells_work"`
}

type projectedPoint struct {
	ScreenXQ int64
	ScreenYQ int64
	CameraX  int64
	CameraY  int64
	CameraZ  int64
	Trace    fixed.Trace
}

type gridCell struct {
	Covered  bool
	MinDepth int64
	MaxDepth int64
	Occluder string
}

type rasterGrid struct {
	Resolution   int
	WidthQ       int64
	HeightQ      int64
	CellPxQ      int64
	Cells        []gridCell
	CellsTouched int
}

type Evaluator struct {
	Project   *model.Project
	Params    model.Params
	Camera    model.CameraSample
	Occluders map[string]model.Occluder
}

func NewEvaluator(project *model.Project, params model.Params, window int) (*Evaluator, *model.AppError) {
	if project == nil || window < 0 || window >= len(project.World.Cameras) {
		return nil, model.SchemaError(0, "camera", "相机窗口越界")
	}
	occ := make(map[string]model.Occluder, len(project.World.Occluders))
	for _, item := range project.World.Occluders {
		occ[item.ID] = item
	}
	return &Evaluator{
		Project:   project,
		Params:    params,
		Camera:    project.World.Cameras[window],
		Occluders: occ,
	}, nil
}

func EvaluateSample(project *model.Project, params model.Params, window int) (*SampleResult, *model.AppError) {
	eval, appErr := NewEvaluator(project, params, window)
	if appErr != nil {
		return nil, appErr
	}
	result := &SampleResult{Window: window, Time: eval.Camera.Time}
	for i := range project.World.Chunks {
		verdict := eval.EvaluateChunk(i)
		result.Chunks = append(result.Chunks, verdict)
		result.RasterCellsWork += verdict.Error.RasterResolution * verdict.Error.RasterResolution
		result.ProjectedCorners += verdict.ProjectedCorners
		switch verdict.Status {
		case StatusVisible:
			result.Visible = append(result.Visible, i)
		case StatusOccluded:
			result.Occluded = append(result.Occluded, i)
		default:
			result.Uncertain = append(result.Uncertain, i)
		}
		if verdict.Error.ProjectionErrorQ > result.MaxProjectionErrorQ {
			result.MaxProjectionErrorQ = verdict.Error.ProjectionErrorQ
		}
		if verdict.Error.VisibilityUpperBoundQ > result.MaxCumulativeErrorQ {
			result.MaxCumulativeErrorQ = verdict.Error.VisibilityUpperBoundQ
		}
	}
	return result, nil
}

func (e *Evaluator) EvaluateChunk(chunkIndex int) ChunkVerdict {
	chunk := e.Project.World.Chunks[chunkIndex]
	q := e.Project.Meta.Precision
	verdict := ChunkVerdict{Chunk: chunkIndex, Label: chunk.Label}

	anchor, anchorOK := e.projectPoint(chunk.Anchor)
	verdict.Anchor = ScreenPoint{XQ: anchor.ScreenXQ, YQ: anchor.ScreenYQ, Depth: anchor.CameraZ}
	verdict.Trace = anchor.Trace
	screenBounds, projectedCorners, cornerResidual := e.projectBounds(chunk.Bounds)
	verdict.ScreenBounds = screenBounds
	verdict.ProjectedCorners = projectedCorners

	span := chunk.Bounds.Size().AbsSum()
	projErrX := e.projectionErrorQ(chunk.Anchor, span, e.Camera.FocalX, e.Camera.TanHalfX, anchor)
	projErrY := e.projectionErrorQ(chunk.Anchor, span, e.Camera.FocalY, e.Camera.TanHalfY, anchor)
	projErr := fixed.Max64(projErrX, projErrY)
	planeErr, margins := e.frustumPlaneError(chunk.Anchor, span, anchor)
	cellPxQ := ceilDivInt64(int64(e.Camera.ScreenWidth)<<q.ScreenFracBits, int64(e.Params.RasterResolution))
	depthErr := e.depthErrorWorldQ(chunk.Anchor, span, anchor)

	verdict.Error = ErrorBreakdown{
		ProjectionResidualQ:     anchor.Trace.RoundingResidualScreenQ + cornerResidual,
		ProjectionErrorQ:        projErr,
		FrustumPlaneErrorWorldQ: planeErr,
		OcclusionCoverageErrorQ: projErr + cellPxQ,
		DepthCompareErrorWorldQ: depthErr,
		CellSizeQ:               cellPxQ,
		WorldStepQ:              q.WorldStep(),
		BasisStepQ:              q.BasisStep(),
		RasterResolution:        e.Params.RasterResolution,
		CameraDistanceQ:         anchor.CameraZ,
		BBoxSpanQ:               span,
		DotProductTerms:         dotProductTerms,
		ProjectionLadderOps:     projectionLadderOps,
		PlaneLadderOps:          planeLadderOps,
		RasterCellAllowance:     rasterCellAllowance,
	}
	verdict.Frustum = e.testFrustum(chunk.Bounds, chunk.Anchor, anchor, margins, planeErr, projErr)

	if !anchorOK || anchor.CameraZ <= e.Camera.Near {
		verdict.Status = StatusOccluded
		verdict.Reason = "frustum_near"
		verdict.ReferenceOccluder = "frustum"
		verdict.VisibilityLowerQ = 0
		verdict.VisibilityUpperQ = 0
	} else if verdict.Frustum.AnchorStatus == "outside" {
		verdict.Status = StatusOccluded
		verdict.Reason = "frustum_culled"
		verdict.ReferenceOccluder = "frustum"
		verdict.VisibilityLowerQ = 0
		verdict.VisibilityUpperQ = 0
	} else if verdict.Frustum.AnchorStatus == "boundary" {
		verdict.Status = StatusUncertain
		verdict.Reason = "frustum_error_band"
		verdict.ReferenceOccluder = "frustum"
		verdict.VisibilityLowerQ = 0
		verdict.VisibilityUpperQ = 65536
	} else {
		e.testOcclusion(&verdict, chunk, anchor, projErr, cellPxQ, depthErr)
	}
	verdict.DepthLowerQ = anchor.CameraZ - depthErr
	verdict.DepthUpperQ = anchor.CameraZ + depthErr
	verdict.Error.VisibilityUpperBoundQ = e.cumulativeBoundQ(verdict, projErr, cellPxQ, planeErr, depthErr, anchor)
	return verdict
}

func (e *Evaluator) projectPoint(p model.Vec3) (projectedPoint, bool) {
	q := e.Project.Meta.Precision
	rel := p.Sub(e.Camera.Position)
	cx, remX, errX := fixed.DotShiftRound(rel.X, rel.Y, rel.Z,
		e.Camera.Right.X, e.Camera.Right.Y, e.Camera.Right.Z, q.BasisFracBits)
	cy, remY, errY := fixed.DotShiftRound(rel.X, rel.Y, rel.Z,
		e.Camera.Up.X, e.Camera.Up.Y, e.Camera.Up.Z, q.BasisFracBits)
	cz, remZ, errZ := fixed.DotShiftRound(rel.X, rel.Y, rel.Z,
		e.Camera.Forward.X, e.Camera.Forward.Y, e.Camera.Forward.Z, q.BasisFracBits)
	out := projectedPoint{CameraX: cx, CameraY: cy, CameraZ: cz}
	addTrace := func(name string, rem int64, err error) {
		detail := ""
		if err != nil {
			detail = err.Error()
		}
		out.Trace.Add(fixed.Step{
			Name: name, Remainder: fmt.Sprintf("%d", rem), Divisor: "1",
			ScaleBits: q.BasisFracBits, ResidualScreenQ: 0, Numerator: detail,
		})
	}
	addTrace("camera_x_dot", remX, errX)
	addTrace("camera_y_dot", remY, errY)
	addTrace("camera_z_dot", remZ, errZ)
	if !anchorValid(out) || out.CameraZ <= 0 {
		return out, false
	}

	sx, remSX, errSX := fixed.MulDivRound(e.Camera.FocalX, out.CameraX, out.CameraZ)
	sy, remSY, errSY := fixed.MulDivRound(e.Camera.FocalY, out.CameraY, out.CameraZ)
	if errSX != nil || errSY != nil {
		return out, false
	}
	out.ScreenXQ = e.Camera.CenterX + sx
	out.ScreenYQ = e.Camera.CenterY - sy
	out.Trace.Add(fixed.Step{
		Name: "screen_x_div", Numerator: fmt.Sprintf("%d", e.Camera.FocalX*out.CameraX),
		Denominator: fmt.Sprintf("%d", out.CameraZ), Quotient: sx,
		Remainder: fmt.Sprintf("%d", remSX), Divisor: fmt.Sprintf("%d", out.CameraZ),
		ScaleBits:       q.ScreenFracBits,
		ResidualScreenQ: ceilDivInt64(fixed.Abs64(remSX), fixed.Abs64(out.CameraZ)),
	})
	out.Trace.Add(fixed.Step{
		Name: "screen_y_div", Numerator: fmt.Sprintf("%d", e.Camera.FocalY*out.CameraY),
		Denominator: fmt.Sprintf("%d", out.CameraZ), Quotient: sy,
		Remainder: fmt.Sprintf("%d", remSY), Divisor: fmt.Sprintf("%d", out.CameraZ),
		ScaleBits:       q.ScreenFracBits,
		ResidualScreenQ: ceilDivInt64(fixed.Abs64(remSY), fixed.Abs64(out.CameraZ)),
	})
	return out, true
}

func anchorValid(p projectedPoint) bool {
	return p.CameraX != 0 || p.CameraY != 0 || p.CameraZ != 0
}

func (e *Evaluator) projectBounds(box model.AABB) (ScreenRect, int, int64) {
	out := ScreenRect{MinXQ: int64(^uint64(0) >> 1), MinYQ: int64(^uint64(0) >> 1)}
	out.MaxXQ = -out.MinXQ
	out.MaxYQ = -out.MinYQ
	count := 0
	var residual int64
	for _, corner := range box.Corners() {
		p, ok := e.projectPoint(corner)
		if !ok {
			continue
		}
		count++
		residual += p.Trace.RoundingResidualScreenQ
		if p.ScreenXQ < out.MinXQ {
			out.MinXQ = p.ScreenXQ
		}
		if p.ScreenXQ > out.MaxXQ {
			out.MaxXQ = p.ScreenXQ
		}
		if p.ScreenYQ < out.MinYQ {
			out.MinYQ = p.ScreenYQ
		}
		if p.ScreenYQ > out.MaxYQ {
			out.MaxYQ = p.ScreenYQ
		}
	}
	return out, count, residual
}

func (e *Evaluator) frustumPlaneError(p model.Vec3, span int64, proj projectedPoint) (int64, [6]int64) {
	q := e.Project.Meta.Precision
	tanX := ratOf(e.Camera.TanHalfX, int(q.ParamFracBits))
	tanY := ratOf(e.Camera.TanHalfY, int(q.ParamFracBits))
	margins := e.planeMargins(proj)
	dotErr := e.projectionErrorWorldQ(p, span, proj)
	cz := ratOf(proj.CameraZ, int(q.WorldFracBits))
	tanStep := new(big.Rat).SetFrac64(1, int64(1)<<q.ParamFracBits)
	extra := new(big.Rat).Mul(tanStep, cz)
	base := new(big.Rat).Set(dotErr)
	maxPlane := base
	edge := new(big.Rat).Add(base, new(big.Rat).Mul(tanX, dotErr))
	edge.Add(edge, extra)
	for i := 2; i < 4; i++ {
		if edge.Cmp(maxPlane) > 0 {
			maxPlane.Set(edge)
		}
	}
	edgeY := new(big.Rat).Add(base, new(big.Rat).Mul(tanY, dotErr))
	edgeY.Add(edgeY, extra)
	for i := 4; i < 6; i++ {
		if edgeY.Cmp(maxPlane) > 0 {
			maxPlane.Set(edgeY)
		}
	}
	// Each plane evaluation also has three fixed-point additions/multiplies.
	round := new(big.Rat).SetFrac64(int64(planeLadderOps), int64(1)<<q.WorldFracBits)
	maxPlane.Add(maxPlane, round)
	return ratToWorldQ(maxPlane, q.WorldFracBits), margins
}

func (e *Evaluator) planeMargins(proj projectedPoint) [6]int64 {
	q := e.Project.Meta.Precision
	tanX := ratOf(e.Camera.TanHalfX, int(q.ParamFracBits))
	tanY := ratOf(e.Camera.TanHalfY, int(q.ParamFracBits))
	cx := ratOf(proj.CameraX, int(q.WorldFracBits))
	cy := ratOf(proj.CameraY, int(q.WorldFracBits))
	cz := ratOf(proj.CameraZ, int(q.WorldFracBits))
	vals := [6]*big.Rat{
		new(big.Rat).Sub(cz, ratOf(e.Camera.Near, int(q.WorldFracBits))),
		new(big.Rat).Sub(ratOf(e.Camera.Far, int(q.WorldFracBits)), cz),
		new(big.Rat).Add(cx, new(big.Rat).Mul(tanX, cz)),
		new(big.Rat).Sub(new(big.Rat).Mul(tanX, cz), cx),
		new(big.Rat).Add(cy, new(big.Rat).Mul(tanY, cz)),
		new(big.Rat).Sub(new(big.Rat).Mul(tanY, cz), cy),
	}
	var margins [6]int64
	for i := range margins {
		margins[i] = ratToWorldQ(vals[i], q.WorldFracBits)
	}
	return margins
}

func (e *Evaluator) testFrustum(box model.AABB, p model.Vec3, proj projectedPoint, margins [6]int64,
	planeErr, projErr int64) FrustumResult {
	q := e.Project.Meta.Precision
	screenW := int64(e.Camera.ScreenWidth) << q.ScreenFracBits
	screenH := int64(e.Camera.ScreenHeight) << q.ScreenFracBits
	res := FrustumResult{PlaneMargins: margins, PlaneErrors: [6]int64{planeErr, planeErr, planeErr, planeErr, planeErr, planeErr}}

	const maxInt64 = int64(^uint64(0) >> 1)
	const minInt64 = -maxInt64
	minMargins := [6]int64{maxInt64, maxInt64, maxInt64, maxInt64, maxInt64, maxInt64}
	maxMargins := [6]int64{minInt64, minInt64, minInt64, minInt64, minInt64, minInt64}
	behind := false
	for _, corner := range box.Corners() {
		cornerProj, ok := e.projectPoint(corner)
		if !ok {
			behind = true
			continue
		}
		cornerMargins := e.planeMargins(cornerProj)
		for i, margin := range cornerMargins {
			if margin < minMargins[i] {
				minMargins[i] = margin
			}
			if margin > maxMargins[i] {
				maxMargins[i] = margin
			}
		}
	}
	res.BBoxMinMargins = minMargins
	res.Status = "intersecting"
	if behind {
		res.Status = "boundary"
	} else {
		outside := false
		for _, margin := range maxMargins {
			if margin < -planeErr {
				outside = true
				break
			}
		}
		if outside {
			res.Status = "outside"
		} else {
			inside := true
			for _, margin := range minMargins {
				if margin <= planeErr {
					inside = false
					break
				}
			}
			if inside {
				res.Status = "inside"
			} else {
				for _, margin := range minMargins {
					if margin <= planeErr && margin >= -planeErr {
						res.Status = "boundary"
						break
					}
				}
			}
		}
	}

	anchorStatus := "inside"
	for i, margin := range margins {
		res.PlaneErrors[i] = planeErr
		if margin < -planeErr {
			anchorStatus = "outside"
			res.BoundaryWidthQ = planeErr
			break
		}
		if margin <= planeErr {
			anchorStatus = "boundary"
		}
	}
	if anchorStatus != "outside" {
		if proj.ScreenXQ < -projErr || proj.ScreenXQ > screenW+projErr ||
			proj.ScreenYQ < -projErr || proj.ScreenYQ > screenH+projErr {
			anchorStatus = "outside"
			res.BoundaryWidthQ = projErr
		} else if proj.ScreenXQ < projErr || proj.ScreenXQ > screenW-projErr ||
			proj.ScreenYQ < projErr || proj.ScreenYQ > screenH-projErr {
			anchorStatus = "boundary"
		}
	}
	res.AnchorStatus = anchorStatus
	if anchorStatus == "inside" {
		res.AnchorInside = true
	}
	res.BoundaryWidthQ = projErr
	return res
}

func (e *Evaluator) testOcclusion(verdict *ChunkVerdict, chunk model.Chunk, anchor projectedPoint,
	projErr, cellPxQ, depthErr int64) {
	grid := e.rasterize(chunk)
	verdict.Error.CellSizeQ = grid.CellPxQ
	verdict.Error.RasterResolution = grid.Resolution
	verdict.Error.OcclusionCoverageErrorQ = projErr + grid.CellPxQ
	if grid.CellsTouched == 0 {
		verdict.Status = StatusVisible
		verdict.Reason = "no_occluder_coverage"
		verdict.VisibilityLowerQ = 65536
		verdict.VisibilityUpperQ = 65536
		return
	}

	q := e.Project.Meta.Precision
	radius := projErr + grid.CellPxQ
	x0 := anchor.ScreenXQ - radius
	x1 := anchor.ScreenXQ + radius
	y0 := anchor.ScreenYQ - radius
	y1 := anchor.ScreenYQ + radius
	screenW := int64(e.Camera.ScreenWidth) << q.ScreenFracBits
	screenH := int64(e.Camera.ScreenHeight) << q.ScreenFracBits
	outsideScreen := x0 < 0 || y0 < 0 || x1 >= screenW || y1 >= screenH
	gx0 := e.gridIndex(x0, grid.WidthQ, grid.Resolution)
	gx1 := e.gridIndex(x1, grid.WidthQ, grid.Resolution)
	gy0 := e.gridIndex(y0, grid.HeightQ, grid.Resolution)
	gy1 := e.gridIndex(y1, grid.HeightQ, grid.Resolution)

	hasVisible := outsideScreen
	hasOccluded := false
	hasDepthUncertain := false
	ref := ""
	candidates := 0
	coveredCandidates := 0
	depthUncertainCells := 0
	seenOcc := map[string]bool{}
	for gy := gy0; gy <= gy1; gy++ {
		for gx := gx0; gx <= gx1; gx++ {
			candidates++
			cell := grid.Cells[gy*grid.Resolution+gx]
			if !cell.Covered {
				hasVisible = true
				continue
			}
			coveredCandidates++
			if cell.Occluder != "" && !seenOcc[cell.Occluder] {
				seenOcc[cell.Occluder] = true
				verdict.CandidateOccluders = append(verdict.CandidateOccluders, cell.Occluder)
			}
			if cell.MaxDepth < anchor.CameraZ-depthErr {
				hasOccluded = true
				if ref == "" {
					ref = cell.Occluder
				}
				continue
			}
			if cell.MinDepth > anchor.CameraZ+depthErr {
				hasVisible = true
				continue
			}
			hasDepthUncertain = true
			depthUncertainCells++
			if ref == "" {
				ref = cell.Occluder
			}
		}
	}
	verdict.CandidateCells = candidates
	verdict.CoveredCandidates = coveredCandidates
	verdict.DepthUncertainCells = depthUncertainCells

	switch {
	case hasOccluded && !hasVisible && !hasDepthUncertain:
		verdict.Status = StatusOccluded
		verdict.Reason = "coverage_all_candidate_cells"
		verdict.ReferenceOccluder = ref
		verdict.VisibilityLowerQ = 0
		verdict.VisibilityUpperQ = 0
	case hasVisible && !hasOccluded && !hasDepthUncertain:
		verdict.Status = StatusVisible
		verdict.Reason = "coverage_candidate_cell_visible"
		verdict.VisibilityLowerQ = 65536
		verdict.VisibilityUpperQ = 65536
	default:
		verdict.Status = StatusUncertain
		verdict.Reason = "coverage_or_depth_error_band"
		verdict.ReferenceOccluder = ref
		verdict.VisibilityLowerQ = 0
		verdict.VisibilityUpperQ = 65536
	}
}

func (e *Evaluator) rasterize(chunk model.Chunk) rasterGrid {
	q := e.Project.Meta.Precision
	res := e.Params.RasterResolution
	widthQ := int64(e.Camera.ScreenWidth) << q.ScreenFracBits
	heightQ := int64(e.Camera.ScreenHeight) << q.ScreenFracBits
	grid := rasterGrid{
		Resolution: res,
		WidthQ:     widthQ,
		HeightQ:    heightQ,
		CellPxQ:    ceilDivInt64(widthQ, int64(res)),
		Cells:      make([]gridCell, res*res),
	}
	for _, ref := range chunk.OccluderRefs {
		occ, ok := e.Occluders[ref]
		if !ok {
			continue
		}
		rect, depthMin, depthMax, ok := e.projectAABB(occ.Bounds)
		if !ok {
			continue
		}
		if rect.MaxXQ < 0 || rect.MaxYQ < 0 || rect.MinXQ >= widthQ || rect.MinYQ >= heightQ {
			continue
		}
		rect.MinXQ = fixed.Max64(rect.MinXQ, 0)
		rect.MinYQ = fixed.Max64(rect.MinYQ, 0)
		rect.MaxXQ = fixed.Min64(rect.MaxXQ, widthQ-1)
		rect.MaxYQ = fixed.Min64(rect.MaxYQ, heightQ-1)
		gx0 := e.gridIndex(rect.MinXQ, widthQ, res)
		gx1 := e.gridIndex(rect.MaxXQ, widthQ, res)
		gy0 := e.gridIndex(rect.MinYQ, heightQ, res)
		gy1 := e.gridIndex(rect.MaxYQ, heightQ, res)
		for gy := gy0; gy <= gy1; gy++ {
			for gx := gx0; gx <= gx1; gx++ {
				idx := gy*res + gx
				cell := &grid.Cells[idx]
				if !cell.Covered {
					grid.CellsTouched++
				}
				if !cell.Covered || depthMin < cell.MinDepth {
					cell.Covered = true
					cell.MinDepth = depthMin
					cell.MaxDepth = depthMax
					cell.Occluder = occ.ID
				}
			}
		}
	}
	return grid
}

func (e *Evaluator) projectAABB(box model.AABB) (ScreenRect, int64, int64, bool) {
	rect := ScreenRect{
		MinXQ: int64(^uint64(0) >> 1), MinYQ: int64(^uint64(0) >> 1),
	}
	rect.MaxXQ = -rect.MinXQ
	rect.MaxYQ = -rect.MinYQ
	depthMin := int64(^uint64(0) >> 1)
	depthMax := int64(-1)
	for _, corner := range box.Corners() {
		p, ok := e.projectPoint(corner)
		if !ok {
			return ScreenRect{}, 0, 0, false
		}
		if p.ScreenXQ < rect.MinXQ {
			rect.MinXQ = p.ScreenXQ
		}
		if p.ScreenXQ > rect.MaxXQ {
			rect.MaxXQ = p.ScreenXQ
		}
		if p.ScreenYQ < rect.MinYQ {
			rect.MinYQ = p.ScreenYQ
		}
		if p.ScreenYQ > rect.MaxYQ {
			rect.MaxYQ = p.ScreenYQ
		}
		if p.CameraZ < depthMin {
			depthMin = p.CameraZ
		}
		if p.CameraZ > depthMax {
			depthMax = p.CameraZ
		}
	}
	return rect, depthMin, depthMax, true
}

func (e *Evaluator) gridIndex(xQ, extentQ int64, resolution int) int {
	if extentQ <= 0 {
		return 0
	}
	if xQ <= 0 {
		return 0
	}
	if xQ >= extentQ {
		return resolution - 1
	}
	idx := int((new(big.Int).Mul(big.NewInt(xQ), big.NewInt(int64(resolution)))).Int64() / extentQ)
	if idx < 0 {
		return 0
	}
	if idx >= resolution {
		return resolution - 1
	}
	return idx
}

func (e *Evaluator) projectionErrorWorldQ(p model.Vec3, span int64, proj projectedPoint) *big.Rat {
	q := e.Project.Meta.Precision
	rel := p.Sub(e.Camera.Position)
	relAbs := new(big.Rat).SetFrac64(absSum(rel)+span/2, int64(1)<<q.WorldFracBits)
	basisAbs := new(big.Rat).SetFrac64(absSum(e.Camera.Forward), int64(1)<<q.BasisFracBits)
	eWorld := new(big.Rat).SetFrac64(1, int64(1)<<(q.WorldFracBits+1))
	eBasis := new(big.Rat).SetFrac64(1, int64(1)<<(q.BasisFracBits+1))
	out := new(big.Rat).Mul(relAbs, eBasis)
	out.Add(out, new(big.Rat).Mul(basisAbs, eWorld))
	out.Add(out, new(big.Rat).Mul(eWorld, eBasis))
	// Three products and two additions: a conservative four-step ladder.
	ladder := new(big.Rat).SetFrac64(int64(projectionLadderOps), int64(1)<<q.WorldFracBits)
	out.Add(out, ladder)
	for _, step := range proj.Trace.Steps {
		if step.ScaleBits == q.BasisFracBits && step.Remainder != "" {
			rem := new(big.Int)
			if _, ok := rem.SetString(step.Remainder, 10); ok {
				r := new(big.Rat).SetInt(rem)
				r.Quo(r, new(big.Rat).SetInt(new(big.Int).Lsh(big.NewInt(1), uint(step.ScaleBits))))
				r.Quo(r, new(big.Rat).SetInt(new(big.Int).Lsh(big.NewInt(1), uint(q.WorldFracBits))))
				r.Abs(r)
				out.Add(out, r)
			}
		}
	}
	return out
}

func (e *Evaluator) projectionErrorQ(p model.Vec3, span, focalQ, tanQ int64, proj projectedPoint) int64 {
	q := e.Project.Meta.Precision
	worldErr := e.projectionErrorWorldQ(p, span, proj)
	cz := new(big.Rat).SetFrac64(proj.CameraZ, int64(1)<<q.WorldFracBits)
	cx := new(big.Rat).SetFrac64(proj.CameraX, int64(1)<<q.WorldFracBits)
	ratio := new(big.Rat).Quo(cx, cz)
	eCz := new(big.Rat).Set(worldErr)
	if eCz.Cmp(cz) >= 0 {
		return int64(1) << 40
	}
	den := new(big.Rat).Sub(cz, eCz)
	eRatio := new(big.Rat).Add(worldErr, new(big.Rat).Mul(ratio, worldErr))
	eRatio.Quo(eRatio, den)
	out := new(big.Rat).Mul(new(big.Rat).SetInt64(focalQ), eRatio)
	tanStep := new(big.Rat).SetFrac64(1, int64(1)<<q.ParamFracBits)
	eFocal := new(big.Rat).Mul(new(big.Rat).SetInt64(focalQ), tanStep)
	eFocal.Quo(eFocal, new(big.Rat).SetInt64(tanQ))
	out.Add(out, new(big.Rat).Mul(ratio, eFocal))
	out.Add(out, new(big.Rat).SetFrac64(q.ScreenStep(), 2))
	out.Add(out, new(big.Rat).SetInt64(proj.Trace.RoundingResidualScreenQ))
	out.Add(out, new(big.Rat).SetInt64(projectionLadderOps))
	out.Abs(out)
	return ratCeilInt64(out)
}

func (e *Evaluator) depthErrorWorldQ(p model.Vec3, span int64, proj projectedPoint) int64 {
	return ratCeilInt64(e.projectionErrorWorldQ(p, span, proj))
}

func (e *Evaluator) cumulativeBoundQ(verdict ChunkVerdict,
	projErr, cellPxQ, planeErr, depthErr int64, anchor projectedPoint) int64 {
	q := e.Project.Meta.Precision
	if anchor.CameraZ <= 0 {
		return verdict.Error.ProjectionErrorQ
	}
	depthPx := new(big.Rat).Mul(
		new(big.Rat).SetInt64(e.Camera.FocalX),
		new(big.Rat).SetFrac64(depthErr, anchor.CameraZ),
	)
	planePx := new(big.Rat).Mul(
		new(big.Rat).SetInt64(e.Camera.FocalX),
		new(big.Rat).SetFrac64(planeErr, anchor.CameraZ),
	)
	total := new(big.Rat).SetInt64(projErr)
	total.Add(total, new(big.Rat).SetInt64(cellPxQ))
	total.Add(total, depthPx)
	total.Add(total, planePx)
	return ratCeilInt64(total) + q.ScreenStep()
}

func absSum(v model.Vec3) int64 {
	x, y, z := v.X, v.Y, v.Z
	if x < 0 {
		x = -x
	}
	if y < 0 {
		y = -y
	}
	if z < 0 {
		z = -z
	}
	return x + y + z
}

func ratOf(v int64, bits int) *big.Rat {
	return new(big.Rat).SetFrac(big.NewInt(v), new(big.Int).Lsh(big.NewInt(1), uint(bits)))
}

func ratToWorldQ(v *big.Rat, bits uint8) int64 {
	scaled := new(big.Rat).Mul(v, new(big.Rat).SetInt(new(big.Int).Lsh(big.NewInt(1), uint(bits))))
	return RatToInt64Rounded(scaled)
}

func ratCeilInt64(v *big.Rat) int64 {
	num := new(big.Int).Set(v.Num())
	den := new(big.Int).Set(v.Denom())
	quo, rem := new(big.Int), new(big.Int)
	quo.QuoRem(num, den, rem)
	if rem.Sign() > 0 {
		quo.Add(quo, big.NewInt(1))
	}
	if !quo.IsInt64() {
		return int64(1) << 40
	}
	return quo.Int64()
}

func ceilDivInt64(a, b int64) int64 {
	if b <= 0 {
		return 0
	}
	if a <= 0 {
		return 0
	}
	return (a + b - 1) / b
}

func SortIndices(v []int) {
	sort.Ints(v)
}
