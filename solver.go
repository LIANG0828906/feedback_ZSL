package acousticfield

import (
	"context"
	"fmt"
	"sort"
)

type wavePacket struct {
	sourceID string
	chunkID  int
	wave     int
	energy   []BandEnergy
}

// EnergyPlan is the deterministic wave-propagation plan shared by all chunks.
// It contains captured source energy, boundary transfers, escapes, truncations
// and quantization records.
type EnergyPlan struct {
	Captured     map[string]map[int][]BandEnergy
	Incoming     map[int][]Transfer
	Outgoing     map[int][]Transfer
	Escapes      map[int][]Escape
	Truncations  map[int][]Truncation
	Transfers    []Transfer
	Neighbors    map[int]map[string]int
	Quantization []Quantization
}

func buildEnergyPlan(level *Level, options Options) (*EnergyPlan, error) {
	options = options.Normalized()
	nb := gridNeighbors(level)
	plan := &EnergyPlan{
		Captured:    make(map[string]map[int][]BandEnergy, len(level.Sources)),
		Incoming:    make(map[int][]Transfer, len(level.Chunks)),
		Outgoing:    make(map[int][]Transfer, len(level.Chunks)),
		Escapes:     make(map[int][]Escape, len(level.Chunks)),
		Truncations: make(map[int][]Truncation, len(level.Chunks)),
		Neighbors:   nb,
	}
	for _, src := range level.Sources {
		plan.Captured[src.ID] = make(map[int][]BandEnergy, len(level.Chunks))
	}

	// Aggregate transfers by stable key so order of packet traversal cannot
	// affect persisted records.
	type transferKey struct {
		from, to, wave int
		source         string
	}
	transferMap := map[transferKey]*Transfer{}
	dirs := [4]string{"left", "right", "bottom", "top"}

	for si := range level.Sources {
		src := &level.Sources[si]
		sortBandEnergy(src.Emission)
		packets := []wavePacket{{sourceID: src.ID, chunkID: src.ChunkID, wave: 0, energy: cloneBandEnergy(src.Emission)}}
		for wave := 0; wave < options.PropagationRounds; wave++ {
			var next []wavePacket
			for _, packet := range packets {
				c, ok := chunkByID(level, packet.chunkID)
				if !ok {
					return nil, &FieldError{Code: CodeValue, ObjectID: src.ID, FieldPath: "chunk_id", Measured: packet.chunkID, Message: "packet references unknown chunk"}
				}
				coeff := absorptionMap(c)
				var captured []BandEnergy
				var remainder []BandEnergy
				for _, e := range packet.energy {
					a, ok := coeff[e.BandID]
					if !ok {
						return nil, schemaError(fmt.Sprintf("chunk-%d", c.ID), "absorption", "band missing: "+e.BandID)
					}
					fieldFraction := Q16 - a
					value, residual := mulQ16Floor(e.Energy, fieldFraction)
					captured = addBandEnergy(captured, e.BandID, value)
					remainder = addBandEnergy(remainder, e.BandID, e.Energy-value)
					plan.Quantization = append(plan.Quantization, Quantization{
						Stage: "field_capture", ChunkID: c.ID, BandID: e.BandID,
						ExactNumerator: e.Energy * fieldFraction, Denominator: Q16, Value: value, Residual: residual,
					})
				}
				sortBandEnergy(captured)
				plan.Captured[src.ID][c.ID] = mergeBandEnergy(plan.Captured[src.ID][c.ID], captured)

				shares := make(map[string][]BandEnergy, 4)
				residuals := make(map[string][]BandEnergy, 4)
				for _, e := range remainder {
					base := e.Energy / 4
					rem := e.Energy % 4
					for d := 0; d < 4; d++ {
						amount := base
						res := int64(0)
						if int64(d) < rem {
							amount++
							res = 1
						}
						shares[dirs[d]] = addBandEnergy(shares[dirs[d]], e.BandID, amount)
						residuals[dirs[d]] = addBandEnergy(residuals[dirs[d]], e.BandID, res)
					}
				}
				for _, side := range dirs {
					amounts := shares[side]
					if len(amounts) == 0 {
						continue
					}
					nbID, exists := nb[c.ID][side]
					if wave+1 < options.PropagationRounds && exists {
						key := transferKey{from: c.ID, to: nbID, wave: wave + 1, source: src.ID}
						t := transferMap[key]
						if t == nil {
							t = &Transfer{
								SourceID: src.ID, FromChunk: c.ID, ToChunk: nbID, Wave: wave + 1,
								Boundary: boundaryFor(c.Bounds, side),
							}
							transferMap[key] = t
						}
						for _, e := range amounts {
							t.Energy = addBandEnergy(t.Energy, e.BandID, e.Energy)
						}
						for _, e := range residuals[side] {
							t.QuantizationResidual = addBandEnergy(t.QuantizationResidual, e.BandID, e.Energy)
						}
						sortBandEnergy(t.Energy)
						next = append(next, wavePacket{sourceID: src.ID, chunkID: nbID, wave: wave + 1, energy: amounts})
					} else {
						reason := "level_boundary"
						if exists {
							reason = "truncation"
						}
						for _, e := range amounts {
							esc := Escape{SourceID: src.ID, ChunkID: c.ID, BandID: e.BandID, Energy: e.Energy, Reason: reason, Wave: wave}
							if !exists {
								b := boundaryFor(c.Bounds, side)
								esc.Boundary = &b
							}
							plan.Escapes[c.ID] = append(plan.Escapes[c.ID], esc)
						}
						if exists {
							plan.Truncations[c.ID] = append(plan.Truncations[c.ID], Truncation{
								Stage: "propagation_round_limit", ChunkID: c.ID, Wave: wave,
								Position: c.Bounds.Center(), Residual: sumBandEnergy(residuals[side]),
							})
						}
					}
				}
			}
			packets = next
			if len(packets) == 0 {
				break
			}
		}
	}

	type sortableTransfer struct {
		key transferKey
		t   *Transfer
	}
	list := make([]sortableTransfer, 0, len(transferMap))
	for k, t := range transferMap {
		sortBandEnergy(t.Energy)
		sortBandEnergy(t.QuantizationResidual)
		t.ID = fmt.Sprintf("%s-%d-%d-w%d", t.SourceID, t.FromChunk, t.ToChunk, t.Wave)
		t.Version = transferDigest(*t)
		list = append(list, sortableTransfer{key: k, t: t})
	}
	sort.Slice(list, func(i, j int) bool {
		a, b := list[i], list[j]
		if a.t.FromChunk != b.t.FromChunk {
			return a.t.FromChunk < b.t.FromChunk
		}
		if a.t.ToChunk != b.t.ToChunk {
			return a.t.ToChunk < b.t.ToChunk
		}
		if a.t.Wave != b.t.Wave {
			return a.t.Wave < b.t.Wave
		}
		return a.t.SourceID < b.t.SourceID
	})
	for _, entry := range list {
		t := *entry.t
		plan.Transfers = append(plan.Transfers, t)
		plan.Outgoing[t.FromChunk] = append(plan.Outgoing[t.FromChunk], t)
		plan.Incoming[t.ToChunk] = append(plan.Incoming[t.ToChunk], t)
	}
	for id := range plan.Incoming {
		sortTransfers(plan.Incoming[id])
	}
	for id := range plan.Outgoing {
		sortTransfers(plan.Outgoing[id])
	}
	for id := range plan.Escapes {
		sort.Slice(plan.Escapes[id], func(i, j int) bool {
			a, b := plan.Escapes[id][i], plan.Escapes[id][j]
			if a.SourceID != b.SourceID {
				return a.SourceID < b.SourceID
			}
			if a.BandID != b.BandID {
				return a.BandID < b.BandID
			}
			return a.Wave < b.Wave
		})
	}
	return plan, nil
}

func absorptionMap(c *Chunk) map[string]int64 {
	out := make(map[string]int64, len(c.Absorption))
	for _, a := range c.Absorption {
		out[a.BandID] = a.Coefficient
	}
	return out
}

func boundaryFor(r Rect, fromSide string) Boundary {
	switch fromSide {
	case "left":
		return Boundary{Side: "left", Position: Point{X: r.MinX, Y: r.Center().Y}, Length: r.Height()}
	case "right":
		return Boundary{Side: "right", Position: Point{X: r.MaxX, Y: r.Center().Y}, Length: r.Height()}
	case "bottom":
		return Boundary{Side: "bottom", Position: Point{X: r.Center().X, Y: r.MinY}, Length: r.Width()}
	default:
		return Boundary{Side: "top", Position: Point{X: r.Center().X, Y: r.MaxY}, Length: r.Width()}
	}
}

func sortTransfers(v []Transfer) {
	sort.Slice(v, func(i, j int) bool {
		if v[i].FromChunk != v[j].FromChunk {
			return v[i].FromChunk < v[j].FromChunk
		}
		if v[i].ToChunk != v[j].ToChunk {
			return v[i].ToChunk < v[j].ToChunk
		}
		if v[i].Wave != v[j].Wave {
			return v[i].Wave < v[j].Wave
		}
		return v[i].SourceID < v[j].SourceID
	})
}

func sumBandEnergy(v []BandEnergy) int64 {
	var n int64
	for _, e := range v {
		n += e.Energy
	}
	return n
}

// SolveChunk computes one chunk deterministically. workerBuffer is a
// worker-local capacity hint; exceeding it fails only this chunk.
func SolveChunk(level *Level, options Options, plan *EnergyPlan, chunkID, workerBuffer int) (ChunkResult, error) {
	options = options.Normalized()
	c, ok := chunkByID(level, chunkID)
	if !ok {
		return ChunkResult{}, &FieldError{Code: CodeValue, ObjectID: fmt.Sprintf("chunk-%d", chunkID), FieldPath: "id", Message: "unknown chunk"}
	}
	src, sok := sourceByChunk(level, chunkID)
	lis, lok := listenerByChunk(level, chunkID)
	if !sok || !lok {
		return ChunkResult{}, schemaError(fmt.Sprintf("chunk-%d", chunkID), "listener/source", "local source-listener pair is required")
	}
	paths, pathQuant, err := enumerateSolverPaths(c, src.Position, lis.Point, options, workerBuffer)
	if err != nil {
		return ChunkResult{}, err
	}
	if len(paths) > options.MaxPathsPerChunk {
		return ChunkResult{}, &FieldError{
			Code: CodeValue, ObjectID: fmt.Sprintf("chunk-%d", chunkID), FieldPath: "paths",
			Measured: len(paths), Min: 1, Max: options.MaxPathsPerChunk, Message: "path buffer budget exhausted",
		}
	}
	abs := absorptionMap(c)
	weights := make(map[string][]int64, len(level.Bands))
	for _, band := range level.Bands {
		bandWeights := make([]int64, len(paths))
		for i := range paths {
			pow := int64(Q16)
			for range paths[i].Reflectors {
				pow, _ = mulQ16Floor(pow, abs[band.ID])
			}
			d := paths[i].LengthMicros
			ref := int64(1_000_000)
			denom := ref*ref + d*d
			gain := int64(0)
			if denom > 0 {
				gain = (ref * ref * Q16) / denom
			}
			if gain <= 0 {
				gain = 1
			}
			w, _ := mulQ16Floor(pow, gain)
			if w <= 0 {
				w = 1
			}
			bandWeights[i] = w
		}
		weights[band.ID] = bandWeights
	}

	var allQuant []Quantization
	allQuant = append(allQuant, pathQuant...)
	for _, q := range plan.Quantization {
		if q.ChunkID == chunkID {
			allQuant = append(allQuant, q)
		}
	}
	var sourceEnergy []SourceEnergy
	var arrival []BandEnergy
	for _, s := range level.Sources {
		capturedByChunk := plan.Captured[s.ID]
		if capturedByChunk == nil {
			continue
		}
		captured := capturedByChunk[chunkID]
		if len(captured) == 0 || sumBandEnergy(captured) == 0 {
			continue
		}
		se := SourceEnergy{SourceID: s.ID}
		for _, band := range level.Bands {
			total := bandValue(captured, band.ID)
			if total == 0 {
				continue
			}
			se.Energy = addBandEnergy(se.Energy, band.ID, total)
			arrival = addBandEnergy(arrival, band.ID, total)
			if len(paths) == 0 {
				continue
			}
			alloc, quant := allocateBandEnergy(total, weights[band.ID])
			for _, q := range quant {
				q.Stage = "path_allocation"
				q.ChunkID = chunkID
				q.BandID = band.ID
				q.PathID = paths[q.Value].ID
				allQuant = append(allQuant, q)
			}
			for i := range alloc {
				if alloc[i] == 0 {
					continue
				}
				paths[i].Energy = addBandEnergy(paths[i].Energy, band.ID, alloc[i])
				paths[i].QuantizationResidual += quantResidualFor(quant, i)
			}
		}
		sortBandEnergy(se.Energy)
		if len(se.Energy) > 0 {
			sourceEnergy = append(sourceEnergy, se)
		}
	}
	sort.Slice(sourceEnergy, func(i, j int) bool { return sourceEnergy[i].SourceID < sourceEnergy[j].SourceID })
	sortBandEnergy(arrival)
	for i := range paths {
		sortBandEnergy(paths[i].Energy)
	}

	outgoing := append([]Transfer(nil), plan.Outgoing[chunkID]...)
	incoming := append([]Transfer(nil), plan.Incoming[chunkID]...)
	escapes := append([]Escape(nil), plan.Escapes[chunkID]...)
	truncs := append([]Truncation(nil), plan.Truncations[chunkID]...)
	sortTransfers(outgoing)
	sortTransfers(incoming)

	contentHash := ChunkContentHash(c)
	params := options.Version()
	deps := buildDependencies(level, contentHash, params, src, incoming)
	cacheKey := canonicalHash(struct {
		Content string       `json:"content"`
		Params  string       `json:"params"`
		Algo    string       `json:"algo"`
		Deps    []Dependency `json:"deps"`
	}{contentHash, params, AlgorithmVersion, deps})

	res := ChunkResult{
		ChunkID: chunkID, Paths: paths, PathDigest: PathDigest(paths),
		ArrivalEnergy: arrival, SourceEnergy: sourceEnergy,
		Incoming: incoming, Outgoing: outgoing, Escapes: escapes,
		Dependencies: deps, Quantization: allQuant, Truncations: truncs,
		ContentHash: contentHash, ParamsVersion: params,
		AlgorithmVersion: AlgorithmVersion, CacheKey: cacheKey,
	}
	if err := verifyChunkConservation(&res, level, options); err != nil {
		return ChunkResult{}, err
	}
	return res, nil
}

func quantResidualFor(q []Quantization, pathIndex int) int64 {
	var n int64
	for _, item := range q {
		if item.Value == int64(pathIndex) {
			n += item.Residual
		}
	}
	return n
}

func allocateBandEnergy(total int64, weights []int64) ([]int64, []Quantization) {
	out := make([]int64, len(weights))
	if total == 0 || len(weights) == 0 {
		return out, nil
	}
	var sum int64
	for _, w := range weights {
		sum += w
	}
	if sum <= 0 {
		out[0] = total
		return out, nil
	}
	type remEntry struct {
		index int
		rem   int64
	}
	rems := make([]remEntry, 0, len(weights))
	var assigned int64
	for i, w := range weights {
		exact := total * w
		q := exact / sum
		out[i] = q
		assigned += q
		rems = append(rems, remEntry{index: i, rem: exact % sum})
	}
	sort.SliceStable(rems, func(i, j int) bool {
		if rems[i].rem != rems[j].rem {
			return rems[i].rem > rems[j].rem
		}
		return rems[i].index < rems[j].index
	})
	left := total - assigned
	for i := 0; i < len(rems) && left > 0; i++ {
		out[rems[i].index]++
		left--
	}
	quant := make([]Quantization, 0, len(weights))
	for i, w := range weights {
		exact := total * w
		q := exact / sum
		quant = append(quant, Quantization{
			ExactNumerator: exact, Denominator: sum, Value: int64(i), Residual: exact - q*sum,
		})
	}
	return out, quant
}

func buildDependencies(level *Level, contentHash, params string, src *Source, incoming []Transfer) []Dependency {
	deps := []Dependency{
		{Kind: "chunk_content", Ref: "self", Version: contentHash},
		{Kind: "parameters", Ref: "options", Version: params},
		{Kind: "algorithm", Ref: "kernel", Version: AlgorithmVersion},
		{Kind: "source_emission", Ref: src.ID, Version: canonicalHash(src.Emission), CacheKey: canonicalHash(src.Emission)},
	}
	for _, t := range incoming {
		upstream := canonicalHash(struct {
			FromContent string `json:"from_content"`
			Transfer    string `json:"transfer"`
			Wave        int    `json:"wave"`
		}{ChunkContentHash(mustChunk(level, t.FromChunk)), t.Version, t.Wave})
		deps = append(deps, Dependency{
			Kind: "boundary_transfer", Ref: t.ID, Version: t.Version,
			CacheKey: upstream, Wave: t.Wave,
		})
	}
	sort.Slice(deps, func(i, j int) bool {
		if deps[i].Kind != deps[j].Kind {
			return deps[i].Kind < deps[j].Kind
		}
		if deps[i].Ref != deps[j].Ref {
			return deps[i].Ref < deps[j].Ref
		}
		return deps[i].Wave < deps[j].Wave
	})
	return deps
}

func mustChunk(level *Level, id int) *Chunk {
	if c, ok := chunkByID(level, id); ok {
		return c
	}
	return &Chunk{ID: id}
}

type solverPathCandidate struct {
	path Path
}

func enumerateSolverPaths(c *Chunk, source, listener Point, options Options, workerBuffer int) ([]Path, []Quantization, error) {
	limit := options.MaxPathsPerChunk
	if workerBuffer > 0 && workerBuffer < limit {
		limit = workerBuffer
	}
	refls := append([]Reflector(nil), c.Reflectors...)
	sortReflectors(refls)
	var candidates []solverPathCandidate
	var quant []Quantization
	overflow := false
	add := func(seq []int) bool {
		p, ok := solverUnfold(source, listener, refls, seq)
		if !ok || !pathInside(c.Bounds, p.Points) {
			return true
		}
		for _, idx := range seq {
			p.Reflectors = append(p.Reflectors, refls[idx].ID)
		}
		candidates = append(candidates, solverPathCandidate{path: p})
		if len(candidates) > limit || len(candidates) > options.MaxPathsPerChunk {
			overflow = true
			return false
		}
		return true
	}
	if !add(nil) {
		return nil, nil, &FieldError{Code: CodeValue, ObjectID: fmt.Sprintf("chunk-%d", c.ID), FieldPath: "paths", Measured: len(candidates), Min: 1, Max: limit, Message: "path buffer budget exhausted"}
	}
	var walk func(depth int, seq []int) bool
	walk = func(depth int, seq []int) bool {
		if depth >= options.MaxReflections {
			return true
		}
		for i := range refls {
			seq = append(seq, i)
			if !add(append([]int(nil), seq...)) {
				return false
			}
			if !walk(depth+1, seq) {
				return false
			}
			seq = seq[:len(seq)-1]
		}
		return true
	}
	if !walk(0, nil) {
		return nil, nil, &FieldError{Code: CodeValue, ObjectID: fmt.Sprintf("chunk-%d", c.ID), FieldPath: "paths", Measured: len(candidates), Min: 1, Max: limit, Message: "path buffer budget exhausted"}
	}
	if overflow {
		return nil, nil, &FieldError{Code: CodeValue, ObjectID: fmt.Sprintf("chunk-%d", c.ID), FieldPath: "paths", Measured: len(candidates), Min: 1, Max: limit, Message: "path buffer budget exhausted"}
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		a, b := candidates[i].path, candidates[j].path
		if len(a.Reflectors) != len(b.Reflectors) {
			return len(a.Reflectors) < len(b.Reflectors)
		}
		as, bs := fmt.Sprint(a.Reflectors), fmt.Sprint(b.Reflectors)
		if as != bs {
			return as < bs
		}
		return a.LengthMicros < b.LengthMicros
	})
	paths := make([]Path, len(candidates))
	for i := range candidates {
		paths[i] = candidates[i].path
		paths[i].ID = pathID(c.ID, i)
		paths[i].ChunkID = c.ID
		paths[i].Energy = []BandEnergy{}
	}
	return paths, quant, nil
}

func solverUnfold(source, listener Point, refls []Reflector, seq []int) (Path, bool) {
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
			return Path{}, false
		}
		points[i+1] = p
		current = p
	}
	length := int64(0)
	for i := 0; i+1 < len(points); i++ {
		length += Distance(points[i], points[i+1])
	}
	if length <= 0 {
		return Path{}, false
	}
	return Path{Points: points, LengthMicros: length}, true
}

func verifyChunkConservation(res *ChunkResult, level *Level, options Options) error {
	for _, src := range level.Sources {
		for _, band := range level.Bands {
			emission := bandValue(src.Emission, band.ID)
			var captured int64
			for _, se := range res.SourceEnergy {
				if se.SourceID == src.ID {
					captured = bandValue(se.Energy, band.ID)
				}
			}
			var esc int64
			for _, e := range res.Escapes {
				if e.SourceID == src.ID && e.BandID == band.ID {
					esc += e.Energy
				}
			}
			if captured < 0 || esc < 0 {
				return &FieldError{Code: CodeCons, ObjectID: fmt.Sprintf("chunk-%d", res.ChunkID), BandID: band.ID, SourceID: src.ID, Message: "negative energy component"}
			}
			// A single chunk's local residual is checked globally after all
			// chunks are available. Per-chunk this guard only rejects overflow.
			if captured > emission+options.ConservationResidue {
				return &FieldError{
					Code: CodeCons, ObjectID: fmt.Sprintf("chunk-%d", res.ChunkID), BandID: band.ID, SourceID: src.ID,
					Values:  map[string]int64{"emission": emission, "chunk_energy": captured, "escape": esc},
					Message: "chunk captured more energy than its source emitted",
				}
			}
		}
	}
	return nil
}

// CheckConservation verifies the global per-source/per-band energy identity:
// emission = sum(chunk arrivals) + boundary net flow + final escape.
func CheckConservation(level *Level, options Options, results []ChunkResult, plan *EnergyPlan) error {
	options = options.Normalized()
	chunkEnergy := make(map[string]map[int]map[string]int64)
	for _, res := range results {
		for _, se := range res.SourceEnergy {
			if chunkEnergy[se.SourceID] == nil {
				chunkEnergy[se.SourceID] = map[int]map[string]int64{}
			}
			if chunkEnergy[se.SourceID][res.ChunkID] == nil {
				chunkEnergy[se.SourceID][res.ChunkID] = map[string]int64{}
			}
			for _, e := range se.Energy {
				chunkEnergy[se.SourceID][res.ChunkID][e.BandID] += e.Energy
			}
		}
	}
	outgoing := map[string]map[string]int64{}
	incoming := map[string]map[string]int64{}
	for _, t := range plan.Transfers {
		if outgoing[t.SourceID] == nil {
			outgoing[t.SourceID] = map[string]int64{}
			incoming[t.SourceID] = map[string]int64{}
		}
		for _, e := range t.Energy {
			outgoing[t.SourceID][e.BandID] += e.Energy
			incoming[t.SourceID][e.BandID] += e.Energy
		}
	}
	escapes := map[string]map[string]int64{}
	for _, items := range plan.Escapes {
		for _, e := range items {
			if escapes[e.SourceID] == nil {
				escapes[e.SourceID] = map[string]int64{}
			}
			escapes[e.SourceID][e.BandID] += e.Energy
		}
	}
	for _, src := range level.Sources {
		for _, band := range level.Bands {
			var captured int64
			for _, bands := range chunkEnergy[src.ID] {
				captured += bands[band.ID]
			}
			net := outgoing[src.ID][band.ID] - incoming[src.ID][band.ID]
			esc := escapes[src.ID][band.ID]
			emission := bandValue(src.Emission, band.ID)
			residual := emission - (captured + net + esc)
			if residual < 0 {
				residual = -residual
			}
			if residual > options.ConservationResidue {
				return &FieldError{
					Code: CodeCons, ObjectID: src.ID, BandID: band.ID, SourceID: src.ID,
					Values: map[string]int64{
						"emission": emission, "chunk_energy": captured,
						"boundary_net_flow": net, "escape": esc, "residual": residual,
					},
					Message: fmt.Sprintf("energy conservation residual exceeds %d", options.ConservationResidue),
				}
			}
		}
	}
	return nil
}

// SolveLevel computes all chunk results using the plan. It is used by tests
// and by the sequential full-rebuild path; the engine may schedule chunk
// calculations in parallel around the same pure function.
func SolveLevel(ctx context.Context, level *Level, options Options) ([]ChunkResult, *EnergyPlan, error) {
	if err := ValidateLevel(level); err != nil {
		return nil, nil, err
	}
	plan, err := buildEnergyPlan(level, options)
	if err != nil {
		return nil, nil, err
	}
	ids := chunkIDsSorted(level)
	out := make([]ChunkResult, 0, len(ids))
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			return nil, plan, err
		}
		res, err := SolveChunk(level, options, plan, id, options.PathBufferPerWorker)
		if err != nil {
			return nil, plan, err
		}
		res.State = ChunkCommitted
		out = append(out, res)
	}
	return out, plan, nil
}

func chunkIDsSorted(level *Level) []int {
	ids := make([]int, 0, len(level.Chunks))
	for _, c := range level.Chunks {
		ids = append(ids, c.ID)
	}
	sort.Ints(ids)
	return ids
}
