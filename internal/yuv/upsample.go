package yuv

import (
	"context"

	"uvenhance/internal/apperr"
)

func InterpolationOrder(rule string) int {
	if rule == UpsampleBilin {
		return 1
	}
	return 0
}

func UpsampleChroma(ctx context.Context, p *Plane, width, height int, alignment, rule string) ([]uint16, error) {
	if width%2 != 0 || height%2 != 0 || p.Width != width/2 || p.Height != height/2 {
		return nil, apperr.Plane(p.Name, "half-resolution chroma plane", "dimension mismatch", "cannot upsample")
	}
	if alignment != AlignCenter && alignment != AlignTopLeft {
		return nil, apperr.Meta("chroma_alignment", "unsupported alignment "+alignment)
	}
	if rule != UpsampleBilin && rule != UpsampleNear {
		return nil, apperr.Meta("upsample", "unsupported rule "+rule)
	}
	out := make([]uint16, width*height)
	for y := 0; y < height; y++ {
		if y%32 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		sy := sourceCoordQ8(y, alignment)
		for x := 0; x < width; x++ {
			sx := sourceCoordQ8(x, alignment)
			sx = clampInt(sx, 0, (p.Width-1)<<7)
			sy = clampInt(sy, 0, (p.Height-1)<<7)
			if rule == UpsampleNear {
				ix := clampInt((sx+64)>>7, 0, p.Width-1)
				iy := clampInt((sy+64)>>7, 0, p.Height-1)
				out[y*width+x] = p.At(ix, iy)
				continue
			}
			ix0 := clampInt(sx>>7, 0, p.Width-1)
			iy0 := clampInt(sy>>7, 0, p.Height-1)
			ix1 := minInt(ix0+1, p.Width-1)
			iy1 := minInt(iy0+1, p.Height-1)
			fx := int64(sx & 127)
			fy := int64(sy & 127)
			top := lerpQ8(p.At(ix0, iy0), p.At(ix1, iy0), fx)
			bottom := lerpQ8(p.At(ix0, iy1), p.At(ix1, iy1), fx)
			out[y*width+x] = clampImage(int64(lerpQ8(top, bottom, fy)), p.BitDepth)
		}
	}
	return out, nil
}

func sourceCoordQ8(v int, alignment string) int {
	if alignment == AlignCenter {
		return v*64 - 32
	}
	return v * 64
}

func lerpQ8(a, b uint16, frac int64) uint16 {
	return uint16((int64(a)*(128-frac) + int64(b)*frac + 64) >> 7)
}

func BuildInterleaved(ctx context.Context, yPlane *Plane, uPlane, vPlane *Plane, alignment, rule string) (*Image, error) {
	width, height := yPlane.Width, yPlane.Height
	u, err := UpsampleChroma(ctx, uPlane, width, height, alignment, rule)
	if err != nil {
		return nil, err
	}
	v, err := UpsampleChroma(ctx, vPlane, width, height, alignment, rule)
	if err != nil {
		return nil, err
	}
	img := &Image{
		Width: width, Height: height, BitDepth: yPlane.BitDepth,
		QuantMin: yPlane.QuantMin, QuantMax: yPlane.QuantMax,
		Pixels: make([]Pixel, width*height),
	}
	for i := range img.Pixels {
		img.Pixels[i] = Pixel{Y: yPlane.Data[i], U: u[i], V: v[i]}
	}
	return img, nil
}

func BuildOriginalImage(ctx context.Context, s *Sample) (*Image, error) {
	return BuildInterleaved(ctx, s.Y, s.U, s.V, s.Metadata.ChromaAlignment, s.Metadata.Upsample)
}
