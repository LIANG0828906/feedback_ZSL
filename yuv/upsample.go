package yuv

import (
	"fmt"
	"math"
)

func NewComposite(meta ImageMeta, y, u, v *Plane) (*Composite, error) {
	return Upsample420(meta, y, u, v)
}

func newCompositeBase(meta ImageMeta, y, u, v *Plane) *Composite {
	if y == nil || u == nil || v == nil {
		return nil
	}
	yMin, yMax := meta.PlaneLimits("Y")
	uMin, uMax := meta.PlaneLimits("U")
	vMin, vMax := meta.PlaneLimits("V")
	return &Composite{
		Width:      meta.Width,
		Height:     meta.Height,
		BitDepth:   meta.BitDepth,
		QuantRange: meta.Quantization.Range,
		YMin:       yMin,
		YMax:       yMax,
		UMin:       uMin,
		UMax:       uMax,
		VMin:       vMin,
		VMax:       vMax,
		Y:          append([]uint16(nil), y.Data...),
		U:          append([]uint16(nil), u.Data...),
		V:          append([]uint16(nil), v.Data...),
	}
}

// Upsample420 applies the declared 4:2:0 chroma alignment and upsampling rule.
// The returned YUV 4:4:4 planes always have exactly the luma dimensions.
func Upsample420(meta ImageMeta, y, u, v *Plane) (*Composite, error) {
	if y == nil || u == nil || v == nil {
		return nil, fmt.Errorf("missing plane")
	}
	if y.Width != meta.Width || y.Height != meta.Height {
		return nil, fmt.Errorf("luma is %dx%d, expected %dx%d", y.Width, y.Height, meta.Width, meta.Height)
	}
	if u.Width != meta.Width/2 || u.Height != meta.Height/2 || v.Width != meta.Width/2 || v.Height != meta.Height/2 {
		return nil, fmt.Errorf("chroma dimensions do not match 4:2:0 layout")
	}
	out := newCompositeBase(meta, y, u, v)
	out.U = make([]uint16, meta.Width*meta.Height)
	out.V = make([]uint16, meta.Width*meta.Height)
	for py := 0; py < meta.Height; py++ {
		for px := 0; px < meta.Width; px++ {
			uValue := sampleChroma(u, px, py, meta.ChromaAlignment, meta.Upsample)
			vValue := sampleChroma(v, px, py, meta.ChromaAlignment, meta.Upsample)
			out.U[py*meta.Width+px] = uValue
			out.V[py*meta.Width+px] = vValue
		}
	}
	return out, nil
}

func sampleChroma(plane *Plane, x, y int, alignment, rule string) uint16 {
	w, h := plane.Width, plane.Height
	if w <= 0 || h <= 0 {
		return 0
	}
	if rule == "nearest" {
		sx, sy := nearestChromaCoord(x, y, w, h, alignment)
		return plane.Data[sy*w+sx]
	}
	fx, fy := bilinearChromaCoord(x, y, w, h, alignment)
	x0 := int(math.Floor(fx))
	y0 := int(math.Floor(fy))
	x1 := x0 + 1
	y1 := y0 + 1
	dx := fx - float64(x0)
	dy := fy - float64(y0)
	x0 = clampIndex(x0, w)
	x1 = clampIndex(x1, w)
	y0 = clampIndex(y0, h)
	y1 = clampIndex(y1, h)
	v00 := float64(plane.Data[y0*w+x0])
	v10 := float64(plane.Data[y0*w+x1])
	v01 := float64(plane.Data[y1*w+x0])
	v11 := float64(plane.Data[y1*w+x1])
	top := v00 + (v10-v00)*dx
	bottom := v01 + (v11-v01)*dx
	value := top + (bottom-top)*dy
	return roundClamp(value, plane.MinCode, plane.MaxCode)
}

func nearestChromaCoord(x, y, w, h int, alignment string) (int, int) {
	var sx, sy int
	if alignment == "center" {
		sx = int(math.Floor((float64(x) + 0.5) / 2.0))
		sy = int(math.Floor((float64(y) + 0.5) / 2.0))
	} else {
		sx = x / 2
		sy = y / 2
	}
	return clampIndex(sx, w), clampIndex(sy, h)
}

func bilinearChromaCoord(x, y, w, h int, alignment string) (float64, float64) {
	var fx, fy float64
	if alignment == "center" {
		fx = (float64(x) - 0.5) / 2.0
		fy = (float64(y) - 0.5) / 2.0
	} else {
		fx = float64(x) / 2.0
		fy = float64(y) / 2.0
	}
	if fx < 0 {
		fx = 0
	}
	if fy < 0 {
		fy = 0
	}
	if fx > float64(w-1) {
		fx = float64(w - 1)
	}
	if fy > float64(h-1) {
		fy = float64(h - 1)
	}
	return fx, fy
}

func clampIndex(index, size int) int {
	if index < 0 {
		return 0
	}
	if index >= size {
		return size - 1
	}
	return index
}
