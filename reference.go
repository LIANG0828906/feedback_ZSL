package bonerig

import (
	"math/big"
)

type refQ struct{ w, x, y, z *big.Int }

func refScale() *big.Int { return new(big.Int).Lsh(big.NewInt(1), 40) }

func bigRoundDiv(n, d *big.Int) *big.Int {
	q, r := new(big.Int).QuoRem(new(big.Int).Set(n), d, new(big.Int))
	twoR := new(big.Int).Abs(r)
	twoR.Mul(twoR, big.NewInt(2))
	if twoR.Cmp(d) >= 0 {
		if r.Sign() < 0 {
			q.Sub(q, big.NewInt(1))
		} else {
			q.Add(q, big.NewInt(1))
		}
	}
	return q
}

func qToRef(q Quat, work, rs *big.Int) refQ {
	scale := big.NewInt(q.Scale)
	if q.Scale == 0 {
		scale = work
	}
	conv := func(v int64) *big.Int {
		return bigRoundDiv(new(big.Int).Mul(big.NewInt(v), rs), scale)
	}
	return refQ{w: conv(q.W), x: conv(q.X), y: conv(q.Y), z: conv(q.Z)}
}

func refNormalize(q refQ, rs *big.Int) refQ {
	sq := new(big.Int)
	for _, v := range []*big.Int{q.w, q.x, q.y, q.z} {
		sq.Add(sq, new(big.Int).Mul(v, v))
	}
	n := new(big.Int).Sqrt(sq)
	if n.Sign() == 0 {
		return refQ{w: new(big.Int).Set(rs), x: big.NewInt(0), y: big.NewInt(0), z: big.NewInt(0)}
	}
	norm := func(v *big.Int) *big.Int {
		return bigRoundDiv(new(big.Int).Mul(v, rs), n)
	}
	return refQ{w: norm(q.w), x: norm(q.x), y: norm(q.y), z: norm(q.z)}
}

func refMul(a, b refQ, rs *big.Int) refQ {
	mul := func(x, y *big.Int) *big.Int { return new(big.Int).Mul(x, y) }
	sub := func(args ...*big.Int) *big.Int {
		r := new(big.Int).Set(args[0])
		for _, v := range args[1:] {
			r.Sub(r, v)
		}
		return r
	}
	add := func(args ...*big.Int) *big.Int {
		r := new(big.Int)
		for _, v := range args {
			r.Add(r, v)
		}
		return r
	}
	div := func(v *big.Int) *big.Int { return bigRoundDiv(v, rs) }
	return refQ{
		w: div(sub(mul(a.w, b.w), mul(a.x, b.x), mul(a.y, b.y), mul(a.z, b.z))),
		x: div(add(mul(a.w, b.x), mul(a.x, b.w), mul(a.y, b.z), mul(a.z, new(big.Int).Neg(b.y)))),
		y: div(add(mul(a.w, b.y), mul(a.x, new(big.Int).Neg(b.z)), mul(a.y, b.w), mul(a.z, b.x))),
		z: div(add(mul(a.w, b.z), mul(a.x, b.y), mul(a.y, new(big.Int).Neg(b.x)), mul(a.z, b.w))),
	}
}

func refConj(q refQ) refQ {
	return refQ{w: new(big.Int).Set(q.w), x: new(big.Int).Neg(q.x), y: new(big.Int).Neg(q.y), z: new(big.Int).Neg(q.z)}
}

func refWeighted(qs []refQ, ws []int64, weightScale int64, rs *big.Int) refQ {
	sum := refQ{w: big.NewInt(0), x: big.NewInt(0), y: big.NewInt(0), z: big.NewInt(0)}
	for i, q := range qs {
		w := big.NewInt(ws[i])
		sum.w.Add(sum.w, new(big.Int).Mul(q.w, w))
		sum.x.Add(sum.x, new(big.Int).Mul(q.x, w))
		sum.y.Add(sum.y, new(big.Int).Mul(q.y, w))
		sum.z.Add(sum.z, new(big.Int).Mul(q.z, w))
	}
	den := big.NewInt(weightScale)
	return refQ{
		w: bigRoundDiv(sum.w, den), x: bigRoundDiv(sum.x, den),
		y: bigRoundDiv(sum.y, den), z: bigRoundDiv(sum.z, den),
	}
}

func refScaleVector(q refQ, num, den int64, rs *big.Int) refQ {
	n := big.NewInt(num)
	d := big.NewInt(den)
	scale := func(v *big.Int) *big.Int { return bigRoundDiv(new(big.Int).Mul(v, n), d) }
	return refNormalize(refQ{w: q.w, x: scale(q.x), y: scale(q.y), z: scale(q.z)}, rs)
}

func refToQuat(q refQ, work, rs *big.Int) Quat {
	conv := func(v *big.Int) int64 {
		return bigRoundDiv(new(big.Int).Mul(v, work), rs).Int64()
	}
	return Quat{W: conv(q.w), X: conv(q.x), Y: conv(q.y), Z: conv(q.z), Scale: work.Int64()}
}

// BuildReferenceTruth creates deterministic high-precision integer truth.
func BuildReferenceTruth(a Asset) TruthManifest {
	rs := refScale()
	work := big.NewInt(a.Precision.WorkScale)
	frames := make([]FrameTruth, 0, len(a.Clip.Frames))
	for _, f := range a.Clip.Frames {
		local := map[string]refQ{}
		world := map[string]refQ{}
		for _, j := range a.Skeleton.Joints {
			q := refNormalize(qToRef(f.Poses[j.ID].Rotation, work, rs), rs)
			local[j.ID] = q
			if j.ParentID == "" {
				world[j.ID] = q
			} else {
				world[j.ID] = refMul(world[j.ParentID], q, rs)
			}
		}
		targetWorld := map[string]refQ{}
		rots := map[string]Quat{}
		trans := map[string]Vec3{}
		byTarget := map[string]MapItem{}
		for _, m := range a.Mapping.Items {
			byTarget[m.TargetJoint] = m
		}
		for _, tj := range a.TargetSkeleton.Joints {
			m := byTarget[tj.ID]
			extra := int64(0)
			for _, inf := range m.ExtraSources {
				extra += inf.Weight
			}
			qs := []refQ{world[m.SourceJoint]}
			ws := []int64{m.WeightScale - extra}
			for _, inf := range m.ExtraSources {
				qs = append(qs, world[inf.SourceJoint])
				ws = append(ws, inf.Weight)
			}
			tw := refNormalize(refWeighted(qs, ws, m.WeightScale, rs), rs)
			targetWorld[tj.ID] = tw
			localTarget := tw
			if tj.ParentID != "" {
				localTarget = refMul(refConj(targetWorld[tj.ParentID]), tw, rs)
			}
			if m.RotScaleNum != m.RotScaleDen {
				localTarget = refScaleVector(localTarget, m.RotScaleNum, m.RotScaleDen, rs)
			} else {
				localTarget = refNormalize(localTarget, rs)
			}
			rots[tj.ID] = refToQuat(localTarget, work, rs)
			v, _ := retargetTranslation(a, m, f, m.SourceJoint, tj.ID)
			trans[tj.ID] = v
		}
		frames = append(frames, FrameTruth{Index: f.Index, TargetRotations: rots, TargetTrans: trans})
	}
	return TruthManifest{
		Seed: a.Seed, Frames: frames, Algorithm: AlgorithmVersion + "-bigint-v1",
		Tolerance: 8_000_000,
	}
}
