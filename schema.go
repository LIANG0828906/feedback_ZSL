package silhouette

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

type v1Point2 struct {
	X int64 `json:"x"`
	Y int64 `json:"y"`
}

type v1Point3 struct {
	X int64 `json:"x"`
	Y int64 `json:"y"`
	Z int64 `json:"z"`
}

type schemaV1 struct {
	Format  string `json:"format"`
	SceneID string `json:"scene_id"`
	Mesh    struct {
		Name          string     `json:"name"`
		Rows          int        `json:"rows"`
		Cols          int        `json:"cols"`
		ControlPoints []v1Point3 `json:"control_points"`
	} `json:"mesh"`
	Outline struct {
		Points []v1Point2 `json:"points"`
		Closed bool       `json:"closed"`
	} `json:"outline"`
	Chunks []struct {
		ID string `json:"id"`
		X0 int64  `json:"x0"`
		Y0 int64  `json:"y0"`
		X1 int64  `json:"x1"`
		Y1 int64  `json:"y1"`
	} `json:"chunks"`
	Budget struct {
		MaxVertices  int    `json:"max_vertices"`
		ParamVersion string `json:"param_version"`
	} `json:"budget"`
	ToleranceMicroLogical int64 `json:"tolerance_micro_logical"`
}

type v2Point2 struct {
	X int64 `json:"x"`
	Y int64 `json:"y"`
}

type v2Point3 struct {
	X int64 `json:"x"`
	Y int64 `json:"y"`
	Z int64 `json:"z"`
}

type schemaV2 struct {
	SchemaVersion int    `json:"schema_version"`
	SceneID       string `json:"scene_id"`
	SurfaceGrid   struct {
		Name          string     `json:"name"`
		Rows          int        `json:"rows"`
		Cols          int        `json:"cols"`
		BasePoint     v2Point3   `json:"base_point"`
		ControlDeltas []v2Point3 `json:"control_deltas"`
	} `json:"surface_grid"`
	Silhouette struct {
		BasePoint   v2Point2   `json:"base_point"`
		PointDeltas []v2Point2 `json:"point_deltas"`
		Closed      bool       `json:"closed"`
	} `json:"silhouette"`
	ChunkBounds []struct {
		ID string `json:"id"`
		X0 int64  `json:"x0"`
		Y0 int64  `json:"y0"`
		X1 int64  `json:"x1"`
		Y1 int64  `json:"y1"`
	} `json:"chunk_bounds"`
	SampleBudget struct {
		MaxVertices  int    `json:"max_vertices"`
		ParamVersion string `json:"param_version"`
	} `json:"sample_budget"`
	ToleranceMilliPx int64 `json:"tolerance_milli_px"`
}

type v3Point2 struct {
	X int64 `json:"x"`
	Y int64 `json:"y"`
}

type v3Point3 struct {
	X int64 `json:"x"`
	Y int64 `json:"y"`
	Z int64 `json:"z"`
}

type schemaV3 struct {
	SchemaVersion int    `json:"schema_version"`
	SceneID       string `json:"scene_id"`
	Surface       struct {
		Name     string     `json:"name"`
		Rows     int        `json:"rows"`
		Cols     int        `json:"cols"`
		Controls []v3Point3 `json:"controls"`
	} `json:"surface"`
	Segments []struct {
		ID              string     `json:"id"`
		Weight          int64      `json:"weight"`
		Controls        []v3Point2 `json:"controls"`
		Closed          bool       `json:"closed"`
		SurfaceRefs     []int      `json:"surface_refs"`
		MetadataVersion string     `json:"metadata_version"`
	} `json:"segments"`
	Tiles []struct {
		ID    string   `json:"id"`
		Min   v3Point2 `json:"min"`
		Max   v3Point2 `json:"max"`
		Order int      `json:"order"`
	} `json:"tiles"`
	Sampling struct {
		MaxVertices           int    `json:"max_vertices"`
		ToleranceMicroPx      int64  `json:"tolerance_micro_px"`
		BudgetParamVersion    string `json:"budget_param_version"`
		ToleranceParamVersion string `json:"tolerance_param_version"`
	} `json:"sampling"`
}

func decodeObject(data []byte, path string) (map[string]json.RawMessage, error) {
	var obj map[string]json.RawMessage
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(&obj); err != nil {
		return nil, (&DomainError{Code: CodeSchema, Path: path, Message: "invalid JSON object"}).
			WithDetail("error", err.Error())
	}
	if obj == nil {
		return nil, NewDomainError(CodeSchema, path, "JSON value must be an object")
	}
	return obj, nil
}

func requireFields(obj map[string]json.RawMessage, path string, fields ...string) error {
	for _, field := range fields {
		if _, ok := obj[field]; !ok {
			return NewDomainError(CodeSchema, path+"."+field, "required field is missing")
		}
	}
	return nil
}

func nestedObject(obj map[string]json.RawMessage, field, path string) (map[string]json.RawMessage, error) {
	raw, ok := obj[field]
	if !ok {
		return nil, NewDomainError(CodeSchema, path+"."+field, "required object is missing")
	}
	var child map[string]json.RawMessage
	if err := json.Unmarshal(raw, &child); err != nil || child == nil {
		return nil, NewDomainError(CodeSchema, path+"."+field, "required value must be an object")
	}
	return child, nil
}

func detectVersion(obj map[string]json.RawMessage) (int, error) {
	if raw, ok := obj["schema_version"]; ok {
		var version int
		if err := json.Unmarshal(raw, &version); err != nil {
			return 0, NewDomainError(CodeSchema, "$.schema_version", "schema version must be an integer")
		}
		if version < 1 || version > 3 {
			return 0, NewDomainError(CodeSchema, "$.schema_version", "unknown schema version")
		}
		return version, nil
	}
	if raw, ok := obj["format"]; ok {
		var format string
		if err := json.Unmarshal(raw, &format); err != nil {
			return 0, NewDomainError(CodeSchema, "$.format", "legacy format must be a string")
		}
		if format == "surface-schema/1" || format == "surface/1" {
			return VersionLegacy, nil
		}
		return 0, (&DomainError{Code: CodeSchema, Path: "$.format", Message: "unknown schema format"}).
			WithDetail("value", format)
	}
	return 0, NewDomainError(CodeSchema, "$.schema_version", "schema version field is missing")
}

// Load maps any declared schema version into the common internal model.
func Load(data []byte) (*Scene, error) {
	root, err := decodeObject(data, "$")
	if err != nil {
		return nil, err
	}
	version, err := detectVersion(root)
	if err != nil {
		return nil, err
	}
	switch version {
	case VersionLegacy:
		return loadV1(root, data)
	case VersionRelative:
		return loadV2(root, data)
	case VersionCurrent:
		return loadV3(root, data)
	default:
		return nil, NewDomainError(CodeSchema, "$.schema_version", "unknown schema version")
	}
}

func loadV1(root map[string]json.RawMessage, data []byte) (*Scene, error) {
	if err := requireFields(root, "$", "format", "scene_id", "mesh", "outline", "chunks", "budget", "tolerance_micro_logical"); err != nil {
		return nil, err
	}
	mesh, err := nestedObject(root, "mesh", "$")
	if err != nil {
		return nil, err
	}
	if err := requireFields(mesh, "$.mesh", "name", "rows", "cols", "control_points"); err != nil {
		return nil, err
	}
	outline, err := nestedObject(root, "outline", "$")
	if err != nil {
		return nil, err
	}
	if err := requireFields(outline, "$.outline", "points", "closed"); err != nil {
		return nil, err
	}
	budget, err := nestedObject(root, "budget", "$")
	if err != nil {
		return nil, err
	}
	if err := requireFields(budget, "$.budget", "max_vertices"); err != nil {
		return nil, err
	}
	var wire schemaV1
	if err := json.Unmarshal(data, &wire); err != nil {
		return nil, (&DomainError{Code: CodeSchema, Path: "$", Message: "cannot decode v1 schema"}).WithDetail("error", err.Error())
	}
	scene := &Scene{
		ID: wire.SceneID, Name: wire.Mesh.Name, SourceVersion: VersionLegacy,
		Surface:   ControlGrid{Name: wire.Mesh.Name, Rows: wire.Mesh.Rows, Cols: wire.Mesh.Cols},
		Budget:    Budget{MaxVertices: wire.Budget.MaxVertices, ParamVersion: wire.Budget.ParamVersion},
		Tolerance: Tolerance{Value: wire.ToleranceMicroLogical, Unit: "micro_logical", ParamVersion: "v1-default"},
	}
	if scene.Budget.ParamVersion == "" {
		scene.Budget.ParamVersion = "v1-budget"
		scene.Traces = append(scene.Traces, defaultFillTrace("$.budget.param_version", 1, 3, scene.Budget.ParamVersion, "schema-v1/default.budget_param_version", "declared default used because the source field is absent"))
	}
	scene.Traces = append(scene.Traces, ConversionTrace{Stage: "load", Path: "$.tolerance_micro_logical", Action: "micro_logical_to_fixed", OldValue: strconv.FormatInt(wire.ToleranceMicroLogical, 10), NewValue: strconv.FormatInt(wire.ToleranceMicroLogical, 10), Unit: "micro_logical", SourceVersion: 1, TargetVersion: 3})
	for i, p := range wire.Mesh.ControlPoints {
		path := fmt.Sprintf("$.mesh.control_points[%d]", i)
		vec, traces, err := convertV1Point3(p, path, 1)
		if err != nil {
			return nil, err
		}
		scene.Surface.Controls = append(scene.Surface.Controls, vec)
		scene.Traces = append(scene.Traces, traces...)
	}
	var outlinePoints []Vec2
	for i, p := range wire.Outline.Points {
		path := fmt.Sprintf("$.outline.points[%d]", i)
		x, err := LogicalToFixed(p.X, path+".x")
		if err != nil {
			return nil, err
		}
		y, err := LogicalToFixed(p.Y, path+".y")
		if err != nil {
			return nil, err
		}
		outlinePoints = append(outlinePoints, Vec2{X: x.Result, Y: y.Result})
		scene.Traces = append(scene.Traces, x.Trace("load", 1, 3), y.Trace("load", 1, 3))
	}
	segments, defaults, err := groupOutlineSegments(outlinePoints, wire.Outline.Closed, 1, 3)
	if err != nil {
		return nil, err
	}
	scene.Segments = segments
	scene.Traces = append(scene.Traces, defaults...)
	for i, c := range wire.Chunks {
		path := fmt.Sprintf("$.chunks[%d]", i)
		tile, traces, err := convertV1Tile(c.ID, c.X0, c.Y0, c.X1, c.Y1, i, path, 1)
		if err != nil {
			return nil, err
		}
		scene.Tiles = append(scene.Tiles, tile)
		scene.Traces = append(scene.Traces, traces...)
	}
	if err := scene.Validate(); err != nil {
		return nil, err
	}
	scene.Normalize()
	return scene, nil
}

func convertV1Point3(p v1Point3, path string, source int) (Vec3, []ConversionTrace, error) {
	x, err := LogicalToFixed(p.X, path+".x")
	if err != nil {
		return Vec3{}, nil, err
	}
	y, err := LogicalToFixed(p.Y, path+".y")
	if err != nil {
		return Vec3{}, nil, err
	}
	z, err := LogicalToFixed(p.Z, path+".z")
	if err != nil {
		return Vec3{}, nil, err
	}
	return Vec3{X: x.Result, Y: y.Result, Z: z.Result}, []ConversionTrace{
		x.Trace("load", source, 3), y.Trace("load", source, 3), z.Trace("load", source, 3),
	}, nil
}

func convertV1Tile(id string, x0, y0, x1, y1 int64, order int, path string, source int) (Tile, []ConversionTrace, error) {
	values := []struct {
		v    int64
		path string
	}{{x0, path + ".x0"}, {y0, path + ".y0"}, {x1, path + ".x1"}, {y1, path + ".y1"}}
	results := make([]int64, 4)
	var traces []ConversionTrace
	for i, value := range values {
		op, err := LogicalToFixed(value.v, value.path)
		if err != nil {
			return Tile{}, nil, err
		}
		results[i] = op.Result
		traces = append(traces, op.Trace("load", source, 3))
	}
	return Tile{ID: id, Min: Vec2{X: results[0], Y: results[1]}, Max: Vec2{X: results[2], Y: results[3]}, Order: order}, traces, nil
}

func groupOutlineSegments(points []Vec2, closed bool, source, target int) ([]CurveSegment, []ConversionTrace, error) {
	if len(points) == 0 || len(points)%4 != 0 {
		return nil, nil, NewDomainError(CodeSchema, "$.outline.points", "outline must contain complete groups of 4 cubic controls")
	}
	var segments []CurveSegment
	var traces []ConversionTrace
	for i := 0; i < len(points); i += 4 {
		id := fmt.Sprintf("seg-%d", i/4)
		seg := CurveSegment{ID: id, Weight: 100, Closed: closed, Controls: [4]Vec2{points[i], points[i+1], points[i+2], points[i+3]}, MetadataVersion: CurveMetaVersion, SurfaceRefs: []int{}}
		segments = append(segments, seg)
		traces = append(traces,
			ConversionTrace{Stage: "load", Path: fmt.Sprintf("$.outline.points[%d:%d]", i, i+4), Action: "infer_segment_group", NewValue: id, Note: "schema has no explicit segment split; every 4 cubic controls form one segment", SourceVersion: source, TargetVersion: target},
			defaultFillTrace("$.segments["+strconv.Itoa(i/4)+"].id", source, target, id, fmt.Sprintf("schema-v%d/default.segment_id", source), "inferred from implicit cubic group"),
			defaultFillTrace("$.segments["+strconv.Itoa(i/4)+"].weight", source, target, "100", fmt.Sprintf("schema-v%d/default.segment_weight", source), "weight is not expressible in this schema"),
			defaultFillTrace("$.segments["+strconv.Itoa(i/4)+"].surface_refs", source, target, "[]", fmt.Sprintf("schema-v%d/default.surface_refs", source), "surface references are not expressible in this schema"),
			defaultFillTrace("$.segments["+strconv.Itoa(i/4)+"].metadata_version", source, target, CurveMetaVersion, fmt.Sprintf("schema-v%d/default.curve_metadata_version", source), "metadata version default"),
		)
	}
	return segments, traces, nil
}

func loadV2(root map[string]json.RawMessage, data []byte) (*Scene, error) {
	if err := requireFields(root, "$", "schema_version", "scene_id", "surface_grid", "silhouette", "chunk_bounds", "sample_budget", "tolerance_milli_px"); err != nil {
		return nil, err
	}
	surface, err := nestedObject(root, "surface_grid", "$")
	if err != nil {
		return nil, err
	}
	if err := requireFields(surface, "$.surface_grid", "name", "rows", "cols", "base_point", "control_deltas"); err != nil {
		return nil, err
	}
	silhouette, err := nestedObject(root, "silhouette", "$")
	if err != nil {
		return nil, err
	}
	if err := requireFields(silhouette, "$.silhouette", "base_point", "point_deltas", "closed"); err != nil {
		return nil, err
	}
	budget, err := nestedObject(root, "sample_budget", "$")
	if err != nil {
		return nil, err
	}
	if err := requireFields(budget, "$.sample_budget", "max_vertices"); err != nil {
		return nil, err
	}
	var wire schemaV2
	if err := json.Unmarshal(data, &wire); err != nil {
		return nil, (&DomainError{Code: CodeSchema, Path: "$", Message: "cannot decode v2 schema"}).WithDetail("error", err.Error())
	}
	scene := &Scene{
		ID: wire.SceneID, Name: wire.SurfaceGrid.Name, SourceVersion: VersionRelative,
		Surface:   ControlGrid{Name: wire.SurfaceGrid.Name, Rows: wire.SurfaceGrid.Rows, Cols: wire.SurfaceGrid.Cols},
		Budget:    Budget{MaxVertices: wire.SampleBudget.MaxVertices, ParamVersion: wire.SampleBudget.ParamVersion},
		Tolerance: Tolerance{Unit: "milli_px", ParamVersion: "v2-default"},
	}
	if scene.Budget.ParamVersion == "" {
		scene.Budget.ParamVersion = "v2-budget"
		scene.Traces = append(scene.Traces, defaultFillTrace("$.sample_budget.param_version", 2, 3, scene.Budget.ParamVersion, "schema-v2/default.budget_param_version", "declared default used because the source field is absent"))
	}
	tol, err := MilliPixelToFixed(wire.ToleranceMilliPx, "$.tolerance_milli_px")
	if err != nil {
		return nil, err
	}
	scene.Tolerance.Value = tol.Result
	scene.Traces = append(scene.Traces, tol.Trace("load", 2, 3))
	base3 := Vec3{X: wire.SurfaceGrid.BasePoint.X, Y: wire.SurfaceGrid.BasePoint.Y, Z: wire.SurfaceGrid.BasePoint.Z}
	scene.Traces = append(scene.Traces, ConversionTrace{Stage: "load", Path: "$.surface_grid.base_point", Action: "relative_base", OldValue: base3String(base3), NewValue: base3String(base3), Unit: "micro_logical", SourceVersion: 2, TargetVersion: 3})
	for i, delta := range wire.SurfaceGrid.ControlDeltas {
		path := fmt.Sprintf("$.surface_grid.control_deltas[%d]", i)
		d := Vec3{X: delta.X, Y: delta.Y, Z: delta.Z}
		vec, ops, err := addVec3(base3, d, path)
		if err != nil {
			return nil, err
		}
		scene.Surface.Controls = append(scene.Surface.Controls, vec)
		for _, op := range ops {
			scene.Traces = append(scene.Traces, op.Trace("load", 2, 3))
		}
	}
	base2 := wire.Silhouette.BasePoint
	scene.Traces = append(scene.Traces, ConversionTrace{Stage: "load", Path: "$.silhouette.base_point", Action: "relative_base", OldValue: base2String(base2), NewValue: base2String(base2), Unit: "micro_logical", SourceVersion: 2, TargetVersion: 3})
	var outlinePoints []Vec2
	for i, delta := range wire.Silhouette.PointDeltas {
		path := fmt.Sprintf("$.silhouette.point_deltas[%d]", i)
		d := Vec2{X: delta.X, Y: delta.Y}
		vec, ops, err := addVec2(Vec2{X: base2.X, Y: base2.Y}, d, path)
		if err != nil {
			return nil, err
		}
		outlinePoints = append(outlinePoints, vec)
		for _, op := range ops {
			scene.Traces = append(scene.Traces, op.Trace("load", 2, 3))
		}
	}
	segments, defaults, err := groupOutlineSegments(outlinePoints, wire.Silhouette.Closed, 2, 3)
	if err != nil {
		return nil, err
	}
	scene.Segments = segments
	scene.Traces = append(scene.Traces, defaults...)
	for i, c := range wire.ChunkBounds {
		path := fmt.Sprintf("$.chunk_bounds[%d]", i)
		scene.Tiles = append(scene.Tiles, Tile{
			ID: c.ID, Min: Vec2{X: c.X0, Y: c.Y0}, Max: Vec2{X: c.X1, Y: c.Y1}, Order: i,
		})
		scene.Traces = append(scene.Traces, ConversionTrace{Stage: "load", Path: path, Action: "micro_logical_to_fixed", OldValue: fmt.Sprintf("(%d,%d)-(%d,%d)", c.X0, c.Y0, c.X1, c.Y1), NewValue: fmt.Sprintf("(%d,%d)-(%d,%d)", c.X0, c.Y0, c.X1, c.Y1), Unit: "micro_logical", SourceVersion: 2, TargetVersion: 3})
	}
	if err := scene.Validate(); err != nil {
		return nil, err
	}
	scene.Normalize()
	return scene, nil
}

func addVec3(a, b Vec3, path string) (Vec3, []FixedOp, error) {
	x, err := AddFixed(a.X, b.X, path+".x")
	if err != nil {
		return Vec3{}, nil, err
	}
	y, err := AddFixed(a.Y, b.Y, path+".y")
	if err != nil {
		return Vec3{}, nil, err
	}
	z, err := AddFixed(a.Z, b.Z, path+".z")
	if err != nil {
		return Vec3{}, nil, err
	}
	return Vec3{X: x.Result, Y: y.Result, Z: z.Result}, []FixedOp{x, y, z}, nil
}

func base3String(v Vec3) string     { return fmt.Sprintf("(%d,%d,%d)", v.X, v.Y, v.Z) }
func base2String(v v2Point2) string { return fmt.Sprintf("(%d,%d)", v.X, v.Y) }

func loadV3(root map[string]json.RawMessage, data []byte) (*Scene, error) {
	if err := requireFields(root, "$", "schema_version", "scene_id", "surface", "segments", "tiles", "sampling"); err != nil {
		return nil, err
	}
	surface, err := nestedObject(root, "surface", "$")
	if err != nil {
		return nil, err
	}
	if err := requireFields(surface, "$.surface", "name", "rows", "cols", "controls"); err != nil {
		return nil, err
	}
	sampling, err := nestedObject(root, "sampling", "$")
	if err != nil {
		return nil, err
	}
	if err := requireFields(sampling, "$.sampling", "max_vertices", "tolerance_micro_px"); err != nil {
		return nil, err
	}
	var wire schemaV3
	if err := json.Unmarshal(data, &wire); err != nil {
		return nil, (&DomainError{Code: CodeSchema, Path: "$", Message: "cannot decode v3 schema"}).WithDetail("error", err.Error())
	}
	scene := &Scene{
		ID: wire.SceneID, Name: wire.Surface.Name, SourceVersion: VersionCurrent,
		Surface:   ControlGrid{Name: wire.Surface.Name, Rows: wire.Surface.Rows, Cols: wire.Surface.Cols},
		Budget:    Budget{MaxVertices: wire.Sampling.MaxVertices, ParamVersion: wire.Sampling.BudgetParamVersion},
		Tolerance: Tolerance{Value: wire.Sampling.ToleranceMicroPx, Unit: "micro_px", ParamVersion: wire.Sampling.ToleranceParamVersion},
	}
	if scene.Budget.ParamVersion == "" {
		scene.Budget.ParamVersion = "v3-budget"
		scene.Traces = append(scene.Traces, defaultFillTrace("$.sampling.budget_param_version", 3, 3, scene.Budget.ParamVersion, "schema-v3/default.budget_param_version", "declared default used because the source field is absent"))
	}
	if scene.Tolerance.ParamVersion == "" {
		scene.Tolerance.ParamVersion = "v3-tolerance"
		scene.Traces = append(scene.Traces, defaultFillTrace("$.sampling.tolerance_param_version", 3, 3, scene.Tolerance.ParamVersion, "schema-v3/default.tolerance_param_version", "declared default used because the source field is absent"))
	}
	op, err := MicroPixelToFixed(wire.Sampling.ToleranceMicroPx, "$.sampling.tolerance_micro_px")
	if err != nil {
		return nil, err
	}
	scene.Tolerance.Value = op.Result
	scene.Traces = append(scene.Traces, op.Trace("load", 3, 3))
	for i, p := range wire.Surface.Controls {
		path := fmt.Sprintf("$.surface.controls[%d]", i)
		for axis, value := range []int64{p.X, p.Y, p.Z} {
			axisName := []string{"x", "y", "z"}[axis]
			conv, err := MicroPixelToFixed(value, fmt.Sprintf("%s.%s", path, axisName))
			if err != nil {
				return nil, err
			}
			scene.Traces = append(scene.Traces, conv.Trace("load", 3, 3))
		}
		scene.Surface.Controls = append(scene.Surface.Controls, Vec3{X: p.X, Y: p.Y, Z: p.Z})
	}
	for i, seg := range wire.Segments {
		path := fmt.Sprintf("$.segments[%d]", i)
		if len(seg.Controls) != 4 {
			return nil, NewDomainError(CodeSchema, path+".controls", "cubic segment must have exactly 4 controls")
		}
		if strings.TrimSpace(seg.ID) == "" {
			return nil, NewDomainError(CodeSchema, path+".id", "required field is missing")
		}
		if seg.Weight <= 0 {
			return nil, NewDomainError(CodeSchema, path+".weight", "weight must be positive")
		}
		curve := CurveSegment{
			ID: seg.ID, Weight: seg.Weight, Closed: seg.Closed,
			SurfaceRefs:     append([]int(nil), seg.SurfaceRefs...),
			MetadataVersion: seg.MetadataVersion,
		}
		if curve.MetadataVersion == "" {
			curve.MetadataVersion = CurveMetaVersion
			scene.Traces = append(scene.Traces, defaultFillTrace(path+".metadata_version", 3, 3, CurveMetaVersion, "schema-v3/default.curve_metadata_version", "declared metadata version default"))
		}
		for j, p := range seg.Controls {
			curve.Controls[j] = Vec2{X: p.X, Y: p.Y}
			scene.Traces = append(scene.Traces,
				ConversionTrace{Stage: "load", Path: fmt.Sprintf("%s.controls[%d].x", path, j), Action: "micro_px_to_fixed", OldValue: strconv.FormatInt(p.X, 10), NewValue: strconv.FormatInt(p.X, 10), Unit: "micro_px", SourceVersion: 3, TargetVersion: 3},
				ConversionTrace{Stage: "load", Path: fmt.Sprintf("%s.controls[%d].y", path, j), Action: "micro_px_to_fixed", OldValue: strconv.FormatInt(p.Y, 10), NewValue: strconv.FormatInt(p.Y, 10), Unit: "micro_px", SourceVersion: 3, TargetVersion: 3},
			)
		}
		scene.Segments = append(scene.Segments, curve)
	}
	for i, tile := range wire.Tiles {
		if strings.TrimSpace(tile.ID) == "" {
			return nil, NewDomainError(CodeSchema, fmt.Sprintf("$.tiles[%d].id", i), "required field is missing")
		}
		order := tile.Order
		if order == 0 {
			order = i
		}
		scene.Tiles = append(scene.Tiles, Tile{
			ID: tile.ID, Min: Vec2{X: tile.Min.X, Y: tile.Min.Y}, Max: Vec2{X: tile.Max.X, Y: tile.Max.Y}, Order: order,
		})
		scene.Traces = append(scene.Traces, ConversionTrace{Stage: "load", Path: fmt.Sprintf("$.tiles[%d]", i), Action: "micro_px_to_fixed", Unit: "micro_px", SourceVersion: 3, TargetVersion: 3})
	}
	if err := scene.Validate(); err != nil {
		return nil, err
	}
	scene.Normalize()
	return scene, nil
}

type ExportResult struct {
	Data   []byte            `json:"data"`
	Traces []ConversionTrace `json:"traces"`
	Lossy  LossyIssues       `json:"lossy"`
}

// Export serializes the internal model into a requested schema. E_LOSSY items
// are returned as data, not as a fatal error, because the produced document is
// still required to be loadable.
func Export(scene *Scene, target int) (*ExportResult, error) {
	if err := scene.Validate(); err != nil {
		return nil, err
	}
	switch target {
	case VersionLegacy:
		return exportV1(scene)
	case VersionRelative:
		return exportV2(scene)
	case VersionCurrent:
		return exportV3(scene)
	default:
		return nil, NewDomainError(CodeSchema, "$.schema_version", "unknown target schema version")
	}
}

func exportV3(scene *Scene) (*ExportResult, error) {
	wire := schemaV3{SchemaVersion: 3, SceneID: scene.ID}
	wire.Surface.Name = scene.Surface.Name
	wire.Surface.Rows = scene.Surface.Rows
	wire.Surface.Cols = scene.Surface.Cols
	for _, p := range scene.Surface.Controls {
		wire.Surface.Controls = append(wire.Surface.Controls, v3Point3{X: p.X, Y: p.Y, Z: p.Z})
	}
	for _, seg := range scene.Segments {
		item := struct {
			ID              string     `json:"id"`
			Weight          int64      `json:"weight"`
			Controls        []v3Point2 `json:"controls"`
			Closed          bool       `json:"closed"`
			SurfaceRefs     []int      `json:"surface_refs"`
			MetadataVersion string     `json:"metadata_version"`
		}{ID: seg.ID, Weight: seg.Weight, Closed: seg.Closed, SurfaceRefs: append([]int(nil), seg.SurfaceRefs...), MetadataVersion: seg.MetadataVersion}
		for _, p := range seg.Controls {
			item.Controls = append(item.Controls, v3Point2{X: p.X, Y: p.Y})
		}
		wire.Segments = append(wire.Segments, item)
	}
	for _, tile := range scene.Tiles {
		wire.Tiles = append(wire.Tiles, struct {
			ID    string   `json:"id"`
			Min   v3Point2 `json:"min"`
			Max   v3Point2 `json:"max"`
			Order int      `json:"order"`
		}{ID: tile.ID, Min: v3Point2{X: tile.Min.X, Y: tile.Min.Y}, Max: v3Point2{X: tile.Max.X, Y: tile.Max.Y}, Order: tile.Order})
	}
	wire.Sampling.MaxVertices = scene.Budget.MaxVertices
	wire.Sampling.ToleranceMicroPx = scene.Tolerance.Value
	wire.Sampling.BudgetParamVersion = scene.Budget.ParamVersion
	wire.Sampling.ToleranceParamVersion = scene.Tolerance.ParamVersion
	data, err := json.MarshalIndent(wire, "", "  ")
	if err != nil {
		return nil, NewDomainError(CodeInternal, "$", err.Error())
	}
	return &ExportResult{Data: data, Traces: []ConversionTrace{{Stage: "export", Path: "$", Action: "fixed_to_micro_px", SourceVersion: scene.SourceVersion, TargetVersion: 3}}}, nil
}

func exportV2(scene *Scene) (*ExportResult, error) {
	wire := schemaV2{SchemaVersion: 2, SceneID: scene.ID}
	wire.SurfaceGrid.Name = scene.Surface.Name
	wire.SurfaceGrid.Rows = scene.Surface.Rows
	wire.SurfaceGrid.Cols = scene.Surface.Cols
	wire.SurfaceGrid.BasePoint = v2Point3{X: scene.Surface.Controls[0].X, Y: scene.Surface.Controls[0].Y, Z: scene.Surface.Controls[0].Z}
	for _, p := range scene.Surface.Controls {
		wire.SurfaceGrid.ControlDeltas = append(wire.SurfaceGrid.ControlDeltas, v2Point3{X: p.X - wire.SurfaceGrid.BasePoint.X, Y: p.Y - wire.SurfaceGrid.BasePoint.Y, Z: p.Z - wire.SurfaceGrid.BasePoint.Z})
	}
	wire.Silhouette.BasePoint = v2Point2{X: scene.Segments[0].Controls[0].X, Y: scene.Segments[0].Controls[0].Y}
	wire.Silhouette.Closed = scene.Segments[0].Closed
	for _, seg := range scene.Segments {
		for _, p := range seg.Controls {
			wire.Silhouette.PointDeltas = append(wire.Silhouette.PointDeltas, v2Point2{X: p.X - wire.Silhouette.BasePoint.X, Y: p.Y - wire.Silhouette.BasePoint.Y})
		}
	}
	for _, tile := range scene.Tiles {
		wire.ChunkBounds = append(wire.ChunkBounds, struct {
			ID string `json:"id"`
			X0 int64  `json:"x0"`
			Y0 int64  `json:"y0"`
			X1 int64  `json:"x1"`
			Y1 int64  `json:"y1"`
		}{ID: tile.ID, X0: tile.Min.X, Y0: tile.Min.Y, X1: tile.Max.X, Y1: tile.Max.Y})
	}
	wire.SampleBudget.MaxVertices = scene.Budget.MaxVertices
	wire.SampleBudget.ParamVersion = scene.Budget.ParamVersion
	tol, err := FixedToMilliPixel(scene.Tolerance.Value, "$.tolerance")
	if err != nil {
		return nil, err
	}
	wire.ToleranceMilliPx = tol.Result
	result := &ExportResult{Traces: []ConversionTrace{tol.Trace("export", scene.SourceVersion, 2)}}
	if tol.Truncated {
		result.Lossy = append(result.Lossy, LossyIssue{
			Code: CodeLossy, Path: "$.tolerance", Source: scene.SourceVersion, Target: 2,
			OldValue: strconv.FormatInt(scene.Tolerance.Value, 10), NewValue: strconv.FormatInt(tol.Result, 10),
			Message: "tolerance was floored to milli-pixel precision; remainder recorded in trace",
		})
	}
	if len(scene.Segments) > 1 {
		result.Lossy = append(result.Lossy, LossyIssue{
			Code: CodeLossy, Path: "$.segments", Source: scene.SourceVersion, Target: 2,
			OldValue: strconv.Itoa(len(scene.Segments)), NewValue: "implicit groups of 4 controls",
			Message: "v2 has no explicit segment split; geometry is kept as flattened cubic groups and split metadata is dropped",
		})
	}
	for i, seg := range scene.Segments {
		if seg.ID != fmt.Sprintf("seg-%d", i) || seg.Weight != 100 || len(seg.SurfaceRefs) > 0 {
			result.Lossy = append(result.Lossy, LossyIssue{
				Code: CodeLossy, Path: fmt.Sprintf("$.segments[%d]", i), Source: scene.SourceVersion, Target: 2,
				OldValue: fmt.Sprintf("id=%s weight=%d refs=%v", seg.ID, seg.Weight, seg.SurfaceRefs),
				NewValue: "implicit-segment",
				Message:  "segment identity, visual weight and surface-reference metadata are not expressible in v2",
			})
		}
	}
	if mixedClosed(scene.Segments) {
		result.Lossy = append(result.Lossy, LossyIssue{
			Code: CodeLossy, Path: "$.silhouette.closed", Source: scene.SourceVersion, Target: 2,
			Message: "segments have mixed closed flags; v2 can only carry one flattened closure flag",
		})
	}
	data, err := json.MarshalIndent(wire, "", "  ")
	if err != nil {
		return nil, NewDomainError(CodeInternal, "$", err.Error())
	}
	result.Data = data
	return result, nil
}

func mixedClosed(segments []CurveSegment) bool {
	if len(segments) < 2 {
		return false
	}
	first := segments[0].Closed
	for _, seg := range segments[1:] {
		if seg.Closed != first {
			return true
		}
	}
	return false
}

func exportV1(scene *Scene) (*ExportResult, error) {
	wire := schemaV1{Format: "surface-schema/1", SceneID: scene.ID}
	wire.Mesh.Name = scene.Surface.Name
	wire.Mesh.Rows = scene.Surface.Rows
	wire.Mesh.Cols = scene.Surface.Cols
	result := &ExportResult{}
	for i, p := range scene.Surface.Controls {
		path := fmt.Sprintf("$.mesh.control_points[%d]", i)
		x, err := FixedToLogical(p.X, RoundTowardZero, path+".x")
		if err != nil {
			return nil, err
		}
		y, err := FixedToLogical(p.Y, RoundTowardZero, path+".y")
		if err != nil {
			return nil, err
		}
		z, err := FixedToLogical(p.Z, RoundTowardZero, path+".z")
		if err != nil {
			return nil, err
		}
		wire.Mesh.ControlPoints = append(wire.Mesh.ControlPoints, v1Point3{X: x.Result, Y: y.Result, Z: z.Result})
		for _, op := range []FixedOp{x, y, z} {
			result.Traces = append(result.Traces, op.Trace("export", scene.SourceVersion, 1))
			if op.Truncated {
				result.Lossy = append(result.Lossy, LossyIssue{
					Code: CodeLossy, Path: op.Field, Source: scene.SourceVersion, Target: 1,
					OldValue: strconv.FormatInt(op.OperandA, 10), NewValue: strconv.FormatInt(op.Result, 10),
					Message: "absolute coordinate was truncated to logical-unit precision; residual is recorded in the trace",
				})
			}
		}
	}
	wire.Outline.Closed = scene.Segments[0].Closed
	for i, seg := range scene.Segments {
		for j, p := range seg.Controls {
			path := fmt.Sprintf("$.outline.points[%d]", i*4+j)
			x, err := FixedToLogical(p.X, RoundTowardZero, path+".x")
			if err != nil {
				return nil, err
			}
			y, err := FixedToLogical(p.Y, RoundTowardZero, path+".y")
			if err != nil {
				return nil, err
			}
			wire.Outline.Points = append(wire.Outline.Points, v1Point2{X: x.Result, Y: y.Result})
			for _, op := range []FixedOp{x, y} {
				result.Traces = append(result.Traces, op.Trace("export", scene.SourceVersion, 1))
				if op.Truncated {
					result.Lossy = append(result.Lossy, LossyIssue{
						Code: CodeLossy, Path: op.Field, Source: scene.SourceVersion, Target: 1,
						OldValue: strconv.FormatInt(op.OperandA, 10), NewValue: strconv.FormatInt(op.Result, 10),
						Message: "absolute coordinate was truncated to logical-unit precision; residual is recorded in the trace",
					})
				}
			}
		}
	}
	for i, tile := range scene.Tiles {
		path := fmt.Sprintf("$.chunks[%d]", i)
		x0, err := FixedToLogical(tile.Min.X, RoundTowardZero, path+".x0")
		if err != nil {
			return nil, err
		}
		y0, err := FixedToLogical(tile.Min.Y, RoundTowardZero, path+".y0")
		if err != nil {
			return nil, err
		}
		x1, err := FixedToLogical(tile.Max.X, RoundTowardZero, path+".x1")
		if err != nil {
			return nil, err
		}
		y1, err := FixedToLogical(tile.Max.Y, RoundTowardZero, path+".y1")
		if err != nil {
			return nil, err
		}
		wire.Chunks = append(wire.Chunks, struct {
			ID string `json:"id"`
			X0 int64  `json:"x0"`
			Y0 int64  `json:"y0"`
			X1 int64  `json:"x1"`
			Y1 int64  `json:"y1"`
		}{ID: tile.ID, X0: x0.Result, Y0: y0.Result, X1: x1.Result, Y1: y1.Result})
		for _, op := range []FixedOp{x0, y0, x1, y1} {
			result.Traces = append(result.Traces, op.Trace("export", scene.SourceVersion, 1))
		}
	}
	wire.Budget.MaxVertices = scene.Budget.MaxVertices
	wire.Budget.ParamVersion = scene.Budget.ParamVersion
	wire.ToleranceMicroLogical = scene.Tolerance.Value
	tol, err := FixedToMicroPixel(scene.Tolerance.Value, "$.tolerance")
	if err != nil {
		return nil, err
	}
	result.Traces = append(result.Traces, tol.Trace("export", scene.SourceVersion, 1))
	if len(scene.Segments) > 1 {
		result.Lossy = append(result.Lossy, LossyIssue{
			Code: CodeLossy, Path: "$.segments", Source: scene.SourceVersion, Target: 1,
			OldValue: strconv.Itoa(len(scene.Segments)), NewValue: "implicit groups of 4 controls",
			Message: "v1 has no explicit segment split; every group of 4 cubic controls is inferred on reload",
		})
	}
	if scene.SourceVersion >= 2 {
		result.Lossy = append(result.Lossy, LossyIssue{
			Code: CodeLossy, Path: "$.surface_grid.base_point", Source: scene.SourceVersion, Target: 1,
			OldValue: "relative-base expression", NewValue: "absolute logical coordinates",
			Message: "relative base expression is flattened to absolute coordinates; reload restores coordinates but not the original expression form",
		})
	}
	for i, seg := range scene.Segments {
		if seg.ID != fmt.Sprintf("seg-%d", i) || seg.Weight != 100 || len(seg.SurfaceRefs) > 0 || seg.MetadataVersion != CurveMetaVersion {
			result.Lossy = append(result.Lossy, LossyIssue{
				Code: CodeLossy, Path: fmt.Sprintf("$.segments[%d]", i), Source: scene.SourceVersion, Target: 1,
				OldValue: fmt.Sprintf("id=%s weight=%d refs=%v meta=%s", seg.ID, seg.Weight, seg.SurfaceRefs, seg.MetadataVersion),
				NewValue: "implicit-segment",
				Message:  "segment identity, weight, reference and metadata version are not expressible in v1",
			})
		}
	}
	if mixedClosed(scene.Segments) {
		result.Lossy = append(result.Lossy, LossyIssue{
			Code: CodeLossy, Path: "$.outline.closed", Source: scene.SourceVersion, Target: 1,
			Message: "segments have mixed closed flags; v1 can only carry one flattened closure flag",
		})
	}
	data, err := json.MarshalIndent(wire, "", "  ")
	if err != nil {
		return nil, NewDomainError(CodeInternal, "$", err.Error())
	}
	result.Data = data
	return result, nil
}
