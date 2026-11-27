package volumefog

import (
	"fmt"
	"math"
)

type mat3 [3][3]float64

func rotationMatrix(yawMdeg, pitchMdeg, rollMdeg int64) mat3 {
	const degToRad = math.Pi / 180.0
	yaw := float64(yawMdeg) / 1000.0 * degToRad
	pitch := float64(pitchMdeg) / 1000.0 * degToRad
	roll := float64(rollMdeg) / 1000.0 * degToRad
	cy, sy := math.Cos(yaw), math.Sin(yaw)
	cp, sp := math.Cos(pitch), math.Sin(pitch)
	cr, sr := math.Cos(roll), math.Sin(roll)
	ry := mat3{{cy, 0, sy}, {0, 1, 0}, {-sy, 0, cy}}
	rx := mat3{{1, 0, 0}, {0, cp, -sp}, {0, sp, cp}}
	rz := mat3{{cr, -sr, 0}, {sr, cr, 0}, {0, 0, 1}}
	return mul3(rz, mul3(ry, rx))
}

func mul3(a, b mat3) mat3 {
	var c mat3
	for i := 0; i < 3; i++ {
		for j := 0; j < 3; j++ {
			c[i][j] = a[i][0]*b[0][j] + a[i][1]*b[1][j] + a[i][2]*b[2][j]
		}
	}
	return c
}

func apply3(m mat3, v [3]float64) [3]float64 {
	return [3]float64{
		m[0][0]*v[0] + m[0][1]*v[1] + m[0][2]*v[2],
		m[1][0]*v[0] + m[1][1]*v[1] + m[1][2]*v[2],
		m[2][0]*v[0] + m[2][1]*v[1] + m[2][2]*v[2],
	}
}

// CameraRay creates an orthographic frustum ray for a pixel.
func CameraRay(cam CameraSample, cameraIndex, px, py int, rayID string) Ray {
	r := rotationMatrix(cam.OrientationMdeg[0], cam.OrientationMdeg[1], cam.OrientationMdeg[2])
	right := apply3(r, [3]float64{1, 0, 0})
	up := apply3(r, [3]float64{0, 1, 0})
	forward := apply3(r, [3]float64{0, 0, 1})
	w := float64(cam.Width)
	h := float64(cam.Height)
	u := (float64(px)+0.5)/w - 0.5
	v := 0.5 - (float64(py)+0.5)/h
	wq := float64(cam.ViewWidthQ) / float64(FixedScale)
	hq := float64(cam.ViewHeightQ) / float64(FixedScale)
	var origin [3]float64
	for a := 0; a < 3; a++ {
		origin[a] = float64(cam.PositionQ[a])/float64(FixedScale) +
			right[a]*u*wq + up[a]*v*hq
	}
	originQ := Vec3i{floatToFixed(origin[0]), floatToFixed(origin[1]), floatToFixed(origin[2])}
	dirQ := Vec3i{floatToFixed(forward[0]), floatToFixed(forward[1]), floatToFixed(forward[2])}
	return Ray{
		ID: rayID, OriginQ: originQ, DirQ: dirQ, NearQ: cam.NearQ, FarQ: cam.FarQ,
		CameraIndex: cameraIndex,
	}
}

func CenterChunkRay(scene *Scene, cx, cy int) (Ray, int, int, error) {
	cam := scene.ActiveCameraSample()
	if cam == nil {
		return Ray{}, 0, 0, schemaErr("", "cameras", "no active camera")
	}
	chunk := scene.ChunkSize
	x0 := cx * chunk
	y0 := cy * chunk
	w := minInt(chunk, cam.Width-x0)
	h := minInt(chunk, cam.Height-y0)
	if w <= 0 || h <= 0 {
		return Ray{}, 0, 0, schemaErr("", "chunk_size", "chunk is outside the viewport")
	}
	px := x0 + w/2
	py := y0 + h/2
	id := fmt.Sprintf("chunk:%d,%d:center:%d,%d", cx, cy, px, py)
	return CameraRay(*cam, scene.ActiveCamera, px, py, id), px, py, nil
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
