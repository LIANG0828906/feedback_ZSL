package culling

import (
	"fmt"
	"math"
	"sort"

	"occlbench/internal/core"
	"occlbench/internal/scene"
)

// coverageGrid 屏幕覆盖栅格：depth 存储该格子已覆盖遮挡体的最近深度，
// owner 记录负责覆盖的遮挡体 ID，用于真值参考遮挡体与差异定位。
type coverageGrid struct {
	res   int
	depth []float64
	owner []int
}

func newCoverageGrid(res int) *coverageGrid {
	g := &coverageGrid{
		res:   res,
		depth: make([]float64, res*res),
		owner: make([]int, res*res),
	}
	for i := range g.depth {
		g.depth[i] = math.Inf(1)
		g.owner[i] = -1
	}
	return g
}

func (g *coverageGrid) index(u, v int) int { return v*g.res + u }

// cover 尝试以遮挡体在某格子的深度覆盖；更近的深度获胜，保证确定性。
func (g *coverageGrid) cover(u, v int, depth float64, occID int) {
	if u < 0 || v < 0 || u >= g.res || v >= g.res {
		return
	}
	idx := g.index(u, v)
	if depth < g.depth[idx] {
		g.depth[idx] = depth
		g.owner[idx] = occID
	}
}

// summary 生成被覆盖格子的稳定摘要，作为已提交窗口栅格摘要的一部分。
func (g *coverageGrid) summary() string {
	cells := make([]int, 0)
	for i, d := range g.depth {
		if math.IsInf(d, 1) == false {
			cells = append(cells, i)
		}
	}
	return fmt.Sprintf("grid%d/cov%d", g.res, len(cells))
}

// rasterizeOccluders 将所有遮挡体的量化包围盒投影到覆盖栅格。
// 采用格子中心采样：仅当格子中心落在遮挡体投影矩形内才标记覆盖，
// 避免屏外足迹被外扩到边缘格子，保证引擎与射线真值在规整布局下精确一致。
func rasterizeOccluders(f Frustum, occluders []scene.Occluder, params Params) *coverageGrid {
	grid := newCoverageGrid(params.GridResolution)
	res := params.GridResolution
	// 按遮挡体 ID 升序处理；同深度时后写入受 cover 的近深度规则约束，结果确定。
	ids := make([]int, 0, len(occluders))
	for i := range occluders {
		ids = append(ids, i)
	}
	sort.Ints(ids)
	for _, id := range ids {
		oc := occluders[id]
		qb := QuantizeBox(oc.Box, params.Quant)
		minU, minV, maxU, maxV, inFront := projectBoxFootprint(f, qb)
		if !inFront {
			continue
		}
		minU, maxU = clipRange(minU, maxU)
		minV, maxV = clipRange(minV, maxV)
		if maxU <= minU || maxV <= minV {
			continue
		}
		// 取遮挡盒最近面深度作为该格子的保守深度。
		nearest := nearestBoxDepth(f, qb)
		u0 := clampInt(int(math.Floor(minU*float64(res))), 0, res-1)
		u1 := clampInt(int(math.Ceil(maxU*float64(res)))-1, 0, res-1)
		v0 := clampInt(int(math.Floor(minV*float64(res))), 0, res-1)
		v1 := clampInt(int(math.Ceil(maxV*float64(res)))-1, 0, res-1)
		for v := v0; v <= v1; v++ {
			for u := u0; u <= u1; u++ {
				cu := (float64(u) + 0.5) / float64(res)
				cv := (float64(v) + 0.5) / float64(res)
				if cu < minU || cu > maxU || cv < minV || cv > maxV {
					continue
				}
				grid.cover(u, v, nearest, oc.ID)
			}
		}
	}
	return grid
}

// projectBoxFootprint 计算包围盒在归一化屏幕坐标的投影外接矩形。
func projectBoxFootprint(f Frustum, box core.AABB) (minU, minV, maxU, maxV float64, inFront bool) {
	minU, minV = 1, 1
	maxU, maxV = 0, 0
	inFront = false
	for _, c := range box.Corners() {
		u, v, _, behind := f.ProjectPoint(c)
		if behind {
			continue
		}
		inFront = true
		if u < minU {
			minU = u
		}
		if u > maxU {
			maxU = u
		}
		if v < minV {
			minV = v
		}
		if v > maxV {
			maxV = v
		}
	}
	return
}

// nearestBoxDepth 包围盒相对相机的最近相机空间深度。
func nearestBoxDepth(f Frustum, box core.AABB) float64 {
	best := math.Inf(1)
	for _, c := range box.Corners() {
		d := c.Sub(f.Origin).Dot(f.Forward)
		if d < best {
			best = d
		}
	}
	return best
}

// occludedBy 判定量化分块盒是否被覆盖栅格中的遮挡体整体遮挡，
// 返回负责遮挡的参考遮挡体 ID（去重升序）。
func occludedBy(f Frustum, box core.AABB, occluders []scene.Occluder, grid *coverageGrid) []int {
	minU, minV, maxU, maxV, inFront := projectBoxFootprint(f, box)
	if !inFront {
		return nil
	}
	// 屏外区域不可能被覆盖栅格遮挡：裁剪到屏幕范围后，任何落在屏外的部分
	// 都使“整体遮挡”不成立（保守判定为可见），避免边缘分块被误剔除。
	minU, maxU = clipRange(minU, maxU)
	minV, maxV = clipRange(minV, maxV)
	if maxU <= minU || maxV <= minV {
		return nil
	}
	res := grid.res
	u0 := clampInt(int(math.Floor(minU*float64(res))), 0, res-1)
	u1 := clampInt(int(math.Ceil(maxU*float64(res)))-1, 0, res-1)
	v0 := clampInt(int(math.Floor(minV*float64(res))), 0, res-1)
	v1 := clampInt(int(math.Ceil(maxV*float64(res)))-1, 0, res-1)

	targetNearest := nearestBoxDepth(f, box)
	hitSet := map[int]bool{}
	total := 0
	covered := 0
	for v := v0; v <= v1; v++ {
		for u := u0; u <= u1; u++ {
			cu := (float64(u) + 0.5) / float64(res)
			cv := (float64(v) + 0.5) / float64(res)
			if cu < minU || cu > maxU || cv < minV || cv > maxV {
				continue
			}
			total++
			idx := grid.index(u, v)
			occID := grid.owner[idx]
			if occID < 0 {
				continue
			}
			// 仅当遮挡体严格位于分块之前才计入，避免同深度自遮挡。
			if grid.depth[idx] < targetNearest {
				covered++
				hitSet[occID] = true
			}
		}
	}
	// 保守整体遮挡：投影 footprint 落在栅格内的所有格子都被更近遮挡体覆盖。
	if total == 0 || covered != total {
		return nil
	}
	hits := make([]int, 0, len(hitSet))
	for id := range hitSet {
		hits = append(hits, id)
	}
	sort.Ints(hits)
	return hits
}

// clipRange 将投影区间裁剪到屏幕 [0,1]；完全在屏外时返回 lo>=hi。
func clipRange(lo, hi float64) (float64, float64) {
	if hi <= 0 || lo >= 1 {
		return 0, 0
	}
	if lo < 0 {
		lo = 0
	}
	if hi > 1 {
		hi = 1
	}
	return lo, hi
}

func clampInt(x, lo, hi int) int {
	if x < lo {
		return lo
	}
	if x > hi {
		return hi
	}
	return x
}
