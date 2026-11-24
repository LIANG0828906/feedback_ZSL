package editorpick

import (
	"math/big"
)

func (e *Engine) makeCandidateLocked(q RayQuery, nodeID string, geometryIndex int, pick localPick) (candidate, bool) {
	d := e.derived[nodeID]
	if d == nil || pick.point[0] == nil {
		return candidate{}, false
	}
	localPoint := quantizeRatVec(pick.point)
	worldPoint, err := d.World.TransformPoint(localPoint)
	if err != nil {
		return candidate{}, false
	}
	tWorld := worldParameter(q.Origin, q.Dir, worldPoint)
	boundUnits := worldBoundUnits(d, localPoint, pick.boundUnits)

	diff := worldPoint.sub(q.Origin)
	distanceUnits := upperSqrtBig(squaresBig(diff.X, diff.Y, diff.Z))
	den := bigDot(bigVecFromFixed(q.Dir), bigVecFromFixed(q.Dir))
	dirFloor := new(big.Int).Sqrt(den)
	tLower := new(big.Rat).Set(tWorld)
	tUpper := new(big.Rat).Set(tWorld)
	if dirFloor.Sign() > 0 && boundUnits > 0 {
		slack := new(big.Rat).SetFrac(big.NewInt(boundUnits), dirFloor)
		tLower = new(big.Rat).Sub(tWorld, slack)
		tUpper = new(big.Rat).Add(tWorld, slack)
	}
	return candidate{
		nodeID:        nodeID,
		geometryIndex: geometryIndex,
		status:        pick.status,
		worldT:        tWorld,
		tLower:        tLower,
		tUpper:        tUpper,
		worldPoint:    worldPoint,
		distance:      Fixed(distanceUnits),
		boundUnits:    boundUnits,
		local:         pick,
	}, true
}

func quantizeRatVec(p [3]*big.Rat) Vec3 {
	x, _ := fixedFromUnitRat(p[0])
	y, _ := fixedFromUnitRat(p[1])
	z, _ := fixedFromUnitRat(p[2])
	return Vec3{x, y, z}
}

func worldParameter(o, d, p Vec3) *big.Rat {
	ov, dv, pv := bigVecFromFixed(o), bigVecFromFixed(d), bigVecFromFixed(p)
	num := bigDot(bigSub(pv, ov), dv)
	den := bigDot(dv, dv)
	if den.Sign() == 0 {
		return big.NewRat(0, 1)
	}
	return new(big.Rat).SetFrac(num, den)
}

func worldBoundUnits(d *Derived, localPoint Vec3, localSlackUnits int64) int64 {
	pointErr := pointErrorUnits(d.World, d.MatrixError, localPoint)
	var maxRowNumerator int64
	for row := 0; row < 3; row++ {
		var rowSum int64
		for col := 0; col < 3; col++ {
			rowSum += absInt64(int64(d.World[row*4+col]))
		}
		if rowSum > maxRowNumerator {
			maxRowNumerator = rowSum
		}
	}
	var numerator big.Int
	numerator.Mul(big.NewInt(maxRowNumerator), big.NewInt(localSlackUnits))
	slack, _ := ceilQuo(&numerator, FixedScale)
	return pointErr + slack + 2
}

func (e *Engine) buildAmbiguityLocked(q RayQuery, c candidate) *Ambiguity {
	geomKind := "geometry"
	if c.geometryIndex >= 0 && c.geometryIndex < len(e.scene.Geometries) {
		geomKind = string(e.scene.Geometries[c.geometryIndex].Kind)
	}
	b := Fixed(c.boundUnits)
	return &Ambiguity{
		Code:          EAmbiguous,
		QueryID:       q.ID,
		NodeID:        c.nodeID,
		GeometryIndex: c.geometryIndex,
		Kind:          geomKind,
		Reason:        c.local.reason,
		Epsilon:       Fixed(c.local.epsilon),
		Margin:        Fixed(c.local.marginUnits),
		Point:         c.worldPoint,
		Lower:         Vec3{c.worldPoint.X - b, c.worldPoint.Y - b, c.worldPoint.Z - b},
		Upper:         Vec3{c.worldPoint.X + b, c.worldPoint.Y + b, c.worldPoint.Z + b},
	}
}
