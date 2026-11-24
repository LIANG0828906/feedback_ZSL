package generator

import (
	"math"

	"strata/internal/model"
)

// SurfaceHeight is deliberately composed from stable elementary functions so
// every supported platform and format produces the same reconstruction input.
func SurfaceHeight(x, y float64) float64 {
	z := 0.452*math.Sin(1.045*x+0.18) +
		0.020*math.Sin(2.17*x+0.42) +
		0.259 -
		0.145*y*y +
		0.014*math.Cos(1.63*y+0.31) +
		0.017*math.Sin(0.91*x+0.63)*math.Cos(1.11*y)
	return z
}

func SurfaceNormal(x, y float64) model.Vec3 {
	const e = 1e-4
	dx := (SurfaceHeight(x+e, y) - SurfaceHeight(x-e, y)) / (2 * e)
	dy := (SurfaceHeight(x, y+e) - SurfaceHeight(x, y-e)) / (2 * e)
	return model.Vec3{X: -dx, Y: -dy, Z: 1}.Normalize()
}

func PointColor(x, y, z float64) (uint8, uint8, uint8) {
	t := math.Max(0, math.Min(1, (x+2.4)/4.8))
	shade := 0.92 + 0.08*math.Sin(1.7*z)
	r := (0.96*(1-t) + 0.19*t) * shade
	g := (0.55*(1-t) + 0.57*t) * shade
	b := (0.24*(1-t) + 0.88*t) * shade
	return uint8(math.Max(0, math.Min(255, r*255))),
		uint8(math.Max(0, math.Min(255, g*255))),
		uint8(math.Max(0, math.Min(255, b*255)))
}
