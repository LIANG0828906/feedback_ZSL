package camera

import (
	"fmt"
	"math"

	"volumekernel/internal/model"
)

type Ray struct {
	ID        string
	Origin    model.Vec3
	Direction model.Vec3
}

func (r Ray) At(t float64) model.Vec3 {
	return r.Origin.Add(r.Direction.Mul(t))
}

func RayForPixel(sample model.CameraSample, vp model.Viewport, x, y int) (Ray, error) {
	if vp.Width <= 0 || vp.Height <= 0 || x < 0 || y < 0 || x >= vp.Width || y >= vp.Height {
		return Ray{}, fmt.Errorf("pixel (%d,%d) outside viewport %dx%d", x, y, vp.Width, vp.Height)
	}
	forward := sample.Target.Sub(sample.Position).Normalize()
	if forward.Length() == 0 {
		return Ray{}, fmt.Errorf("camera %s has zero forward direction", sample.ID)
	}
	right := forward.Sub(sample.Up.Mul(forward.Dot(sample.Up))).Normalize()
	if right.Length() == 0 {
		return Ray{}, fmt.Errorf("camera %s has degenerate up vector", sample.ID)
	}
	up := right.Cross(forward).Normalize()
	aspect := sample.Aspect
	if aspect <= 0 {
		aspect = float64(vp.Width) / float64(vp.Height)
	}
	tanHalf := math.Tan(sample.FOVDeg * math.Pi / 360)
	ndcX := (float64(x)+0.5)/float64(vp.Width)*2 - 1
	ndcY := 1 - (float64(y)+0.5)/float64(vp.Height)*2
	dir := forward.Add(right.Mul(ndcX * tanHalf * aspect)).Add(up.Mul(ndcY * tanHalf)).Normalize()
	return Ray{
		ID:        fmt.Sprintf("ray:%s:%d:%d", sample.ID, x, y),
		Origin:    sample.Position,
		Direction: dir,
	}, nil
}

func (r Ray) AtTicks(ticks int64, scale int64) model.Vec3 {
	return r.At(float64(ticks) / float64(scale))
}
