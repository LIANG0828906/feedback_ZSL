package workbench

import (
	"fmt"
	"math"
)

// lagrangeWeights returns an interpolation stencil containing the integer
// coordinate nearest the fractional sample position. Even-order stencils keep
// the floor sample on the left side, which makes integer offsets exactly
// reproducible and avoids a scheduler-dependent tie break.
func lagrangeWeights(t float64, order int) (int, int, [6]float64) {
	n := order + 1
	base := int(math.Floor(t))
	start := base - (n-1)/2
	var nodes [6]float64
	for i := 0; i < n; i++ {
		nodes[i] = float64(start + i)
	}
	var weights [6]float64
	for i := 0; i < n; i++ {
		w := 1.0
		for j := 0; j < n; j++ {
			if i == j {
				continue
			}
			w *= (t - nodes[j]) / (nodes[i] - nodes[j])
		}
		weights[i] = w
	}
	return start, n, weights
}

type registerContext struct {
	scene     *Scene
	order     int
	maxOffset float64
	bandScale float64
	qMax      float64
}

func newRegisterContext(scene *Scene, cfg ProcessConfig) (*registerContext, error) {
	if cfg.InterpolationOrder < 1 || cfg.InterpolationOrder > 5 {
		return nil, fieldErr(CodePara, "interpolationOrder", cfg.InterpolationOrder, "1..5", "interpolation order is out of range")
	}
	if cfg.MaxRegistrationOffset <= 0 || !finite(cfg.MaxRegistrationOffset) {
		return nil, fieldErr(CodePara, "maxRegistrationOffsetPx", cfg.MaxRegistrationOffset, ">0", "registration correction range is invalid")
	}
	return &registerContext{
		scene:     scene,
		order:     cfg.InterpolationOrder,
		maxOffset: cfg.MaxRegistrationOffset,
		bandScale: scene.Meta.BandRadianceScale,
		qMax:      float64(maxCode(scene.Meta.BitDepth)),
	}, nil
}

func (r *registerContext) validateOffset(offset RegistrationOffset) error {
	if math.Abs(offset.DX) > r.maxOffset || math.Abs(offset.DY) > r.maxOffset {
		actual := math.Max(math.Abs(offset.DX), math.Abs(offset.DY))
		e := fieldErr(CodeReg, "truth.registration", actual, "<="+fmt.Sprint(r.maxOffset), "registration offset exceeds correctable range")
		e.BandPair = offset.Name()
		e.Offset = &actual
		return e
	}
	return nil
}

func (r *registerContext) sampleBand(b, x, y int, dx, dy float64) float64 {
	img := &r.scene.Bands[b]
	// Registration offsets describe where a non-reference band landed on the
	// sensor. Reading x-dx reverses that displacement.
	fx := float64(x) - dx
	fy := float64(y) - dy
	startX, nx, wx := lagrangeWeights(fx, r.order)
	startY, ny, wy := lagrangeWeights(fy, r.order)
	var value float64
	for j := 0; j < ny; j++ {
		iy := clampInt(startY+j, 0, img.Height-1)
		row := iy * img.Width
		var rowValue float64
		for i := 0; i < nx; i++ {
			ix := clampInt(startX+i, 0, img.Width-1)
			rowValue += wx[i] * float64(img.Data[row+ix])
		}
		value += wy[j] * rowValue
	}
	radiance := value / r.qMax * r.bandScale
	if radiance < 0 {
		return 0
	}
	return radiance
}

func (r *registerContext) alignedPixel(x, y int) ([8]float64, error) {
	var out [8]float64
	for b := 0; b < r.scene.Meta.BandCount; b++ {
		offset := r.scene.Truth.Registration[b]
		if err := r.validateOffset(offset); err != nil {
			return out, err
		}
		out[b] = r.sampleBand(b, x, y, offset.DX, offset.DY)
	}
	return out, nil
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
