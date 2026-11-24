package geometry

import (
	"math/big"

	"occlusionlab/internal/model"
)

// ExactProjection is the generator-side reference path. It uses rational
// arithmetic on the exact quantized scene integers and is never derived from
// the fixed-point evaluator.
type ExactProjection struct {
	CameraX *big.Rat
	CameraY *big.Rat
	Depth   *big.Rat
	ScreenX *big.Rat
	ScreenY *big.Rat
}

func rat(v int64) *big.Rat          { return new(big.Rat).SetInt64(v) }
func addRat(a, b *big.Rat) *big.Rat { return new(big.Rat).Add(a, b) }
func subRat(a, b *big.Rat) *big.Rat { return new(big.Rat).Sub(a, b) }
func mulRat(a, b *big.Rat) *big.Rat { return new(big.Rat).Mul(a, b) }

func dotRat(rel model.Vec3, basis model.Vec3, shift uint8) *big.Rat {
	out := new(big.Rat)
	out.Add(out, mulRat(rat(rel.X), rat(basis.X)))
	out.Add(out, mulRat(rat(rel.Y), rat(basis.Y)))
	out.Add(out, mulRat(rat(rel.Z), rat(basis.Z)))
	out.Quo(out, new(big.Rat).SetInt(new(big.Int).Lsh(big.NewInt(1), uint(shift))))
	return out
}

func ExactProject(cam model.CameraSample, p model.Vec3, basisBits uint8) (ExactProjection, bool) {
	rel := p.Sub(cam.Position)
	cx := dotRat(rel, cam.Right, basisBits)
	cy := dotRat(rel, cam.Up, basisBits)
	cz := dotRat(rel, cam.Forward, basisBits)
	if cz.Cmp(rat(cam.Near)) <= 0 || cz.Cmp(rat(cam.Far)) > 0 {
		return ExactProjection{}, false
	}
	sx := new(big.Rat).Quo(mulRat(rat(cam.FocalX), cx), cz)
	sx.Add(sx, rat(cam.CenterX))
	sy := new(big.Rat).Quo(mulRat(rat(cam.FocalY), cy), cz)
	sy.Sub(rat(cam.CenterY), sy)
	return ExactProjection{
		CameraX: cx,
		CameraY: cy,
		Depth:   cz,
		ScreenX: sx,
		ScreenY: sy,
	}, true
}

type ExactRect struct {
	MinX, MinY *big.Rat
	MaxX, MaxY *big.Rat
	MinDepth   *big.Rat
	MaxDepth   *big.Rat
}

func ExactOccluderRect(cam model.CameraSample, box model.AABB, basisBits uint8) (ExactRect, bool) {
	corners := box.Corners()
	var proj ExactProjection
	var ok bool
	first := true
	out := ExactRect{}
	for _, corner := range corners {
		proj, ok = ExactProject(cam, corner, basisBits)
		if !ok {
			return ExactRect{}, false
		}
		if first {
			out.MinX = new(big.Rat).Set(proj.ScreenX)
			out.MaxX = new(big.Rat).Set(proj.ScreenX)
			out.MinY = new(big.Rat).Set(proj.ScreenY)
			out.MaxY = new(big.Rat).Set(proj.ScreenY)
			out.MinDepth = new(big.Rat).Set(proj.Depth)
			out.MaxDepth = new(big.Rat).Set(proj.Depth)
			first = false
			continue
		}
		if proj.ScreenX.Cmp(out.MinX) < 0 {
			out.MinX.Set(proj.ScreenX)
		}
		if proj.ScreenX.Cmp(out.MaxX) > 0 {
			out.MaxX.Set(proj.ScreenX)
		}
		if proj.ScreenY.Cmp(out.MinY) < 0 {
			out.MinY.Set(proj.ScreenY)
		}
		if proj.ScreenY.Cmp(out.MaxY) > 0 {
			out.MaxY.Set(proj.ScreenY)
		}
		if proj.Depth.Cmp(out.MinDepth) < 0 {
			out.MinDepth.Set(proj.Depth)
		}
		if proj.Depth.Cmp(out.MaxDepth) > 0 {
			out.MaxDepth.Set(proj.Depth)
		}
	}
	return out, !first
}

func (r ExactRect) Contains(x, y *big.Rat) bool {
	return x.Cmp(r.MinX) >= 0 && x.Cmp(r.MaxX) <= 0 &&
		y.Cmp(r.MinY) >= 0 && y.Cmp(r.MaxY) <= 0
}

func RatToInt64Rounded(v *big.Rat) int64 {
	num := new(big.Int).Set(v.Num())
	den := new(big.Int).Set(v.Denom())
	quo, rem := new(big.Int), new(big.Int)
	quo.QuoRem(num, den, rem)
	if new(big.Int).Lsh(new(big.Int).Abs(rem), 1).Cmp(new(big.Int).Abs(den)) >= 0 {
		if num.Sign() < 0 {
			quo.Sub(quo, big.NewInt(1))
		} else {
			quo.Add(quo, big.NewInt(1))
		}
	}
	return quo.Int64()
}

func RatAbsSub(a, b *big.Rat) *big.Rat {
	out := new(big.Rat).Sub(a, b)
	return out.Abs(out)
}

func RatToFloat(v *big.Rat) float64 {
	f, _ := v.Float64()
	return f
}
