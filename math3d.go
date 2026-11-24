package editorpick

import (
	"math"
	"math/big"
)

type Vec3 struct {
	X Fixed `json:"x"`
	Y Fixed `json:"y"`
	Z Fixed `json:"z"`
}

func Vec3FromFloat(x, y, z float64) Vec3 {
	return Vec3{X: FixedFromFloat(x), Y: FixedFromFloat(y), Z: FixedFromFloat(z)}
}

func (v Vec3) Float64() [3]float64 {
	return [3]float64{v.X.Float64(), v.Y.Float64(), v.Z.Float64()}
}

func (v Vec3) add(o Vec3) Vec3 { return Vec3{v.X + o.X, v.Y + o.Y, v.Z + o.Z} }
func (v Vec3) sub(o Vec3) Vec3 { return Vec3{v.X - o.X, v.Y - o.Y, v.Z - o.Z} }

func (v Vec3) mulScalar(s Fixed) (Vec3, error) {
	x, err := mulFixed(v.X, s)
	if err != nil {
		return Vec3{}, err
	}
	y, err := mulFixed(v.Y, s)
	if err != nil {
		return Vec3{}, err
	}
	z, err := mulFixed(v.Z, s)
	if err != nil {
		return Vec3{}, err
	}
	return Vec3{x, y, z}, nil
}

func dotFixed(a, b Vec3) (Fixed, error) {
	xx, err := mulFixed(a.X, b.X)
	if err != nil {
		return 0, err
	}
	yy, err := mulFixed(a.Y, b.Y)
	if err != nil {
		return 0, err
	}
	zz, err := mulFixed(a.Z, b.Z)
	if err != nil {
		return 0, err
	}
	return xx + yy + zz, nil
}

func crossFixed(a, b Vec3) (Vec3, error) {
	aybz, err := mulFixed(a.Y, b.Z)
	if err != nil {
		return Vec3{}, err
	}
	azby, err := mulFixed(a.Z, b.Y)
	if err != nil {
		return Vec3{}, err
	}
	azbx, err := mulFixed(a.Z, b.X)
	if err != nil {
		return Vec3{}, err
	}
	axbz, err := mulFixed(a.X, b.Z)
	if err != nil {
		return Vec3{}, err
	}
	axby, err := mulFixed(a.X, b.Y)
	if err != nil {
		return Vec3{}, err
	}
	aybx, err := mulFixed(a.Y, b.X)
	if err != nil {
		return Vec3{}, err
	}
	return Vec3{X: aybz - azby, Y: azbx - axbz, Z: axby - aybx}, nil
}

// Mat4 is a row-major 4x4 matrix in declared fixed units.
type Mat4 [16]Fixed

func IdentityMat4() Mat4 {
	var m Mat4
	one := Fixed(FixedScale)
	m[0], m[5], m[10], m[15] = one, one, one, one
	return m
}

func TranslationMat4(v Vec3) Mat4 {
	m := IdentityMat4()
	m[3], m[7], m[11] = v.X, v.Y, v.Z
	return m
}

func ScaleMat4(v Vec3) Mat4 {
	one := Fixed(FixedScale)
	return Mat4{v.X, 0, 0, 0, 0, v.Y, 0, 0, 0, 0, v.Z, 0, 0, 0, 0, one}
}

// AxisRotationMat4 builds a deterministic axis rotation. Trigonometry is only
// used to quantize the initial fixed matrix; all later products are fixed.
func AxisRotationMat4(axis string, angleRadians float64) Mat4 {
	c := FixedFromFloat(math.Cos(angleRadians))
	s := FixedFromFloat(math.Sin(angleRadians))
	switch axis {
	case "x":
		return Mat4{
			Fixed(FixedScale), 0, 0, 0,
			0, c, -s, 0,
			0, s, c, 0,
			0, 0, 0, Fixed(FixedScale),
		}
	case "y":
		return Mat4{
			c, 0, s, 0,
			0, Fixed(FixedScale), 0, 0,
			-s, 0, c, 0,
			0, 0, 0, Fixed(FixedScale),
		}
	default: // z is the editor sample's primary plane.
		return Mat4{
			c, -s, 0, 0,
			s, c, 0, 0,
			0, 0, Fixed(FixedScale), 0,
			0, 0, 0, Fixed(FixedScale),
		}
	}
}

func Mat4FromFloat64(a [16]float64) Mat4 {
	var m Mat4
	for i, v := range a {
		m[i] = FixedFromFloat(v)
	}
	return m
}

func (m Mat4) Mul(n Mat4) (Mat4, error) {
	var out Mat4
	for row := 0; row < 4; row++ {
		for col := 0; col < 4; col++ {
			var numerator big.Int
			for k := 0; k < 4; k++ {
				var term big.Int
				term.Mul(big.NewInt(int64(m[row*4+k])), big.NewInt(int64(n[k*4+col])))
				numerator.Add(&numerator, &term)
			}
			q, ok := roundedQuo(&numerator, FixedScale)
			if !ok {
				return Mat4{}, errOverflow
			}
			out[row*4+col] = Fixed(q)
		}
	}
	return out, nil
}

func (m Mat4) TransformPoint(p Vec3) (Vec3, error) {
	homogeneous := [4]int64{int64(p.X), int64(p.Y), int64(p.Z), FixedScale}
	var out Vec3
	values := [3]*Fixed{&out.X, &out.Y, &out.Z}
	for row := 0; row < 3; row++ {
		var numerator big.Int
		for col := 0; col < 4; col++ {
			var term big.Int
			term.Mul(big.NewInt(int64(m[row*4+col])), big.NewInt(homogeneous[col]))
			numerator.Add(&numerator, &term)
		}
		q, ok := roundedQuo(&numerator, FixedScale)
		if !ok {
			return Vec3{}, errOverflow
		}
		*values[row] = Fixed(q)
	}
	return out, nil
}

func (m Mat4) maxAbsUnit() int64 {
	var max int64
	for _, v := range m {
		a := int64(absFixed(v))
		if a > max {
			max = a
		}
	}
	return max
}

// Inverse computes the exact inverse of the quantized matrix with rational
// Gauss-Jordan elimination, then quantizes each result once.
func (m Mat4) Inverse() (Mat4, error) {
	var aug [4][8]*big.Rat
	for r := 0; r < 4; r++ {
		for c := 0; c < 4; c++ {
			aug[r][c] = big.NewRat(int64(m[r*4+c]), FixedScale)
		}
		for c := 4; c < 8; c++ {
			if c-4 == r {
				aug[r][c] = big.NewRat(1, 1)
			} else {
				aug[r][c] = big.NewRat(0, 1)
			}
		}
	}
	for col := 0; col < 4; col++ {
		pivot := col
		for pivot < 4 && aug[pivot][col].Sign() == 0 {
			pivot++
		}
		if pivot == 4 {
			return Mat4{}, errSingular
		}
		if pivot != col {
			aug[col], aug[pivot] = aug[pivot], aug[col]
		}
		pivotValue := new(big.Rat).Set(aug[col][col])
		for c := 0; c < 8; c++ {
			aug[col][c] = new(big.Rat).Quo(aug[col][c], pivotValue)
		}
		for r := 0; r < 4; r++ {
			if r == col || aug[r][col].Sign() == 0 {
				continue
			}
			factor := new(big.Rat).Set(aug[r][col])
			for c := 0; c < 8; c++ {
				sub := new(big.Rat).Mul(factor, aug[col][c])
				aug[r][c] = new(big.Rat).Sub(aug[r][c], sub)
			}
		}
	}
	var out Mat4
	for r := 0; r < 4; r++ {
		for c := 0; c < 4; c++ {
			v, err := fixedFromRat(aug[r][c+4])
			if err != nil {
				return Mat4{}, err
			}
			out[r*4+c] = v
		}
	}
	return out, nil
}

func fixedFromRat(r *big.Rat) (Fixed, error) {
	scaled := new(big.Rat).Mul(new(big.Rat).Set(r), big.NewRat(FixedScale, 1))
	return roundRatToFixedUnits(scaled)
}

// fixedFromUnitRat rounds a rational whose coordinates are already in fixed
// units (rather than real-valued coordinates) to the nearest Fixed value.
func fixedFromUnitRat(r *big.Rat) (Fixed, error) {
	return roundRatToFixedUnits(r)
}

func roundRatToFixedUnits(r *big.Rat) (Fixed, error) {
	num := r.Num()
	den := r.Denom()
	q, rem := new(big.Int).QuoRem(new(big.Int).Set(num), den, new(big.Int))
	twice := new(big.Int).Mul(rem, bigTwo)
	if twice.Cmp(den) >= 0 {
		q.Add(q, bigOne)
	}
	if !q.IsInt64() {
		return 0, errOverflow
	}
	return Fixed(q.Int64()), nil
}

type AABB struct {
	Min Vec3 `json:"min"`
	Max Vec3 `json:"max"`
}

func NewAABB(min, max Vec3) AABB { return AABB{Min: min, Max: max}.Normalized() }

func (b AABB) Normalized() AABB {
	out := b
	if out.Min.X > out.Max.X {
		out.Min.X, out.Max.X = out.Max.X, out.Min.X
	}
	if out.Min.Y > out.Max.Y {
		out.Min.Y, out.Max.Y = out.Max.Y, out.Min.Y
	}
	if out.Min.Z > out.Max.Z {
		out.Min.Z, out.Max.Z = out.Max.Z, out.Min.Z
	}
	return out
}

func (b AABB) Valid() bool {
	return b.Min.X <= b.Max.X && b.Min.Y <= b.Max.Y && b.Min.Z <= b.Max.Z
}

func (b AABB) Center() Vec3 {
	return Vec3{
		X: (b.Min.X + b.Max.X) / 2,
		Y: (b.Min.Y + b.Max.Y) / 2,
		Z: (b.Min.Z + b.Max.Z) / 2,
	}
}

func (b AABB) Extents() Vec3 {
	return Vec3{X: b.Max.X - b.Min.X, Y: b.Max.Y - b.Min.Y, Z: b.Max.Z - b.Min.Z}
}

func (b AABB) Union(o AABB) AABB {
	return AABB{
		Min: Vec3{X: minFixed(b.Min.X, o.Min.X), Y: minFixed(b.Min.Y, o.Min.Y), Z: minFixed(b.Min.Z, o.Min.Z)},
		Max: Vec3{X: maxFixed(b.Max.X, o.Max.X), Y: maxFixed(b.Max.Y, o.Max.Y), Z: maxFixed(b.Max.Z, o.Max.Z)},
	}
}

func (b AABB) Expand(amount Fixed) AABB {
	return AABB{
		Min: Vec3{b.Min.X - amount, b.Min.Y - amount, b.Min.Z - amount},
		Max: Vec3{b.Max.X + amount, b.Max.Y + amount, b.Max.Z + amount},
	}
}

type Sphere struct {
	Center Vec3  `json:"center"`
	Radius Fixed `json:"radius"`
}

var errSingular = domainStringError("matrix is singular")

type domainStringError string

func (e domainStringError) Error() string { return string(e) }
