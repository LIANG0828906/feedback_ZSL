package editorpick

import (
	"math/big"
)

type ratVec3 [3]*big.Rat

func ratVec(v Vec3) ratVec3 {
	return ratVec3{big.NewRat(int64(v.X), 1), big.NewRat(int64(v.Y), 1), big.NewRat(int64(v.Z), 1)}
}

func ratSub(a, b ratVec3) ratVec3 {
	return ratVec3{new(big.Rat).Sub(a[0], b[0]), new(big.Rat).Sub(a[1], b[1]), new(big.Rat).Sub(a[2], b[2])}
}

func ratDot(a, b ratVec3) *big.Rat {
	return new(big.Rat).Add(
		new(big.Rat).Mul(a[0], b[0]),
		new(big.Rat).Add(new(big.Rat).Mul(a[1], b[1]), new(big.Rat).Mul(a[2], b[2])),
	)
}

func ratCross(a, b ratVec3) ratVec3 {
	return ratVec3{
		new(big.Rat).Sub(new(big.Rat).Mul(a[1], b[2]), new(big.Rat).Mul(a[2], b[1])),
		new(big.Rat).Sub(new(big.Rat).Mul(a[2], b[0]), new(big.Rat).Mul(a[0], b[2])),
		new(big.Rat).Sub(new(big.Rat).Mul(a[0], b[1]), new(big.Rat).Mul(a[1], b[0])),
	}
}

func ratMax(a, b *big.Rat) *big.Rat {
	if a.Cmp(b) >= 0 {
		return a
	}
	return b
}

func ratMin(a, b *big.Rat) *big.Rat {
	if a.Cmp(b) <= 0 {
		return a
	}
	return b
}

// rayAABBInterval returns the raw slab interval. A nil upper bound means +Inf.
func rayAABBInterval(o, d Vec3, b AABB) (*big.Rat, *big.Rat, bool) {
	lo := big.NewRat(0, 1)
	var hi *big.Rat
	origins := [3]Fixed{o.X, o.Y, o.Z}
	dirs := [3]Fixed{d.X, d.Y, d.Z}
	mins := [3]Fixed{b.Min.X, b.Min.Y, b.Min.Z}
	maxs := [3]Fixed{b.Max.X, b.Max.Y, b.Max.Z}
	for axis := 0; axis < 3; axis++ {
		ov, dv := int64(origins[axis]), int64(dirs[axis])
		minv, maxv := int64(mins[axis]), int64(maxs[axis])
		if dv == 0 {
			if ov < minv || ov > maxv {
				return nil, nil, false
			}
			continue
		}
		t1 := new(big.Rat).SetFrac(big.NewInt(minv-ov), big.NewInt(dv))
		t2 := new(big.Rat).SetFrac(big.NewInt(maxv-ov), big.NewInt(dv))
		if t1.Cmp(t2) > 0 {
			t1, t2 = t2, t1
		}
		lo = ratMax(lo, t1)
		if hi == nil {
			hi = t2
		} else {
			hi = ratMin(hi, t2)
		}
		if hi != nil && lo.Cmp(hi) > 0 {
			return nil, nil, false
		}
	}
	if hi != nil && hi.Sign() < 0 {
		return nil, nil, false
	}
	return lo, hi, true
}

type rootInterval struct {
	lo *big.Rat
	hi *big.Rat
}

func raySphereFirstInterval(o, d, c Vec3, radius Fixed) (rootInterval, bool) {
	ox := big.NewInt(int64(o.X) - int64(c.X))
	oy := big.NewInt(int64(o.Y) - int64(c.Y))
	oz := big.NewInt(int64(o.Z) - int64(c.Z))
	dx := big.NewInt(int64(d.X))
	dy := big.NewInt(int64(d.Y))
	dz := big.NewInt(int64(d.Z))
	r := big.NewInt(int64(radius))

	aa := new(big.Int).Add(new(big.Int).Mul(dx, dx), new(big.Int).Add(new(big.Int).Mul(dy, dy), new(big.Int).Mul(dz, dz)))
	if aa.Sign() == 0 {
		return rootInterval{}, false
	}
	h := new(big.Int).Add(new(big.Int).Mul(ox, dx), new(big.Int).Add(new(big.Int).Mul(oy, dy), new(big.Int).Mul(oz, dz)))
	cc := new(big.Int).Add(new(big.Int).Mul(ox, ox), new(big.Int).Add(new(big.Int).Mul(oy, oy), new(big.Int).Mul(oz, oz)))
	r2 := new(big.Int).Mul(r, r)
	cc.Sub(cc, r2)
	disc := new(big.Int).Sub(new(big.Int).Mul(h, h), new(big.Int).Mul(aa, cc))
	if disc.Sign() < 0 {
		return rootInterval{}, false
	}
	sf, sc := sqrtFloorCeil(disc)
	negH := new(big.Int).Neg(h)
	nearLo := new(big.Rat).SetFrac(new(big.Int).Sub(negH, sc), aa)
	nearHi := new(big.Rat).SetFrac(new(big.Int).Sub(negH, sf), aa)
	farLo := new(big.Rat).SetFrac(new(big.Int).Add(negH, sf), aa)
	farHi := new(big.Rat).SetFrac(new(big.Int).Add(negH, sc), aa)

	if nearHi.Sign() < 0 {
		if farHi.Sign() < 0 {
			return rootInterval{}, false
		}
		return rootInterval{lo: farLo, hi: farHi}, true
	}
	if nearLo.Sign() <= 0 && nearHi.Sign() >= 0 {
		return rootInterval{lo: big.NewRat(0, 1), hi: nearHi}, true
	}
	if nearLo.Sign() >= 0 {
		return rootInterval{lo: nearLo, hi: nearHi}, true
	}
	return rootInterval{lo: farLo, hi: farHi}, farHi.Sign() >= 0
}

func sqrtFloorCeil(v *big.Int) (*big.Int, *big.Int) {
	floor := new(big.Int).Sqrt(v)
	var sq big.Int
	sq.Mul(floor, floor)
	ceil := new(big.Int).Add(new(big.Int).Set(floor), big.NewInt(1))
	if sq.Cmp(v) == 0 {
		ceil.Set(floor)
	}
	return floor, ceil
}

type triangleEval struct {
	hit      bool
	parallel bool
	t        *big.Rat
	u        *big.Rat
	v        *big.Rat
	det      *big.Rat
}

func rayTriangleEval(o, d, a, b, c Vec3) triangleEval {
	or, dr, ar, br, cr := ratVec(o), ratVec(d), ratVec(a), ratVec(b), ratVec(c)
	e1 := ratSub(br, ar)
	e2 := ratSub(cr, ar)
	p := ratCross(dr, e2)
	det := ratDot(e1, p)
	if det.Sign() == 0 {
		return triangleEval{parallel: true, det: det}
	}
	tv := ratSub(or, ar)
	u := new(big.Rat).Quo(ratDot(tv, p), det)
	q := ratCross(tv, e1)
	v := new(big.Rat).Quo(ratDot(dr, q), det)
	t := new(big.Rat).Quo(ratDot(e2, q), det)

	zero := big.NewRat(0, 1)
	one := big.NewRat(1, 1)
	sumUV := new(big.Rat).Add(u, v)
	inside := u.Cmp(zero) >= 0 && u.Cmp(one) <= 0 &&
		v.Cmp(zero) >= 0 && v.Cmp(one) <= 0 &&
		sumUV.Cmp(one) <= 0 && t.Sign() >= 0
	return triangleEval{hit: inside, t: t, u: u, v: v, det: det}
}
