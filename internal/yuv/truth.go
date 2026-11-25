package yuv

import (
	"context"
	"fmt"

	"uvenhance/internal/apperr"
)

func ValidateTruth(ctx context.Context, sample *Sample) error {
	md := sample.Metadata
	for _, b := range md.Blocks {
		if b.CenterX < 0 || b.CenterX >= md.Width || b.CenterY < 0 || b.CenterY >= md.Height {
			return apperr.Meta("blocks.center", "block center outside image: "+b.Name)
		}
		cx, cy := b.CenterX/2, b.CenterY/2
		if sample.Y.At(b.CenterX, b.CenterY) != b.Y ||
			sample.U.At(cx, cy) != b.U || sample.V.At(cx, cy) != b.V {
			return apperr.Plane("YUV", "block center truth", "geometry/plane mismatch", "truth mismatch for block "+b.Name)
		}
	}
	for _, s := range md.LumaSamples {
		if s.X < 0 || s.X >= md.Width || s.Y < 0 || s.Y >= md.Height {
			return apperr.Meta("luma_samples", "sample outside image")
		}
		if got := sample.Y.At(s.X, s.Y); got != s.Expected {
			return apperr.Plane("Y", fmt.Sprintf("sample(%d,%d)=%d", s.X, s.Y, s.Expected),
				fmt.Sprintf("%d", got), "luma truth mismatch")
		}
	}
	for _, s := range md.ChromaSamples {
		p := sample.U
		if s.Plane == "V" {
			p = sample.V
		} else if s.Plane != "U" {
			return apperr.Meta("chroma_samples.plane", "must be U or V")
		}
		if s.X < 0 || s.X >= p.Width || s.Y < 0 || s.Y >= p.Height {
			return apperr.Meta("chroma_samples", "sample outside plane")
		}
		if got := p.At(s.X, s.Y); got != s.Expected {
			return apperr.Plane(s.Plane, fmt.Sprintf("sample(%d,%d)=%d", s.X, s.Y, s.Expected),
				fmt.Sprintf("%d", got), "chroma truth mismatch")
		}
	}
	img, err := BuildOriginalImage(ctx, sample)
	if err != nil {
		return err
	}
	for _, s := range md.Interleaved {
		if s.X < 0 || s.X >= md.Width || s.Y < 0 || s.Y >= md.Height {
			return apperr.Meta("interleaved_samples", "sample outside image")
		}
		p := img.At(s.X, s.Y)
		if p.Y != s.Expected || p.U != s.U || p.V != s.V {
			return apperr.Plane("YUV", fmt.Sprintf("(%d,%d)=%d/%d/%d", s.X, s.Y, s.Expected, s.U, s.V),
				fmt.Sprintf("%d/%d/%d", p.Y, p.U, p.V), "interleaved truth mismatch")
		}
	}
	return nil
}
