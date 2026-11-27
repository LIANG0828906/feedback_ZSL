package terrainbake

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

// LoadProject is the single ingress for all schema versions. It returns a
// fully normalized internal project and a trace of every lossy or
// representation-changing step.
func LoadProject(data []byte) (*Project, error) {
	data = bytes.TrimSpace(data)
	if len(data) == 0 {
		return nil, errDecode("$", "empty JSON document")
	}
	var env map[string]json.RawMessage
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, errDecode("$", "invalid JSON: "+err.Error())
	}
	version, versionField, err := detectVersion(env)
	if err != nil {
		return nil, err
	}
	switch version {
	case SchemaV1:
		return loadV1(data, versionField)
	case SchemaV2:
		return loadV2(data, versionField)
	case SchemaV3:
		return loadV3(data, versionField)
	default:
		return nil, errSchema(versionField, "unknown schema version")
	}
}

func detectVersion(env map[string]json.RawMessage) (SchemaVersion, string, error) {
	type candidate struct {
		field string
		value json.RawMessage
	}
	var candidates []candidate
	for _, f := range []string{"schema_version", "version", "schema"} {
		if raw, ok := env[f]; ok {
			candidates = append(candidates, candidate{field: f, value: raw})
		}
	}
	if len(candidates) == 0 {
		return 0, "schema_version", errSchema("schema_version", "required version field is missing")
	}
	if len(candidates) > 1 {
		return 0, "schema_version", errSchema("schema_version", "ambiguous version fields")
	}
	c := candidates[0]
	var s string
	if err := json.Unmarshal(c.value, &s); err == nil {
		v, ok := versionFromString(s)
		if !ok {
			return 0, c.field, errSchema(c.field, "unknown schema version "+strconv.Quote(s))
		}
		return v, c.field, nil
	}
	var n int
	if err := json.Unmarshal(c.value, &n); err == nil {
		v := SchemaVersion(n)
		if !IsKnownSchemaVersion(v) {
			return 0, c.field, errSchema(c.field, fmt.Sprintf("unknown schema version %d", n))
		}
		return v, c.field, nil
	}
	return 0, c.field, errSchema(c.field, "schema version must be a supported number or string")
}

func versionFromString(s string) (SchemaVersion, bool) {
	s = strings.ToLower(strings.TrimSpace(s))
	switch s {
	case "1", "v1", "terrain/v1", "terrain.v1", "terrain-1":
		return SchemaV1, true
	case "2", "v2", "terrain/v2", "terrain.v2", "terrain-2":
		return SchemaV2, true
	case "3", "v3", "terrain/v3", "terrain.v3", "terrain-3":
		return SchemaV3, true
	default:
		return 0, false
	}
}

func decodeStrict(data []byte, dst any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	if err := dec.Decode(dst); err != nil {
		return errDecode("$", "JSON decode failed: "+err.Error())
	}
	return nil
}

func req(path string, values ...any) error {
	for i, v := range values {
		present := false
		switch x := v.(type) {
		case *int64:
			present = x != nil
		case *int:
			present = x != nil
		case *uint64:
			present = x != nil
		case *string:
			present = x != nil
		case *float64:
			present = x != nil
		case []string:
			present = x != nil
		default:
			present = v != nil
		}
		if !present {
			return errSchema(path, fmt.Sprintf("required field at component %d is missing", i))
		}
	}
	return nil
}

func loadV1(data []byte, versionField string) (*Project, error) {
	var doc v1Doc
	if err := decodeStrict(data, &doc); err != nil {
		return nil, err
	}
	if err := req(versionField, doc.SchemaVersion); err != nil {
		return nil, err
	}
	if *doc.SchemaVersion != 1 {
		return nil, errSchema(versionField, "v1 loader received a different version")
	}
	if err := req("seed", doc.Seed); err != nil {
		return nil, err
	}
	if err := req("world", doc.World); err != nil {
		return nil, err
	}
	if err := req("world.min_x", doc.World.MinX); err != nil {
		return nil, err
	}
	if err := req("world.min_y", doc.World.MinY); err != nil {
		return nil, err
	}
	if err := req("world.max_x", doc.World.MaxX); err != nil {
		return nil, err
	}
	if err := req("world.max_y", doc.World.MaxY); err != nil {
		return nil, err
	}
	if err := req("world.boundary", doc.World.Boundary); err != nil {
		return nil, err
	}
	if len(doc.Chunks) == 0 {
		return nil, errSchema("chunks", "at least one chunk is required")
	}
	if doc.Truth == nil {
		return nil, errSchema("truth", "required field is missing")
	}
	p := &Project{SourceVersion: SchemaV1, GeneratedBy: GeneratedBy}
	worldIV, err := BoundaryToHalfOpen("world", *doc.World.MinX, *doc.World.MaxX, *doc.World.Boundary)
	if err != nil {
		return nil, err
	}
	yIV, err := BoundaryToHalfOpen("world.y", *doc.World.MinY, *doc.World.MaxY, *doc.World.Boundary)
	if err != nil {
		return nil, err
	}
	p.World = World{
		Rect: Rect{MinX: worldIV.Min, MinY: yIV.Min, MaxX: worldIV.Max, MaxY: yIV.Max},
		Seed: *doc.Seed,
		Name: "schema-v1",
	}
	p.Traces = append(p.Traces,
		ConversionTrace{SourceVersion: SchemaV1, Path: versionField, Operation: "schema_load", From: "v1", To: "internal", Note: "v1 numeric schema_version"},
		TraceBoundary(SchemaV1, "world", *doc.World.Boundary, worldIV),
	)
	for i, chunk := range doc.Chunks {
		path := fmt.Sprintf("chunks[%d]", i)
		if chunk.ID == nil || chunk.Bounds == nil || chunk.Budget == nil {
			return nil, errSchema(path, "id, bounds and budget are required")
		}
		if err := req(path+".id", chunk.ID); err != nil {
			return nil, err
		}
		if err := req(path+".bounds.min_x", chunk.Bounds.MinX); err != nil {
			return nil, err
		}
		if err := req(path+".bounds.min_y", chunk.Bounds.MinY); err != nil {
			return nil, err
		}
		if err := req(path+".bounds.max_x", chunk.Bounds.MaxX); err != nil {
			return nil, err
		}
		if err := req(path+".bounds.max_y", chunk.Bounds.MaxY); err != nil {
			return nil, err
		}
		if err := req(path+".bounds.boundary", chunk.Bounds.Boundary); err != nil {
			return nil, err
		}
		if err := req(path+".budget.max_vertices", chunk.Budget.MaxVertices); err != nil {
			return nil, err
		}
		if err := req(path+".budget.max_triangles", chunk.Budget.MaxTriangles); err != nil {
			return nil, err
		}
		if err := req(path+".chord_tol_permille", chunk.ChordTolPermille); err != nil {
			return nil, err
		}
		rounding := "half_up"
		if chunk.ToleranceRounding != nil {
			rounding = *chunk.ToleranceRounding
		}
		xIV, err := BoundaryToHalfOpen(path+".bounds.x", *chunk.Bounds.MinX, *chunk.Bounds.MaxX, *chunk.Bounds.Boundary)
		if err != nil {
			return nil, err
		}
		yIV, err := BoundaryToHalfOpen(path+".bounds.y", *chunk.Bounds.MinY, *chunk.Bounds.MaxY, *chunk.Bounds.Boundary)
		if err != nil {
			return nil, err
		}
		um, residual, err := ToleranceToUM(*chunk.ChordTolPermille, "permille", rounding)
		if err != nil {
			return nil, err
		}
		rect := Rect{MinX: xIV.Min, MinY: yIV.Min, MaxX: xIV.Max, MaxY: yIV.Max}
		p.Blocks = append(p.Blocks, BlockSpec{
			ID:                 *chunk.ID,
			Origin:             Point{X: rect.MinX, Y: rect.MinY},
			Size:               Size{W: rect.Width(), H: rect.Height()},
			Rect:               rect,
			Budget:             Budget{MaxVertices: *chunk.Budget.MaxVertices, MaxTriangles: *chunk.Budget.MaxTriangles},
			ToleranceUM:        um,
			ToleranceScale:     "permille",
			ToleranceRounding:  rounding,
			Neighbors:          append([]string(nil), chunk.Neighbors...),
			DeclaredNeighbors:  chunk.Neighbors != nil,
			SourceCoverageRule: *chunk.Bounds.Boundary,
		})
		p.Traces = append(p.Traces,
			TraceBoundary(SchemaV1, path+".bounds", *chunk.Bounds.Boundary, xIV),
			TraceTolerance(SchemaV1, path+".chord_tol_permille", "permille", rounding, *chunk.ChordTolPermille, um, residual),
		)
		seams, err := convertRawSeams(chunk.Seams, path+".seams")
		if err != nil {
			return nil, err
		}
		p.Seams = append(p.Seams, seams...)
	}
	p.Seams = dedupeSeams(p.Seams)
	if p.Seams == nil || len(p.Seams) == 0 {
		p.Seams = SeamSpecsFromBlocks(p.Blocks)
		p.Traces = append(p.Traces, ConversionTrace{
			SourceVersion: SchemaV1,
			Path:          "chunks[].seams",
			Operation:     "adjacency_inference",
			From:          "coverage",
			To:            "shared edges",
			Note:          "no explicit seam list; inferred from adjacent closed intervals",
		})
	}
	populateNeighborsFromSeams(p)
	if err := fillTruthV1(p, doc.Truth); err != nil {
		return nil, err
	}
	p.Camera = convertCameras(doc.CameraTrack, "camera_track")
	if p.Camera == nil {
		p.Camera = []CameraSample{}
	}
	if err := p.ValidateTopology(); err != nil {
		return nil, err
	}
	return p, nil
}

func loadV2(data []byte, versionField string) (*Project, error) {
	var doc v2Doc
	if err := decodeStrict(data, &doc); err != nil {
		return nil, err
	}
	if err := req(versionField, doc.Version); err != nil {
		return nil, err
	}
	if err := req("seed", doc.Seed); err != nil {
		return nil, err
	}
	if doc.World == nil {
		return nil, errSchema("world", "required field is missing")
	}
	if err := req("world.min_x", doc.World.MinX); err != nil {
		return nil, err
	}
	if err := req("world.min_y", doc.World.MinY); err != nil {
		return nil, err
	}
	if err := req("world.max_x", doc.World.MaxX); err != nil {
		return nil, err
	}
	if err := req("world.max_y", doc.World.MaxY); err != nil {
		return nil, err
	}
	if err := req("world.coverage", doc.World.Coverage); err != nil {
		return nil, err
	}
	if doc.Truth == nil {
		return nil, errSchema("truth", "required field is missing")
	}
	if len(doc.Chunks) == 0 {
		return nil, errSchema("chunks", "at least one chunk is required")
	}
	p := &Project{SourceVersion: SchemaV2, GeneratedBy: GeneratedBy}
	xIV, err := BoundaryToHalfOpen("world.x", *doc.World.MinX, *doc.World.MaxX, *doc.World.Coverage)
	if err != nil {
		return nil, err
	}
	yIV, err := BoundaryToHalfOpen("world.y", *doc.World.MinY, *doc.World.MaxY, *doc.World.Coverage)
	if err != nil {
		return nil, err
	}
	p.World = World{
		Rect: Rect{MinX: xIV.Min, MinY: yIV.Min, MaxX: xIV.Max, MaxY: yIV.Max},
		Seed: *doc.Seed,
		Name: "schema-v2",
	}
	p.Traces = append(p.Traces,
		ConversionTrace{SourceVersion: SchemaV2, Path: versionField, Operation: "schema_load", From: "*doc.Version", To: "internal", Note: "v2 string version field"},
		TraceBoundary(SchemaV2, "world", *doc.World.Coverage, xIV),
	)
	for i, chunk := range doc.Chunks {
		path := fmt.Sprintf("chunks[%d]", i)
		if chunk.ID == nil || chunk.Origin == nil || chunk.Extent == nil {
			return nil, errSchema(path, "id, origin and extent are required")
		}
		if chunk.Coverage == nil {
			return nil, errSchema(path+".coverage", "required field is missing")
		}
		if err := req(path+".origin.x", chunk.Origin.X); err != nil {
			return nil, err
		}
		if err := req(path+".origin.y", chunk.Origin.Y); err != nil {
			return nil, err
		}
		if err := req(path+".extent.w", chunk.Extent.W); err != nil {
			return nil, err
		}
		if err := req(path+".extent.h", chunk.Extent.H); err != nil {
			return nil, err
		}
		if err := req(path+".vertex_budget", chunk.VertexBudget); err != nil {
			return nil, err
		}
		if err := req(path+".triangle_budget", chunk.TriangleBudget); err != nil {
			return nil, err
		}
		if err := req(path+".chord_tolerance_bp", chunk.ChordTolBP); err != nil {
			return nil, err
		}
		rounding := "floor"
		if chunk.ToleranceRound != nil {
			rounding = *chunk.ToleranceRound
		}
		maxX, err := AddChecked(path+".origin.x+extent.w", *chunk.Origin.X, *chunk.Extent.W)
		if err != nil {
			return nil, err
		}
		maxY, err := AddChecked(path+".origin.y+extent.h", *chunk.Origin.Y, *chunk.Extent.H)
		if err != nil {
			return nil, err
		}
		xi, err := BoundaryToHalfOpen(path+".x", *chunk.Origin.X, maxX, *chunk.Coverage)
		if err != nil {
			return nil, err
		}
		yi, err := BoundaryToHalfOpen(path+".y", *chunk.Origin.Y, maxY, *chunk.Coverage)
		if err != nil {
			return nil, err
		}
		um, residual, err := ToleranceToUM(*chunk.ChordTolBP, "basis_points", rounding)
		if err != nil {
			return nil, err
		}
		// v2 declares a half-open origin/extent directly; the normalized
		// interval is required to agree with it.
		rect := Rect{
			MinX: *chunk.Origin.X,
			MinY: *chunk.Origin.Y,
			MaxX: *chunk.Origin.X + *chunk.Extent.W,
			MaxY: *chunk.Origin.Y + *chunk.Extent.H,
		}
		if xi.Min != rect.MinX || xi.Max != rect.MaxX || yi.Min != rect.MinY || yi.Max != rect.MaxY {
			return nil, errSchema(path+".coverage", "coverage normalization disagrees with origin/extent")
		}
		p.Blocks = append(p.Blocks, BlockSpec{
			ID:                 *chunk.ID,
			Origin:             Point{X: rect.MinX, Y: rect.MinY},
			Size:               Size{W: rect.Width(), H: rect.Height()},
			Rect:               rect,
			Budget:             Budget{MaxVertices: *chunk.VertexBudget, MaxTriangles: *chunk.TriangleBudget},
			ToleranceUM:        um,
			ToleranceScale:     "basis_points",
			ToleranceRounding:  rounding,
			DeclaredNeighbors:  false,
			SourceCoverageRule: *chunk.Coverage,
		})
		p.Traces = append(p.Traces,
			TraceBoundary(SchemaV2, path+".bounds", *chunk.Coverage, xi),
			TraceTolerance(SchemaV2, path+".chord_tolerance_bp", "basis_points", rounding, *chunk.ChordTolBP, um, residual),
		)
	}
	seams, err := convertRawSeams(doc.SeamConstraints, "seam_constraints")
	if err != nil {
		return nil, err
	}
	p.Seams = seams
	p.Traces = append(p.Traces, ConversionTrace{
		SourceVersion: SchemaV2,
		Path:          "chunks[].adjacency",
		Operation:     "adjacency_inference",
		From:          "omitted",
		To:            "coverage-derived bidirectional seams",
		Note:          "v2 has no adjacency table; seam constraints only describe boundary sampling",
	})
	if len(p.Seams) == 0 {
		p.Seams = SeamSpecsFromBlocks(p.Blocks)
	}
	populateNeighborsFromSeams(p)
	if err := fillTruthV2(p, doc.Truth); err != nil {
		return nil, err
	}
	p.Camera = convertCameras(doc.Camera, "camera")
	if p.Camera == nil {
		p.Camera = []CameraSample{}
	}
	if err := p.ValidateTopology(); err != nil {
		return nil, err
	}
	return p, nil
}

func loadV3(data []byte, versionField string) (*Project, error) {
	var doc v3Doc
	if err := decodeStrict(data, &doc); err != nil {
		return nil, err
	}
	if err := req(versionField, doc.Schema); err != nil {
		return nil, err
	}
	if err := req("seed", doc.Seed); err != nil {
		return nil, err
	}
	if doc.World == nil {
		return nil, errSchema("world_bounds_rel", "required field is missing")
	}
	if err := req("world_bounds_rel.min_u", doc.World.MinU); err != nil {
		return nil, err
	}
	if err := req("world_bounds_rel.min_v", doc.World.MinV); err != nil {
		return nil, err
	}
	if err := req("world_bounds_rel.max_u", doc.World.MaxU); err != nil {
		return nil, err
	}
	if err := req("world_bounds_rel.max_v", doc.World.MaxV); err != nil {
		return nil, err
	}
	if err := req("world_bounds_rel.convention", doc.World.Convention); err != nil {
		return nil, err
	}
	if doc.Truth == nil {
		return nil, errSchema("truth", "required field is missing")
	}
	if len(doc.Blocks) == 0 {
		return nil, errSchema("blocks", "at least one block is required")
	}
	p := &Project{SourceVersion: SchemaV3, GeneratedBy: GeneratedBy}
	xIV, err := BoundaryToHalfOpen("world.u", *doc.World.MinU, *doc.World.MaxU, *doc.World.Convention)
	if err != nil {
		return nil, err
	}
	yIV, err := BoundaryToHalfOpen("world.v", *doc.World.MinV, *doc.World.MaxV, *doc.World.Convention)
	if err != nil {
		return nil, err
	}
	worldOrigin := Point{X: xIV.Min, Y: yIV.Min}
	p.World = World{
		Rect: Rect{MinX: xIV.Min, MinY: yIV.Min, MaxX: xIV.Max, MaxY: yIV.Max},
		Seed: *doc.Seed,
		Name: "schema-v3",
	}
	p.Traces = append(p.Traces,
		ConversionTrace{SourceVersion: SchemaV3, Path: versionField, Operation: "schema_load", From: "*doc.Schema", To: "internal", Note: "v3 string schema field"},
		TraceBoundary(SchemaV3, "world_bounds_rel", *doc.World.Convention, xIV),
	)
	for i, block := range doc.Blocks {
		path := fmt.Sprintf("blocks[%d]", i)
		if block.ID == nil || block.BoundsRel == nil || block.Sampling == nil {
			return nil, errSchema(path, "id, bounds_rel and sampling are required")
		}
		if err := req(path+".bounds_rel.min_u", block.BoundsRel.MinU); err != nil {
			return nil, err
		}
		if err := req(path+".bounds_rel.min_v", block.BoundsRel.MinV); err != nil {
			return nil, err
		}
		if err := req(path+".bounds_rel.max_u", block.BoundsRel.MaxU); err != nil {
			return nil, err
		}
		if err := req(path+".bounds_rel.max_v", block.BoundsRel.MaxV); err != nil {
			return nil, err
		}
		if err := req(path+".bounds_rel.convention", block.BoundsRel.Convention); err != nil {
			return nil, err
		}
		if err := req(path+".sampling.max_vertices", block.Sampling.MaxVertices); err != nil {
			return nil, err
		}
		if err := req(path+".sampling.max_triangles", block.Sampling.MaxTriangles); err != nil {
			return nil, err
		}
		if err := req(path+".chord_tol_permille", block.ChordTolPermille); err != nil {
			return nil, err
		}
		rounding := "ceil"
		if block.ToleranceRounding != nil {
			rounding = *block.ToleranceRounding
		}
		absMinX, err := RelativeToAbsolute(path+".bounds_rel.min_u", *block.BoundsRel.MinU, worldOrigin.X)
		if err != nil {
			return nil, err
		}
		absMinY, err := RelativeToAbsolute(path+".bounds_rel.min_v", *block.BoundsRel.MinV, worldOrigin.Y)
		if err != nil {
			return nil, err
		}
		absMaxX, err := RelativeToAbsolute(path+".bounds_rel.max_u", *block.BoundsRel.MaxU, worldOrigin.X)
		if err != nil {
			return nil, err
		}
		absMaxY, err := RelativeToAbsolute(path+".bounds_rel.max_v", *block.BoundsRel.MaxV, worldOrigin.Y)
		if err != nil {
			return nil, err
		}
		xi, err := BoundaryToHalfOpen(path+".bounds_rel.u", absMinX, absMaxX, *block.BoundsRel.Convention)
		if err != nil {
			return nil, err
		}
		yi, err := BoundaryToHalfOpen(path+".bounds_rel.v", absMinY, absMaxY, *block.BoundsRel.Convention)
		if err != nil {
			return nil, err
		}
		um, residual, err := ToleranceToUM(*block.ChordTolPermille, "permille", rounding)
		if err != nil {
			return nil, err
		}
		rect := Rect{MinX: xi.Min, MinY: yi.Min, MaxX: xi.Max, MaxY: yi.Max}
		p.Blocks = append(p.Blocks, BlockSpec{
			ID:                 *block.ID,
			Origin:             Point{X: rect.MinX, Y: rect.MinY},
			Size:               Size{W: rect.Width(), H: rect.Height()},
			Rect:               rect,
			Budget:             Budget{MaxVertices: *block.Sampling.MaxVertices, MaxTriangles: *block.Sampling.MaxTriangles},
			ToleranceUM:        um,
			ToleranceScale:     "permille",
			ToleranceRounding:  rounding,
			Neighbors:          SortedUniqueStrings(block.Adjacent),
			DeclaredNeighbors:  block.Adjacent != nil,
			SourceCoverageRule: *block.BoundsRel.Convention,
		})
		p.Traces = append(p.Traces,
			TraceBoundary(SchemaV3, path+".bounds_rel", *block.BoundsRel.Convention, xi),
			TraceTolerance(SchemaV3, path+".chord_tol_permille", "permille", rounding, *block.ChordTolPermille, um, residual),
			ConversionTrace{
				SourceVersion: SchemaV3,
				Path:          path + ".bounds_rel",
				Operation:     "relative_to_absolute",
				From:          fmt.Sprintf("u=%d,v=%d", *block.BoundsRel.MinU, *block.BoundsRel.MinV),
				To:            fmt.Sprintf("x=%d,y=%d", absMinX, absMinY),
				Note:          "block bounds are relative to world origin",
			},
		)
		seams, err := convertRawSeams(block.Seams, path+".seams")
		if err != nil {
			return nil, err
		}
		p.Seams = append(p.Seams, seams...)
	}
	p.Seams = dedupeSeams(p.Seams)
	if len(p.Seams) == 0 {
		p.Seams = SeamSpecsFromBlocks(p.Blocks)
		p.Traces = append(p.Traces, ConversionTrace{
			SourceVersion: SchemaV3,
			Path:          "blocks[].seams",
			Operation:     "adjacency_inference",
			From:          "geometry",
			To:            "shared edges",
			Note:          "seam list omitted in v3 document",
		})
	}
	populateNeighborsFromSeams(p)
	if err := fillTruthV3(p, doc.Truth); err != nil {
		return nil, err
	}
	if doc.Camera != nil {
		p.Camera = convertCameras(doc.Camera.Samples, "camera.samples")
	}
	if p.Camera == nil {
		p.Camera = []CameraSample{}
	}
	if err := p.ValidateTopology(); err != nil {
		return nil, err
	}
	return p, nil
}

func dedupeSeams(in []SeamSpec) []SeamSpec {
	seen := map[string]bool{}
	out := make([]SeamSpec, 0, len(in))
	for _, s := range in {
		k := s.EdgeKey() + ":" + string(s.Axis)
		if seen[k] {
			continue
		}
		seen[k] = true
		out = append(out, s)
	}
	return out
}

func populateNeighborsFromSeams(p *Project) {
	for i := range p.Blocks {
		for _, s := range p.Seams {
			if s.A == p.Blocks[i].ID {
				p.Blocks[i].Neighbors = append(p.Blocks[i].Neighbors, s.B)
			} else if s.B == p.Blocks[i].ID {
				p.Blocks[i].Neighbors = append(p.Blocks[i].Neighbors, s.A)
			}
		}
		p.Blocks[i].Neighbors = SortedUniqueStrings(p.Blocks[i].Neighbors)
	}
}

func convertRawSeams(raws []rawSeam, path string) ([]SeamSpec, error) {
	var out []SeamSpec
	for i, r := range raws {
		p := fmt.Sprintf("%s[%d]", path, i)
		if err := req(p+".id", r.ID); err != nil {
			return nil, err
		}
		if err := req(p+".block_a", r.A); err != nil {
			return nil, err
		}
		if err := req(p+".block_b", r.B); err != nil {
			return nil, err
		}
		if err := req(p+".axis", r.Axis); err != nil {
			return nil, err
		}
		if err := req(p+".coordinate", r.Coordinate); err != nil {
			return nil, err
		}
		if err := req(p+".start", r.Start); err != nil {
			return nil, err
		}
		if err := req(p+".end", r.End); err != nil {
			return nil, err
		}
		axis := SeamAxis(strings.ToLower(strings.TrimSpace(*r.Axis)))
		if axis != AxisVertical && axis != AxisHorizontal {
			return nil, errSchema(p+".axis", "axis must be vertical or horizontal")
		}
		spacing := int64(DefaultSeamSpacing)
		if r.SampleSpacing != nil {
			spacing = *r.SampleSpacing
		}
		angle := 45.0
		if r.AngleLimitDeg != nil {
			angle = *r.AngleLimitDeg
		}
		out = append(out, SeamSpec{
			ID:            *r.ID,
			A:             *r.A,
			B:             *r.B,
			Axis:          axis,
			Coordinate:    *r.Coordinate,
			Start:         *r.Start,
			End:           *r.End,
			SampleSpacing: spacing,
			AngleLimitDeg: angle,
		})
	}
	return out, nil
}

func convertCameras(raws []rawCamera, path string) []CameraSample {
	out := make([]CameraSample, 0, len(raws))
	for i, r := range raws {
		if r.TimeMS == nil || r.PosX == nil || r.PosY == nil {
			continue
		}
		c := CameraSample{
			TimeMS:   *r.TimeMS,
			Position: Point{X: *r.PosX, Y: *r.PosY},
			YawDeg:   floatValue(r.Yaw),
			PitchDeg: floatValue(r.Pitch),
			FOVDeg:   floatValue(r.FOV),
			Near:     floatValue(r.Near),
			Far:      floatValue(r.Far),
		}
		if r.Focus != nil {
			c.Focus = *r.Focus
		}
		out = append(out, c)
		_ = i
		_ = path
	}
	return out
}

func floatValue(v *float64) float64 {
	if v == nil {
		return 0
	}
	return *v
}

func fillTruthV1(p *Project, t *rawTruthV1) error {
	if err := fillSamples(p, t.Vertices, false, false, "truth.vertices"); err != nil {
		return err
	}
	if t.MaxTriangles == nil {
		return errSchema("truth.max_triangles", "required field is missing")
	}
	p.Truth.MaxTriangles = t.MaxTriangles
	return fillSeamsAndFractures(p, t.Seams, t.Fractures)
}

func fillTruthV2(p *Project, t *rawTruthV2) error {
	if err := fillSamples(p, t.Vertices, true, false, "truth.vertices"); err != nil {
		return err
	}
	if t.TriangleCaps == nil {
		return errSchema("truth.triangle_caps", "required field is missing")
	}
	p.Truth.MaxTriangles = t.TriangleCaps
	return fillSeamsAndFractures(p, t.Seams, t.Fractures)
}

func fillTruthV3(p *Project, t *rawTruthV3) error {
	if err := fillSamples(p, t.Vertices, false, true, "truth.vertices"); err != nil {
		return err
	}
	if t.TriangleCaps == nil {
		return errSchema("truth.triangle_limits", "required field is missing")
	}
	p.Truth.MaxTriangles = t.TriangleCaps
	return fillSeamsAndFractures(p, t.Seams, t.Fractures)
}

func fillSamples(p *Project, raws []rawTruthSample, relativeU, relativeDU bool, path string) error {
	for i, r := range raws {
		ep := fmt.Sprintf("%s[%d]", path, i)
		if err := req(ep+".block", r.Block); err != nil {
			return err
		}
		if r.HeightMM == nil {
			return errSchema(ep+".height_mm", "required field is missing")
		}
		b, ok := p.BlockByID(*r.Block)
		if !ok {
			return errAdj(ep+".block", "block", "truth references unknown block "+*r.Block)
		}
		var x, y int64
		var err error
		switch {
		case relativeU && r.U != nil && r.V != nil:
			x, err = RelativeToAbsolute(ep+".u", *r.U, b.Rect.MinX)
			if err == nil {
				y, err = RelativeToAbsolute(ep+".v", *r.V, b.Rect.MinY)
			}
			p.Traces = append(p.Traces, ConversionTrace{
				SourceVersion: p.SourceVersion,
				Path:          ep,
				Operation:     "vertex_relative_to_absolute",
				From:          fmt.Sprintf("u=%d,v=%d", *r.U, *r.V),
				To:            fmt.Sprintf("x=%d,y=%d", x, y),
			})
		case relativeDU && r.DU != nil && r.DV != nil:
			x, err = RelativeToAbsolute(ep+".du", *r.DU, b.Rect.MinX)
			if err == nil {
				y, err = RelativeToAbsolute(ep+".dv", *r.DV, b.Rect.MinY)
			}
			p.Traces = append(p.Traces, ConversionTrace{
				SourceVersion: p.SourceVersion,
				Path:          ep,
				Operation:     "vertex_relative_to_absolute",
				From:          fmt.Sprintf("du=%d,dv=%d", *r.DU, *r.DV),
				To:            fmt.Sprintf("x=%d,y=%d", x, y),
			})
		case r.X != nil && r.Y != nil:
			x, y = *r.X, *r.Y
		default:
			return errSchema(ep, "sample coordinates are missing for this schema version")
		}
		if err != nil {
			return err
		}
		p.Truth.Samples = append(p.Truth.Samples, TruthSample{
			BlockID:  *r.Block,
			X:        x,
			Y:        y,
			HeightMM: *r.HeightMM,
		})
	}
	return nil
}

func fillSeamsAndFractures(p *Project, seams []rawSeamTruth, fractures []rawFractureTruth) error {
	for i, s := range seams {
		path := fmt.Sprintf("truth.seams[%d]", i)
		if err := req(path+".id", s.ID); err != nil {
			return err
		}
		if err := req(path+".axis", s.Axis); err != nil {
			return err
		}
		if err := req(path+".start", s.Start); err != nil {
			return err
		}
		if err := req(path+".end", s.End); err != nil {
			return err
		}
		p.Truth.Seams = append(p.Truth.Seams, SeamTruth{
			SeamID:    *s.ID,
			Axis:      SeamAxis(*s.Axis),
			Start:     *s.Start,
			End:       *s.End,
			Points:    append([]int64(nil), s.Points...),
			HeightsMM: append([]int64(nil), s.HeightsMM...),
		})
	}
	for i, f := range fractures {
		path := fmt.Sprintf("truth.fractures[%d]", i)
		if err := req(path+".id", f.ID); err != nil {
			return err
		}
		if err := req(path+".seam", f.SeamID); err != nil {
			return err
		}
		if err := req(path+".kind", f.Kind); err != nil {
			return err
		}
		ft := FractureTruth{ID: *f.ID, SeamID: *f.SeamID, Kind: *f.Kind}
		if f.At != nil {
			ft.At = *f.At
		}
		if f.ToleranceUM != nil {
			ft.ToleranceUM = *f.ToleranceUM
		}
		if f.AngleLimit != nil {
			ft.AngleLimit = *f.AngleLimit
		}
		if f.Expected != nil {
			ft.Expected = *f.Expected
		}
		p.Truth.Fractures = append(p.Truth.Fractures, ft)
	}
	return nil
}
