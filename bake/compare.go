package bake

import (
	"fmt"
	"math"

	"terrainbake/model"
)

// Diff identifies the first exact discrepancy between two bake paths.
type Diff struct {
	ChunkID string             `json:"chunk_id,omitempty"`
	Time    int                `json:"time,omitempty"`
	Vertex  *model.VertexPoint `json:"vertex,omitempty"`
	World   *model.Vec2        `json:"world,omitempty"`
	Field   string             `json:"field"`
	Left    string             `json:"left"`
	Right   string             `json:"right"`
}

// DiffReport is the deterministic first-mismatch result.
type DiffReport struct {
	Equal bool  `json:"equal"`
	First *Diff `json:"first,omitempty"`
}

// CompareResults verifies commit sequence and per-point shadow, sky
// visibility, and fixed irradiance equality.
func CompareResults(left, right *BakeResult) (*DiffReport, error) {
	if left == nil || right == nil {
		return nil, model.NewError(model.CodeValue, "both bake results are required")
	}
	if len(left.CommitSequence) != len(right.CommitSequence) {
		return nil, diffError(&Diff{
			Field: "commit_sequence_length",
			Left:  fmt.Sprintf("%d", len(left.CommitSequence)),
			Right: fmt.Sprintf("%d", len(right.CommitSequence)),
		})
	}
	for i := range left.CommitSequence {
		a, b := left.CommitSequence[i], right.CommitSequence[i]
		if a != b {
			return nil, diffError(&Diff{
				ChunkID: b.ChunkID, Time: b.Time, Field: "commit_sequence",
				Left: fmt.Sprintf("%+v", a), Right: fmt.Sprintf("%+v", b),
			})
		}
	}
	for _, ref := range left.CommitSequence {
		a, okA := left.Unit(ref.ChunkID, ref.Time)
		b, okB := right.Unit(ref.ChunkID, ref.Time)
		if !okA || !okB {
			return nil, diffError(&Diff{
				ChunkID: ref.ChunkID, Time: ref.Time, Field: "unit_presence",
				Left: fmt.Sprintf("%t", okA), Right: fmt.Sprintf("%t", okB),
			})
		}
		if d := compareUnits(a, b); d != nil {
			return nil, diffError(d)
		}
	}
	return &DiffReport{Equal: true}, nil
}

func compareUnits(a, b *BakedUnit) *Diff {
	if a.ChunkID != b.ChunkID || a.Time != b.Time {
		return &Diff{ChunkID: b.ChunkID, Time: b.Time, Field: "unit_identity", Left: UnitID(a.ChunkID, a.Time), Right: UnitID(b.ChunkID, b.Time)}
	}
	if len(a.Points) != len(b.Points) {
		return &Diff{ChunkID: b.ChunkID, Time: b.Time, Field: "point_count", Left: fmt.Sprintf("%d", len(a.Points)), Right: fmt.Sprintf("%d", len(b.Points))}
	}
	pointsB := make(map[model.VertexPoint]*model.PointResult, len(b.Points))
	for i := range b.Points {
		pointsB[b.Points[i].Vertex] = &b.Points[i]
	}
	for i := range a.Points {
		pa := a.Points[i]
		pb, ok := pointsB[pa.Vertex]
		if !ok {
			vertex := pa.Vertex
			return &Diff{ChunkID: b.ChunkID, Time: b.Time, Vertex: &vertex, Field: "vertex_presence",
				Left: fmt.Sprintf("%+v", pa.Vertex), Right: "missing"}
		}
		vertex := pa.Vertex
		world := model.Vec2{X: pa.World.X, Z: pa.World.Z}
		if pa.DirectShadow != pb.DirectShadow {
			return &Diff{ChunkID: b.ChunkID, Time: b.Time, Vertex: &vertex, World: &world, Field: "direct_shadow",
				Left: fmt.Sprintf("%t", pa.DirectShadow), Right: fmt.Sprintf("%t", pb.DirectShadow)}
		}
		if pa.SkyVisibilityFixed != pb.SkyVisibilityFixed {
			return &Diff{ChunkID: b.ChunkID, Time: b.Time, Vertex: &vertex, World: &world, Field: "sky_visibility_fixed",
				Left: fmt.Sprintf("%d", pa.SkyVisibilityFixed), Right: fmt.Sprintf("%d", pb.SkyVisibilityFixed)}
		}
		for ch := 0; ch < 3; ch++ {
			if pa.IrradianceFixed[ch] != pb.IrradianceFixed[ch] {
				return &Diff{ChunkID: b.ChunkID, Time: b.Time, Vertex: &vertex, World: &world,
					Field: "irradiance_fixed." + channelName(ch),
					Left:  fmt.Sprintf("%d", pa.IrradianceFixed[ch]),
					Right: fmt.Sprintf("%d", pb.IrradianceFixed[ch])}
			}
		}
	}
	return nil
}

func diffError(d *Diff) error {
	e := model.NewError(model.CodeDiff, "bake paths differ at field %s (%s != %s)", d.Field, d.Left, d.Right).
		WithField(d.Field)
	if d.ChunkID != "" {
		e.WithChunk(d.ChunkID)
		e.WithTime(d.Time)
	}
	if d.Vertex != nil {
		e.WithPoint(*d.Vertex)
	}
	if d.World != nil {
		e.WithWorld(*d.World)
	}
	return e
}

// PointDeviation records truth error for one sample.
type PointDeviation struct {
	ChunkID         string            `json:"chunk_id"`
	Time            int               `json:"time"`
	Vertex          model.VertexPoint `json:"vertex"`
	World           model.Vec2        `json:"world"`
	ShadowMismatch  bool              `json:"shadow_mismatch"`
	SkyError        float64           `json:"sky_error"`
	IrradianceError [3]float64        `json:"irradiance_error"`
}

// TruthReport is a tolerance-sensitive comparison against generator truth.
type TruthReport struct {
	Tolerance          float64          `json:"tolerance"`
	Points             int              `json:"points"`
	ShadowMismatches   int              `json:"shadow_mismatches"`
	OverTolerance      int              `json:"over_tolerance"`
	WithinTolerance    int              `json:"within_tolerance"`
	MaxSkyError        float64          `json:"max_sky_error"`
	MaxIrradianceError float64          `json:"max_irradiance_error"`
	Violations         []PointDeviation `json:"violations,omitempty"`
}

// AnalyzeTruth computes deviation statistics without returning an error.
func AnalyzeTruth(result *BakeResult, truth model.Truth, tolerance float64) TruthReport {
	report := TruthReport{Tolerance: tolerance}
	for _, tp := range truth.Points {
		report.Points++
		unit, ok := result.Unit(tp.ChunkID, tp.Time)
		if !ok {
			report.ShadowMismatches++
			report.OverTolerance++
			report.Violations = append(report.Violations, PointDeviation{
				ChunkID: tp.ChunkID, Time: tp.Time, Vertex: tp.Vertex,
				World: model.Vec2{X: tp.World.X, Z: tp.World.Z}, ShadowMismatch: true,
			})
			continue
		}
		point, ok := unit.Vertex(tp.Vertex.X, tp.Vertex.Z)
		if !ok {
			report.ShadowMismatches++
			report.OverTolerance++
			report.Violations = append(report.Violations, PointDeviation{
				ChunkID: tp.ChunkID, Time: tp.Time, Vertex: tp.Vertex,
				World: model.Vec2{X: tp.World.X, Z: tp.World.Z}, ShadowMismatch: true,
			})
			continue
		}
		dev := PointDeviation{
			ChunkID: tp.ChunkID, Time: tp.Time, Vertex: tp.Vertex,
			World: model.Vec2{X: tp.World.X, Z: tp.World.Z},
		}
		if point.DirectShadow != tp.DirectShadow {
			dev.ShadowMismatch = true
			report.ShadowMismatches++
		}
		dev.SkyError = math.Abs(point.SkyVisibility - tp.SkyVisibility)
		if dev.SkyError > report.MaxSkyError {
			report.MaxSkyError = dev.SkyError
		}
		over := dev.ShadowMismatch || dev.SkyError > tolerance
		for ch := 0; ch < 3; ch++ {
			dev.IrradianceError[ch] = math.Abs(point.Irradiance[ch] - tp.Irradiance[ch])
			if dev.IrradianceError[ch] > report.MaxIrradianceError {
				report.MaxIrradianceError = dev.IrradianceError[ch]
			}
			if dev.IrradianceError[ch] > tolerance {
				over = true
			}
		}
		if over {
			report.OverTolerance++
			report.Violations = append(report.Violations, dev)
		}
	}
	report.WithinTolerance = report.Points - report.OverTolerance
	return report
}

// CompareTruth enforces exact shadow agreement and declared tolerance for sky
// visibility and irradiance.
func CompareTruth(result *BakeResult, truth model.Truth, tolerance float64) (*TruthReport, error) {
	report := AnalyzeTruth(result, truth, tolerance)
	if report.ShadowMismatches == 0 && report.OverTolerance == 0 {
		return &report, nil
	}
	if len(report.Violations) == 0 {
		return &report, model.NewError(model.CodeDiff, "truth comparison failed")
	}
	v := report.Violations[0]
	maxChannel := 0
	for ch := 1; ch < 3; ch++ {
		if v.IrradianceError[ch] > v.IrradianceError[maxChannel] {
			maxChannel = ch
		}
	}
	field := "shadow"
	if !v.ShadowMismatch {
		if v.SkyError > v.IrradianceError[maxChannel] {
			field = "sky_visibility"
		} else {
			field = "irradiance." + channelName(maxChannel)
		}
	}
	e := model.NewError(model.CodeDiff, "truth deviation exceeds tolerance %g (shadow=%t sky=%g irr=%.9g)",
		tolerance, v.ShadowMismatch, v.SkyError, v.IrradianceError[maxChannel]).
		WithChunk(v.ChunkID).WithTime(v.Time).WithPoint(v.Vertex).WithWorld(v.World).WithField(field)
	if field == "irradiance."+channelName(maxChannel) || field == "sky_visibility" {
		e.WithChannel(stringsTrimPrefix(field))
	}
	return &report, e
}

func stringsTrimPrefix(field string) string {
	if len(field) > 12 && field[:12] == "irradiance." {
		return field[12:]
	}
	if field == "sky_visibility" {
		return "visibility"
	}
	return ""
}
