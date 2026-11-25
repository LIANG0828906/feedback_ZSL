package vectorcore

import "fmt"

// CompositePath returns a new aggregate bitmap with one path's coverage and
// paint applied. The input bitmap is never mutated, which makes cache entries
// safe to reuse and replace atomically.
func CompositePath(dst *Bitmap, cov Coverage, paint Paint, pathID string) (*Bitmap, error) {
	if paint.Opacity < 0 || paint.Opacity > 10000 {
		return nil, &Error{Code: CodeMode, Op: "composite", PathID: pathID, Field: "opacity", Keyframe: -1, TimeIndex: -1,
			Err: fmt.Errorf("opacity outside [0,10000]")}
	}
	switch paint.Blend {
	case BlendSourceOver, BlendNormal, BlendAdd, BlendMultiply, BlendScreen:
	default:
		return nil, &Error{Code: CodeMode, Op: "composite", PathID: pathID, Field: "blendMode", Keyframe: -1, TimeIndex: -1,
			Err: fmt.Errorf("unsupported blend mode %q", paint.Blend)}
	}
	if dst == nil || cov.Width != dst.Width || cov.Height != dst.Height || len(cov.Values) != dst.Width*dst.Height {
		return nil, &Error{Code: CodeMode, Op: "composite", PathID: pathID, Field: "coverage", Keyframe: -1, TimeIndex: -1,
			Err: fmt.Errorf("coverage dimensions do not match bitmap")}
	}
	out := dst.Clone()
	for i, c := range cov.Values {
		if c == 0 {
			continue
		}
		sa := int64(paint.Color.A) * int64(paint.Opacity) * int64(c) / (255 * 10000)
		if sa <= 0 {
			continue
		}
		if sa > 65535 {
			sa = 65535
		}
		sa8 := int(sa * 255 / 65535)
		if sa8 == 0 {
			sa8 = 1
		}
		o := i * 4
		da := int(out.Pixels[o+3])
		src := [3]int{int(paint.Color.R), int(paint.Color.G), int(paint.Color.B)}
		var dstc [3]int
		for ch := 0; ch < 3; ch++ {
			dstc[ch] = int(out.Pixels[o+ch])
		}
		if da == 0 {
			out.Pixels[o] = uint8(src[0])
			out.Pixels[o+1] = uint8(src[1])
			out.Pixels[o+2] = uint8(src[2])
			out.Pixels[o+3] = uint8(sa8)
			continue
		}
		for ch := 0; ch < 3; ch++ {
			out.Pixels[o+ch] = uint8(blendChannel(paint.Blend, src[ch], dstc[ch], sa8))
		}
		out.Pixels[o+3] = uint8(clampByte(da + (255-da)*sa8/255))
	}
	return out, nil
}

func blendChannel(mode BlendMode, src, dst, sa int) int {
	switch mode {
	case BlendAdd:
		return clampByte(dst + src*sa/255)
	case BlendMultiply:
		blend := src * dst / 255
		return clampByte(dst + (blend-dst)*sa/255)
	case BlendScreen:
		blend := 255 - (255-src)*(255-dst)/255
		return clampByte(dst + (blend-dst)*sa/255)
	default:
		return clampByte((src*sa + dst*(255-sa)) / 255)
	}
}

func clampByte(v int) int {
	if v < 0 {
		return 0
	}
	if v > 255 {
		return 255
	}
	return v
}
