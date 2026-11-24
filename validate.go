package terrain

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"slices"
)

func LoadProject(path string) (*Project, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ParseProject(data)
}

func WriteProject(path string, project *Project) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(project, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

func ParseProject(data []byte) (*Project, error) {
	var root map[string]json.RawMessage
	if err := json.Unmarshal(data, &root); err != nil {
		return nil, NewSchemaError("$", nil, "invalid JSON object: %v", err)
	}

	p := &Project{}
	var err error
	if p.SchemaVersion, err = requiredString(root, "schema_version", "$.schema_version", nil); err != nil {
		return nil, err
	}
	if p.ProjectID, err = requiredString(root, "project_id", "$.project_id", nil); err != nil {
		return nil, err
	}
	if p.AlgorithmVersion, err = requiredString(root, "algorithm_version", "$.algorithm_version", nil); err != nil {
		return nil, err
	}
	if p.ParameterVersion, err = requiredString(root, "parameter_version", "$.parameter_version", nil); err != nil {
		return nil, err
	}
	if raw, ok := root["seed"]; ok && string(raw) != "null" {
		if p.Seed, err = decodeRaw[int64](raw, "$.seed", nil); err != nil {
			return nil, err
		}
	}

	blocksRaw, err := requiredRaw(root, "blocks", "$.blocks", nil)
	if err != nil {
		return nil, err
	}
	var rawBlocks []json.RawMessage
	if err := json.Unmarshal(blocksRaw, &rawBlocks); err != nil {
		return nil, NewSchemaError("$.blocks", nil, "must be an array of block objects: %v", err)
	}
	p.Blocks = make([]Block, 0, len(rawBlocks))
	for i, raw := range rawBlocks {
		block, bn, err := parseBlock(raw, i)
		if err != nil {
			return nil, err
		}
		p.Blocks = append(p.Blocks, block)
		_ = bn
	}

	if raw, ok := root["shared_boundaries"]; ok && string(raw) != "null" {
		if p.SharedBoundaries, err = decodeRaw[[]SharedBoundary](raw, "$.shared_boundaries", nil); err != nil {
			return nil, err
		}
	}
	if raw, ok := root["alignment_constraints"]; ok && string(raw) != "null" {
		if p.AlignmentConstraints, err = decodeRaw[[]AlignmentConstraint](raw, "$.alignment_constraints", nil); err != nil {
			return nil, err
		}
	}
	if raw, ok := root["truth"]; ok && string(raw) != "null" {
		if p.Truth, err = decodeRaw[Truth](raw, "$.truth", nil); err != nil {
			return nil, err
		}
	} else {
		return nil, NewSchemaError("$.truth", nil, "field is missing")
	}

	if err := ValidateProject(p); err != nil {
		return nil, err
	}
	return p, nil
}

func parseBlock(raw json.RawMessage, arrayIndex int) (Block, *int, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		bn := arrayIndex
		return Block{}, &bn, NewSchemaError(fmt.Sprintf("blocks[%d]", arrayIndex), &bn, "must be an object: %v", err)
	}
	bn := arrayIndex
	path := fmt.Sprintf("blocks[%d]", arrayIndex)
	b := Block{BlockNo: arrayIndex}

	if rawNo, ok := fields["block_no"]; ok && string(rawNo) != "null" {
		no, err := decodeRaw[int](rawNo, path+".block_no", &bn)
		if err != nil {
			return b, &bn, err
		}
		b.BlockNo = no
		bn = no
	} else {
		return b, &bn, NewSchemaError(path+".block_no", &bn, "field is missing")
	}

	var err error
	if b.ID, err = requiredString(fields, "id", path+".id", &bn); err != nil {
		return b, &bn, err
	}
	if rawFeature, ok := fields["feature"]; ok && string(rawFeature) != "null" {
		if b.Feature, err = decodeRaw[string](rawFeature, path+".feature", &bn); err != nil {
			return b, &bn, err
		}
	}
	if b.OriginX, err = requiredFloat(fields, "origin_x", path+".origin_x", &bn); err != nil {
		return b, &bn, err
	}
	if b.OriginZ, err = requiredFloat(fields, "origin_z", path+".origin_z", &bn); err != nil {
		return b, &bn, err
	}
	if b.SampleBudget, err = requiredInt(fields, "sample_budget", path+".sample_budget", &bn); err != nil {
		return b, &bn, err
	}
	if b.ErrorTolerance, err = requiredFloat(fields, "error_tolerance", path+".error_tolerance", &bn); err != nil {
		return b, &bn, err
	}
	if b.ErrorTolerance < 0 || math.IsNaN(b.ErrorTolerance) || math.IsInf(b.ErrorTolerance, 0) {
		return b, &bn, NewSchemaError(path+".error_tolerance", &bn, "must be a finite number >= 0")
	}
	if b.SampleBudget <= 0 {
		return b, &bn, NewSchemaError(path+".sample_budget", &bn, "must be a positive integer")
	}

	gridRaw, err := requiredRaw(fields, "grid", path+".grid", &bn)
	if err != nil {
		return b, &bn, err
	}
	if b.Grid, err = parseGrid(gridRaw, path+".grid", &bn); err != nil {
		return b, &bn, err
	}

	covRaw, err := requiredRaw(fields, "coverage_xz", path+".coverage_xz", &bn)
	if err != nil {
		return b, &bn, err
	}
	if b.CoverageXZ, err = decodeRaw[[4]float64](covRaw, path+".coverage_xz", &bn); err != nil {
		values, err2 := decodeRaw[[]float64](covRaw, path+".coverage_xz", &bn)
		if err2 != nil || len(values) != 4 {
			return b, &bn, NewSchemaError(path+".coverage_xz", &bn, "must contain exactly four [min_x,min_z,max_x,max_z] numbers")
		}
		copy(b.CoverageXZ[:], values)
	}

	if rawRP, ok := fields["required_points"]; ok && string(rawRP) != "null" {
		if b.RequiredPoints, err = decodeRaw[[]GridPoint](rawRP, path+".required_points", &bn); err != nil {
			return b, &bn, err
		}
	}
	return b, &bn, nil
}

func parseGrid(raw json.RawMessage, path string, bn *int) (Grid, error) {
	var fields map[string]json.RawMessage
	var g Grid
	if err := json.Unmarshal(raw, &fields); err != nil {
		return g, NewSchemaError(path, bn, "must be an object: %v", err)
	}
	var err error
	if g.Width, err = requiredInt(fields, "width", path+".width", bn); err != nil {
		return g, err
	}
	if g.Height, err = requiredInt(fields, "height", path+".height", bn); err != nil {
		return g, err
	}
	if g.CellSize, err = requiredFloat(fields, "cell_size", path+".cell_size", bn); err != nil {
		return g, err
	}
	if g.Width < 2 || g.Height < 2 {
		return g, NewSchemaError(path+".width/height", bn, "grid width and height must be at least 2")
	}
	if g.CellSize <= 0 || math.IsNaN(g.CellSize) || math.IsInf(g.CellSize, 0) {
		return g, NewSchemaError(path+".cell_size", bn, "must be a finite positive number")
	}
	heightsRaw, err := requiredRaw(fields, "heights", path+".heights", bn)
	if err != nil {
		return g, err
	}
	if g.Heights, err = decodeRaw[[]float64](heightsRaw, path+".heights", bn); err != nil {
		return g, err
	}
	want := g.Width * g.Height
	if len(g.Heights) != want {
		return g, NewSchemaError(path+".heights", bn, "length %d does not match width*height=%d", len(g.Heights), want)
	}
	for i, h := range g.Heights {
		if math.IsNaN(h) || math.IsInf(h, 0) {
			return g, NewSchemaError(fmt.Sprintf("%s.heights[%d]", path, i), bn, "height must be finite")
		}
	}
	return g, nil
}

func decodeRaw[T any](raw json.RawMessage, path string, bn *int) (T, error) {
	var zero T
	if len(raw) == 0 || string(raw) == "null" {
		return zero, NewSchemaError(path, bn, "field is missing or null")
	}
	if err := json.Unmarshal(raw, &zero); err != nil {
		return zero, NewSchemaError(path, bn, "invalid field type: %v", err)
	}
	return zero, nil
}

func requiredRaw(fields map[string]json.RawMessage, key, path string, bn *int) (json.RawMessage, error) {
	raw, ok := fields[key]
	if !ok || string(raw) == "null" {
		return nil, NewSchemaError(path, bn, "field %q is missing", key)
	}
	return raw, nil
}

func requiredString(fields map[string]json.RawMessage, key, path string, bn *int) (string, error) {
	raw, err := requiredRaw(fields, key, path, bn)
	if err != nil {
		return "", err
	}
	v, err := decodeRaw[string](raw, path, bn)
	if err != nil {
		return "", err
	}
	if v == "" {
		return "", NewSchemaError(path, bn, "must not be empty")
	}
	return v, nil
}

func requiredInt(fields map[string]json.RawMessage, key, path string, bn *int) (int, error) {
	raw, err := requiredRaw(fields, key, path, bn)
	if err != nil {
		return 0, err
	}
	return decodeRaw[int](raw, path, bn)
}

func requiredFloat(fields map[string]json.RawMessage, key, path string, bn *int) (float64, error) {
	raw, err := requiredRaw(fields, key, path, bn)
	if err != nil {
		return 0, err
	}
	return decodeRaw[float64](raw, path, bn)
}

func ValidateProject(p *Project) error {
	if p == nil {
		return NewSchemaError("$", nil, "project is nil")
	}
	if p.SchemaVersion != FormatVersion {
		return NewSchemaError("$.schema_version", nil, "unsupported schema %q, want %q", p.SchemaVersion, FormatVersion)
	}
	if len(p.Blocks) == 0 {
		return NewSchemaError("$.blocks", nil, "at least one block is required")
	}
	for i := range p.Blocks {
		b := &p.Blocks[i]
		bn := b.BlockNo
		path := fmt.Sprintf("blocks[%d]", i)
		if b.BlockNo != i {
			return NewSchemaError(path+".block_no", &bn, "block_no %d must equal array index %d", b.BlockNo, i)
		}
		if b.SampleBudget <= 0 {
			return NewSchemaError(path+".sample_budget", &bn, "must be a positive integer")
		}
		if b.ErrorTolerance < 0 {
			return NewSchemaError(path+".error_tolerance", &bn, "must be non-negative")
		}
		if b.Grid.Width < 2 || b.Grid.Height < 2 {
			return NewSchemaError(path+".grid", &bn, "width/height must be at least 2")
		}
		if len(b.Grid.Heights) != b.Grid.Width*b.Grid.Height {
			return NewSchemaError(path+".grid.heights", &bn, "length %d does not match width*height=%d", len(b.Grid.Heights), b.Grid.Width*b.Grid.Height)
		}
		wantCov := [4]float64{b.OriginX, b.OriginZ, b.OriginX + float64(b.Grid.Width-1)*b.Grid.CellSize, b.OriginZ + float64(b.Grid.Height-1)*b.Grid.CellSize}
		for k := range wantCov {
			if !floatEqual(b.CoverageXZ[k], wantCov[k]) {
				return NewSchemaError(path+".coverage_xz", &bn, "coverage %v is inconsistent with grid size/origin/cell; want %v", b.CoverageXZ, wantCov)
			}
		}
		for _, pt := range b.RequiredPoints {
			if !pointInGrid(pt, b.Grid) {
				return NewSchemaError(path+".required_points", &bn, "point %+v is outside grid", pt)
			}
		}
	}

	constraintByID := map[string]*AlignmentConstraint{}
	for i := range p.AlignmentConstraints {
		c := &p.AlignmentConstraints[i]
		bn := c.BlockA
		path := fmt.Sprintf("alignment_constraints[%d]", i)
		if c.ID == "" {
			return NewSchemaError(path+".id", &bn, "constraint id is missing")
		}
		if _, exists := constraintByID[c.ID]; exists {
			return NewSchemaError(path+".id", &bn, "duplicate constraint id %q", c.ID)
		}
		if err := validateBlockRef(p, c.BlockA, path+".block_a", &bn); err != nil {
			return err
		}
		bn = c.BlockB
		if err := validateBlockRef(p, c.BlockB, path+".block_b", &bn); err != nil {
			return err
		}
		if c.BlockA == c.BlockB {
			return NewSchemaError(path+".block_b", &bn, "constraint cannot reference the same block")
		}
		if !validSide(c.SideA) {
			return NewSchemaError(path+".side_a", &c.BlockA, "invalid side %q", c.SideA)
		}
		if !validSide(c.SideB) {
			return NewSchemaError(path+".side_b", &c.BlockB, "invalid side %q", c.SideB)
		}
		if len(c.SampleColumnA) < 2 || len(c.SampleColumnA) != len(c.SampleColumnB) {
			return NewSchemaError(path+".sample_column_a", &c.BlockA, "shared-edge sample columns must have equal length >= 2, got %d and %d", len(c.SampleColumnA), len(c.SampleColumnB))
		}
		edgeA := edgeLength(p.Blocks[c.BlockA].Grid, c.SideA)
		edgeB := edgeLength(p.Blocks[c.BlockB].Grid, c.SideB)
		if edgeA != edgeB {
			return NewSchemaError(path+".sample_column_a", &c.BlockA, "edge sample length mismatch: block %d %s has %d, block %d %s has %d", c.BlockA, c.SideA, edgeA, c.BlockB, c.SideB, edgeB)
		}
		for _, pos := range c.SampleColumnA {
			if pos < 0 || pos >= edgeA {
				return NewSchemaError(path+".sample_column_a", &c.BlockA, "edge sample %d outside 0..%d", pos, edgeA-1)
			}
		}
		for _, pos := range c.SampleColumnB {
			if pos < 0 || pos >= edgeB {
				return NewSchemaError(path+".sample_column_b", &c.BlockB, "edge sample %d outside 0..%d", pos, edgeB-1)
			}
		}
		if c.MaxFoldAngleDeg < 0 {
			return NewSchemaError(path+".max_fold_angle_deg", &c.BlockA, "must be non-negative")
		}
		if c.AlignmentBufferCells < 0 {
			return NewSchemaError(path+".alignment_buffer_cells", &c.BlockA, "must be non-negative")
		}
		constraintByID[c.ID] = c
	}

	boundaryIDs := map[string]bool{}
	for i := range p.SharedBoundaries {
		b := &p.SharedBoundaries[i]
		bn := b.BlockA
		path := fmt.Sprintf("shared_boundaries[%d]", i)
		if b.ID == "" {
			return NewSchemaError(path+".id", &bn, "boundary id is missing")
		}
		if boundaryIDs[b.ID] {
			return NewSchemaError(path+".id", &bn, "duplicate boundary id %q", b.ID)
		}
		boundaryIDs[b.ID] = true
		if err := validateBlockRef(p, b.BlockA, path+".block_a", &bn); err != nil {
			return err
		}
		bn = b.BlockB
		if err := validateBlockRef(p, b.BlockB, path+".block_b", &bn); err != nil {
			return err
		}
		if !validSide(b.SideA) || !validSide(b.SideB) {
			return NewSchemaError(path+".side", &b.BlockA, "invalid boundary side: %q/%q", b.SideA, b.SideB)
		}
		if b.ConstraintID == "" {
			return NewSchemaError(path+".constraint_id", &b.BlockA, "constraint reference is missing")
		}
		c, ok := constraintByID[b.ConstraintID]
		if !ok {
			return NewSchemaError(path+".constraint_id", &b.BlockA, "alignment constraint %q does not exist", b.ConstraintID)
		}
		if c.BlockA != b.BlockA || c.BlockB != b.BlockB || c.SideA != b.SideA || c.SideB != b.SideB {
			return NewSchemaError(path+".constraint_id", &b.BlockA, "constraint %q endpoints/sides do not match boundary", b.ConstraintID)
		}
		edgeLenA := edgeLength(p.Blocks[b.BlockA].Grid, b.SideA)
		edgeLenB := edgeLength(p.Blocks[b.BlockB].Grid, b.SideB)
		a0 := sidePoint(p.Blocks[b.BlockA].Grid, p.Blocks[b.BlockA].OriginX, p.Blocks[b.BlockA].OriginZ, b.SideA, 0)
		a1 := sidePoint(p.Blocks[b.BlockA].Grid, p.Blocks[b.BlockA].OriginX, p.Blocks[b.BlockA].OriginZ, b.SideA, edgeLenA-1)
		b0 := sidePoint(p.Blocks[b.BlockB].Grid, p.Blocks[b.BlockB].OriginX, p.Blocks[b.BlockB].OriginZ, b.SideB, 0)
		b1 := sidePoint(p.Blocks[b.BlockB].Grid, p.Blocks[b.BlockB].OriginX, p.Blocks[b.BlockB].OriginZ, b.SideB, edgeLenB-1)
		if horizontalDistance(a0, b0) > 1e-6 || horizontalDistance(a1, b1) > 1e-6 {
			return NewSchemaError(path, &b.BlockA, "shared-edge endpoint coordinates do not align: %s/%s vs %s/%s",
				xyz(a0), xyz(a1), xyz(b0), xyz(b1))
		}
	}

	for i, vt := range p.Truth.VertexChecks {
		if err := validateBlockRef(p, vt.BlockNo, fmt.Sprintf("truth.vertex_checks[%d].block_no", i), &vt.BlockNo); err != nil {
			return err
		}
		if !pointInGrid(GridPoint{X: vt.GridX, Y: vt.GridY}, p.Blocks[vt.BlockNo].Grid) {
			bn := vt.BlockNo
			return NewSchemaError(fmt.Sprintf("truth.vertex_checks[%d]", i), &bn, "grid point (%d,%d) outside block", vt.GridX, vt.GridY)
		}
		if vt.Tolerance < 0 {
			bn := vt.BlockNo
			return NewSchemaError(fmt.Sprintf("truth.vertex_checks[%d].tolerance", i), &bn, "must be non-negative")
		}
	}
	for i, be := range p.Truth.BlockErrorBounds {
		if err := validateBlockRef(p, be.BlockNo, fmt.Sprintf("truth.block_error_bounds[%d].block_no", i), &be.BlockNo); err != nil {
			return err
		}
		if be.DeclaredMaxHeightError < 0 {
			bn := be.BlockNo
			return NewSchemaError(fmt.Sprintf("truth.block_error_bounds[%d]", i), &bn, "declared error bound must be non-negative")
		}
	}
	for i, bc := range p.Truth.BoundaryChecks {
		if !boundaryIDs[bc.BoundaryID] {
			return NewSchemaError(fmt.Sprintf("truth.boundary_checks[%d].boundary_id", i), nil, "boundary reference %q is missing", bc.BoundaryID)
		}
		if bc.MaxAllowedFoldAngle < 0 {
			return NewSchemaError(fmt.Sprintf("truth.boundary_checks[%d]", i), nil, "max angle must be non-negative")
		}
	}
	return nil
}

func validateBlockRef(p *Project, no int, path string, bn *int) error {
	if no < 0 || no >= len(p.Blocks) {
		return NewSchemaError(path, bn, "block number %d is outside 0..%d", no, len(p.Blocks)-1)
	}
	return nil
}

func validSide(side string) bool {
	return slices.Contains([]string{SideNorth, SideSouth, SideEast, SideWest}, side)
}

func edgeLength(g Grid, side string) int {
	if side == SideEast || side == SideWest {
		return g.Height
	}
	return g.Width
}

func pointInGrid(pt GridPoint, g Grid) bool {
	return pt.X >= 0 && pt.X < g.Width && pt.Y >= 0 && pt.Y < g.Height
}

func gridIndex(g Grid, x, y int) int { return y*g.Width + x }

func gridHeight(g Grid, x, y int) float64 { return g.Heights[gridIndex(g, x, y)] }

func horizontalDistance(a, b Vertex) float64 {
	dx, dz := a.X-b.X, a.Z-b.Z
	return math.Hypot(dx, dz)
}

func xyz(v Vertex) string {
	return fmt.Sprintf("(%.3f,%.3f,%.3f)", v.X, v.Y, v.Z)
}
