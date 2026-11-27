package stage

import (
	"fmt"
	"math"
	"sort"
)

func validateSampling(cfg SamplingConfig, fixtureID string) error {
	if len(cfg.DistancesMM) == 0 {
		return &Error{Code: CodeValue, Message: "sampling distances are empty", Field: "sampling.distances_mm", FixtureID: fixtureID}
	}
	for _, d := range cfg.DistancesMM {
		if d <= 0 {
			return &Error{Code: CodeValue, Message: "sampling distance must be positive", Field: "sampling.distances_mm", FixtureID: fixtureID}
		}
	}
	if cfg.Density < 1 || cfg.Density > 64 {
		return &Error{
			Code: CodeValue, Message: "sampling density outside [1,64]",
			Field: "sampling.density", FixtureID: fixtureID, Actual: cfg.Density,
		}
	}
	if cfg.AngularSegments < 4 || cfg.AngularSegments > 360 {
		return &Error{
			Code: CodeValue, Message: "angular segments outside [4,360]",
			Field: "sampling.angular_segments", FixtureID: fixtureID, Actual: cfg.AngularSegments,
		}
	}
	if cfg.CellMM < 1 || cfg.CellMM > 10_000 {
		return &Error{
			Code: CodeValue, Message: "accumulation cell outside [1,10000] mm",
			Field: "sampling.cell_mm", FixtureID: fixtureID, Actual: cfg.CellMM,
		}
	}
	if cfg.AccumulationBits < 1 || cfg.AccumulationBits > 64 {
		return &Error{
			Code: CodeValue, Message: "accumulation precision outside [1,64] bits",
			Field: "sampling.accumulation_bits", FixtureID: fixtureID, Actual: cfg.AccumulationBits,
		}
	}
	return nil
}

// SampleFixture constructs cross-sections of the cone at declared distances.
// Inner samples are exact; the inner/outer transition band is explicitly
// uncertain with [25%, 100%] intensity bounds. Points are de-duplicated and
// sorted before accumulation.
func SampleFixture(pose FixturePose, level int64, rgb Vec3, cfg SamplingConfig) ([]LightPoint, error) {
	if err := validateSampling(cfg, pose.FixtureID); err != nil {
		return nil, err
	}
	dir := [3]float64{pose.Direction[0], pose.Direction[1], pose.Direction[2]}
	if norm2(dir) < 1e-24 {
		return nil, &Error{Code: CodeParam, Message: "zero direction vector", Field: "direction", FixtureID: pose.FixtureID}
	}
	dir = normalize(dir)
	right, up := basis(dir)
	pos := [3]float64{float64(pose.PositionMM[0]), float64(pose.PositionMM[1]), float64(pose.PositionMM[2])}
	innerRad := float64(pose.InnerHalfMicroDeg) / 1_000_000 * math.Pi / 180
	outerRad := float64(pose.OuterHalfMicroDeg) / 1_000_000 * math.Pi / 180
	if pose.InnerHalfMicroDeg > pose.OuterHalfMicroDeg {
		return nil, &Error{Code: CodeParam, Message: "inner cone angle exceeds outer", Field: "inner_half_microdeg", FixtureID: pose.FixtureID}
	}
	if outerRad >= math.Pi/2 {
		return nil, &Error{Code: CodeValue, Message: "outer half angle cannot be sampled at or beyond 90 degrees", Field: "outer_half_microdeg", FixtureID: pose.FixtureID}
	}

	seen := make(map[string]struct{})
	points := make([]LightPoint, 0, len(cfg.DistancesMM)*(1+cfg.Density*cfg.AngularSegments))
	add := func(distance int64, cx, cy, cz float64) error {
		coord := Vec3{roundInt64(cx), roundInt64(cy), roundInt64(cz)}
		dx := float64(coord[0]) - pos[0]
		dy := float64(coord[1]) - pos[1]
		dz := float64(coord[2]) - pos[2]
		if dx == 0 && dy == 0 && dz == 0 {
			return &Error{
				Code: CodeValue, Message: "sample point coincides with light position",
				Field: "sampling.distances_mm", FixtureID: pose.FixtureID,
				Coordinate: []int64{coord[0], coord[1], coord[2]},
			}
		}
		key := fmt.Sprintf("%d:%d,%d,%d", distance, coord[0], coord[1], coord[2])
		if _, ok := seen[key]; ok {
			return nil
		}
		seen[key] = struct{}{}
		// The cross-section is a 3D disc; use the full transverse offset to
		// include the local up/right components.
		off := [3]float64{cx - (pos[0] + dir[0]*float64(distance)), cy - (pos[1] + dir[1]*float64(distance)), cz - (pos[2] + dir[2]*float64(distance))}
		// Classify from the exact unrounded radius, not the rounded coordinate,
		// so uncertainty marking is stable and explainable.
		radial := math.Sqrt(off[0]*off[0] + off[1]*off[1] + off[2]*off[2])
		innerRadius := math.Tan(innerRad) * float64(distance)
		outerRadius := math.Tan(outerRad) * float64(distance)
		uncertain := radial > innerRadius
		intensity := level
		lower, upper := level, level
		if uncertain {
			lower = level / 4
			upper = level
			intensity = (lower + upper) / 2
		}
		if radial > outerRadius+0.5 {
			return nil
		}
		contrib := [3]int64{}
		for ch := 0; ch < 3; ch++ {
			v, _, err := ScaleInt(intensity, rgb[ch], 10_000, RoundTowardZero, "sample.color_contribution")
			if err != nil {
				return err
			}
			contrib[ch] = v
		}
		points = append(points, LightPoint{
			Key: key, CoordinateMM: coord, DistanceMM: distance,
			Intensity: intensity, LowerBound: lower, UpperBound: upper,
			ColorContribution: contrib, Uncertain: uncertain,
		})
		return nil
	}

	for _, distance := range cfg.DistancesMM {
		if distance == 0 {
			return nil, &Error{Code: CodeValue, Message: "sample distance is zero", Field: "sampling.distances_mm", FixtureID: pose.FixtureID}
		}
		center := [3]float64{
			pos[0] + dir[0]*float64(distance),
			pos[1] + dir[1]*float64(distance),
			pos[2] + dir[2]*float64(distance),
		}
		if err := add(distance, center[0], center[1], center[2]); err != nil {
			return nil, err
		}
		outerRadius := math.Tan(outerRad) * float64(distance)
		for ring := 1; ring <= cfg.Density; ring++ {
			r := outerRadius * float64(ring) / float64(cfg.Density)
			for seg := 0; seg < cfg.AngularSegments; seg++ {
				a := 2 * math.Pi * float64(seg) / float64(cfg.AngularSegments)
				x := center[0] + right[0]*r*math.Cos(a) + up[0]*r*math.Sin(a)
				y := center[1] + right[1]*r*math.Cos(a) + up[1]*r*math.Sin(a)
				z := center[2] + right[2]*r*math.Cos(a) + up[2]*r*math.Sin(a)
				if err := add(distance, x, y, z); err != nil {
					return nil, err
				}
			}
		}
	}
	sort.Slice(points, func(i, j int) bool {
		if points[i].DistanceMM != points[j].DistanceMM {
			return points[i].DistanceMM < points[j].DistanceMM
		}
		for k := 0; k < 3; k++ {
			if points[i].CoordinateMM[k] != points[j].CoordinateMM[k] {
				return points[i].CoordinateMM[k] < points[j].CoordinateMM[k]
			}
		}
		return points[i].Key < points[j].Key
	})
	return points, nil
}

func basis(dir [3]float64) ([3]float64, [3]float64) {
	helper := [3]float64{0, 0, 1}
	if math.Abs(dir[2]) > 0.9 {
		helper = [3]float64{0, 1, 0}
	}
	right := [3]float64{
		helper[1]*dir[2] - helper[2]*dir[1],
		helper[2]*dir[0] - helper[0]*dir[2],
		helper[0]*dir[1] - helper[1]*dir[0],
	}
	right = normalize(right)
	up := [3]float64{
		right[1]*dir[2] - right[2]*dir[1],
		right[2]*dir[0] - right[0]*dir[2],
		right[0]*dir[1] - right[1]*dir[0],
	}
	return right, normalize(up)
}

func roundInt64(v float64) int64 {
	if v >= 0 {
		return int64(math.Floor(v + 0.5))
	}
	return int64(math.Ceil(v - 0.5))
}

func aggregatePoints(samples []FixtureSample, cfg SamplingConfig) []AggregatePoint {
	step := accumulationStep(cfg.AccumulationBits)
	type acc struct {
		intensity  int64
		lower      int64
		upper      int64
		color      [3]int64
		uncertain  bool
		people     []string
		coordinate Vec3
	}
	cells := make(map[Vec3]*acc)
	for _, sample := range samples {
		for _, point := range sample.Points {
			cell := floorCell(point.CoordinateMM, cfg.CellMM)
			a := cells[cell]
			if a == nil {
				a = &acc{coordinate: point.CoordinateMM}
				cells[cell] = a
			}
			a.intensity += point.Intensity
			a.lower += point.LowerBound
			a.upper += point.UpperBound
			for ch := 0; ch < 3; ch++ {
				a.color[ch] += point.ColorContribution[ch]
			}
			a.uncertain = a.uncertain || point.Uncertain
			a.people = append(a.people, sample.FixtureID)
		}
	}
	keys := make([]Vec3, 0, len(cells))
	for cell := range cells {
		keys = append(keys, cell)
	}
	sort.Slice(keys, func(i, j int) bool {
		for k := 0; k < 3; k++ {
			if keys[i][k] != keys[j][k] {
				return keys[i][k] < keys[j][k]
			}
		}
		return false
	})
	out := make([]AggregatePoint, 0, len(keys))
	for _, cell := range keys {
		a := cells[cell]
		out = append(out, AggregatePoint{
			Cell: cell, CoordinateMM: a.coordinate, Intensity: quantizeDown(a.intensity, step),
			LowerBound: quantizeDown(a.lower, step), UpperBound: quantizeDown(a.upper, step),
			ColorContribution: [3]int64{
				quantizeDown(a.color[0], step), quantizeDown(a.color[1], step), quantizeDown(a.color[2], step),
			},
			Uncertain: a.uncertain, Contributors: sortedUnique(a.people),
		})
	}
	return out
}

// accumulationStep maps declared effective levels over the 0..10000 internal
// intensity/chroma scale to an integer quantum. 32-bit sampling therefore keeps
// step=1, while the 12-bit preview intentionally coarsens overlap totals.
func accumulationStep(bits int) int64 {
	if bits >= 20 {
		return 1
	}
	levels := int64(1) << uint(bits)
	step := (10_000 + levels - 1) / levels
	if step < 1 {
		step = 1
	}
	return step
}

func quantizeDown(v, step int64) int64 {
	if step <= 1 {
		return v
	}
	return floorDiv(v, step) * step
}

func floorCell(v Vec3, cell int64) Vec3 {
	return Vec3{floorDiv(v[0], cell), floorDiv(v[1], cell), floorDiv(v[2], cell)}
}

func floorDiv(a, b int64) int64 {
	q := a / b
	if a%b != 0 && ((a < 0) != (b < 0)) {
		q--
	}
	return q
}
