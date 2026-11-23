package main

import (
	"encoding/binary"
	"encoding/json"
	"flag"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"

	"terrain-bake/bake"
	"terrain-bake/project"
)

func main() {
	out := flag.String("out", "data", "output data directory")
	seed := flag.Int64("seed", 20261124, "global deterministic seed")
	force := flag.Bool("force", false, "replace an existing data directory")
	flag.Parse()

	if _, err := os.Stat(*out); err == nil && !*force {
		fatalf("output directory %s already exists; pass -force to replace it", *out)
	}
	if err := os.RemoveAll(*out); err != nil {
		fatalf("remove old output: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(*out, "blocks"), 0o755); err != nil {
		fatalf("create data directory: %v", err)
	}

	manifest := buildManifest(*seed)
	writeFiles(*out, manifest)
	writeJSON(filepath.Join(*out, "truth.json"), project.Truth{
		SchemaVersion:  1,
		HeightSamples:  []project.HeightTruth{},
		MaterialRatios: []project.RatioTruth{},
		InstanceCounts: []project.CountTruth{},
		Seams:          []project.SeamAngleTruth{},
	})
	writeJSON(filepath.Join(*out, "manifest.json"), manifest)

	p, e := project.Load(*out)
	if e != nil {
		fatalf("load generated project: %v", e)
	}
	params := bake.ParamsFromManifest(p.Manifest)
	artifacts := make(map[string]*bake.Artifact, len(p.BlockOrder))
	var bakeErr error
	for _, id := range p.BlockOrder {
		a, err := bake.BakeBlock(p, id, params)
		if err != nil {
			bakeErr = err
			break
		}
		artifacts[id] = a
	}
	if bakeErr != nil {
		fatalf("bake generated sample: %v", bakeErr)
	}
	truth := deriveTruth(p)
	writeJSON(filepath.Join(*out, "truth.json"), truth)

	reloaded, e := project.Load(*out)
	if e != nil {
		fatalf("reload generated project: %v", e)
	}
	report := bake.ValidateTruth(reloaded, artifacts)
	if !report.OK {
		fatalf("generated truth did not validate: %+v", report.Mismatches)
	}
	fmt.Printf("generated %s: %d blocks, %d seams, %d truth checks, seed=%d\n",
		*out, len(manifest.Blocks), len(manifest.Seams), report.Checks, *seed)
}

func buildManifest(seed int64) project.Manifest {
	positions := [][2]int{
		{0, 0}, {1, 0}, {0, 1}, {1, 1}, {3, 0}, {3, 1},
	}
	blockCols, blockRows, spacing := 24, 16, 1.0
	materials := []project.Material{
		{ID: 0, Name: "grass", Color: [3]float64{0.27, 0.58, 0.23}},
		{ID: 1, Name: "rock", Color: [3]float64{0.48, 0.46, 0.42}},
		{ID: 2, Name: "sand", Color: [3]float64{0.78, 0.67, 0.40}},
	}
	manifest := project.Manifest{
		SchemaVersion: 1,
		Seed:          seed,
		Grid: project.GridConfig{
			Cols: 4, Rows: 2, BlockCols: blockCols, BlockRows: blockRows, Spacing: spacing,
		},
		Materials: materials,
		Bake: project.BakeConfig{
			Subdivisions:          2,
			QuantMax:              10000,
			SeamMaxIterations:     5,
			SeamAngleThresholdDeg: 0.25,
			SeamMaterialBandCells: 1,
			ParamsVersion:         "terrain-bake-v1",
			MaxWorkers:            4,
			MaxVerticesPerThread:  4096,
		},
		TruthFile: "truth.json",
	}
	for _, pos := range positions {
		col, row := pos[0], pos[1]
		id := fmt.Sprintf("B_%d_%d", col, row)
		block := project.Block{
			ID:   id,
			Col:  col,
			Row:  row,
			Grid: project.GridSize{Cols: blockCols, Rows: blockRows},
			Origin: project.Vec2{
				X: float64(col*(blockCols-1)) * spacing,
				Z: float64(row*(blockRows-1)) * spacing,
			},
			Height:      project.FileSpec{File: filepath.ToSlash(filepath.Join("blocks", id, "height.f64")), Encoding: "f64le", Cols: blockCols, Rows: blockRows},
			Material:    project.MaterialSpec{File: filepath.ToSlash(filepath.Join("blocks", id, "material.u16")), Encoding: "u16le", Cols: blockCols, Rows: blockRows, Layers: len(materials), QuantMax: 10000},
			Passability: project.FileSpec{File: filepath.ToSlash(filepath.Join("blocks", id, "passability.u8")), Encoding: "u8", Cols: blockCols, Rows: blockRows},
			Scatter: project.ScatterConfig{
				Seed:           seed + int64(col*131+row*17),
				DensityPerCell: 1.65,
				MinMask:        120,
				MaxInstances:   360,
			},
		}
		manifest.Blocks = append(manifest.Blocks, block)
	}
	// Two disconnected terrain islands: the omitted column 2 is an open-water
	// gap. Editing one island therefore leaves the other island's cache
	// artifacts untouched.
	seams := []struct {
		a, b string
		ea   string
		eb   string
	}{
		{"B_0_0", "B_1_0", "right", "left"},
		{"B_0_1", "B_1_1", "right", "left"},
		{"B_0_0", "B_0_1", "top", "bottom"},
		{"B_1_0", "B_1_1", "top", "bottom"},
		{"B_3_0", "B_3_1", "top", "bottom"},
	}
	for i, s := range seams {
		manifest.Seams = append(manifest.Seams, project.Seam{
			ID: fmt.Sprintf("S_%02d", i+1), A: s.a, B: s.b, EdgeA: s.ea, EdgeB: s.eb,
		})
	}
	return manifest
}

func writeFiles(root string, m project.Manifest) {
	for _, def := range m.Blocks {
		dir := filepath.Join(root, "blocks", def.ID)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			fatalf("create block directory: %v", err)
		}
		heights := make([]float64, def.Grid.Cols*def.Grid.Rows)
		weights := make([]uint16, def.Grid.Cols*def.Grid.Rows*def.Material.Layers)
		mask := make([]byte, def.Grid.Cols*def.Grid.Rows)
		for r := 0; r < def.Grid.Rows; r++ {
			for c := 0; c < def.Grid.Cols; c++ {
				x := def.Origin.X + float64(c)*m.Grid.Spacing
				z := def.Origin.Z + float64(r)*m.Grid.Spacing
				heights[r*def.Grid.Cols+c] = analyticHeight(x, z, m.Seed)
				raw := analyticMaterial(x, z, m.Seed)
				q := quantize(raw, def.Material.QuantMax)
				copy(weights[(r*def.Grid.Cols+c)*def.Material.Layers:], q)
				mask[r*def.Grid.Cols+c] = analyticMask(x, z, m.Seed)
			}
		}
		heightBytes := make([]byte, len(heights)*8)
		for i, h := range heights {
			binary.LittleEndian.PutUint64(heightBytes[i*8:], math.Float64bits(h))
		}
		weightBytes := make([]byte, len(weights)*2)
		for i, w := range weights {
			binary.LittleEndian.PutUint16(weightBytes[i*2:], w)
		}
		if err := os.WriteFile(filepath.Join(root, def.Height.File), heightBytes, 0o644); err != nil {
			fatalf("write height: %v", err)
		}
		if err := os.WriteFile(filepath.Join(root, def.Material.File), weightBytes, 0o644); err != nil {
			fatalf("write material: %v", err)
		}
		if err := os.WriteFile(filepath.Join(root, def.Passability.File), mask, 0o644); err != nil {
			fatalf("write mask: %v", err)
		}
	}
}

func deriveTruth(p *project.Project) project.Truth {
	truth := project.Truth{SchemaVersion: 1}
	sampleCells := [][2]int{{2, 3}, {8, 5}, {14, 9}, {20, 13}}
	for _, id := range p.BlockOrder {
		bd := p.Blocks[id]
		def := bd.Def
		for _, cell := range sampleCells {
			c, r := cell[0], cell[1]
			if c >= def.Grid.Cols {
				c = def.Grid.Cols - 1
			}
			if r >= def.Grid.Rows {
				r = def.Grid.Rows - 1
			}
			x := def.Origin.X + float64(c)*p.Manifest.Grid.Spacing
			z := def.Origin.Z + float64(r)*p.Manifest.Grid.Spacing
			truth.HeightSamples = append(truth.HeightSamples, project.HeightTruth{
				Block: id, Col: c, Row: r, Value: analyticHeight(x, z, p.Manifest.Seed), Tolerance: 1e-9,
			})
		}
		layerSums := make([]float64, def.Material.Layers)
		for i, w := range bd.Weights {
			layerSums[i%def.Material.Layers] += float64(w)
		}
		for layer, sum := range layerSums {
			truth.MaterialRatios = append(truth.MaterialRatios, project.RatioTruth{
				Block: id, Layer: layer,
				Ratio:     sum / float64(def.Grid.Cols*def.Grid.Rows*def.Material.QuantMax),
				Tolerance: 0.006,
			})
		}
		passable := 0
		for r := 0; r < def.Grid.Rows-1; r++ {
			for c := 0; c < def.Grid.Cols-1; c++ {
				if int(bd.Mask[r*def.Grid.Cols+c]) >= def.Scatter.MinMask {
					passable++
				}
			}
		}
		expected := int(math.Round(def.Scatter.DensityPerCell * float64(passable)))
		if expected > def.Scatter.MaxInstances {
			expected = def.Scatter.MaxInstances
		}
		truth.InstanceCounts = append(truth.InstanceCounts, project.CountTruth{
			Block: id, Count: expected, Tolerance: 2,
		})
	}
	for _, s := range p.Manifest.Seams {
		truth.Seams = append(truth.Seams, project.SeamAngleTruth{
			A: s.A, B: s.B, MaxAngleDeg: 0, Tolerance: 1e-4,
		})
	}
	return truth
}

func analyticHeight(x, z float64, seed int64) float64 {
	phase := float64(seed%1000) / 1000 * math.Pi
	return 1.1*math.Sin(0.11*x+phase) +
		0.7*math.Cos(0.17*z+0.4*phase) +
		0.35*math.Sin(0.07*(x+z)+0.9) +
		0.006*x - 0.003*z
}

func analyticMaterial(x, z float64, seed int64) []float64 {
	phase := float64(seed%997) / 997 * 2 * math.Pi
	w0 := 0.34 + 0.22*math.Sin(0.13*x+phase) + 0.10*math.Cos(0.19*z)
	w1 := 0.33 + 0.20*math.Cos(0.09*z+0.7*phase) + 0.08*math.Sin(0.21*x)
	w2 := 1 - w0 - w1
	if w2 < 0.05 {
		w2 = 0.05
		scale := (1 - w2) / (w0 + w1)
		w0 *= scale
		w1 *= scale
	}
	return []float64{w0, w1, w2}
}

func analyticMask(x, z float64, seed int64) byte {
	v := math.Sin(0.16*x+0.31*float64(seed%31)) + math.Cos(0.12*z-0.2)
	switch {
	case v > 0.25:
		return 230
	case v > -0.35:
		return 160
	default:
		return 40
	}
}

func quantize(raw []float64, quant int) []uint16 {
	s := 0.0
	for _, v := range raw {
		s += v
	}
	out := make([]uint16, len(raw))
	type rem struct {
		i int
		f float64
	}
	var rs []rem
	total := 0
	for i, v := range raw {
		scaled := v / s * float64(quant)
		floor := math.Floor(scaled)
		out[i] = uint16(floor)
		total += int(floor)
		rs = append(rs, rem{i: i, f: scaled - floor})
	}
	sort.Slice(rs, func(i, j int) bool {
		if rs[i].f == rs[j].f {
			return rs[i].i < rs[j].i
		}
		return rs[i].f > rs[j].f
	})
	for i := 0; i < quant-total; i++ {
		out[rs[i].i]++
	}
	return out
}

func writeJSON(path string, value any) {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		fatalf("marshal JSON: %v", err)
	}
	data = append(data, '\n')
	if err := os.WriteFile(path, data, 0o644); err != nil {
		fatalf("write %s: %v", path, err)
	}
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "gen-data: "+format+"\n", args...)
	os.Exit(1)
}
