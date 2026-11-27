package grid

import (
	"fmt"
	"math"
	"math/big"
	"math/bits"
)

const declaredRounding = "truncate-toward-zero"

func addChecked(field string, a, b int64) (int64, error) {
	if (b > 0 && a > math.MaxInt64-b) || (b < 0 && a < math.MinInt64-b) {
		return 0, overflowError(field, a, b)
	}
	return a + b, nil
}

func subChecked(field string, a, b int64) (int64, error) {
	if b == math.MinInt64 {
		if a >= 0 {
			return 0, overflowError(field, a, b)
		}
	} else {
		if (b > 0 && a < math.MinInt64+b) || (b < 0 && a > math.MaxInt64+b) {
			return 0, overflowError(field, a, b)
		}
	}
	return a - b, nil
}

func mulChecked(field string, a, b int64) (int64, error) {
	product := new(big.Int).Mul(big.NewInt(a), big.NewInt(b))
	if !product.IsInt64() {
		return 0, overflowError(field, a, b)
	}
	return product.Int64(), nil
}

// MMToUnits converts a schema millimetre field to internal micrometres.
func MMToUnits(objectID, field string, mm int64) (int64, error) {
	v, err := mulChecked(field, mm, PositionScale)
	if err != nil {
		ge := err.(*Error)
		ge.ObjectID = objectID
		ge.Field = field
		return 0, ge
	}
	return v, nil
}

// UnitsToMM converts internal units to millimetres. Non-declared sub-mm values
// are truncated toward zero and every non-zero remainder is traced.
func UnitsToMM(objectID, field string, units int64, traces *[]FieldTrace) int64 {
	mm := units / PositionScale
	rem := units % PositionScale
	if rem != 0 && traces != nil {
		*traces = append(*traces, FieldTrace{
			Kind: "truncation", ObjectID: objectID, Field: field,
			Position: "units->mm", OldValue: fmt.Sprint(units), NewValue: fmt.Sprint(mm),
			Rounding: declaredRounding, Numerator: units, Denominator: PositionScale, Remainder: rem,
		})
	}
	return mm
}

func PermilleToBPS(objectID, field string, permille int64) (int64, error) {
	v, err := mulChecked(field, permille, 10)
	if err != nil {
		ge := err.(*Error)
		ge.ObjectID = objectID
		ge.Field = field
		return 0, ge
	}
	return v, nil
}

func BPSToPermille(objectID, field string, bps int64, traces *[]FieldTrace) int64 {
	v := bps / 10
	rem := bps % 10
	if rem != 0 && traces != nil {
		*traces = append(*traces, FieldTrace{
			Kind: "truncation", ObjectID: objectID, Field: field,
			Position: "bps->permille", OldValue: fmt.Sprint(bps), NewValue: fmt.Sprint(v),
			Rounding: declaredRounding, Numerator: bps, Denominator: 10, Remainder: rem,
		})
	}
	return v
}

func TicksToMS(objectID, field string, ticks int64) (int64, error) {
	v, err := mulChecked(field, ticks, TickMilliseconds)
	if err != nil {
		ge := err.(*Error)
		ge.ObjectID = objectID
		ge.Field = field
		return 0, ge
	}
	return v, nil
}

func MSToTicks(objectID, field string, ms int64, traces *[]FieldTrace) int64 {
	v := ms / TickMilliseconds
	rem := ms % TickMilliseconds
	if rem != 0 && traces != nil {
		*traces = append(*traces, FieldTrace{
			Kind: "truncation", ObjectID: objectID, Field: field,
			Position: "ms->tick", OldValue: fmt.Sprint(ms), NewValue: fmt.Sprint(v),
			Rounding: declaredRounding, Numerator: ms, Denominator: TickMilliseconds, Remainder: rem,
		})
	}
	return v
}

// EndpointDeltas are the v3 integer encoding: anchor relative to the previous
// obstacle's A endpoint and B relative to A.
type EndpointDeltas struct {
	Anchor   Vec2
	Endpoint Vec2
}

func AbsoluteToDeltas(o Obstacle, previousA *Point, fieldPrefix string) (EndpointDeltas, []FieldTrace, error) {
	var traces []FieldTrace
	base := Point{}
	if previousA != nil {
		base = *previousA
	}
	ax, err := subChecked(fieldPrefix+".anchorDelta.x", o.A.X, base.X)
	if err != nil {
		return EndpointDeltas{}, nil, withObject(err, o.ID)
	}
	ay, err := subChecked(fieldPrefix+".anchorDelta.y", o.A.Y, base.Y)
	if err != nil {
		return EndpointDeltas{}, nil, withObject(err, o.ID)
	}
	ex, err := subChecked(fieldPrefix+".endpointDelta.x", o.B.X, o.A.X)
	if err != nil {
		return EndpointDeltas{}, nil, withObject(err, o.ID)
	}
	ey, err := subChecked(fieldPrefix+".endpointDelta.y", o.B.Y, o.A.Y)
	if err != nil {
		return EndpointDeltas{}, nil, withObject(err, o.ID)
	}
	return EndpointDeltas{Anchor: Vec2{X: ax, Y: ay}, Endpoint: Vec2{X: ex, Y: ey}}, traces, nil
}

func DeltasToAbsolute(id, fieldPrefix string, deltas []EndpointDeltas) ([]Obstacle, error) {
	out := make([]Obstacle, len(deltas))
	var prevA Point
	for i, d := range deltas {
		a, err := addChecked(fieldPrefix+".anchorDelta.x", prevA.X, d.Anchor.X)
		if err != nil {
			return nil, withObject(err, id)
		}
		ay, err := addChecked(fieldPrefix+".anchorDelta.y", prevA.Y, d.Anchor.Y)
		if err != nil {
			return nil, withObject(err, id)
		}
		bx, err := addChecked(fieldPrefix+".endpointDelta.x", a, d.Endpoint.X)
		if err != nil {
			return nil, withObject(err, id)
		}
		by, err := addChecked(fieldPrefix+".endpointDelta.y", ay, d.Endpoint.Y)
		if err != nil {
			return nil, withObject(err, id)
		}
		out[i].A = Point{X: a, Y: ay}
		out[i].B = Point{X: bx, Y: by}
		prevA = Point{X: a, Y: ay}
	}
	return out, nil
}

func withObject(err error, id string) error {
	if ge, ok := err.(*Error); ok && ge.ObjectID == "" {
		ge.ObjectID = id
	}
	return err
}

// RightNormal returns the declared default face: the right-hand normal of the
// directed A->B segment, rounded to NormalScale fixed-point units.
func RightNormal(a, b Vec2) Vec2 {
	dx := b.X - a.X
	dy := b.Y - a.Y
	// Right normal in screen coordinates (y down): (dy, -dx).
	nx := dy
	ny := -dx
	length := isqrt64(nx*nx + ny*ny) // products are safe for generated coordinates
	if length == 0 {
		return Vec2{X: 0, Y: -NormalScale}
	}
	return Vec2{
		X: (nx*NormalScale + length/2) / length,
		Y: (ny*NormalScale + length/2) / length,
	}
}

func isqrt64(v int64) int64 {
	if v <= 0 {
		return 0
	}
	n := uint64(v)
	x := int64(1) << uint((bits.Len64(n)+1)/2)
	for {
		y := (x + v/x) / 2
		if y >= x {
			return x
		}
		x = y
	}
}

// NormalUnitValid checks the declared 5 permille unit-length tolerance.
func NormalUnitValid(n Vec2) bool {
	const minPermille = 995
	const maxPermille = 1005
	lo := NormalScale * minPermille / 1000
	hi := NormalScale * maxPermille / 1000
	sq := n.X*n.X + n.Y*n.Y
	return sq >= lo*lo && sq <= hi*hi
}

// NormalMatchesSegment checks perpendicularity with a small fixed tolerance.
func NormalMatchesSegment(a, b, n Vec2) bool {
	dx := b.X - a.X
	dy := b.Y - a.Y
	dot := dx*n.X + dy*n.Y
	if dot < 0 {
		dot = -dot
	}
	manhattan := abs64(dx) + abs64(dy)
	return dot <= manhattan*12
}

func abs64(v int64) int64 {
	if v < 0 {
		return -v
	}
	return v
}
