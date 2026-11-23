package morph

import "math"

// FrameGeometry maps a padded world-space bounding box to an integer frame.
// The sampling grid changes coverage integration, while frame dimensions depend
// on the exact geometry bbox and scale. Keeping dimensions stable across grid
// choices is required for pixel-exact cross-run comparison.
type FrameGeometry struct {
	GeometryBBox  BBox    `json:"geometry_bbox"`
	WorldBBox     BBox    `json:"world_bbox"`
	Width         int     `json:"width"`
	Height        int     `json:"height"`
	Scale         float64 `json:"scale"`
	PaddingPixels int     `json:"padding_pixels"`
}

func DeriveFrameGeometry(geometry BBox, scale float64, paddingPixels, maxPixels int) (FrameGeometry, error) {
	return DeriveFrameGeometryAligned(geometry, scale, paddingPixels, maxPixels, 4)
}

// DeriveFrameGeometryAligned rounds frame dimensions up to a deterministic
// pixel-grid multiple. Grids 1..4 share a 4-pixel alignment, which keeps fine
// and reduced-subsample modes directly pixel-comparable; larger grids use their
// own alignment.
func DeriveFrameGeometryAligned(geometry BBox, scale float64, paddingPixels, maxPixels, grid int) (FrameGeometry, error) {
	if geometry.IsEmpty() {
		return FrameGeometry{}, withField(newError(CodePath, "frame geometry is empty"), "geometry_bbox")
	}
	if !finite(scale) || scale <= 0 {
		return FrameGeometry{}, withField(newError(CodeParam, "scale must be finite and positive"), "scale")
	}
	if paddingPixels < 0 {
		return FrameGeometry{}, withField(newError(CodeParam, "padding must be non-negative"), "padding_pixels")
	}
	if maxPixels <= 0 {
		return FrameGeometry{}, withField(newError(CodeParam, "max frame pixels must be positive"), "max_frame_pixels")
	}
	pad := float64(paddingPixels) / scale
	world := geometry.Expand(pad)
	w := int(math.Ceil(world.Width() * scale))
	h := int(math.Ceil(world.Height() * scale))
	align := 4
	if grid > align {
		align = grid
	}
	if align > 1 {
		w = ((w + align - 1) / align) * align
		h = ((h + align - 1) / align) * align
	}
	if w <= 0 || h <= 0 {
		return FrameGeometry{}, withField(newError(CodeParam, "derived frame has zero size"), "geometry_bbox")
	}
	if w > maxPixels || h > maxPixels {
		e := withField(newError(CodeParam, "derived frame exceeds max frame pixels"), "max_frame_pixels")
		e.Measured = intToString(maxInt(w, h))
		e.Expected = intToString(maxPixels)
		return FrameGeometry{}, e
	}
	return FrameGeometry{
		GeometryBBox: geometry, WorldBBox: world, Width: w, Height: h,
		Scale: scale, PaddingPixels: paddingPixels,
	}, nil
}

func (f FrameGeometry) WorldToPixel(p Vec2) Vec2 {
	return Vec2{
		X: (p.X - f.WorldBBox.Min.X) * f.Scale,
		Y: (f.WorldBBox.Max.Y - p.Y) * f.Scale,
	}
}

func (f FrameGeometry) PixelToWorld(x, y float64) Vec2 {
	return Vec2{
		X: f.WorldBBox.Min.X + x/f.Scale,
		Y: f.WorldBBox.Max.Y - y/f.Scale,
	}
}

func intToString(v int) string {
	// Kept local to avoid pulling strconv into hot validation paths.
	if v == 0 {
		return "0"
	}
	neg := v < 0
	if neg {
		v = -v
	}
	buf := [24]byte{}
	i := len(buf)
	for v > 0 {
		i--
		buf[i] = byte('0' + v%10)
		v /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
