package culling

import (
	"math"

	"occlbench/internal/core"
)

// QuantPrecision 包围盒量化精度。预览与精确路径必须使用同一精度实例。
type QuantPrecision struct {
	// GridStep 世界单位下的量化步长；>0 时角点按步长向外取整（保守量化）。
	GridStep float64
	Version  string
}

// DefaultQuantPrecision 默认量化精度，与 scene.QuantVersion 对应。
func DefaultQuantPrecision() QuantPrecision {
	return QuantPrecision{GridStep: 0.0625, Version: sceneQuantVersion}
}

// 与 scene.QuantVersion 保持一致的局部常量，避免包循环。
const sceneQuantVersion = "quant-1.0.0"

// Validate 校验量化参数。
func (q QuantPrecision) Validate() error {
	if q.GridStep <= 0 || math.IsNaN(q.GridStep) || math.IsInf(q.GridStep, 0) {
		return core.NewError(core.EPara, "quant gridStep out of range",
			map[string]any{"field": "quant.gridStep", "value": q.GridStep})
	}
	if q.Version == "" {
		return core.NewError(core.EPara, "quant version empty", map[string]any{"field": "quant.version"})
	}
	return nil
}

// quantizeScalar 向外取整，保证量化盒不小于原盒（保守）。
func quantizeScalar(x, step float64, ceil bool) float64 {
	n := x / step
	if ceil {
		return math.Ceil(n) * step
	}
	return math.Floor(n) * step
}

// QuantizeBox 对包围盒做保守量化并返回 8 个角点。
func QuantizeBox(box core.AABB, q QuantPrecision) core.AABB {
	step := q.GridStep
	return core.AABB{
		Min: core.Vec3{
			X: quantizeScalar(box.Min.X, step, false),
			Y: quantizeScalar(box.Min.Y, step, false),
			Z: quantizeScalar(box.Min.Z, step, false),
		},
		Max: core.Vec3{
			X: quantizeScalar(box.Max.X, step, true),
			Y: quantizeScalar(box.Max.Y, step, true),
			Z: quantizeScalar(box.Max.Z, step, true),
		},
	}
}

// QuantizedCorners 量化后返回角点，供视锥与栅格化共用。
func QuantizedCorners(box core.AABB, q QuantPrecision) (core.AABB, [8]core.Vec3) {
	qb := QuantizeBox(box, q)
	return qb, qb.Corners()
}
