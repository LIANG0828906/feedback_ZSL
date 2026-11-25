package maskqa

import "math"

const (
	DirE  = "east"
	DirSE = "south_east"
	DirS  = "south"
	DirSW = "south_west"
	DirW  = "west"
	DirNW = "north_west"
	DirN  = "north"
	DirNE = "north_east"
)

// DirectionTables contains the three source encoding vocabularies.
var DirectionTables = map[string]map[string]string{
	"arrow8_legacy_v1": {
		"1": DirE, "2": DirS, "3": DirW, "4": DirN,
		"5": DirSE, "6": DirSW, "7": DirNW, "8": DirNE,
	},
	"compass_clock_v2": {
		"E": DirE, "0": DirE,
		"SE": DirSE, "1": DirSE,
		"S": DirS, "2": DirS,
		"SW": DirSW, "3": DirSW,
		"W": DirW, "4": DirW,
		"NW": DirNW, "5": DirNW,
		"N": DirN, "6": DirN,
		"NE": DirNE, "7": DirNE,
	},
	"canonical_snake_v3": {
		DirE: DirE, DirSE: DirSE, DirS: DirS, DirSW: DirSW,
		DirW: DirW, DirNW: DirNW, DirN: DirN, DirNE: DirNE,
		"south-east": DirSE, "south-west": DirSW,
		"north-west": DirNW, "north-east": DirNE,
	},
}

var categoryAngles = map[string]float64{
	DirE: 0, DirSE: 45, DirS: 90, DirSW: 135,
	DirW: 180, DirNW: -135, DirN: -90, DirNE: -45,
}

func CategoryAngle(category string) float64 {
	return categoryAngles[category]
}

// DecodeDirection maps a source direction code through its declared table.
func DecodeDirection(version, table, code, path string) (string, error) {
	tbl, ok := DirectionTables[table]
	if !ok {
		return "", schemaError(version, path, "unknown direction table %q", table)
	}
	cat, ok := tbl[code]
	if !ok {
		return "", schemaError(version, path, "direction code %q is not in table %s", code, table)
	}
	return cat, nil
}

// EncodeDirection maps canonical category to a source table code.
func EncodeDirection(table, category string) (string, error) {
	for code, cat := range DirectionTables[table] {
		if cat == category && !isAliasCode(table, code) {
			return code, nil
		}
	}
	return "", paramError("direction_table", table, "category %q cannot be encoded by %s", category, table)
}

func isAliasCode(table, code string) bool {
	if table == "compass_clock_v2" {
		switch code {
		case "0", "1", "2", "3", "4", "5", "6", "7":
			return true
		}
	}
	if table == "canonical_snake_v3" {
		switch code {
		case "south-east", "south-west", "north-west", "north-east":
			return true
		}
	}
	return false
}

// AngleFromVector returns atan2(dy,dx) in degrees in image coordinates.
func AngleFromVector(dx, dy float64) float64 {
	return math.Atan2(dy, dx) * 180 / math.Pi
}

// CategoryFromAngle snaps an angle to the nearest eight-wind category.
func CategoryFromAngle(angle float64) string {
	a := math.Mod(angle, 360)
	if a < 0 {
		a += 360
	}
	idx := int(math.Round(a/45)) % 8
	return []string{DirE, DirSE, DirS, DirSW, DirW, DirNW, DirN, DirNE}[idx]
}

// OrientAngleToExpected resolves PCA's +/-180 ambiguity by choosing the equivalent angle closest to expected.
func OrientAngleToExpected(theta, expected float64) float64 {
	best := theta
	bestDist := math.Abs(AngleDistanceDeg(theta, expected))
	for _, candidate := range []float64{theta + 180, theta - 180} {
		d := math.Abs(AngleDistanceDeg(candidate, expected))
		if d < bestDist {
			bestDist = d
			best = candidate
		}
	}
	return best
}

// AngleDistanceDeg returns the signed shortest angular distance a-b in degrees.
func AngleDistanceDeg(a, b float64) float64 {
	d := math.Mod(a-b+180, 360)
	if d < 0 {
		d += 360
	}
	return d - 180
}

func strokeAngle(s Stroke) float64 {
	return AngleFromVector(float64(s.End.X-s.Start.X), float64(s.End.Y-s.Start.Y))
}

// SideProbe moves the segment midpoint to one of the two boundary sides.
func SideProbe(s Stroke, sideSign, offsetPx int) Point {
	mx := float64(s.Start.X+s.End.X) / 2.0
	my := float64(s.Start.Y+s.End.Y) / 2.0
	dx := float64(s.End.X - s.Start.X)
	dy := float64(s.End.Y - s.Start.Y)
	length := math.Hypot(dx, dy)
	if length == 0 {
		return Point{X: int(math.Round(mx)), Y: int(math.Round(my))}
	}
	nx := -dy / length
	ny := dx / length
	return Point{
		X: int(math.Round(mx + float64(sideSign)*nx*float64(offsetPx))),
		Y: int(math.Round(my + float64(sideSign)*ny*float64(offsetPx))),
	}
}
