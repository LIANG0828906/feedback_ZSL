package pipeline

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"runtime"
	"sort"
	"sync"
	"time"

	"strata/internal/generator"
	"strata/internal/model"
)

type Config struct {
	VoxelSize         float64            `json:"voxelSize"`
	OutlierSigma      float64            `json:"outlierSigma"`
	NormalK           int                `json:"normalK"`
	ResampleSpacing   float64            `json:"resampleSpacing"`
	ChunkSize         float64            `json:"chunkSize"`
	PreviewSample     float64            `json:"previewSample"`
	MaxWorkers        int                `json:"maxWorkers"`
	VertexBufferLimit int                `json:"vertexBufferLimit"`
	ChunkBias         map[string]float64 `json:"chunkBias,omitempty"`
}

func DefaultConfig() Config {
	return Config{
		VoxelSize: 0.002, OutlierSigma: 2.0, NormalK: 12,
		ResampleSpacing: 0.1, ChunkSize: 1.2, PreviewSample: 0.35,
		MaxWorkers: 4, VertexBufferLimit: 250000,
	}
}

func (c Config) Validate() error {
	checkRange := func(name string, value, lo, hi float64) error {
		if math.IsNaN(value) || value < lo || value > hi {
			return model.NewError(model.EPARA, fmt.Sprintf("%s must be in [%g, %g]", name, lo, hi), "params."+name, 0, map[string]any{
				"value": value, "min": lo, "max": hi,
			})
		}
		return nil
	}
	if err := checkRange("voxelSize", c.VoxelSize, 0.0001, 0.1); err != nil {
		return err
	}
	if err := checkRange("outlierSigma", c.OutlierSigma, 0.5, 8); err != nil {
		return err
	}
	if c.NormalK < 3 || c.NormalK > 64 {
		return model.NewError(model.EPARA, "normalK must be in [3, 64]", "params.normalK", 0, map[string]any{"value": c.NormalK})
	}
	if err := checkRange("resampleSpacing", c.ResampleSpacing, 0.02, 0.5); err != nil {
		return err
	}
	if err := checkRange("chunkSize", c.ChunkSize, 0.3, 5); err != nil {
		return err
	}
	if err := checkRange("previewSample", c.PreviewSample, 0.05, 1); err != nil {
		return err
	}
	if c.MaxWorkers < 1 || c.MaxWorkers > 16 {
		return model.NewError(model.EPARA, "maxWorkers must be in [1, 16]", "params.maxWorkers", 0, map[string]any{"value": c.MaxWorkers})
	}
	if c.VertexBufferLimit < 256 || c.VertexBufferLimit > 4_000_000 {
		return model.NewError(model.EPARA, "vertexBufferLimit must be in [256, 4000000]", "params.vertexBufferLimit", 0, nil)
	}
	for id, bias := range c.ChunkBias {
		if math.IsNaN(bias) || math.Abs(bias) > 0.5 {
			return model.NewError(model.EPARA, "chunk bias must be in [-0.5, 0.5]", "params.chunkBias."+id, 0, map[string]any{"value": bias})
		}
	}
	n := int(math.Round(c.ChunkSize/c.ResampleSpacing)) + 1
	if n*n > c.VertexBufferLimit {
		return model.NewError(model.EPARA, "per-chunk vertex buffer exceeds vertexBufferLimit", "params.vertexBufferLimit", 0, map[string]any{
			"verticesPerChunk": n * n, "limit": c.VertexBufferLimit,
		})
	}
	return nil
}

func (c Config) Hash() string {
	data, _ := json.Marshal(c)
	return model.HashBytes(data)
}

type StageStats struct {
	InputPoints   int     `json:"inputPoints"`
	CleanPoints   int     `json:"cleanPoints"`
	RemovedPoints int     `json:"removedPoints"`
	VoxelPoints   int     `json:"voxelPoints"`
	DenoiseMs     float64 `json:"denoiseMs"`
	BuildMs       float64 `json:"buildMs"`
	Workers       int     `json:"workers"`
}

type Result struct {
	CleanPoints []model.Point    `json:"-"`
	Removed     []int            `json:"removedIndices"`
	Mesh        *model.Mesh      `json:"mesh"`
	Features    model.FeatureSet `json:"features"`
	Stats       StageStats       `json:"stats"`
}

type Builder struct {
	mu          sync.Mutex
	buildCount  int
	chunkErrors map[string]string
}

func NewBuilder() *Builder {
	return &Builder{chunkErrors: map[string]string{}}
}

func (b *Builder) BuildCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buildCount
}

func (b *Builder) Build(geometry *model.Geometry, cfg Config, mode string, sampleRate float64) (*Result, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	start := time.Now()
	clean, removed, err := Denoise(geometry.Points, cfg)
	if err != nil {
		return nil, err
	}
	clean = VoxelFilter(clean, cfg.VoxelSize)
	denoiseMs := durationMS(time.Since(start))
	mesh, err := b.buildAll(clean, cfg, mode, sampleRate, nil, nil)
	if err != nil {
		return nil, err
	}
	return &Result{
		CleanPoints: clean,
		Removed:     removed,
		Mesh:        mesh,
		Features:    MeshFeatures(mesh),
		Stats: StageStats{
			InputPoints: len(geometry.Points), CleanPoints: len(clean),
			RemovedPoints: len(removed), VoxelPoints: len(clean),
			DenoiseMs: denoiseMs, BuildMs: durationMS(time.Since(start)),
			Workers: min(cfg.MaxWorkers, runtime.NumCPU()),
		},
	}, nil
}

func (b *Builder) BuildIncremental(cleanPoints []model.Point, cfg Config, previous *model.Mesh, reasons map[string]string, paramsVersion int, mode string, sampleRate float64) (*model.Mesh, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if previous == nil {
		return b.buildAll(cleanPoints, cfg, mode, sampleRate, nil, nil)
	}
	mesh, err := b.buildAll(cleanPoints, cfg, mode, sampleRate, previous, reasons)
	if err != nil {
		return nil, err
	}
	mesh.ParamsVersion = paramsVersion
	return mesh, nil
}

type chunkDef struct {
	ID     string
	Row    int
	Col    int
	Bounds model.Bounds
}

type chunkBuild struct {
	chunk    model.ChunkMesh
	vertices []model.MeshVertex
	faces    []model.MeshFace
	reused   bool
	reason   string
	err      error
}

func (b *Builder) buildAll(clean []model.Point, cfg Config, mode string, sampleRate float64, previous *model.Mesh, reasons map[string]string) (*model.Mesh, error) {
	bounds := PointsBounds(clean)
	if bounds.IsEmpty() {
		return nil, model.NewError(model.EDECODE, "point cloud contains no usable points after denoise", "points", 0, nil)
	}
	chunkDefs := MakeChunks(bounds, cfg.ChunkSize)
	prevMap := map[string]*model.ChunkMesh{}
	if previous != nil {
		for i := range previous.Chunks {
			prevMap[previous.Chunks[i].ID] = &previous.Chunks[i]
		}
	}
	results := make([]chunkBuild, len(chunkDefs))
	jobs := make(chan int)
	workers := min(cfg.MaxWorkers, runtime.NumCPU(), len(chunkDefs))
	if workers < 1 {
		workers = 1
	}
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for idx := range jobs {
				def := chunkDefs[idx]
				reason, affected := reasons[def.ID]
				if previous != nil && !affected && prevMap[def.ID] != nil && prevMap[def.ID].Error == "" {
					results[idx] = reuseChunk(previous, prevMap[def.ID])
					continue
				}
				built, err := buildChunk(clean, def, cfg)
				built.reason = reason
				if built.reason == "" {
					built.reason = "initial"
				}
				if err != nil {
					built.err = err
					built.chunk = model.ChunkMesh{
						ID: def.ID, Row: def.Row, Col: def.Col, Bounds: def.Bounds,
						Reason: "failed", Error: err.Error(),
					}
				} else {
					built.chunk.Rebuilt = true
					built.chunk.Reason = built.reason
				}
				results[idx] = built
			}
		}()
	}
	for i := range chunkDefs {
		jobs <- i
	}
	close(jobs)
	wg.Wait()
	mesh := &model.Mesh{
		Bounds: model.EmptyBounds(), ParamsHash: cfg.Hash(), ParamsVersion: 1,
		Algorithm: model.AlgorithmVersion, BuildMode: mode, SampleRate: sampleRate,
	}
	if previous != nil {
		mesh.ParamsVersion = previous.ParamsVersion
	}
	for i := range results {
		res := &results[i]
		vertexStart := len(mesh.Vertices)
		faceStart := len(mesh.Faces)
		if res.err != nil {
			res.chunk.VertexStart, res.chunk.FaceStart = vertexStart, faceStart
			res.chunk.VertexCount, res.chunk.FaceCount = 0, 0
			mesh.Chunks = append(mesh.Chunks, res.chunk)
			continue
		}
		mesh.Vertices = append(mesh.Vertices, res.vertices...)
		for _, f := range res.faces {
			f.A += vertexStart
			f.B += vertexStart
			f.C += vertexStart
			mesh.Faces = append(mesh.Faces, f)
		}
		res.chunk.VertexStart = vertexStart
		res.chunk.FaceStart = faceStart
		res.chunk.VertexCount = len(res.vertices)
		res.chunk.FaceCount = len(res.faces)
		mesh.Chunks = append(mesh.Chunks, res.chunk)
		mesh.Bounds.Union(res.chunk.Bounds)
	}
	mesh.ContentHash = model.HashMesh(mesh.Vertices, mesh.Faces, mesh.Chunks)
	mesh.RequestedChunks = sortedReasonKeys(reasons)
	b.mu.Lock()
	b.buildCount++
	b.chunkErrors = map[string]string{}
	for _, c := range mesh.Chunks {
		if c.Error != "" {
			b.chunkErrors[c.ID] = c.Error
		}
	}
	b.mu.Unlock()
	return mesh, nil
}

func reuseChunk(previous *model.Mesh, prev *model.ChunkMesh) chunkBuild {
	vertices := append([]model.MeshVertex(nil), previous.Vertices[prev.VertexStart:prev.VertexStart+prev.VertexCount]...)
	faces := make([]model.MeshFace, prev.FaceCount)
	base := prev.VertexStart
	for i := 0; i < prev.FaceCount; i++ {
		f := previous.Faces[prev.FaceStart+i]
		f.A -= base
		f.B -= base
		f.C -= base
		faces[i] = f
	}
	meta := *prev
	meta.Rebuilt = false
	meta.Reused = true
	meta.Reason = "unchanged"
	return chunkBuild{chunk: meta, vertices: vertices, faces: faces, reused: true, reason: "unchanged"}
}

func MakeChunks(bounds model.Bounds, chunkSize float64) []chunkDef {
	cols := int(math.Ceil((bounds.Max.X - bounds.Min.X) / chunkSize))
	rows := int(math.Ceil((bounds.Max.Y - bounds.Min.Y) / chunkSize))
	if cols < 1 {
		cols = 1
	}
	if rows < 1 {
		rows = 1
	}
	out := make([]chunkDef, 0, rows*cols)
	for row := 0; row < rows; row++ {
		for col := 0; col < cols; col++ {
			minX := bounds.Min.X + float64(col)*chunkSize
			minY := bounds.Min.Y + float64(row)*chunkSize
			maxX := minX + chunkSize
			maxY := minY + chunkSize
			if col == cols-1 {
				maxX = bounds.Max.X
			}
			if row == rows-1 {
				maxY = bounds.Max.Y
			}
			out = append(out, chunkDef{
				ID: generator.ChunkID(row, col), Row: row, Col: col,
				Bounds: model.Bounds{Min: model.Vec3{X: minX, Y: minY}, Max: model.Vec3{X: maxX, Y: maxY}},
			})
		}
	}
	return out
}

func buildChunk(points []model.Point, def chunkDef, cfg Config) (chunkBuild, error) {
	n := int(math.Round(cfg.ChunkSize/cfg.ResampleSpacing)) + 1
	if n < 2 {
		n = 2
	}
	if n*n > cfg.VertexBufferLimit {
		return chunkBuild{}, model.NewError(model.EPARA, "chunk vertex buffer limit exceeded", "params.vertexBufferLimit", 0, map[string]any{
			"chunk": def.ID, "vertices": n * n, "limit": cfg.VertexBufferLimit,
		})
	}
	indices := pointsInBounds(points, def.Bounds, cfg.ResampleSpacing*1.01)
	if len(indices) == 0 {
		return chunkBuild{}, model.NewError(model.EDECODE, "chunk has no source points", def.ID, 0, nil)
	}
	grid := make([][]float64, n)
	intensities := make([][]float64, n)
	colors := make([][][3]byte, n)
	for iy := 0; iy < n; iy++ {
		grid[iy] = make([]float64, n)
		intensities[iy] = make([]float64, n)
		colors[iy] = make([][3]byte, n)
	}
	for iy := 0; iy < n; iy++ {
		y := def.Bounds.Min.Y + float64(iy)*cfg.ResampleSpacing
		for ix := 0; ix < n; ix++ {
			x := def.Bounds.Min.X + float64(ix)*cfg.ResampleSpacing
			weights := sampleLocal(points, indices, model.Vec3{X: x, Y: y}, cfg.ResampleSpacing)
			if len(weights) == 0 {
				return chunkBuild{}, model.NewError(model.EDECODE, "uniform resampling found an empty sample cell", def.ID, 0, map[string]any{"x": x, "y": y})
			}
			z, intensity, r, g, blue, total := 0.0, 0.0, 0.0, 0.0, 0.0, 0.0
			for k, weight := range weights {
				if weight == 0 {
					continue
				}
				p := points[indices[k]]
				z += weight * p.Z
				intensity += weight * p.Intensity
				r += weight * float64(p.R)
				g += weight * float64(p.G)
				blue += weight * float64(p.B)
				total += weight
			}
			if total < 1e-12 {
				return chunkBuild{}, model.NewError(model.EDECODE, "resampling weights collapsed", def.ID, 0, nil)
			}
			grid[iy][ix] = z/total + cfg.ChunkBias[def.ID]
			intensities[iy][ix] = intensity / total
			if r+g+blue < 1e-9 {
				cr, cg, cb := generator.PointColor(x, y, grid[iy][ix])
				colors[iy][ix] = [3]byte{cr, cg, cb}
			} else {
				colors[iy][ix] = [3]byte{
					uint8(math.Max(0, math.Min(255, r/total))),
					uint8(math.Max(0, math.Min(255, g/total))),
					uint8(math.Max(0, math.Min(255, blue/total))),
				}
			}
		}
	}
	vertices := make([]model.MeshVertex, 0, n*n)
	for iy := 0; iy < n; iy++ {
		y := def.Bounds.Min.Y + float64(iy)*cfg.ResampleSpacing
		for ix := 0; ix < n; ix++ {
			x := def.Bounds.Min.X + float64(ix)*cfg.ResampleSpacing
			vertices = append(vertices, model.MeshVertex{
				Position: model.Vec3{X: x, Y: y, Z: grid[iy][ix]},
				Normal:   gridNormal(grid, ix, iy, cfg.ResampleSpacing, n),
				Color:    colors[iy][ix], Intensity: intensities[iy][ix],
			})
		}
	}
	faces := make([]model.MeshFace, 0, (n-1)*(n-1)*2)
	for iy := 0; iy < n-1; iy++ {
		for ix := 0; ix < n-1; ix++ {
			a := iy*n + ix
			b := a + 1
			c := a + n
			d := c + 1
			faces = append(faces, model.MeshFace{A: a, B: b, C: c, Chunk: def.ID})
			faces = append(faces, model.MeshFace{A: b, B: d, C: c, Chunk: def.ID})
		}
	}
	bounds := model.EmptyBounds()
	for _, v := range vertices {
		bounds.Include(v.Position)
	}
	chunk := model.ChunkMesh{
		ID: def.ID, Row: def.Row, Col: def.Col, Bounds: bounds,
		VertexCount: len(vertices), FaceCount: len(faces), PointIndices: indices,
		ContentKey: ChunkContentKey(points, indices, cfg, def.ID), GridN: n,
	}
	chunk.ContentHash = model.HashBytes(model.EncodeMeshPayload(vertices, faces))
	return chunkBuild{chunk: chunk, vertices: vertices, faces: faces}, nil
}

func pointsInBounds(points []model.Point, b model.Bounds, halo float64) []int {
	idx := make([]int, 0, 256)
	for i, p := range points {
		if p.X >= b.Min.X-halo && p.X <= b.Max.X+halo && p.Y >= b.Min.Y-halo && p.Y <= b.Max.Y+halo {
			idx = append(idx, i)
		}
	}
	return idx
}

func sampleLocal(points []model.Point, indices []int, target model.Vec3, radius float64) []float64 {
	type candidate struct {
		pos int
		d2  float64
	}
	cands := make([]candidate, 0, len(indices))
	limit := radius * radius * 4
	for _, idx := range indices {
		p := points[idx]
		dx, dy := p.X-target.X, p.Y-target.Y
		d2 := dx*dx + dy*dy
		if d2 < 1e-18 {
			out := make([]float64, len(indices))
			for i, j := range indices {
				if j == idx {
					out[i] = 1
				}
			}
			return out
		}
		if d2 <= limit {
			cands = append(cands, candidate{pos: idx, d2: d2})
		}
	}
	sort.Slice(cands, func(i, j int) bool {
		if cands[i].d2 != cands[j].d2 {
			return cands[i].d2 < cands[j].d2
		}
		return cands[i].pos < cands[j].pos
	})
	if len(cands) > 8 {
		cands = cands[:8]
	}
	if len(cands) == 0 {
		best, bestD := -1, math.Inf(1)
		for _, idx := range indices {
			p := points[idx]
			dx, dy := p.X-target.X, p.Y-target.Y
			d2 := dx*dx + dy*dy
			if d2 < bestD {
				best, bestD = idx, d2
			}
		}
		if best < 0 {
			return nil
		}
		out := make([]float64, len(indices))
		for i, j := range indices {
			if j == best {
				out[i] = 1
			}
		}
		return out
	}
	out := make([]float64, len(indices))
	total := 0.0
	for _, c := range cands {
		w := 1.0 / (c.d2 + 1e-9)
		out[indexAt(indices, c.pos)] = w
		total += w
	}
	for i := range out {
		out[i] /= total
	}
	return out
}

func indexAt(indices []int, value int) int {
	for i, v := range indices {
		if v == value {
			return i
		}
	}
	return -1
}

func gridNormal(grid [][]float64, ix, iy int, spacing float64, n int) model.Vec3 {
	x0, x1 := max(0, ix-1), min(n-1, ix+1)
	y0, y1 := max(0, iy-1), min(n-1, iy+1)
	dx := (grid[iy][x1] - grid[iy][x0]) / (float64(x1-x0) * spacing)
	dy := (grid[y1][ix] - grid[y0][ix]) / (float64(y1-y0) * spacing)
	return model.Vec3{X: -dx, Y: -dy, Z: 1}.Normalize()
}

func ChunkContentKey(points []model.Point, indices []int, cfg Config, chunkID string) string {
	parts := [][]byte{[]byte(chunkID), []byte(cfg.Hash()), []byte(model.AlgorithmVersion)}
	var buf [8]byte
	for _, i := range indices {
		p := points[i]
		binary.LittleEndian.PutUint64(buf[:], math.Float64bits(p.X))
		parts = append(parts, append([]byte(nil), buf[:]...))
		binary.LittleEndian.PutUint64(buf[:], math.Float64bits(p.Y))
		parts = append(parts, append([]byte(nil), buf[:]...))
		binary.LittleEndian.PutUint64(buf[:], math.Float64bits(p.Z))
		parts = append(parts, append([]byte(nil), buf[:]...))
		parts = append(parts, []byte{p.R, p.G, p.B, uint8(clamp01(p.Intensity) * 255)})
	}
	return model.HashBytes(parts...)
}

func PlanInvalidation(mesh *model.Mesh, chunkID, reason string) map[string]string {
	reasons := map[string]string{}
	if mesh == nil {
		return reasons
	}
	var target *model.ChunkMesh
	for i := range mesh.Chunks {
		if mesh.Chunks[i].ID == chunkID {
			target = &mesh.Chunks[i]
			break
		}
	}
	if target == nil {
		return reasons
	}
	reasons[chunkID] = reason
	for i := range mesh.Chunks {
		c := &mesh.Chunks[i]
		if c.ID == chunkID {
			continue
		}
		if abs(c.Row-target.Row) <= 1 && abs(c.Col-target.Col) <= 1 {
			reasons[c.ID] = "neighbor-invalidation"
		}
	}
	return reasons
}

func PlanAll(mesh *model.Mesh, reason string) map[string]string {
	reasons := map[string]string{}
	if mesh == nil {
		return reasons
	}
	for _, c := range mesh.Chunks {
		reasons[c.ID] = reason
	}
	return reasons
}

func ChunkForPoint(mesh *model.Mesh, pointIndex int) (string, bool) {
	if mesh == nil {
		return "", false
	}
	for _, c := range mesh.Chunks {
		for _, idx := range c.PointIndices {
			if idx == pointIndex {
				return c.ID, true
			}
		}
	}
	return "", false
}

func PointsBounds(points []model.Point) model.Bounds {
	b := model.EmptyBounds()
	for _, p := range points {
		b.Include(p.Position())
	}
	return b
}

func MeshFeatures(mesh *model.Mesh) model.FeatureSet {
	var f model.FeatureSet
	if mesh == nil || len(mesh.Vertices) == 0 {
		return f
	}
	peak, trough := mesh.Vertices[0].Position, mesh.Vertices[0].Position
	minCorner, maxCorner := mesh.Vertices[0].Position, mesh.Vertices[0].Position
	for _, v := range mesh.Vertices {
		p := v.Position
		if p.Z > peak.Z {
			peak = p
		}
		if p.Z < trough.Z {
			trough = p
		}
		if p.X < minCorner.X || (p.X == minCorner.X && p.Y < minCorner.Y) {
			minCorner = p
		}
		if p.X > maxCorner.X || (p.X == maxCorner.X && p.Y > maxCorner.Y) {
			maxCorner = p
		}
	}
	ridge := peak
	for _, v := range mesh.Vertices {
		p := v.Position
		if p.X > -1.6 && p.X < 1.6 && p.Y > -1.2 && p.Y < 1.2 && p.Z > ridge.Z {
			ridge = p
		}
	}
	return model.FeatureSet{Peak: peak, Trough: trough, Ridge: ridge, MinCorner: minCorner, MaxCorner: maxCorner}
}

func Denoise(points []model.Point, cfg Config) ([]model.Point, []int, error) {
	if len(points) == 0 {
		return nil, nil, model.NewError(model.EDECODE, "empty point cloud", "points", 0, nil)
	}
	k := min(cfg.NormalK, len(points)-1)
	if k < 1 {
		return append([]model.Point(nil), points...), nil, nil
	}
	distances := make([]float64, len(points))
	scratch := make([]float64, 0, len(points)-1)
	for i, p := range points {
		scratch = scratch[:0]
		for j, q := range points {
			if i == j {
				continue
			}
			dx, dy, dz := p.X-q.X, p.Y-q.Y, p.Z-q.Z
			scratch = append(scratch, math.Sqrt(dx*dx+dy*dy+dz*dz))
		}
		sort.Float64s(scratch)
		sum := 0.0
		for n := 0; n < k; n++ {
			sum += scratch[n]
		}
		distances[i] = sum / float64(k)
	}
	mean, variance := 0.0, 0.0
	for _, d := range distances {
		mean += d
	}
	mean /= float64(len(distances))
	for _, d := range distances {
		variance += (d - mean) * (d - mean)
	}
	std := math.Sqrt(variance / float64(len(distances)))
	threshold := mean + cfg.OutlierSigma*std
	clean := make([]model.Point, 0, len(points))
	removed := make([]int, 0)
	for i, p := range points {
		if distances[i] > threshold {
			removed = append(removed, i)
		} else {
			clean = append(clean, p)
		}
	}
	return clean, removed, nil
}

func VoxelFilter(points []model.Point, voxel float64) []model.Point {
	if voxel <= 0 {
		return points
	}
	seen := make(map[[3]int64]struct{}, len(points))
	out := make([]model.Point, 0, len(points))
	for _, p := range points {
		key := [3]int64{
			int64(math.Floor(p.X / voxel)),
			int64(math.Floor(p.Y / voxel)),
			int64(math.Floor(p.Z / voxel)),
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, p)
	}
	return out
}

func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

func durationMS(d time.Duration) float64 {
	return float64(d.Microseconds()) / 1000
}

func sortedReasonKeys(reasons map[string]string) []string {
	out := make([]string, 0, len(reasons))
	for k := range reasons {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
