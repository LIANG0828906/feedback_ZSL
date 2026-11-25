package loader

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"terrainpatch/internal/model"
)

type projectFile struct {
	SchemaVersion          *string  `json:"schemaVersion"`
	Seed                   *int64   `json:"seed"`
	FixedScale             *int64   `json:"fixedScale"`
	AngleThresholdMicroDeg *int64   `json:"angleThresholdMicroDeg"`
	TileIDs                []string `json:"tileIds"`
}

type declarationDTO struct {
	ID           *string         `json:"id"`
	Order        *int            `json:"order"`
	OriginX      *int64          `json:"originX"`
	OriginY      *int64          `json:"originY"`
	Coverage     *model.Coverage `json:"coverage"`
	SampleBudget *int            `json:"sampleBudget"`
	Tolerance    *int64          `json:"tolerance"`
	MaxTriangles *int            `json:"maxTriangles"`
	ParamVersion *int            `json:"paramVersion"`
	HeightMin    *int64          `json:"heightMin"`
	HeightMax    *int64          `json:"heightMax"`
}

type declarationsFile struct {
	Declarations map[string]declarationDTO `json:"declarations"`
}

type elevationsFile struct {
	Grids map[string]json.RawMessage `json:"grids"`
}

type gridDTO struct {
	Width    *int       `json:"width"`
	Height   *int       `json:"height"`
	CellSize *int64     `json:"cellSize"`
	Heights  *[][]int64 `json:"heights"`
}

type columnDTO struct {
	Order *int   `json:"order"`
	X     *int64 `json:"x"`
	Y     *int64 `json:"y"`
}

type sideDTO struct {
	TileID  *string      `json:"tileId"`
	Edge    *string      `json:"edge"`
	Columns *[]columnDTO `json:"columns"`
}

type boundaryDTO struct {
	ID                     *string  `json:"id"`
	Axis                   *string  `json:"axis"`
	Value                  *int64   `json:"value"`
	AngleThresholdMicroDeg *int64   `json:"angleThresholdMicroDeg"`
	SideA                  *sideDTO `json:"sideA"`
	SideB                  *sideDTO `json:"sideB"`
	OrderSignature         *string  `json:"orderSignature"`
}

type boundariesFile struct {
	Boundaries []boundaryDTO `json:"boundaries"`
}

func LoadProject(dir string) (*model.Project, *model.AppError) {
	var pf projectFile
	if err := readJSON(filepath.Join(dir, "project.json"), &pf); err != nil {
		return nil, err
	}
	if pf.SchemaVersion == nil || *pf.SchemaVersion == "" {
		return nil, schemaErr("project.schemaVersion missing")
	}
	if *pf.SchemaVersion != model.SchemaVersion {
		return nil, schemaErr(fmt.Sprintf("unsupported schema %q", *pf.SchemaVersion))
	}
	if pf.Seed == nil {
		return nil, schemaErr("project.seed missing")
	}
	if pf.FixedScale == nil || *pf.FixedScale <= 0 {
		return nil, metaErr("", "fixedScale", "fixedScale must be positive").WithActual(pf.FixedScale)
	}
	if pf.AngleThresholdMicroDeg == nil || *pf.AngleThresholdMicroDeg < 0 {
		return nil, metaErr("", "angleThresholdMicroDeg", "angleThresholdMicroDeg must be non-negative")
	}
	if len(pf.TileIDs) == 0 {
		return nil, schemaErr("project.tileIds missing or empty")
	}

	var declFile declarationsFile
	if err := readJSON(filepath.Join(dir, "declarations.json"), &declFile); err != nil {
		return nil, err
	}
	var elevFile elevationsFile
	if err := readJSON(filepath.Join(dir, "elevations.json"), &elevFile); err != nil {
		return nil, err
	}
	var bFile boundariesFile
	if err := readJSON(filepath.Join(dir, "boundaries.json"), &bFile); err != nil {
		return nil, err
	}

	truthPath := filepath.Join(dir, "truth.json")
	var truth model.Truth
	if _, statErr := os.Stat(truthPath); statErr != nil {
		return nil, schemaErr("truth.json missing")
	}
	if err := readJSON(truthPath, &truth); err != nil {
		return nil, err
	}

	probesPath := filepath.Join(dir, "probes.json")
	var probes struct {
		Probes []model.AttributionProbe `json:"probes"`
	}
	if _, err := os.Stat(probesPath); err == nil {
		if jerr := readJSON(probesPath, &probes); jerr != nil {
			return nil, jerr
		}
	}

	p := &model.Project{
		SchemaVersion:          *pf.SchemaVersion,
		Seed:                   *pf.Seed,
		FixedScale:             *pf.FixedScale,
		AngleThresholdMicroDeg: *pf.AngleThresholdMicroDeg,
		Truth:                  truth,
		Probes:                 probes.Probes,
		Dir:                    dir,
	}

	for _, id := range pf.TileIDs {
		d, ok := declFile.Declarations[id]
		if !ok {
			return nil, schemaErr("declaration missing for tile "+id).WithTile(id, "declarations")
		}
		tile, err := convertDeclaration(id, d)
		if err != nil {
			return nil, err
		}
		rawGrid, ok := elevFile.Grids[id]
		if !ok {
			return nil, schemaErr("elevation grid missing").WithTile(id, "grids")
		}
		var gd gridDTO
		if jerr := json.Unmarshal(rawGrid, &gd); jerr != nil {
			return nil, schemaErr("invalid elevation grid: "+jerr.Error()).WithTile(id, "grid")
		}
		grid, gerr := convertGrid(id, gd)
		if gerr != nil {
			return nil, gerr
		}
		tile.Grid = grid
		if cerr := validateCoverage(*tile); cerr != nil {
			return nil, cerr
		}
		p.Tiles = append(p.Tiles, *tile)
	}

	if err := validateCoverageConflicts(p.Tiles); err != nil {
		return nil, err
	}

	for _, bd := range bFile.Boundaries {
		b, err := convertBoundary(bd)
		if err != nil {
			return nil, err
		}
		if err := validateBoundary(p, b); err != nil {
			return nil, err
		}
		p.Boundaries = append(p.Boundaries, b)
	}
	if err := validateTruth(p); err != nil {
		return nil, err
	}
	return p, nil
}

func readJSON(path string, target any) *model.AppError {
	b, err := os.ReadFile(path)
	if err != nil {
		return model.NewError(model.CodeSchema, "cannot read "+filepath.Base(path)+": "+err.Error()).
			WithDetails(map[string]any{"path": path})
	}
	if err := json.Unmarshal(b, target); err != nil {
		return model.NewError(model.CodeSchema, "invalid JSON in "+filepath.Base(path)+": "+err.Error()).
			WithDetails(map[string]any{"path": path})
	}
	return nil
}

func schemaErr(msg string) *model.AppError {
	return model.NewError(model.CodeSchema, msg)
}

func metaErr(tile, field, msg string) *model.AppError {
	return model.NewError(model.CodeMeta, msg).WithTile(tile, field)
}

func convertDeclaration(id string, d declarationDTO) (*model.Tile, *model.AppError) {
	if d.ID != nil && *d.ID != "" && *d.ID != id {
		return nil, metaErr(id, "id", "declaration id does not match map key")
	}
	if d.Order == nil {
		return nil, schemaErr("order missing").WithTile(id, "order")
	}
	if d.OriginX == nil {
		return nil, schemaErr("originX missing").WithTile(id, "originX")
	}
	if d.OriginY == nil {
		return nil, schemaErr("originY missing").WithTile(id, "originY")
	}
	if d.Coverage == nil {
		return nil, schemaErr("coverage missing").WithTile(id, "coverage")
	}
	if d.SampleBudget == nil {
		return nil, schemaErr("sampleBudget missing").WithTile(id, "sampleBudget")
	}
	if *d.SampleBudget <= 0 {
		return nil, metaErr(id, "sampleBudget", "sampleBudget must be a positive integer").WithActual(*d.SampleBudget)
	}
	if d.Tolerance == nil {
		return nil, schemaErr("tolerance missing").WithTile(id, "tolerance")
	}
	if *d.Tolerance < 0 {
		return nil, metaErr(id, "tolerance", "tolerance must be non-negative").WithActual(*d.Tolerance)
	}
	if d.MaxTriangles == nil {
		return nil, schemaErr("maxTriangles missing").WithTile(id, "maxTriangles")
	}
	if *d.MaxTriangles <= 0 {
		return nil, metaErr(id, "maxTriangles", "maxTriangles must be positive").WithActual(*d.MaxTriangles)
	}
	if d.ParamVersion == nil || *d.ParamVersion < 1 {
		return nil, metaErr(id, "paramVersion", "paramVersion must be >= 1").WithActual(d.ParamVersion)
	}
	if d.HeightMin == nil || d.HeightMax == nil {
		return nil, schemaErr("heightMin/heightMax missing").WithTile(id, "heightRange")
	}
	if *d.HeightMin > *d.HeightMax {
		return nil, metaErr(id, "heightRange", "heightMin must be <= heightMax")
	}
	return &model.Tile{
		ID: id, Order: *d.Order, OriginX: *d.OriginX, OriginY: *d.OriginY,
		Coverage: *d.Coverage, SampleBudget: *d.SampleBudget, Tolerance: *d.Tolerance,
		MaxTriangles: *d.MaxTriangles, ParamVersion: *d.ParamVersion,
		HeightMin: *d.HeightMin, HeightMax: *d.HeightMax,
	}, nil
}

func convertGrid(id string, gd gridDTO) (model.Grid, *model.AppError) {
	if gd.Width == nil || gd.Height == nil || gd.CellSize == nil || gd.Heights == nil {
		return model.Grid{}, schemaErr("grid width, height, cellSize and heights are required").WithTile(id, "grid")
	}
	if *gd.Width <= 0 || *gd.Height <= 0 {
		return model.Grid{}, metaErr(id, "grid.width", "grid dimensions must be positive").WithActual([2]int{*gd.Width, *gd.Height})
	}
	if *gd.CellSize <= 0 {
		return model.Grid{}, metaErr(id, "grid.cellSize", "cellSize must be positive").WithActual(*gd.CellSize)
	}
	h := *gd.Heights
	if len(h) != *gd.Height {
		return model.Grid{}, metaErr(id, "grid.heights", "height row count does not match grid.height").
			WithActual(len(h)).WithLimit(*gd.Height)
	}
	for y, row := range h {
		if len(row) != *gd.Width {
			return model.Grid{}, metaErr(id, "grid.heights", fmt.Sprintf("row %d length does not match grid.width", y)).
				WithActual(len(row)).WithLimit(*gd.Width)
		}
	}
	return model.Grid{Width: *gd.Width, Height: *gd.Height, CellSize: *gd.CellSize, Heights: h}, nil
}

func validateCoverage(t model.Tile) *model.AppError {
	wantMaxX := t.OriginX + int64(t.Grid.Width-1)*t.Grid.CellSize
	wantMaxY := t.OriginY + int64(t.Grid.Height-1)*t.Grid.CellSize
	c := t.Coverage
	if c.MinX != t.OriginX || c.MinY != t.OriginY || c.MaxX != wantMaxX || c.MaxY != wantMaxY {
		return metaErr(t.ID, "coverage", "coverage does not match grid dimensions").
			WithActual(c).WithLimit(model.Coverage{MinX: t.OriginX, MinY: t.OriginY, MaxX: wantMaxX, MaxY: wantMaxY})
	}
	for y := 0; y < t.Grid.Height; y++ {
		for x := 0; x < t.Grid.Width; x++ {
			z := t.Grid.Heights[y][x]
			if z < t.HeightMin || z > t.HeightMax {
				return metaErr(t.ID, "grid.heights", "height is outside declared bounds").
					WithActual(map[string]any{"x": x, "y": y, "z": z}).
					WithLimit([2]int64{t.HeightMin, t.HeightMax})
			}
		}
	}
	return nil
}

func validateCoverageConflicts(tiles []model.Tile) *model.AppError {
	for i := 0; i < len(tiles); i++ {
		for j := i + 1; j < len(tiles); j++ {
			a, b := tiles[i].Coverage, tiles[j].Coverage
			if a.MinX < b.MaxX && a.MaxX > b.MinX && a.MinY < b.MaxY && a.MaxY > b.MinY {
				return metaErr(tiles[i].ID, "coverage", "tile coverage overlaps another tile").
					WithDetails(map[string]any{"otherTile": tiles[j].ID, "coverage": a, "otherCoverage": b})
			}
		}
	}
	return nil
}

func convertBoundary(d boundaryDTO) (model.Boundary, *model.AppError) {
	if d.ID == nil || *d.ID == "" {
		return model.Boundary{}, schemaErr("boundary.id missing")
	}
	id := *d.ID
	if d.Axis == nil || (*d.Axis != "vertical" && *d.Axis != "horizontal") {
		return model.Boundary{}, schemaErr("boundary.axis must be vertical or horizontal").WithBoundary(id, "axis")
	}
	if d.Value == nil {
		return model.Boundary{}, schemaErr("boundary.value missing").WithBoundary(id, "value")
	}
	if d.SideA == nil || d.SideB == nil {
		return model.Boundary{}, schemaErr("both boundary sides are required").WithBoundary(id, "sides")
	}
	a, err := convertSide(id, "sideA", *d.SideA)
	if err != nil {
		return model.Boundary{}, err
	}
	b, err := convertSide(id, "sideB", *d.SideB)
	if err != nil {
		return model.Boundary{}, err
	}
	if d.OrderSignature == nil || *d.OrderSignature == "" {
		return model.Boundary{}, schemaErr("orderSignature missing").WithBoundary(id, "orderSignature")
	}
	threshold := int64(100_000)
	if d.AngleThresholdMicroDeg != nil {
		threshold = *d.AngleThresholdMicroDeg
	}
	return model.Boundary{
		ID: id, Axis: *d.Axis, Value: *d.Value, AngleThresholdMicroDeg: threshold,
		SideA: a, SideB: b, OrderSignature: *d.OrderSignature,
	}, nil
}

func convertSide(bid, field string, d sideDTO) (model.BoundarySide, *model.AppError) {
	if d.TileID == nil || *d.TileID == "" {
		return model.BoundarySide{}, schemaErr(field+".tileId missing").WithBoundary(bid, field+".tileId")
	}
	if d.Edge == nil || !validEdge(*d.Edge) {
		return model.BoundarySide{}, schemaErr(field+".edge invalid").WithBoundary(bid, field+".edge")
	}
	if d.Columns == nil || len(*d.Columns) == 0 {
		return model.BoundarySide{}, schemaErr(field+".columns missing or empty").WithBoundary(bid, field+".columns").WithTile(*d.TileID, "boundary")
	}
	cols := make([]model.BoundaryColumn, 0, len(*d.Columns))
	for i, c := range *d.Columns {
		if c.Order == nil || c.X == nil || c.Y == nil {
			return model.BoundarySide{}, schemaErr(fmt.Sprintf("%s.columns[%d] order/x/y required", field, i)).
				WithBoundary(bid, field+".columns")
		}
		cols = append(cols, model.BoundaryColumn{Order: *c.Order, X: *c.X, Y: *c.Y})
	}
	return model.BoundarySide{TileID: *d.TileID, Edge: *d.Edge, Columns: cols}, nil
}

func validEdge(e string) bool {
	return e == "left" || e == "right" || e == "top" || e == "bottom"
}

func validateBoundary(p *model.Project, b model.Boundary) *model.AppError {
	ta := p.Tile(b.SideA.TileID)
	tb := p.Tile(b.SideB.TileID)
	if ta == nil || tb == nil {
		return schemaErr("boundary references unknown tile").WithBoundary(b.ID, "sides").
			WithDetails(map[string]any{"tileA": b.SideA.TileID, "tileB": b.SideB.TileID})
	}
	lineA, err := edgeLine(*ta, b.SideA.Edge)
	if err != nil {
		return err.WithBoundary(b.ID, b.SideA.TileID)
	}
	lineB, err := edgeLine(*tb, b.SideB.Edge)
	if err != nil {
		return err.WithBoundary(b.ID, b.SideB.TileID)
	}
	if lineA != lineB {
		return metaErr("", "coverage", "shared boundary edges do not describe the same line").
			WithBoundary(b.ID, "coverage").WithDetails(map[string]any{"lineA": lineA, "lineB": lineB})
	}
	constant := lineA[0]
	if b.Axis == "horizontal" {
		constant = lineA[1]
	}
	if b.Value != constant {
		return metaErr("", "value", "boundary value does not match tile coverage").WithBoundary(b.ID, "value").
			WithActual(b.Value).WithLimit(constant)
	}

	ca := normalizedColumns(b.SideA.Columns)
	cb := normalizedColumns(b.SideB.Columns)
	if len(ca) != len(cb) {
		return metaErr("", "columns", "shared edge boundary sample column lengths differ").
			WithBoundary(b.ID, "columns").
			WithTile(b.SideA.TileID+"/"+b.SideB.TileID, "columns").
			WithActual(map[string]int{"sideA": len(ca), "sideB": len(cb)})
	}
	for i := range ca {
		if ca[i].X != cb[i].X || ca[i].Y != cb[i].Y {
			return metaErr("", "order", "shared edge vertex order/coordinates are inconsistent").
				WithBoundary(b.ID, "order").
				WithTile(b.SideA.TileID+"/"+b.SideB.TileID, "order").
				WithActual(map[string]any{"index": i, "sideA": ca[i], "sideB": cb[i]})
		}
	}
	sig := model.DigestJSON(ca)[:16]
	if b.OrderSignature != sig {
		return metaErr("", "orderSignature", "declared order signature does not match columns").
			WithBoundary(b.ID, "orderSignature").WithActual(b.OrderSignature).WithLimit(sig)
	}
	for _, c := range ca {
		if c.X < ta.Coverage.MinX || c.X > ta.Coverage.MaxX || c.Y < ta.Coverage.MinY || c.Y > ta.Coverage.MaxY {
			return metaErr(ta.ID, "columns", "boundary column outside tile coverage").WithBoundary(b.ID, "columns")
		}
	}
	return nil
}

func edgeLine(t model.Tile, edge string) ([4]int64, *model.AppError) {
	c := t.Coverage
	switch edge {
	case "left":
		return [4]int64{c.MinX, c.MinY, c.MinX, c.MaxY}, nil
	case "right":
		return [4]int64{c.MaxX, c.MinY, c.MaxX, c.MaxY}, nil
	case "top":
		return [4]int64{c.MinX, c.MinY, c.MaxX, c.MinY}, nil
	case "bottom":
		return [4]int64{c.MinX, c.MaxY, c.MaxX, c.MaxY}, nil
	}
	return [4]int64{}, schemaErr("invalid edge "+edge).WithTile(t.ID, "edge")
}

func normalizedColumns(cols []model.BoundaryColumn) []model.BoundaryColumn {
	out := append([]model.BoundaryColumn{}, cols...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].Order < out[j].Order })
	for i := range out {
		out[i].Order = i
	}
	return out
}

func validateTruth(p *model.Project) *model.AppError {
	if len(p.Truth.ExpectedHeights) == 0 {
		return schemaErr("truth.expectedHeights missing")
	}
	for _, h := range p.Truth.ExpectedHeights {
		t := p.Tile(h.TileID)
		if t == nil {
			return schemaErr("truth expected height references unknown tile").WithTile(h.TileID, "truth")
		}
		gx := int((h.X - t.OriginX) / t.Grid.CellSize)
		gy := int((h.Y - t.OriginY) / t.Grid.CellSize)
		if gx < 0 || gx >= t.Grid.Width || gy < 0 || gy >= t.Grid.Height {
			return metaErr(t.ID, "truth.expectedHeights", "truth coordinate outside grid").WithActual(h)
		}
		if t.Grid.Heights[gy][gx] != h.Z {
			return metaErr(t.ID, "truth.expectedHeights", "truth height does not equal generated grid").
				WithActual(t.Grid.Heights[gy][gx]).WithLimit(h.Z)
		}
	}
	if len(p.Truth.TriangleLimits) != len(p.Tiles) {
		return schemaErr("truth.triangleLimits must contain every tile")
	}
	for _, l := range p.Truth.TriangleLimits {
		t := p.Tile(l.TileID)
		if t == nil {
			return schemaErr("truth triangle limit references unknown tile").WithTile(l.TileID, "truth")
		}
		if l.MaxTriangles != t.MaxTriangles {
			return metaErr(t.ID, "truth.triangleLimits", "truth maxTriangles does not match declaration")
		}
	}
	for _, seq := range p.Truth.AlignedSequences {
		b := p.Boundary(seq.BoundaryID)
		if b == nil {
			return schemaErr("truth aligned sequence references unknown boundary").WithBoundary(seq.BoundaryID, "truth")
		}
		if len(seq.Columns) != len(b.SideA.Columns) {
			return metaErr("", "truth.alignedSequences", "truth aligned sequence length mismatch").WithBoundary(b.ID, "truth")
		}
	}
	return nil
}
