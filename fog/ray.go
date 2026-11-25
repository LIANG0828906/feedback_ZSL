package fog

import (
	"fmt"
	"math"
)

type Ray struct {
	ID         string `json:"id"`
	CameraTime int64  `json:"camera_time"`
	X          int    `json:"x"`
	Y          int    `json:"y"`
	Origin     Vec3   `json:"origin"`
	Direction  Vec3   `json:"direction"`
}

func CameraRay(cam Camera, x, y, width, height int) Ray {
	forward, right, up := cameraBasis(cam)
	ndcX := (2*(float64(x)+0.5)/float64(width) - 1)
	ndcY := (1 - 2*(float64(y)+0.5)/float64(height))
	tanHalf := math.Tan(cam.FOVY * 0.5)
	dir := forward.Add(right.Mul(ndcX * tanHalf * cam.Aspect)).Add(up.Mul(ndcY * tanHalf)).Normalize()
	return Ray{
		ID:         RayID(cam.LogicalTime, x, y),
		CameraTime: cam.LogicalTime,
		X:          x, Y: y,
		Origin:    cam.Position,
		Direction: dir,
	}
}

func RayID(cameraTime int64, x, y int) string {
	return fmt.Sprintf("ray:c%d:x%d:y%d", cameraTime, x, y)
}

func cameraBasis(cam Camera) (forward, right, up Vec3) {
	cp := math.Cos(cam.Pitch)
	forward = V(math.Sin(cam.Yaw)*cp, math.Sin(cam.Pitch), math.Cos(cam.Yaw)*cp).Normalize()
	worldUp := V(0, 1, 0)
	right = worldUp.Cross(forward)
	if right.LengthSquared() < 1e-12 {
		right = V(1, 0, 0)
	} else {
		right = right.Normalize()
	}
	up = forward.Cross(right).Normalize()
	if math.Abs(cam.Roll) > 1e-15 {
		c, s := math.Cos(cam.Roll), math.Sin(cam.Roll)
		r2 := right.Mul(c).Add(up.Mul(s))
		u2 := up.Mul(c).Sub(right.Mul(s))
		right, up = r2, u2
	}
	return forward, right, up
}

func (r Ray) PointAt(t float64) Vec3 {
	return r.Origin.Add(r.Direction.Mul(t))
}
