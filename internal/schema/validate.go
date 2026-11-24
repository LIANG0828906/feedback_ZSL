package schema

import (
	"fmt"

	"occlusionlab/internal/model"
)

func ValidateQuantization(q model.Quantization) *model.AppError {
	type check struct {
		name string
		val  uint8
		min  uint8
		max  uint8
	}
	for _, c := range []check{
		{"quantization.world_frac_bits", q.WorldFracBits, 4, 20},
		{"quantization.basis_frac_bits", q.BasisFracBits, 8, 24},
		{"quantization.screen_frac_bits", q.ScreenFracBits, 4, 16},
		{"quantization.param_frac_bits", q.ParamFracBits, 8, 24},
	} {
		if c.val < c.min || c.val > c.max {
			return model.ParamError(c.name, fmt.Sprintf("定点精度位宽必须在 [%d,%d] 内", c.min, c.max))
		}
	}
	return nil
}

func ValidateParams(p model.Params) *model.AppError {
	if err := ValidateQuantization(p.Quantization); err != nil {
		return err
	}
	type check struct {
		name     string
		value    int
		min, max int
	}
	for _, c := range []check{
		{"raster_resolution", p.RasterResolution, 8, 256},
		{"max_workers", p.MaxWorkers, 1, 16},
		{"raster_buffer_cells", p.RasterBufferCells, 64, 1 << 20},
		{"max_resident", p.MaxResident, 1, 4096},
		{"load_cost_budget", p.LoadCostBudget, 1, 1 << 24},
		{"version", p.Version, 1, 1 << 30},
	} {
		if c.value < c.min || c.value > c.max {
			return model.ParamError(c.name, fmt.Sprintf("参数值 %d 必须在 [%d,%d] 内", c.value, c.min, c.max))
		}
	}
	if p.RasterResolution*p.RasterResolution > p.RasterBufferCells {
		return model.ParamError("raster_resolution",
			fmt.Sprintf("栅格 %dx%d 超过每个工作线程的 %d 单元缓冲上限",
				p.RasterResolution, p.RasterResolution, p.RasterBufferCells))
	}
	if p.TolerancePxQ <= 0 {
		return model.ParamError("tolerance_px_q", "投影容差必须为正定点像素值")
	}
	return nil
}

func ValidateProject(project *model.Project) *model.AppError {
	if project == nil {
		return model.SchemaError(0, "project", "项目为空")
	}
	if project.Meta.SchemaVersion != model.SchemaVersion {
		return &model.AppError{
			Code: model.CodeSchema, Field: "meta.schema_version",
			Message: fmt.Sprintf("不支持的场景版本 %d", project.Meta.SchemaVersion),
		}
	}
	if project.Meta.GeneratorVersion == "" {
		return model.SchemaError(0, "meta.generator_version", "生成器版本不能为空")
	}
	if err := ValidateQuantization(project.Meta.Precision); err != nil {
		return err
	}
	if project.Meta.TolerancePxQ <= 0 {
		return model.ParamError("meta.tolerance_px_q", "容差必须为正定点像素值")
	}

	if err := validateAABB(project.World.WorldBounds, 0, "world.world_bounds"); err != nil {
		return err
	}
	if len(project.World.Chunks) == 0 {
		return model.SchemaError(0, "world.chunks", "至少需要一个分块")
	}
	occluderIDs := make(map[string]struct{}, len(project.World.Occluders))
	for i, occ := range project.World.Occluders {
		if occ.ID == "" {
			return model.SchemaError(0, fmt.Sprintf("occluders[%d].id", i), "遮挡体编号不能为空")
		}
		if _, exists := occluderIDs[occ.ID]; exists {
			return model.SchemaError(0, fmt.Sprintf("occluders[%d].id", i), "遮挡体编号重复")
		}
		occluderIDs[occ.ID] = struct{}{}
		if err := validateAABB(occ.Bounds, 0, fmt.Sprintf("occluders[%d].bounds", i)); err != nil {
			return err
		}
		if occ.SourceChunk < 0 || occ.SourceChunk >= len(project.World.Chunks) {
			return &model.AppError{
				Code: model.CodeSchema, Field: fmt.Sprintf("occluders[%d].source_chunk", i),
				Message: "遮挡体来源分块越界",
			}
		}
	}

	for i, chunk := range project.World.Chunks {
		if chunk.Index != i {
			return &model.AppError{
				Code: model.CodeSchema, Chunk: i, Field: fmt.Sprintf("chunks[%d].index", i),
				Message: fmt.Sprintf("分块索引必须连续且等于位置编号，期望 %d，实际 %d", i, chunk.Index),
			}
		}
		if err := validateAABB(chunk.Bounds, i, fmt.Sprintf("chunks[%d].bounds", i)); err != nil {
			return err
		}
		if !chunk.Bounds.Contains(chunk.Anchor) {
			return &model.AppError{
				Code: model.CodeSchema, Chunk: i, Field: fmt.Sprintf("chunks[%d].anchor", i),
				Message: "锚点必须位于分块包围体内",
			}
		}
		for j, ref := range chunk.OccluderRefs {
			if _, ok := occluderIDs[ref]; !ok {
				return &model.AppError{
					Code: model.CodeSchema, Chunk: i,
					Field:   fmt.Sprintf("chunks[%d].occluder_refs[%d]", i, j),
					Message: fmt.Sprintf("遮挡体引用越界: %q", ref),
				}
			}
		}
		if chunk.Complexity <= 0 {
			return &model.AppError{
				Code: model.CodeSchema, Chunk: i, Field: fmt.Sprintf("chunks[%d].complexity", i),
				Message: "几何复杂度必须为正数",
			}
		}
		if chunk.LoadCost < 0 {
			return &model.AppError{
				Code: model.CodeSchema, Chunk: i, Field: fmt.Sprintf("chunks[%d].load_cost", i),
				Message: "加载成本不能为负",
			}
		}
	}

	if len(project.World.Cameras) == 0 {
		return model.SchemaError(0, "world.cameras", "相机轨迹不能为空")
	}
	var lastTime *int64
	for i, cam := range project.World.Cameras {
		if cam.Window != i {
			return &model.AppError{
				Code: model.CodeSchema, Window: i, Field: fmt.Sprintf("cameras[%d].window", i),
				Message: "相机窗口编号必须连续",
			}
		}
		if lastTime != nil && cam.Time <= *lastTime {
			return &model.AppError{
				Code: model.CodeSchema, Window: i, Field: fmt.Sprintf("cameras[%d].time", i),
				Message: "相机轨迹采样时刻必须严格单调递增",
			}
		}
		t := cam.Time
		lastTime = &t
		if cam.Near <= 0 || cam.Far <= cam.Near {
			return &model.AppError{
				Code: model.CodeSchema, Window: i, Field: fmt.Sprintf("cameras[%d].far", i),
				Message: "远平面必须大于近平面且近平面为正",
			}
		}
		if cam.Right == (model.Vec3{}) || cam.Up == (model.Vec3{}) || cam.Forward == (model.Vec3{}) {
			return &model.AppError{
				Code: model.CodeSchema, Window: i, Field: fmt.Sprintf("cameras[%d].forward", i),
				Message: "相机基向量不能为零",
			}
		}
		if cam.TanHalfX <= 0 || cam.TanHalfY <= 0 || cam.FocalX == 0 || cam.FocalY == 0 {
			return &model.AppError{
				Code: model.CodeSchema, Window: i, Field: fmt.Sprintf("cameras[%d].tan_half_x", i),
				Message: "视锥切线与焦距必须为正",
			}
		}
		if cam.ScreenWidth <= 0 || cam.ScreenHeight <= 0 {
			return &model.AppError{
				Code: model.CodeSchema, Window: i, Field: fmt.Sprintf("cameras[%d].screen_width", i),
				Message: "屏幕尺寸必须为正",
			}
		}
	}

	if len(project.Truth.Samples) != len(project.World.Cameras) {
		return &model.AppError{
			Code: model.CodeSchema, Field: "truth.samples",
			Message: "真值采样数与相机窗口数不一致",
		}
	}
	for i, sample := range project.Truth.Samples {
		if sample.Window != i || sample.Time != project.World.Cameras[i].Time {
			return &model.AppError{
				Code: model.CodeSchema, Window: i, Field: fmt.Sprintf("truth.samples[%d].time", i),
				Message: "真值采样窗口或时刻与相机轨迹不一致",
			}
		}
		if len(sample.Chunks) != len(project.World.Chunks) {
			return &model.AppError{
				Code: model.CodeSchema, Window: i, Field: fmt.Sprintf("truth.samples[%d].chunks", i),
				Message: "真值必须覆盖每个分块",
			}
		}
		seen := make([]bool, len(project.World.Chunks))
		for _, truthChunk := range sample.Chunks {
			if truthChunk.Chunk < 0 || truthChunk.Chunk >= len(project.World.Chunks) || seen[truthChunk.Chunk] {
				return &model.AppError{
					Code: model.CodeSchema, Window: i, Chunk: truthChunk.Chunk,
					Field:   fmt.Sprintf("truth.samples[%d].chunks", i),
					Message: "真值分块编号越界或重复",
				}
			}
			seen[truthChunk.Chunk] = true
			if truthChunk.ReferenceOccluder != "" {
				if _, ok := occluderIDs[truthChunk.ReferenceOccluder]; !ok {
					return &model.AppError{
						Code: model.CodeSchema, Window: i, Chunk: truthChunk.Chunk,
						Field:   fmt.Sprintf("truth.samples[%d].chunks.reference_occluder", i),
						Message: "真值参考遮挡体越界",
					}
				}
			}
		}
	}
	if project.Truth.TolerancePxQ <= 0 {
		return model.ParamError("truth.tolerance_px_q", "真值容差必须为正定点像素值")
	}
	for i, projection := range project.Truth.KnownProjections {
		if projection.Window < 0 || projection.Window >= len(project.World.Cameras) {
			return &model.AppError{
				Code: model.CodeSchema, Window: projection.Window, Field: fmt.Sprintf("truth.known_projections[%d].window", i),
				Message: "已知投影的窗口越界",
			}
		}
		if projection.Chunk < 0 || projection.Chunk >= len(project.World.Chunks) {
			return &model.AppError{
				Code: model.CodeSchema, Chunk: projection.Chunk,
				Field:   fmt.Sprintf("truth.known_projections[%d].chunk", i),
				Message: "已知投影的分块越界",
			}
		}
	}
	return nil
}

func validateAABB(box model.AABB, chunk int, field string) *model.AppError {
	values := []int64{
		box.Min.X, box.Min.Y, box.Min.Z,
		box.Max.X, box.Max.Y, box.Max.Z,
	}
	for _, v := range values {
		if v < -model.MaxAbsQuantized || v > model.MaxAbsQuantized {
			return &model.AppError{
				Code: model.CodeSchema, Chunk: chunk, Field: field,
				Message: "包围体数值超出定点可表示范围",
			}
		}
	}
	if !box.Valid() {
		return &model.AppError{
			Code: model.CodeSchema, Chunk: chunk, Field: field,
			Message: "包围体最小角必须不大于最大角",
		}
	}
	return nil
}
