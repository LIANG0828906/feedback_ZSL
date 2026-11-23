package project

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
)

// Load reads a deterministic terrain project rooted at the directory that
// contains manifest.json. Edits are replayed from edits.json when present.
func Load(root string) (*Project, *Error) {
	absRoot, err := filepath.Abs(root)
	if err != nil {
		return nil, NewError(CodeDecode, "", "root", "resolve project root: %v", err)
	}
	manifestPath := filepath.Join(absRoot, "manifest.json")
	var wm wireManifest
	if e := decodeJSONFile(manifestPath, &wm, "manifest.json"); e != nil {
		return nil, e
	}

	manifest, e := manifestFromWire(&wm)
	if e != nil {
		return nil, e
	}

	p := &Project{
		Root:       absRoot,
		Manifest:   manifest,
		Blocks:     make(map[string]*BlockData, len(manifest.Blocks)),
		BlockOrder: make([]string, 0, len(manifest.Blocks)),
	}
	seenIDs := make(map[string]bool, len(manifest.Blocks))
	seenPos := make(map[string]string, len(manifest.Blocks))
	for i := range manifest.Blocks {
		block := manifest.Blocks[i]
		if seenIDs[block.ID] {
			return nil, NewError(CodeSchema, block.ID, "id", "duplicate block identifier")
		}
		seenIDs[block.ID] = true
		posKey := fmt.Sprintf("%d:%d", block.Col, block.Row)
		if other, ok := seenPos[posKey]; ok {
			return nil, NewError(CodeSchema, block.ID, "col,row", "block position duplicates %s", other)
		}
		seenPos[posKey] = block.ID
		if e := validateBlockAgainstManifest(&block, manifest); e != nil {
			return nil, e
		}
		bd, e := loadBlockData(absRoot, block, manifest)
		if e != nil {
			return nil, e
		}
		p.Blocks[block.ID] = bd
		p.BlockOrder = append(p.BlockOrder, block.ID)
	}
	if len(p.BlockOrder) == 0 {
		return nil, NewError(CodeSchema, "", "blocks", "project must contain at least one block")
	}

	if e := validateSeams(manifest, p); e != nil {
		return nil, e
	}
	truth, e := loadTruth(absRoot, manifest, p)
	if e != nil {
		return nil, e
	}
	p.Truth = truth

	edits, e := loadEdits(absRoot, p)
	if e != nil {
		return nil, e
	}
	p.Edits = edits
	p.TruthStale = len(edits) > 0
	p.RecomputeContentHash()
	return p, nil
}

func decodeJSONFile(path string, dest any, field string) *Error {
	data, err := os.ReadFile(path)
	if err != nil {
		return NewError(CodeDecode, "", field, "read file: %v", err)
	}
	if err := json.Unmarshal(data, dest); err != nil {
		var ute *json.UnmarshalTypeError
		if errors.As(err, &ute) {
			f := ute.Field
			if f == "" {
				f = field
			}
			return NewError(CodeSchema, "", f, "invalid JSON field type: %v", err)
		}
		return NewError(CodeDecode, "", field, "decode JSON: %v", err)
	}
	return nil
}

func manifestFromWire(w *wireManifest) (Manifest, *Error) {
	var m Manifest
	if w.SchemaVersion == nil {
		return m, missing("", "schemaVersion")
	}
	m.SchemaVersion = *w.SchemaVersion
	if m.SchemaVersion != 1 {
		return m, NewError(CodeSchema, "", "schemaVersion", "unsupported schema version %d", m.SchemaVersion)
	}
	if w.Seed == nil {
		return m, missing("", "seed")
	}
	m.Seed = *w.Seed
	if w.Grid == nil {
		return m, missing("", "grid")
	}
	var e *Error
	if m.Grid, e = gridFromWire(w.Grid, ""); e != nil {
		return m, e
	}
	if w.Materials == nil {
		return m, missing("", "materials")
	}
	if len(*w.Materials) == 0 {
		return m, NewError(CodeSchema, "", "materials", "at least one material layer is required")
	}
	m.Materials = make([]Material, len(*w.Materials))
	for i, wmat := range *w.Materials {
		if wmat.ID == nil {
			return m, missing("", fmt.Sprintf("materials[%d].id", i))
		}
		if wmat.Name == nil {
			return m, missing("", fmt.Sprintf("materials[%d].name", i))
		}
		m.Materials[i].ID = *wmat.ID
		m.Materials[i].Name = *wmat.Name
		if wmat.Color != nil {
			m.Materials[i].Color = *wmat.Color
		} else {
			m.Materials[i].Color = [3]float64{0.5, 0.5, 0.5}
		}
		if m.Materials[i].ID != i {
			return m, NewError(CodeSchema, "", fmt.Sprintf("materials[%d].id", i), "material id must equal layer index %d", i)
		}
		for _, c := range m.Materials[i].Color {
			if !finite(c) || c < 0 || c > 1 {
				return m, NewError(CodeSchema, "", fmt.Sprintf("materials[%d].color", i), "color components must be finite and in [0,1]")
			}
		}
	}
	if w.Bake == nil {
		return m, missing("", "bake")
	}
	if m.Bake, e = bakeFromWire(w.Bake); e != nil {
		return m, e
	}
	if w.Blocks == nil {
		return m, missing("", "blocks")
	}
	if w.Seams == nil {
		return m, missing("", "seams")
	}
	if w.TruthFile == nil {
		return m, missing("", "truthFile")
	}
	m.TruthFile = *w.TruthFile
	if m.TruthFile == "" {
		return m, missing("", "truthFile")
	}
	m.Blocks = make([]Block, len(*w.Blocks))
	for i := range *w.Blocks {
		b, e := blockFromWire(&(*w.Blocks)[i], i)
		if e != nil {
			return m, e
		}
		m.Blocks[i] = b
	}
	m.Seams = make([]Seam, len(*w.Seams))
	for i := range *w.Seams {
		s, e := seamFromWire(&(*w.Seams)[i], i)
		if e != nil {
			return m, e
		}
		m.Seams[i] = s
	}
	return m, nil
}

func gridFromWire(w *wireGrid, prefix string) (GridConfig, *Error) {
	var g GridConfig
	if w.Cols == nil {
		return g, missingField(prefix, "cols")
	}
	if w.Rows == nil {
		return g, missingField(prefix, "rows")
	}
	if w.BlockCols == nil {
		return g, missingField(prefix, "blockCols")
	}
	if w.BlockRows == nil {
		return g, missingField(prefix, "blockRows")
	}
	if w.Spacing == nil {
		return g, missingField(prefix, "spacing")
	}
	g = GridConfig{
		Cols:      *w.Cols,
		Rows:      *w.Rows,
		BlockCols: *w.BlockCols,
		BlockRows: *w.BlockRows,
		Spacing:   *w.Spacing,
	}
	switch {
	case g.Cols <= 0:
		return g, NewError(CodeSchema, "", field(prefix, "cols"), "grid cols must be positive")
	case g.Rows <= 0:
		return g, NewError(CodeSchema, "", field(prefix, "rows"), "grid rows must be positive")
	case g.BlockCols < 2:
		return g, NewError(CodeSchema, "", field(prefix, "blockCols"), "blockCols must be at least 2")
	case g.BlockRows < 2:
		return g, NewError(CodeSchema, "", field(prefix, "blockRows"), "blockRows must be at least 2")
	case !finite(g.Spacing) || g.Spacing <= 0:
		return g, NewError(CodeSchema, "", field(prefix, "spacing"), "spacing must be finite and positive")
	}
	return g, nil
}

func bakeFromWire(w *wireBake) (BakeConfig, *Error) {
	var b BakeConfig
	if w.Subdivisions == nil {
		return b, missing("", "bake.subdivisions")
	}
	if w.QuantMax == nil {
		return b, missing("", "bake.quantMax")
	}
	if w.SeamMaxIterations == nil {
		return b, missing("", "bake.seamMaxIterations")
	}
	if w.SeamAngleThresholdDeg == nil {
		return b, missing("", "bake.seamAngleThresholdDeg")
	}
	if w.SeamMaterialBandCells == nil {
		return b, missing("", "bake.seamMaterialBandCells")
	}
	if w.ParamsVersion == nil {
		return b, missing("", "bake.paramsVersion")
	}
	if w.MaxWorkers == nil {
		return b, missing("", "bake.maxWorkers")
	}
	if w.MaxVerticesPerThread == nil {
		return b, missing("", "bake.maxVerticesPerThread")
	}
	b = BakeConfig{
		Subdivisions:          *w.Subdivisions,
		QuantMax:              *w.QuantMax,
		SeamMaxIterations:     *w.SeamMaxIterations,
		SeamAngleThresholdDeg: *w.SeamAngleThresholdDeg,
		SeamMaterialBandCells: *w.SeamMaterialBandCells,
		ParamsVersion:         *w.ParamsVersion,
		MaxWorkers:            *w.MaxWorkers,
		MaxVerticesPerThread:  *w.MaxVerticesPerThread,
	}
	switch {
	case b.Subdivisions <= 0:
		return b, NewError(CodeSchema, "", "bake.subdivisions", "subdivisions must be positive")
	case b.QuantMax <= 0 || b.QuantMax > 65535:
		return b, NewError(CodeSchema, "", "bake.quantMax", "quantMax must be in [1,65535]")
	case b.SeamMaxIterations < 0:
		return b, NewError(CodeSchema, "", "bake.seamMaxIterations", "seamMaxIterations must be non-negative")
	case !finite(b.SeamAngleThresholdDeg) || b.SeamAngleThresholdDeg < 0:
		return b, NewError(CodeSchema, "", "bake.seamAngleThresholdDeg", "seam angle threshold must be finite and non-negative")
	case !finite(b.SeamMaterialBandCells) || b.SeamMaterialBandCells < 0:
		return b, NewError(CodeSchema, "", "bake.seamMaterialBandCells", "material band must be finite and non-negative")
	case strings.TrimSpace(b.ParamsVersion) == "":
		return b, missing("", "bake.paramsVersion")
	case b.MaxWorkers <= 0:
		return b, NewError(CodeSchema, "", "bake.maxWorkers", "maxWorkers must be positive")
	case b.MaxVerticesPerThread <= 0:
		return b, NewError(CodeSchema, "", "bake.maxVerticesPerThread", "maxVerticesPerThread must be positive")
	}
	return b, nil
}

func blockFromWire(w *wireBlock, index int) (Block, *Error) {
	var b Block
	pathPrefix := fmt.Sprintf("blocks[%d]", index)
	if w.ID == nil {
		return b, missing("", pathPrefix+".id")
	}
	b.ID = *w.ID
	if b.ID == "" {
		return b, NewError(CodeSchema, "", pathPrefix+".id", "block id must not be empty")
	}
	if strings.ContainsAny(b.ID, `/\`) || b.ID == "." || b.ID == ".." || strings.ContainsRune(b.ID, 0) {
		return b, NewError(CodeSchema, b.ID, "id", "block id must be a safe single path component")
	}
	if w.Col == nil {
		return b, missing(b.ID, "col")
	}
	if w.Row == nil {
		return b, missing(b.ID, "row")
	}
	b.Col = *w.Col
	b.Row = *w.Row
	if w.Grid == nil {
		return b, missing(b.ID, "grid")
	}
	if w.Grid.Cols == nil {
		return b, missing(b.ID, "grid.cols")
	}
	if w.Grid.Rows == nil {
		return b, missing(b.ID, "grid.rows")
	}
	b.Grid = GridSize{Cols: *w.Grid.Cols, Rows: *w.Grid.Rows}
	if w.Origin == nil {
		return b, missing(b.ID, "origin")
	}
	if w.Origin.X == nil {
		return b, missing(b.ID, "origin.x")
	}
	if w.Origin.Z == nil {
		return b, missing(b.ID, "origin.z")
	}
	b.Origin = Vec2{X: *w.Origin.X, Z: *w.Origin.Z}
	if w.Height == nil {
		return b, missing(b.ID, "height")
	}
	var e *Error
	if b.Height, e = fileSpecFromWire(w.Height, b.ID, "height"); e != nil {
		return b, e
	}
	if w.Material == nil {
		return b, missing(b.ID, "material")
	}
	if b.Material, e = materialSpecFromWire(w.Material, b.ID); e != nil {
		return b, e
	}
	if w.Passability == nil {
		return b, missing(b.ID, "passability")
	}
	if b.Passability, e = fileSpecFromWire(w.Passability, b.ID, "passability"); e != nil {
		return b, e
	}
	if w.Scatter == nil {
		return b, missing(b.ID, "scatter")
	}
	if w.Scatter.Seed == nil {
		return b, missing(b.ID, "scatter.seed")
	}
	if w.Scatter.DensityPerCell == nil {
		return b, missing(b.ID, "scatter.densityPerCell")
	}
	if w.Scatter.MinMask == nil {
		return b, missing(b.ID, "scatter.minMask")
	}
	if w.Scatter.MaxInstances == nil {
		return b, missing(b.ID, "scatter.maxInstances")
	}
	b.Scatter = ScatterConfig{
		Seed:           *w.Scatter.Seed,
		DensityPerCell: *w.Scatter.DensityPerCell,
		MinMask:        *w.Scatter.MinMask,
		MaxInstances:   *w.Scatter.MaxInstances,
	}
	if !finite(b.Origin.X) || !finite(b.Origin.Z) {
		return b, NewError(CodeSchema, b.ID, "origin", "origin must be finite")
	}
	if !finite(b.Scatter.DensityPerCell) || b.Scatter.DensityPerCell < 0 {
		return b, NewError(CodeSchema, b.ID, "scatter.densityPerCell", "density must be finite and non-negative")
	}
	if b.Scatter.MinMask < 0 || b.Scatter.MinMask > 255 {
		return b, NewError(CodeSchema, b.ID, "scatter.minMask", "minMask must be in [0,255]")
	}
	if b.Scatter.MaxInstances < 0 {
		return b, NewError(CodeSchema, b.ID, "scatter.maxInstances", "maxInstances must be non-negative")
	}
	return b, nil
}

func fileSpecFromWire(w *wireFileSpec, block, name string) (FileSpec, *Error) {
	var f FileSpec
	if w.File == nil {
		return f, missing(block, name+".file")
	}
	if w.Encoding == nil {
		return f, missing(block, name+".encoding")
	}
	if w.Cols == nil {
		return f, missing(block, name+".cols")
	}
	if w.Rows == nil {
		return f, missing(block, name+".rows")
	}
	f = FileSpec{File: *w.File, Encoding: *w.Encoding, Cols: *w.Cols, Rows: *w.Rows}
	if f.File == "" {
		return f, missing(block, name+".file")
	}
	return f, nil
}

func materialSpecFromWire(w *wireMaterialSpec, block string) (MaterialSpec, *Error) {
	var m MaterialSpec
	if w.File == nil {
		return m, missing(block, "material.file")
	}
	if w.Encoding == nil {
		return m, missing(block, "material.encoding")
	}
	if w.Cols == nil {
		return m, missing(block, "material.cols")
	}
	if w.Rows == nil {
		return m, missing(block, "material.rows")
	}
	if w.Layers == nil {
		return m, missing(block, "material.layers")
	}
	if w.QuantMax == nil {
		return m, missing(block, "material.quantMax")
	}
	m = MaterialSpec{
		File: *w.File, Encoding: *w.Encoding, Cols: *w.Cols, Rows: *w.Rows,
		Layers: *w.Layers, QuantMax: *w.QuantMax,
	}
	if m.File == "" {
		return m, missing(block, "material.file")
	}
	return m, nil
}

func seamFromWire(w *wireSeam, index int) (Seam, *Error) {
	var s Seam
	prefix := fmt.Sprintf("seams[%d]", index)
	if w.ID == nil {
		return s, missing("", prefix+".id")
	}
	if w.A == nil {
		return s, missing("", prefix+".a")
	}
	if w.B == nil {
		return s, missing("", prefix+".b")
	}
	if w.EdgeA == nil {
		return s, missing("", prefix+".edgeA")
	}
	if w.EdgeB == nil {
		return s, missing("", prefix+".edgeB")
	}
	s = Seam{ID: *w.ID, A: *w.A, B: *w.B, EdgeA: *w.EdgeA, EdgeB: *w.EdgeB}
	if s.ID == "" || s.A == "" || s.B == "" {
		return s, NewError(CodeSchema, "", prefix, "seam id, a and b must not be empty")
	}
	return s, nil
}

func validateBlockAgainstManifest(b *Block, m Manifest) *Error {
	if b.Col < 0 || b.Col >= m.Grid.Cols || b.Row < 0 || b.Row >= m.Grid.Rows {
		return NewError(CodeSize, b.ID, "col,row", "block position (%d,%d) is outside world grid %dx%d", b.Col, b.Row, m.Grid.Cols, m.Grid.Rows)
	}
	if b.Grid.Cols != m.Grid.BlockCols {
		return NewError(CodeSize, b.ID, "grid.cols", "block grid cols=%d, manifest blockCols=%d", b.Grid.Cols, m.Grid.BlockCols)
	}
	if b.Grid.Rows != m.Grid.BlockRows {
		return NewError(CodeSize, b.ID, "grid.rows", "block grid rows=%d, manifest blockRows=%d", b.Grid.Rows, m.Grid.BlockRows)
	}
	if b.Height.Encoding != "f64le" {
		return NewError(CodeSchema, b.ID, "height.encoding", "height encoding must be f64le")
	}
	if b.Material.Encoding != "u16le" {
		return NewError(CodeSchema, b.ID, "material.encoding", "material encoding must be u16le")
	}
	if b.Passability.Encoding != "u8" && b.Passability.Encoding != "u8le" {
		return NewError(CodeSchema, b.ID, "passability.encoding", "passability encoding must be u8 or u8le")
	}
	if b.Height.Cols != b.Grid.Cols {
		return NewError(CodeSize, b.ID, "height.cols", "height cols=%d, block grid cols=%d", b.Height.Cols, b.Grid.Cols)
	}
	if b.Height.Rows != b.Grid.Rows {
		return NewError(CodeSize, b.ID, "height.rows", "height rows=%d, block grid rows=%d", b.Height.Rows, b.Grid.Rows)
	}
	if b.Material.Cols != b.Grid.Cols {
		return NewError(CodeSize, b.ID, "material.cols", "material cols=%d, block grid cols=%d", b.Material.Cols, b.Grid.Cols)
	}
	if b.Material.Rows != b.Grid.Rows {
		return NewError(CodeSize, b.ID, "material.rows", "material rows=%d, block grid rows=%d", b.Material.Rows, b.Grid.Rows)
	}
	if b.Passability.Cols != b.Grid.Cols {
		return NewError(CodeSize, b.ID, "passability.cols", "passability cols=%d, block grid cols=%d", b.Passability.Cols, b.Grid.Cols)
	}
	if b.Passability.Rows != b.Grid.Rows {
		return NewError(CodeSize, b.ID, "passability.rows", "passability rows=%d, block grid rows=%d", b.Passability.Rows, b.Grid.Rows)
	}
	if b.Material.Layers != len(m.Materials) {
		return NewError(CodeSchema, b.ID, "material.layers", "material layers=%d, manifest material layers=%d", b.Material.Layers, len(m.Materials))
	}
	if b.Material.QuantMax != m.Bake.QuantMax {
		return NewError(CodeSchema, b.ID, "material.quantMax", "material quantMax=%d, bake quantMax=%d", b.Material.QuantMax, m.Bake.QuantMax)
	}
	if b.Material.Layers <= 0 || b.Material.QuantMax <= 0 {
		return NewError(CodeSchema, b.ID, "material", "material layer count and quantMax must be positive")
	}
	return nil
}

func loadBlockData(root string, b Block, m Manifest) (*BlockData, *Error) {
	heightPath, e := resolveDataPath(root, b.Height.File, b.ID, "height.file")
	if e != nil {
		return nil, e
	}
	heightBytes, e := readResource(heightPath, b.ID, "height.file")
	if e != nil {
		return nil, e
	}
	heightCount := b.Height.Cols * b.Height.Rows
	if len(heightBytes) != heightCount*8 {
		return nil, NewError(CodeDecode, b.ID, "height.file", "height file has %d bytes, expected %d", len(heightBytes), heightCount*8)
	}
	heights := make([]float64, heightCount)
	for i := range heights {
		bits := binary.LittleEndian.Uint64(heightBytes[i*8 : i*8+8])
		heights[i] = math.Float64frombits(bits)
		if !finite(heights[i]) {
			return nil, NewError(CodeDecode, b.ID, "height.file", "height sample %d is not finite", i)
		}
	}

	matPath, e := resolveDataPath(root, b.Material.File, b.ID, "material.file")
	if e != nil {
		return nil, e
	}
	matBytes, e := readResource(matPath, b.ID, "material.file")
	if e != nil {
		return nil, e
	}
	weightCount := b.Material.Cols * b.Material.Rows * b.Material.Layers
	if len(matBytes) != weightCount*2 {
		return nil, NewError(CodeDecode, b.ID, "material.file", "material file has %d bytes, expected %d", len(matBytes), weightCount*2)
	}
	weights := make([]uint16, weightCount)
	for i := range weights {
		weights[i] = binary.LittleEndian.Uint16(matBytes[i*2 : i*2+2])
	}
	for row := 0; row < b.Material.Rows; row++ {
		for col := 0; col < b.Material.Cols; col++ {
			base := (row*b.Material.Cols + col) * b.Material.Layers
			sum := 0
			for layer := 0; layer < b.Material.Layers; layer++ {
				w := int(weights[base+layer])
				if w < 0 || w > b.Material.QuantMax {
					return nil, NewError(CodeWeight, b.ID, "material.weights", "weight %d at (%d,%d) layer %d is outside [0,%d]", w, col, row, layer, b.Material.QuantMax).
						WithDetail("col", col).WithDetail("row", row).WithDetail("layer", layer).WithDetail("value", w)
				}
				sum += w
			}
			if sum != b.Material.QuantMax {
				return nil, NewError(CodeWeight, b.ID, "material.weights", "weights at (%d,%d) sum to %d, expected %d", col, row, sum, b.Material.QuantMax).
					WithDetail("col", col).WithDetail("row", row).WithDetail("sum", sum).WithDetail("expected", b.Material.QuantMax)
			}
		}
	}

	maskPath, e := resolveDataPath(root, b.Passability.File, b.ID, "passability.file")
	if e != nil {
		return nil, e
	}
	maskBytes, e := readResource(maskPath, b.ID, "passability.file")
	if e != nil {
		return nil, e
	}
	maskCount := b.Passability.Cols * b.Passability.Rows
	if len(maskBytes) != maskCount {
		return nil, NewError(CodeDecode, b.ID, "passability.file", "mask file has %d bytes, expected %d", len(maskBytes), maskCount)
	}
	mask := append([]byte(nil), maskBytes...)

	return &BlockData{Def: b, Height: heights, Weights: weights, Mask: mask}, nil
}

func readResource(path, block, field string) ([]byte, *Error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, NewError(CodeDecode, block, field, "read resource: %v", err)
	}
	return data, nil
}

func resolveDataPath(root, rel, block, field string) (string, *Error) {
	if strings.TrimSpace(rel) == "" {
		return "", missing(block, field)
	}
	if filepath.IsAbs(rel) {
		return "", NewError(CodeSchema, block, field, "resource path must be relative to data root")
	}
	clean := filepath.Clean(rel)
	if clean == "." || strings.HasPrefix(clean, "..") {
		return "", NewError(CodeSchema, block, field, "resource path must stay inside data root")
	}
	abs := filepath.Join(root, clean)
	relToRoot, err := filepath.Rel(root, abs)
	if err != nil || strings.HasPrefix(relToRoot, "..") {
		return "", NewError(CodeSchema, block, field, "resource path escapes data root")
	}
	return abs, nil
}

func validateSeams(m Manifest, p *Project) *Error {
	seen := map[string]bool{}
	pairs := map[string]bool{}
	for i := range m.Seams {
		s := m.Seams[i]
		a, okA := p.Blocks[s.A]
		if !okA {
			return NewError(CodeSchema, s.A, fmt.Sprintf("seams[%d].a", i), "seam references unknown block %q", s.A)
		}
		b, okB := p.Blocks[s.B]
		if !okB {
			return NewError(CodeSchema, s.B, fmt.Sprintf("seams[%d].b", i), "seam references unknown block %q", s.B)
		}
		if s.A == s.B {
			return NewError(CodeSchema, s.A, fmt.Sprintf("seams[%d]", i), "seam cannot connect a block to itself")
		}
		if seen[s.ID] {
			return NewError(CodeSchema, "", fmt.Sprintf("seams[%d].id", i), "duplicate seam id %q", s.ID)
		}
		seen[s.ID] = true
		if !validEdge(s.EdgeA) || !validEdge(s.EdgeB) {
			return NewError(CodeSchema, s.A, fmt.Sprintf("seams[%d].edgeA", i), "edge must be right, left, top or bottom")
		}
		if oppositeEdge(s.EdgeA) != s.EdgeB {
			return NewError(CodeSchema, s.A, fmt.Sprintf("seams[%d].edgeB", i), "edgeA %s must pair with edgeB %s, got %s", s.EdgeA, oppositeEdge(s.EdgeA), s.EdgeB)
		}
		switch s.EdgeA {
		case "right":
			if a.Def.Row != b.Def.Row || a.Def.Col+1 != b.Def.Col {
				return NewError(CodeSize, s.A, fmt.Sprintf("seams[%d]", i), "right seam requires B immediately to the right of A")
			}
		case "left":
			if a.Def.Row != b.Def.Row || a.Def.Col-1 != b.Def.Col {
				return NewError(CodeSize, s.A, fmt.Sprintf("seams[%d]", i), "left seam requires B immediately to the left of A")
			}
		case "top":
			if a.Def.Col != b.Def.Col || a.Def.Row+1 != b.Def.Row {
				return NewError(CodeSize, s.A, fmt.Sprintf("seams[%d]", i), "top seam requires B immediately above A")
			}
		case "bottom":
			if a.Def.Col != b.Def.Col || a.Def.Row-1 != b.Def.Row {
				return NewError(CodeSize, s.A, fmt.Sprintf("seams[%d]", i), "bottom seam requires B immediately below A")
			}
		}
		if s.EdgeA == "left" || s.EdgeA == "right" {
			if a.Def.Grid.Rows != b.Def.Grid.Rows {
				return NewError(CodeSize, s.A, fmt.Sprintf("seams[%d]", i), "horizontal seam sample counts differ")
			}
		} else if a.Def.Grid.Cols != b.Def.Grid.Cols {
			return NewError(CodeSize, s.A, fmt.Sprintf("seams[%d]", i), "vertical seam sample counts differ")
		}
		pairKey := s.A + "|" + s.B
		if s.B < s.A {
			pairKey = s.B + "|" + s.A
		}
		if pairs[pairKey] {
			return NewError(CodeSchema, s.A, fmt.Sprintf("seams[%d]", i), "duplicate seam for block pair %s/%s", s.A, s.B)
		}
		pairs[pairKey] = true
	}
	return nil
}

func validEdge(e string) bool {
	return e == "left" || e == "right" || e == "top" || e == "bottom"
}

func oppositeEdge(e string) string {
	switch e {
	case "left":
		return "right"
	case "right":
		return "left"
	case "top":
		return "bottom"
	case "bottom":
		return "top"
	default:
		return ""
	}
}

func loadTruth(root string, m Manifest, p *Project) (Truth, *Error) {
	truthPath, e := resolveDataPath(root, m.TruthFile, "", "truthFile")
	if e != nil {
		return Truth{}, e
	}
	var wt wireTruth
	if e := decodeJSONFile(truthPath, &wt, "truthFile"); e != nil {
		return Truth{}, e
	}
	var t Truth
	if wt.SchemaVersion == nil {
		return t, missing("", "truth.schemaVersion")
	}
	t.SchemaVersion = *wt.SchemaVersion
	if t.SchemaVersion != 1 {
		return t, NewError(CodeSchema, "", "truth.schemaVersion", "unsupported truth schema version %d", t.SchemaVersion)
	}
	if wt.HeightSamples == nil {
		return t, missing("", "truth.heightSamples")
	}
	if wt.MaterialRatios == nil {
		return t, missing("", "truth.materialRatios")
	}
	if wt.InstanceCounts == nil {
		return t, missing("", "truth.instanceCounts")
	}
	if wt.Seams == nil {
		return t, missing("", "truth.seams")
	}
	for i := range *wt.HeightSamples {
		w := (*wt.HeightSamples)[i]
		if w.Block == nil {
			return t, missing("", fmt.Sprintf("truth.heightSamples[%d].block", i))
		}
		if w.Col == nil {
			return t, missing(*w.Block, fmt.Sprintf("truth.heightSamples[%d].col", i))
		}
		if w.Row == nil {
			return t, missing(*w.Block, fmt.Sprintf("truth.heightSamples[%d].row", i))
		}
		if w.Value == nil {
			return t, missing(*w.Block, fmt.Sprintf("truth.heightSamples[%d].value", i))
		}
		if w.Tolerance == nil {
			return t, missing(*w.Block, fmt.Sprintf("truth.heightSamples[%d].tolerance", i))
		}
		bd, ok := p.Blocks[*w.Block]
		if !ok {
			return t, NewError(CodeSchema, *w.Block, fmt.Sprintf("truth.heightSamples[%d].block", i), "unknown block")
		}
		if *w.Col < 0 || *w.Col >= bd.Def.Grid.Cols || *w.Row < 0 || *w.Row >= bd.Def.Grid.Rows {
			return t, NewError(CodeSize, *w.Block, fmt.Sprintf("truth.heightSamples[%d]", i), "sample coordinate out of range")
		}
		if !finite(*w.Value) || !finite(*w.Tolerance) || *w.Tolerance < 0 {
			return t, NewError(CodeSchema, *w.Block, fmt.Sprintf("truth.heightSamples[%d]", i), "value and tolerance must be finite, tolerance non-negative")
		}
		t.HeightSamples = append(t.HeightSamples, HeightTruth{Block: *w.Block, Col: *w.Col, Row: *w.Row, Value: *w.Value, Tolerance: *w.Tolerance})
	}
	for i := range *wt.MaterialRatios {
		w := (*wt.MaterialRatios)[i]
		if w.Block == nil {
			return t, missing("", fmt.Sprintf("truth.materialRatios[%d].block", i))
		}
		if w.Layer == nil {
			return t, missing(*w.Block, fmt.Sprintf("truth.materialRatios[%d].layer", i))
		}
		if w.Ratio == nil {
			return t, missing(*w.Block, fmt.Sprintf("truth.materialRatios[%d].ratio", i))
		}
		if w.Tolerance == nil {
			return t, missing(*w.Block, fmt.Sprintf("truth.materialRatios[%d].tolerance", i))
		}
		b, ok := p.Blocks[*w.Block]
		if !ok {
			return t, NewError(CodeSchema, *w.Block, fmt.Sprintf("truth.materialRatios[%d].block", i), "unknown block")
		}
		if *w.Layer < 0 || *w.Layer >= b.Def.Material.Layers {
			return t, NewError(CodeSchema, *w.Block, fmt.Sprintf("truth.materialRatios[%d].layer", i), "material layer out of range")
		}
		if !finite(*w.Ratio) || *w.Ratio < 0 || *w.Ratio > 1 || !finite(*w.Tolerance) || *w.Tolerance < 0 {
			return t, NewError(CodeSchema, *w.Block, fmt.Sprintf("truth.materialRatios[%d]", i), "ratio must be in [0,1] and tolerance non-negative")
		}
		t.MaterialRatios = append(t.MaterialRatios, RatioTruth{Block: *w.Block, Layer: *w.Layer, Ratio: *w.Ratio, Tolerance: *w.Tolerance})
	}
	for i := range *wt.InstanceCounts {
		w := (*wt.InstanceCounts)[i]
		if w.Block == nil {
			return t, missing("", fmt.Sprintf("truth.instanceCounts[%d].block", i))
		}
		if w.Count == nil {
			return t, missing(*w.Block, fmt.Sprintf("truth.instanceCounts[%d].count", i))
		}
		if w.Tolerance == nil {
			return t, missing(*w.Block, fmt.Sprintf("truth.instanceCounts[%d].tolerance", i))
		}
		b, ok := p.Blocks[*w.Block]
		if !ok {
			return t, NewError(CodeSchema, *w.Block, fmt.Sprintf("truth.instanceCounts[%d].block", i), "unknown block")
		}
		_ = b
		if *w.Count < 0 || *w.Tolerance < 0 {
			return t, NewError(CodeSchema, *w.Block, fmt.Sprintf("truth.instanceCounts[%d]", i), "count and tolerance must be non-negative")
		}
		t.InstanceCounts = append(t.InstanceCounts, CountTruth{Block: *w.Block, Count: *w.Count, Tolerance: *w.Tolerance})
	}
	for i := range *wt.Seams {
		w := (*wt.Seams)[i]
		if w.A == nil {
			return t, missing("", fmt.Sprintf("truth.seams[%d].a", i))
		}
		if w.B == nil {
			return t, missing("", fmt.Sprintf("truth.seams[%d].b", i))
		}
		if w.MaxAngleDeg == nil {
			return t, missing(*w.A, fmt.Sprintf("truth.seams[%d].maxAngleDeg", i))
		}
		if w.Tolerance == nil {
			return t, missing(*w.A, fmt.Sprintf("truth.seams[%d].tolerance", i))
		}
		if _, ok := p.Blocks[*w.A]; !ok {
			return t, NewError(CodeSchema, *w.A, fmt.Sprintf("truth.seams[%d].a", i), "unknown block")
		}
		if _, ok := p.Blocks[*w.B]; !ok {
			return t, NewError(CodeSchema, *w.B, fmt.Sprintf("truth.seams[%d].b", i), "unknown block")
		}
		if !finite(*w.MaxAngleDeg) || *w.MaxAngleDeg < 0 || !finite(*w.Tolerance) || *w.Tolerance < 0 {
			return t, NewError(CodeSchema, *w.A, fmt.Sprintf("truth.seams[%d]", i), "angle and tolerance must be finite and non-negative")
		}
		t.Seams = append(t.Seams, SeamAngleTruth{A: *w.A, B: *w.B, MaxAngleDeg: *w.MaxAngleDeg, Tolerance: *w.Tolerance})
	}
	return t, nil
}

func missing(block, fieldName string) *Error {
	return NewError(CodeSchema, block, fieldName, "required field is missing")
}

func missingField(prefix, name string) *Error {
	return missing("", field(prefix, name))
}

func field(prefix, name string) string {
	if prefix == "" {
		return name
	}
	return prefix + "." + name
}

func finite(v float64) bool {
	return !math.IsNaN(v) && !math.IsInf(v, 0)
}
