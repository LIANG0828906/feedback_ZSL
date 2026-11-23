package generator

import (
	"math"
	"sort"

	"occlbench/internal/core"
	"occlbench/internal/scene"
)

// buildGroundTruth 用与引擎完全独立的射线-AABB 求交参考实现生成真值。
func buildGroundTruth(w *scene.World, cfg Config) {
	gt := scene.GroundTruth{
		Visible:       make([][]int, len(w.Trajectory.Samples)),
		OccluderBasis: map[int][]int{},
	}
	chunks := w.AllChunks()

	for si, cam := range w.Trajectory.Samples {
		fwd := cam.Forward.Normalize()
		up := cam.Up.Normalize()
		right := up.Cross(fwd).Normalize()
		up = right.Cross(fwd).Normalize()
		tanHalfFovy := math.Tan(cam.FOVy * math.Pi / 360)
		tanHalfFovx := tanHalfFovy * cam.Aspect

		visible := []int{}
		for _, ch := range chunks {
			vis, basis := rayReferenceVisible(cam.Pos, fwd, right, up, tanHalfFovx, tanHalfFovy, cam.Near, cam.Far, ch, w.Occluders)
			if vis {
				visible = append(visible, ch.ID)
			} else if len(basis) > 0 {
				gt.OccluderBasis[ch.ID] = basis
			}
		}
		gt.Visible[si] = visible
	}

	// 屏幕投影真值：在若干采样点投影分块中心。
	for si := 0; si < len(w.Trajectory.Samples); si += 3 {
		cam := w.Trajectory.Samples[si]
		for ci, ch := range chunks {
			if ci%4 != 0 {
				continue
			}
			px, py, ok := projectToScreen(cam, ch.Box.Center())
			if !ok {
				continue
			}
			gt.ScreenChecks = append(gt.ScreenChecks, scene.ScreenCheck{
				SampleIndex: si, ChunkID: ch.ID, X: px, Y: py,
			})
			proj := scene.ScreenPoint{ChunkID: ch.ID, X: px, Y: py}
			w.Trajectory.Samples[si].Projection = &proj
		}
	}
	w.GroundTruth = gt
}

const refRays = 96

// rayReferenceVisible 在分块盒投影 footprint 上均匀发射射线，
// 视锥外判不可见；盒内所有命中射线都先被更近遮挡体拦截才判遮挡。
func rayReferenceVisible(
	origin, fwd, right, up core.Vec3,
	tanHalfFovx, tanHalfFovy, near, far float64,
	ch scene.Chunk, occluders []scene.Occluder,
) (bool, []int) {
	center := ch.Box.Center()
	rel := center.Sub(origin)
	camZ := rel.Dot(fwd)
	if camZ <= near || camZ >= far {
		return false, nil
	}
	// 视锥阶段与剔除引擎使用完全相同的几何平面测试，保证两实现视锥标准一致；
	// 两实现的差异仅保留在遮挡判定（射线求交 vs 覆盖栅格）。
	if !planeFrustumContainsBox(origin, fwd, right, up, tanHalfFovx, tanHalfFovy, near, far, ch.Box) {
		return false, nil
	}

	// 在分块盒投影到相机平面的子矩形（裁剪到视锥）内自适应发射射线，
	// 即使分块仅以窄缝进入视锥也能被充分采样，避免固定网格漏采样。
	subX0, subX1, subY0, subY1 := projectedSubRect(origin, fwd, right, up, tanHalfFovx, tanHalfFovy, ch.Box)
	hitBasis := map[int]bool{}
	hitCount := 0
	blockedCount := 0
	for i := 0; i < refRays; i++ {
		for j := 0; j < refRays; j++ {
			fx := subX0 + (float64(i)+0.5)/float64(refRays)*(subX1-subX0)
			fy := subY0 + (float64(j)+0.5)/float64(refRays)*(subY1-subY0)
			dir := fwd.
				Add(right.Scale(fx * tanHalfFovx)).
				Add(up.Scale(fy * tanHalfFovy)).Normalize()
			tMin, hit := rayAABB(origin, dir, ch.Box)
			if !hit || tMin < near || tMin > far {
				continue
			}
			hitCount++
			blocker, blockT := nearestBlocker(origin, dir, tMin, occluders)
			if blocker >= 0 && blockT < tMin {
				blockedCount++
				hitBasis[blocker] = true
			}
		}
	}
	if hitCount == 0 {
		return false, nil
	}
	if blockedCount == hitCount {
		basis := make([]int, 0, len(hitBasis))
		for id := range hitBasis {
			basis = append(basis, id)
		}
		sort.Ints(basis)
		return false, basis
	}
	return true, nil
}

// projectedSubRect 计算分块盒角点在相机近平面上的 NDC 包围子矩形（裁剪到 [-1,1]）。
func projectedSubRect(origin, fwd, right, up core.Vec3, tanHalfFovx, tanHalfFovy float64, box core.AABB) (x0, x1, y0, y1 float64) {
	x0, y0 = 1, 1
	x1, y1 = -1, -1
	for _, c := range box.Corners() {
		rel := c.Sub(origin)
		z := rel.Dot(fwd)
		if z <= 0 {
			continue
		}
		nx := rel.Dot(right) / (z * tanHalfFovx)
		ny := rel.Dot(up) / (z * tanHalfFovy)
		if nx < -1 || nx > 1 || ny < -1 || ny > 1 {
			// 仍纳入但后续裁剪，保证窄缝边界被覆盖。
		}
		if nx < x0 {
			x0 = nx
		}
		if nx > x1 {
			x1 = nx
		}
		if ny < y0 {
			y0 = ny
		}
		if ny > y1 {
			y1 = ny
		}
	}
	x0 = clampF(x0, -1, 1)
	x1 = clampF(x1, -1, 1)
	y0 = clampF(y0, -1, 1)
	y1 = clampF(y1, -1, 1)
	if x1 <= x0 {
		x1 = x0 + 1e-6
	}
	if y1 <= y0 {
		y1 = y0 + 1e-6
	}
	return x0, x1, y0, y1
}

func clampF(x, lo, hi float64) float64 {
	if x < lo {
		return lo
	}
	if x > hi {
		return hi
	}
	return x
}

// rayAABB slab 法射线-AABB 求交，返回最近正向距离。
func rayAABB(origin, dir core.Vec3, box core.AABB) (float64, bool) {
	tmin := 0.0
	tmax := math.Inf(1)
	originArr := [3]float64{origin.X, origin.Y, origin.Z}
	dirArr := [3]float64{dir.X, dir.Y, dir.Z}
	minArr := [3]float64{box.Min.X, box.Min.Y, box.Min.Z}
	maxArr := [3]float64{box.Max.X, box.Max.Y, box.Max.Z}
	for a := 0; a < 3; a++ {
		if math.Abs(dirArr[a]) < 1e-12 {
			if originArr[a] < minArr[a] || originArr[a] > maxArr[a] {
				return 0, false
			}
			continue
		}
		inv := 1 / dirArr[a]
		t1 := (minArr[a] - originArr[a]) * inv
		t2 := (maxArr[a] - originArr[a]) * inv
		if t1 > t2 {
			t1, t2 = t2, t1
		}
		if t1 > tmin {
			tmin = t1
		}
		if t2 < tmax {
			tmax = t2
		}
		if tmax < tmin {
			return 0, false
		}
	}
	if tmin < 0 {
		tmin = 0
	}
	return tmin, true
}

// nearestBlocker 返回在 target 距离前拦截射线的最近遮挡体 ID 及其距离。
func nearestBlocker(origin, dir core.Vec3, target float64, occluders []scene.Occluder) (int, float64) {
	best := math.Inf(1)
	bestID := -1
	for _, oc := range occluders {
		t, hit := rayAABB(origin, dir, oc.Box)
		if hit && t >= 0 && t < target && t < best {
			best = t
			bestID = oc.ID
		}
	}
	return bestID, best
}

// planeFrustumContainsBox 用与 culling.ExtractFrustum 相同的 tan 形式平面做盒测试。
func planeFrustumContainsBox(origin, fwd, right, up core.Vec3, tanHalfFovx, tanHalfFovy, near, far float64, box core.AABB) bool {
	type plane struct {
		n core.Vec3
		d float64
	}
	mk := func(normal, point core.Vec3) plane {
		n := normal.Normalize()
		return plane{n: n, d: -n.Dot(point)}
	}
	planes := [6]plane{
		mk(fwd.Add(right.Scale(tanHalfFovx)), origin), // left
		mk(fwd.Sub(right.Scale(tanHalfFovx)), origin), // right
		mk(fwd.Add(up.Scale(tanHalfFovy)), origin),    // bottom
		mk(fwd.Sub(up.Scale(tanHalfFovy)), origin),    // top
		mk(fwd, origin.Add(fwd.Scale(near))),          // near
		mk(fwd.Scale(-1), origin.Add(fwd.Scale(far))), // far
	}
	for _, c := range box.Corners() {
		insideAll := true
		for _, p := range planes {
			if p.n.Dot(c)+p.d <= 0 {
				insideAll = false
				break
			}
		}
		if insideAll {
			return true
		}
	}
	return false
}

// projectToScreen 投影到 1280x720 像素坐标（原点左上）。
func projectToScreen(cam scene.CameraSample, p core.Vec3) (x, y float64, ok bool) {
	fwd := cam.Forward.Normalize()
	up := cam.Up.Normalize()
	right := up.Cross(fwd).Normalize()
	up = right.Cross(fwd).Normalize()
	rel := p.Sub(cam.Pos)
	z := rel.Dot(fwd)
	if z <= cam.Near {
		return 0, 0, false
	}
	tanHalfFovy := math.Tan(cam.FOVy * math.Pi / 360)
	ndcX := rel.Dot(right) / (z * tanHalfFovy * cam.Aspect)
	ndcY := rel.Dot(up) / (z * tanHalfFovy)
	const w, h = 1280, 720
	x = (ndcX*0.5 + 0.5) * w
	y = (0.5 - ndcY*0.5) * h
	return math.Round(x*100) / 100, math.Round(y*100) / 100, true
}
