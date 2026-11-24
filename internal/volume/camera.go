package volume

import (
	"math"

	"volumediag/internal/model"
)

type CameraFrame struct {
	Forward model.Vec3
	Right   model.Vec3
	Up      model.Vec3
	TanY    float64
	Aspect  float64
}

func NewCameraFrame(c *model.Camera, width, height int) (CameraFrame, bool) {
	forward, ok := c.Target.Sub(c.Position).Normalize()
	if !ok {
		return CameraFrame{}, false
	}
	right, ok := forward.Cross(c.Up).Normalize()
	if !ok {
		return CameraFrame{}, false
	}
	up := right.Cross(forward)
	return CameraFrame{
		Forward: forward,
		Right:   right,
		Up:      up,
		TanY:    math.Tan(c.FovYDeg * math.Pi / 360),
		Aspect:  float64(width) / float64(height),
	}, true
}

func (f CameraFrame) Ray(c *model.Camera, width, height, x, y int) (model.Vec3, model.Vec3) {
	u := (float64(x)+0.5)/float64(width)*2 - 1
	v := 1 - (float64(y)+0.5)/float64(height)*2
	dir := f.Forward.
		Add(f.Right.Mul(u * f.TanY * f.Aspect)).
		Add(f.Up.Mul(v * f.TanY))
	dir, _ = dir.Normalize()
	return c.Position, dir
}

func (f CameraFrame) RayNDC(c *model.Camera, u, v float64) (model.Vec3, model.Vec3) {
	dir := f.Forward.
		Add(f.Right.Mul(u * f.TanY * f.Aspect)).
		Add(f.Up.Mul(v * f.TanY))
	dir, _ = dir.Normalize()
	return c.Position, dir
}
