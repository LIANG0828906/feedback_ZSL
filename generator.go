package acousticfield

import (
	"fmt"
	"sort"
)

type BandSpec struct {
	ID       string
	CenterHz int64
}

type GeneratorConfig struct {
	Seed              int64
	Cols              int
	Rows              int
	WidthMicros       int64
	HeightMicros      int64
	Bands             []BandSpec
	MaxReflections    int
	PropagationRounds int
	MaxPaths          int
}

func DefaultGeneratorConfig(seed int64) GeneratorConfig {
	return GeneratorConfig{
		Seed:              seed,
		Cols:              6,
		Rows:              5,
		WidthMicros:       12_000_000,
		HeightMicros:      10_000_000,
		MaxReflections:    2,
		PropagationRounds: 3,
		MaxPaths:          64,
		Bands: []BandSpec{
			{ID: "125Hz", CenterHz: 125},
			{ID: "500Hz", CenterHz: 500},
			{ID: "2000Hz", CenterHz: 2000},
		},
	}
}

func Generate(seed int64) (*Sample, error) {
	return GenerateConfig(DefaultGeneratorConfig(seed))
}

// ComputeTruth recomputes the analytic truth for a level. It is exported so
// edited levels can still be judged against generator truth rather than
// against solver output.
func ComputeTruth(level *Level, options Options) Truth {
	options = options.Normalized()
	return generateTruth(level, GeneratorConfig{
		Seed:              level.Seed,
		MaxReflections:    options.MaxReflections,
		PropagationRounds: options.PropagationRounds,
		MaxPaths:          options.MaxPathsPerChunk,
	})
}

// GenerateConfig deterministically creates a complete planar sample and its
// analytic truth from cfg.Seed. It never invokes the reconstruction solver.
func GenerateConfig(cfg GeneratorConfig) (*Sample, error) {
	if cfg.Cols <= 0 || cfg.Rows <= 0 {
		return nil, valueError("generator", "grid", fmt.Sprintf("%dx%d", cfg.Cols, cfg.Rows), "positive", "positive")
	}
	if cfg.WidthMicros <= 0 || cfg.HeightMicros <= 0 {
		return nil, valueError("generator", "bounds", fmt.Sprintf("%dx%d", cfg.WidthMicros, cfg.HeightMicros), 1, int64(^uint64(0)>>1))
	}
	if cfg.WidthMicros%int64(cfg.Cols) != 0 || cfg.HeightMicros%int64(cfg.Rows) != 0 {
		return nil, valueError("generator", "bounds", "not divisible by grid", "divisible", "divisible")
	}
	if len(cfg.Bands) == 0 {
		return nil, schemaError("generator", "bands", "at least one band is required")
	}
	if cfg.MaxReflections <= 0 {
		cfg.MaxReflections = 2
	}
	if cfg.PropagationRounds <= 0 {
		cfg.PropagationRounds = cfg.MaxReflections + 1
	}
	if cfg.MaxPaths <= 0 {
		cfg.MaxPaths = 64
	}
	bands := append([]BandSpec(nil), cfg.Bands...)
	sort.Slice(bands, func(i, j int) bool { return bands[i].ID < bands[j].ID })
	for i := range bands {
		if bands[i].ID == "" || bands[i].CenterHz <= 0 {
			return nil, valueError("generator", fmt.Sprintf("bands[%d]", i), bands[i], "positive id/hz", "positive id/hz")
		}
	}

	rng := newSplitMix(uint64(cfg.Seed) ^ 0x6a09e667f3bcc909)
	level := &Level{
		ID:     fmt.Sprintf("level-seed-%d", cfg.Seed),
		Seed:   cfg.Seed,
		Bounds: Rect{MinX: 0, MinY: 0, MaxX: cfg.WidthMicros, MaxY: cfg.HeightMicros},
	}
	for _, b := range bands {
		level.Bands = append(level.Bands, Band{ID: b.ID, CenterHz: b.CenterHz})
	}

	cw := cfg.WidthMicros / int64(cfg.Cols)
	ch := cfg.HeightMicros / int64(cfg.Rows)
	for row := 0; row < cfg.Rows; row++ {
		for col := 0; col < cfg.Cols; col++ {
			id := row*cfg.Cols + col
			bounds := Rect{
				MinX: int64(col) * cw,
				MinY: int64(row) * ch,
				MaxX: int64(col+1) * cw,
				MaxY: int64(row+1) * ch,
			}
			chunk := Chunk{ID: id, Bounds: bounds}
			for _, b := range bands {
				// 0.12 .. 0.55, strictly positive and below unity.
				chunk.Absorption = append(chunk.Absorption, Absorption{
					BandID:      b.ID,
					Coefficient: 8_000 + rng.int64n(28_000),
				})
			}
			chunk.Reflectors = []Reflector{
				{
					ID: fmt.Sprintf("c%03d-left", id), A: Point{X: bounds.MinX, Y: bounds.MinY},
					B: Point{X: bounds.MinX, Y: bounds.MaxY}, Normal: Vec{X: Q16, Y: 0}, Material: "left-wall",
				},
				{
					ID: fmt.Sprintf("c%03d-right", id), A: Point{X: bounds.MaxX, Y: bounds.MinY},
					B: Point{X: bounds.MaxX, Y: bounds.MaxY}, Normal: Vec{X: -Q16, Y: 0}, Material: "right-wall",
				},
				{
					ID: fmt.Sprintf("c%03d-bottom", id), A: Point{X: bounds.MinX, Y: bounds.MinY},
					B: Point{X: bounds.MaxX, Y: bounds.MinY}, Normal: Vec{X: 0, Y: Q16}, Material: "bottom-wall",
				},
				{
					ID: fmt.Sprintf("c%03d-top", id), A: Point{X: bounds.MinX, Y: bounds.MaxY},
					B: Point{X: bounds.MaxX, Y: bounds.MaxY}, Normal: Vec{X: 0, Y: -Q16}, Material: "top-wall",
				},
			}
			sortAbsorption(chunk.Absorption)
			sortReflectors(chunk.Reflectors)
			level.Chunks = append(level.Chunks, chunk)
		}
	}

	for i := range level.Chunks {
		c := &level.Chunks[i]
		cx := c.Bounds.Width() / 2
		cy := c.Bounds.Height() / 2
		sx := c.Bounds.MinX + cx + rng.int64n(c.Bounds.Width()/5) - c.Bounds.Width()/10
		sy := c.Bounds.MinY + cy + rng.int64n(c.Bounds.Height()/5) - c.Bounds.Height()/10
		lx := c.Bounds.MinX + cx + rng.int64n(c.Bounds.Width()/4) - c.Bounds.Width()/8
		ly := c.Bounds.MinY + cy + rng.int64n(c.Bounds.Height()/4) - c.Bounds.Height()/8
		src := Source{
			ID:       fmt.Sprintf("src-%03d", c.ID),
			ChunkID:  c.ID,
			Position: Point{X: sx, Y: sy},
		}
		for _, b := range bands {
			src.Emission = append(src.Emission, BandEnergy{
				BandID: b.ID,
				Energy: 3_000_000 + rng.int64n(1_000_000),
			})
		}
		sortBandEnergy(src.Emission)
		level.Sources = append(level.Sources, src)
		level.Listeners = append(level.Listeners, Listener{
			ID:      fmt.Sprintf("lis-%03d", c.ID),
			ChunkID: c.ID,
			Point:   Point{X: lx, Y: ly},
		})
	}

	if err := ValidateLevel(level); err != nil {
		return nil, err
	}
	truth := generateTruth(level, cfg)
	return &Sample{Level: level, Truth: truth}, nil
}

type splitMix struct {
	state uint64
}

func newSplitMix(seed uint64) *splitMix { return &splitMix{state: seed} }

func (r *splitMix) next() uint64 {
	r.state += 0x9e3779b97f4a7c15
	z := r.state
	z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9
	z = (z ^ (z >> 27)) * 0x94d049bb133111eb
	return z ^ (z >> 31)
}

func (r *splitMix) int64n(n int64) int64 {
	if n <= 0 {
		return 0
	}
	return int64(r.next() % uint64(n))
}

type truthPacket struct {
	chunk  int
	wave   int
	energy []BandEnergy
}

// generateTruth computes truth with a separate closed-form routing routine.
// This routine intentionally does not call the solver or any cache code.
func generateTruth(level *Level, cfg GeneratorConfig) Truth {
	tr := Truth{LevelID: level.ID}
	for _, b := range level.Bands {
		tr.Bands = append(tr.Bands, b.ID)
	}
	sort.Strings(tr.Bands)
	type chunkAccumulator struct {
		energy []BandEnergy
		source []SourceEnergy
	}
	acc := make(map[int]*chunkAccumulator, len(level.Chunks))
	for _, c := range level.Chunks {
		acc[c.ID] = &chunkAccumulator{}
	}
	sourceEscapes := make(map[string][]BandEnergy)
	neighbor := gridNeighbors(level)

	for _, src := range level.Sources {
		sourceEscapes[src.ID] = nil
		packets := []truthPacket{{chunk: src.ChunkID, wave: 0, energy: cloneBandEnergy(src.Emission)}}
		for wave := 0; wave < cfg.PropagationRounds; wave++ {
			var next []truthPacket
			for _, p := range packets {
				c, ok := chunkByID(level, p.chunk)
				if !ok {
					continue
				}
				coeff := map[string]int64{}
				for _, a := range c.Absorption {
					coeff[a.BandID] = a.Coefficient
				}
				var captured []BandEnergy
				var remainder []BandEnergy
				for _, e := range p.energy {
					fieldFraction := Q16 - coeff[e.BandID]
					capEnergy := e.Energy * fieldFraction / Q16
					captured = addBandEnergy(captured, e.BandID, capEnergy)
					remainder = addBandEnergy(remainder, e.BandID, e.Energy-capEnergy)
				}
				sortBandEnergy(captured)
				acc[p.chunk].energy = mergeBandEnergy(acc[p.chunk].energy, captured)
				acc[p.chunk].source = mergeSourceEnergy(acc[p.chunk].source, SourceEnergy{SourceID: src.ID, Energy: captured})

				dirs := [4]string{"left", "right", "bottom", "top"}
				shares := make(map[string][]BandEnergy, 4)
				for _, e := range remainder {
					base := e.Energy / 4
					rem := e.Energy % 4
					for d := 0; d < 4; d++ {
						amount := base
						if int64(d) < rem {
							amount++
						}
						shares[dirs[d]] = addBandEnergy(shares[dirs[d]], e.BandID, amount)
					}
				}
				for _, side := range dirs {
					nb, exists := neighbor[p.chunk][side]
					energies := shares[side]
					if wave+1 < cfg.PropagationRounds && exists {
						if len(energies) > 0 {
							next = append(next, truthPacket{chunk: nb, wave: wave + 1, energy: energies})
						}
					} else {
						for _, e := range energies {
							sourceEscapes[src.ID] = addBandEnergy(sourceEscapes[src.ID], e.BandID, e.Energy)
						}
					}
				}
			}
			packets = next
			if len(packets) == 0 {
				break
			}
		}
		// Packets that exist only after the last processed wave are already
		// converted to escape energy by the branch above.
	}

	chunkIDs := make([]int, 0, len(level.Chunks))
	for id := range acc {
		chunkIDs = append(chunkIDs, id)
	}
	sort.Ints(chunkIDs)
	for _, id := range chunkIDs {
		a := acc[id]
		sortBandEnergy(a.energy)
		sort.Slice(a.source, func(i, j int) bool { return a.source[i].SourceID < a.source[j].SourceID })
		for i := range a.source {
			sortBandEnergy(a.source[i].Energy)
		}
		tr.Chunks = append(tr.Chunks, TruthChunk{ChunkID: id, Energy: a.energy, SourceEnergy: a.source})
	}

	for _, c := range level.Chunks {
		src, sok := sourceByChunk(level, c.ID)
		lis, lok := listenerByChunk(level, c.ID)
		if !sok || !lok {
			continue
		}
		paths := analyticChunkPaths(c, src.Position, lis.Point, cfg.MaxReflections, cfg.MaxPaths)
		for i, p := range paths {
			tr.Paths = append(tr.Paths, TruthPath{
				ChunkID: c.ID, PathID: pathID(c.ID, i), Reflectors: append([]string(nil), p.reflectors...), LengthMicros: p.length,
			})
		}
		chunkEnergy := acc[c.ID].energy
		total := int64(0)
		var bandsOut []BandEnergy
		for _, b := range level.Bands {
			e := bandValue(chunkEnergy, b.ID)
			bandsOut = append(bandsOut, BandEnergy{BandID: b.ID, Energy: e})
			total += e
		}
		sortBandEnergy(bandsOut)
		tr.Listeners = append(tr.Listeners, TruthListener{
			ListenerID: lis.ID, ChunkID: c.ID, BandEnergy: bandsOut, TotalEnergy: total,
		})
	}

	sort.Slice(tr.Sources, func(i, j int) bool { return tr.Sources[i].SourceID < tr.Sources[j].SourceID })
	for _, src := range level.Sources {
		esc := sourceEscapes[src.ID]
		sortBandEnergy(esc)
		tr.Sources = append(tr.Sources, TruthSource{SourceID: src.ID, Emission: src.Emission, Escape: esc})
	}
	sort.Slice(tr.Sources, func(i, j int) bool { return tr.Sources[i].SourceID < tr.Sources[j].SourceID })
	sort.Slice(tr.Listeners, func(i, j int) bool { return tr.Listeners[i].ListenerID < tr.Listeners[j].ListenerID })
	sort.Slice(tr.Paths, func(i, j int) bool {
		if tr.Paths[i].ChunkID != tr.Paths[j].ChunkID {
			return tr.Paths[i].ChunkID < tr.Paths[j].ChunkID
		}
		return tr.Paths[i].PathID < tr.Paths[j].PathID
	})
	tr.Fingerprint = truthFingerprint(tr)
	return tr
}

func truthFingerprint(t Truth) string {
	t.Fingerprint = ""
	return canonicalHash(t)
}

type analyticPath struct {
	reflectors []string
	points     []Point
	length     int64
}

func analyticChunkPaths(c Chunk, source, listener Point, maxReflections int, maxPaths int) []analyticPath {
	refls := append([]Reflector(nil), c.Reflectors...)
	sortReflectors(refls)
	var out []analyticPath
	add := func(refs []int) {
		p, ok := analyticUnfold(source, listener, refls, refs)
		if !ok || !pathInside(c.Bounds, p.points) {
			return
		}
		for _, refl := range refs {
			p.reflectors = append(p.reflectors, refls[refl].ID)
		}
		out = append(out, p)
	}
	add(nil)
	var walk func(depth int, last int, seq []int)
	walk = func(depth int, last int, seq []int) {
		if depth >= maxReflections || len(out) >= maxPaths {
			return
		}
		for i := range refls {
			seq = append(seq, i)
			add(append([]int(nil), seq...))
			walk(depth+1, i, seq)
			seq = seq[:len(seq)-1]
			if len(out) >= maxPaths {
				return
			}
		}
	}
	walk(0, -1, nil)
	sort.SliceStable(out, func(i, j int) bool {
		if len(out[i].reflectors) != len(out[j].reflectors) {
			return len(out[i].reflectors) < len(out[j].reflectors)
		}
		a := fmt.Sprint(out[i].reflectors)
		b := fmt.Sprint(out[j].reflectors)
		if a != b {
			return a < b
		}
		return out[i].length < out[j].length
	})
	if len(out) > maxPaths {
		out = out[:maxPaths]
	}
	return out
}

func analyticUnfold(source, listener Point, refls []Reflector, seq []int) (analyticPath, bool) {
	images := []Point{source}
	img := source
	for _, idx := range seq {
		img = reflectPoint(img, refls[idx])
		images = append(images, img)
	}
	points := make([]Point, len(seq)+2)
	points[0] = source
	points[len(points)-1] = listener
	current := listener
	for i := len(seq) - 1; i >= 0; i-- {
		p, ok := intersectReflector(images[i+1], current, refls[seq[i]])
		if !ok || !pointOnSegment(p, refls[seq[i]].A, refls[seq[i]].B, 2) {
			return analyticPath{}, false
		}
		points[i+1] = p
		current = p
	}
	length := int64(0)
	for i := 0; i+1 < len(points); i++ {
		length += Distance(points[i], points[i+1])
	}
	if length <= 0 {
		return analyticPath{}, false
	}
	return analyticPath{points: points, length: length}, true
}

func pathInside(bounds Rect, points []Point) bool {
	for _, p := range points {
		if !pointInRect(p, bounds) {
			return false
		}
	}
	return true
}

func pathID(chunkID, index int) string {
	return fmt.Sprintf("c%03d-p%03d", chunkID, index)
}

func gridNeighbors(level *Level) map[int]map[string]int {
	out := make(map[int]map[string]int, len(level.Chunks))
	for _, c := range level.Chunks {
		for _, other := range level.Chunks {
			if c.ID == other.ID {
				continue
			}
			if c.Bounds.MaxX == other.Bounds.MinX && rangesOverlap(c.Bounds.MinY, c.Bounds.MaxY, other.Bounds.MinY, other.Bounds.MaxY) {
				if out[c.ID] == nil {
					out[c.ID] = map[string]int{}
				}
				out[c.ID]["right"] = other.ID
			}
			if c.Bounds.MinX == other.Bounds.MaxX && rangesOverlap(c.Bounds.MinY, c.Bounds.MaxY, other.Bounds.MinY, other.Bounds.MaxY) {
				if out[c.ID] == nil {
					out[c.ID] = map[string]int{}
				}
				out[c.ID]["left"] = other.ID
			}
			if c.Bounds.MaxY == other.Bounds.MinY && rangesOverlap(c.Bounds.MinX, c.Bounds.MaxX, other.Bounds.MinX, other.Bounds.MaxX) {
				if out[c.ID] == nil {
					out[c.ID] = map[string]int{}
				}
				out[c.ID]["top"] = other.ID
			}
			if c.Bounds.MinY == other.Bounds.MaxY && rangesOverlap(c.Bounds.MinX, c.Bounds.MaxX, other.Bounds.MinX, other.Bounds.MaxX) {
				if out[c.ID] == nil {
					out[c.ID] = map[string]int{}
				}
				out[c.ID]["bottom"] = other.ID
			}
		}
	}
	return out
}

func rangesOverlap(aMin, aMax, bMin, bMax int64) bool {
	return aMin < bMax && bMin < aMax
}

func cloneBandEnergy(v []BandEnergy) []BandEnergy {
	out := append([]BandEnergy(nil), v...)
	sortBandEnergy(out)
	return out
}

func mergeBandEnergy(dst, src []BandEnergy) []BandEnergy {
	for _, e := range src {
		dst = addBandEnergy(dst, e.BandID, e.Energy)
	}
	return dst
}

func mergeSourceEnergy(dst []SourceEnergy, add SourceEnergy) []SourceEnergy {
	for i := range dst {
		if dst[i].SourceID == add.SourceID {
			dst[i].Energy = mergeBandEnergy(dst[i].Energy, add.Energy)
			return dst
		}
	}
	dst = append(dst, SourceEnergy{SourceID: add.SourceID, Energy: cloneBandEnergy(add.Energy)})
	sort.Slice(dst, func(i, j int) bool { return dst[i].SourceID < dst[j].SourceID })
	return dst
}
