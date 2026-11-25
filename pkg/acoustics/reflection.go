package acoustics

import (
	"math"
	"math/big"

	"roomacoustics/pkg/fixedpoint"
)

// ReflectionHit is one valid first-order reflection path.
type ReflectionHit struct {
	ReflectorID            string  `json:"reflector_id"`
	RoomID                 string  `json:"room_id"`
	InputPoint             Point   `json:"input_point"`
	ReflectionPoint        Point   `json:"reflection_point"`
	LocalLengthFixed       int64   `json:"local_length_fixed"`
	LocalLengthM           float64 `json:"local_length_m"`
	PrefixLengthFixed      int64   `json:"prefix_length_fixed,omitempty"`
	PathLengthFixed        int64   `json:"path_length_fixed"`
	PathLengthM            float64 `json:"path_length_m"`
	IncidenceAngleMicroRad int64   `json:"incidence_angle_micro_rad"`
	IncidenceAngleDeg      float64 `json:"incidence_angle_deg"`
	Absorption             float64 `json:"absorption"`
	SPLDB                  float64 `json:"spl_db"`
}

// SolveReflection performs the mirror-image construction for one reflector.
// A nil hit with nil error means the infinite-line intersection is outside
// either the mirror segment or the reflector's effective segment.
func SolveReflection(input, listener Point, ref Reflector) (*ReflectionHit, error) {
	s := fixedpoint.FromMeters(input.X, input.Y)
	l := fixedpoint.FromMeters(listener.X, listener.Y)
	a := fixedpoint.FromMeters(ref.A.X, ref.A.Y)
	b := fixedpoint.FromMeters(ref.B.X, ref.B.Y)
	d := fixedpoint.V{X: b.X - a.X, Y: b.Y - a.Y}
	denD := new(big.Int).Add(big.NewInt(d.X*d.X), big.NewInt(d.Y*d.Y))
	if denD.Sign() == 0 {
		return nil, nil
	}

	sa := fixedpoint.V{X: s.X - a.X, Y: s.Y - a.Y}
	tNum := big.NewInt(sa.X*d.X + sa.Y*d.Y)
	t := new(big.Rat).SetFrac(tNum, denD)

	fx := new(big.Rat).Add(big.NewRat(a.X, 1), new(big.Rat).Mul(t, big.NewRat(d.X, 1)))
	fy := new(big.Rat).Add(big.NewRat(a.Y, 1), new(big.Rat).Mul(t, big.NewRat(d.Y, 1)))
	sp := struct{ x, y *big.Rat }{
		x: new(big.Rat).Sub(new(big.Rat).Mul(big.NewRat(2, 1), fx), big.NewRat(s.X, 1)),
		y: new(big.Rat).Sub(new(big.Rat).Mul(big.NewRat(2, 1), fy), big.NewRat(s.Y, 1)),
	}
	eX := new(big.Rat).Sub(big.NewRat(l.X, 1), sp.x)
	eY := new(big.Rat).Sub(big.NewRat(l.Y, 1), sp.y)

	aSpX := new(big.Rat).Sub(big.NewRat(a.X, 1), sp.x)
	aSpY := new(big.Rat).Sub(big.NewRat(a.Y, 1), sp.y)
	// cross(A-Sp,d)
	numU := new(big.Rat).Sub(
		new(big.Rat).Mul(aSpX, big.NewRat(d.Y, 1)),
		new(big.Rat).Mul(aSpY, big.NewRat(d.X, 1)),
	)
	// cross(e,d)
	denU := new(big.Rat).Sub(
		new(big.Rat).Mul(eX, big.NewRat(d.Y, 1)),
		new(big.Rat).Mul(eY, big.NewRat(d.X, 1)),
	)
	if denU.Sign() == 0 {
		return nil, nil
	}
	u := new(big.Rat).Quo(numU, denU)
	if u.Cmp(big.NewRat(0, 1)) < 0 || u.Cmp(big.NewRat(1, 1)) > 0 {
		return nil, nil
	}

	rx := new(big.Rat).Add(sp.x, new(big.Rat).Mul(u, eX))
	ry := new(big.Rat).Add(sp.y, new(big.Rat).Mul(u, eY))
	rFixed := fixedpoint.V{X: fixedpoint.RoundRatToFixed(rx), Y: fixedpoint.RoundRatToFixed(ry)}

	ra := fixedpoint.V{X: rFixed.X - a.X, Y: rFixed.Y - a.Y}
	vRat := new(big.Rat).SetFrac(big.NewInt(fixedpoint.Dot(ra, d)), denD)
	roundingEps := big.NewRat(1, 1_000_000)
	if vRat.Cmp(new(big.Rat).Sub(big.NewRat(0, 1), roundingEps)) < 0 ||
		vRat.Cmp(new(big.Rat).Add(big.NewRat(1, 1), roundingEps)) > 0 {
		return nil, nil
	}

	localLength := fixedpoint.Distance(s, rFixed) + fixedpoint.Distance(rFixed, l)
	if localLength <= 0 {
		return nil, nil
	}
	angleMicro := incidenceAngleMicro(s, rFixed, d)
	rxM, ryM := rFixed.Meters()

	return &ReflectionHit{
		ReflectorID:            ref.ID,
		RoomID:                 ref.RoomID,
		InputPoint:             input,
		ReflectionPoint:        Point{X: rxM, Y: ryM},
		LocalLengthFixed:       localLength,
		LocalLengthM:           fixedToMeters(localLength),
		PathLengthFixed:        localLength,
		PathLengthM:            fixedToMeters(localLength),
		IncidenceAngleMicroRad: angleMicro,
		IncidenceAngleDeg:      float64(angleMicro) / float64(fixedpoint.Scale) * 180 / math.Pi,
		Absorption:             ref.Absorption,
	}, nil
}

// WithPrefixAndLevel applies door-chain prefix length and acoustic parameters.
func (h ReflectionHit) WithPrefixAndLevel(prefixFixed int64, powerDB float64) ReflectionHit {
	h.PrefixLengthFixed = prefixFixed
	h.PathLengthFixed = h.LocalLengthFixed + prefixFixed
	h.PathLengthM = fixedToMeters(h.PathLengthFixed)
	h.SPLDB = ExpectedSPL(powerDB, h.PathLengthFixed, h.Absorption)
	return h
}

func incidenceAngleMicro(source, reflection, d fixedpoint.V) int64 {
	toSource := fixedpoint.V{X: source.X - reflection.X, Y: source.Y - reflection.Y}
	dist := fixedpoint.Length(toSource)
	if dist == 0 {
		return 0
	}
	normal := fixedpoint.UnitNormal(d)
	dot := fixedpoint.AbsInt64(fixedpoint.Dot(toSource, normal))
	cosQ := fixedpoint.ClampInt64(dot/dist, 0, fixedpoint.Scale)
	angle := math.Acos(float64(cosQ) / float64(fixedpoint.Scale))
	return int64(math.Round(angle * float64(fixedpoint.Scale)))
}

// ExpectedSPL uses spherical distance attenuation and one-reflection
// absorption energy loss. d is fixed-point metres.
func ExpectedSPL(powerDB float64, pathLengthFixed int64, absorption float64) float64 {
	d := math.Max(float64(pathLengthFixed)/float64(fixedpoint.Scale), 1e-6)
	absorptionTerm := 10 * math.Log10(math.Max(1-absorption, 1e-12))
	return powerDB - 20*math.Log10(d) + absorptionTerm
}

// AggregateSPL sums incoherent reflection energies.
func AggregateSPL(levels []float64) *float64 {
	if len(levels) == 0 {
		return nil
	}
	max := levels[0]
	for _, v := range levels[1:] {
		if v > max {
			max = v
		}
	}
	sum := 0.0
	for _, v := range levels {
		sum += math.Pow(10, (v-max)/10)
	}
	out := max + 10*math.Log10(sum)
	return &out
}
