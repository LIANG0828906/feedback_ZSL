package culling

import (
	"math"

	"occlbench/internal/core"
	"occlbench/internal/scene"
)

// Plane 平面方程 normal·p + d = 0，法线指向视锥内部；视锥内点满足 side > 0。
type Plane struct {
	N core.Vec3
	D float64
}

func (p Plane) Side(v core.Vec3) float64 { return p.N.Dot(v) + p.D }

// Frustum 由相机内外参提取的 6 个世界空间平面（左、右、下、上、近、远）。
type Frustum struct {
	Planes      [6]Plane
	Origin      core.Vec3
	Forward     core.Vec3
	Right       core.Vec3
	Up          core.Vec3
	FOVy        float64
	Aspect      float64
	Near        float64
	Far         float64
	TanHalfFovy float64
	TanHalfFovx float64
}

// ExtractFrustum 由相机采样几何构造视锥平面。该实现被预览与精确两条路径共享。
func ExtractFrustum(s scene.CameraSample) Frustum {
	fwd := s.Forward.Normalize()
	up := s.Up.Normalize()
	// 正交化，避免 forward/up 非严格正交带来的偏差。
	right := up.Cross(fwd).Normalize() // 屏幕右侧单位向量
	up = right.Cross(fwd).Normalize()

	halfFovy := s.FOVy * math.Pi / 360
	halfFovx := math.Atan(math.Tan(halfFovy) * s.Aspect)
	origin := s.Pos

	// 构造法线指向视锥内部的平面（内点 side>0），侧面过相机原点。
	mk := func(normal core.Vec3, point core.Vec3) Plane {
		n := normal.Normalize()
		return Plane{N: n, D: -n.Dot(point)}
	}
	// 以 tan 形式构造内法线，使平面与中心轴夹角严格等于视锥半角，过相机原点。
	tx := math.Tan(halfFovx)
	ty := math.Tan(halfFovy)
	left := mk(fwd.Add(right.Scale(tx)), origin)
	rightP := mk(fwd.Sub(right.Scale(tx)), origin)
	bottom := mk(fwd.Add(up.Scale(ty)), origin)
	top := mk(fwd.Sub(up.Scale(ty)), origin)
	nearP := mk(fwd, origin.Add(fwd.Scale(s.Near)))
	farP := mk(fwd.Scale(-1), origin.Add(fwd.Scale(s.Far)))

	tanHalfFovy := math.Tan(halfFovy)
	return Frustum{
		Planes:      [6]Plane{left, rightP, bottom, top, nearP, farP},
		Origin:      origin,
		Forward:     fwd,
		Right:       right,
		Up:          up,
		FOVy:        s.FOVy,
		Aspect:      s.Aspect,
		Near:        s.Near,
		Far:         s.Far,
		TanHalfFovy: tanHalfFovy,
		TanHalfFovx: tanHalfFovy * s.Aspect,
	}
}

// BoxInFrustum 保守视锥测试：任一角点在所有平面内侧则视为相交/在内。
// 量化后的角点由调用方统一传入，保证预览与精确路径量化精度一致。
func BoxInFrustum(f Frustum, corners [8]core.Vec3) bool {
	for _, p := range f.Planes {
		allOutside := true // 所有角点都在该平面外侧（side<=0）则整体在外。
		for _, c := range corners {
			if p.Side(c) > 0 {
				allOutside = false
				break
			}
		}
		if allOutside {
			return false
		}
	}
	return true
}

// ProjectPoint 将世界点投影到归一化屏幕坐标 (u,v)∈[0,1] 与相机空间深度。
func (f Frustum) ProjectPoint(p core.Vec3) (u, v, depth float64, behind bool) {
	rel := p.Sub(f.Origin)
	camZ := rel.Dot(f.Forward)
	if camZ <= f.Near {
		return 0, 0, camZ, true
	}
	camX := rel.Dot(f.Right)
	camY := rel.Dot(f.Up)
	ndcX := camX / (camZ * f.TanHalfFovx)
	ndcY := camY / (camZ * f.TanHalfFovy)
	u = ndcX*0.5 + 0.5
	v = 0.5 - ndcY*0.5
	return u, v, camZ, false
}
