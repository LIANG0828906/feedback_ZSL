// Package engine contains the deterministic in-chunk reflection kernel.
package engine

import (
	"fmt"
	"sort"
	"strings"

	"acousticfield/internal/acoustic"
)

// ScreenChunk performs all pure local path screening and energy accumulation for one chunk.
// It does not mutate inputs and is safe to call concurrently for different chunks.
func ScreenChunk(chunk acoustic.Chunk, allSources []acoustic.Source, allListeners []acoustic.Listener, params acoustic.Params, pathBufferCap int) (*acoustic.ChunkResult, error) {
	if pathBufferCap <= 0 {
		pathBufferCap = params.Budget.PathBufferPerWorker
	}
	sources := make([]acoustic.Source, 0)
	for _, s := range allSources {
		if s.ChunkID == chunk.ID {
			sources = append(sources, s)
		}
	}
	listeners := make([]acoustic.Listener, 0)
	for _, l := range allListeners {
		if l.ChunkID == chunk.ID {
			listeners = append(listeners, l)
		}
	}
	sort.Slice(sources, func(i, j int) bool { return sources[i].ID < sources[j].ID })
	sort.Slice(listeners, func(i, j int) bool { return listeners[i].ID < listeners[j].ID })

	surfaces := append([]acoustic.Surface(nil), chunk.Surfaces...)
	sort.Slice(surfaces, func(i, j int) bool { return surfaces[i].ID < surfaces[j].ID })

	bandCount := len(chunk.Bands)
	result := &acoustic.ChunkResult{
		ChunkID:          chunk.ID,
		Row:              chunk.Row,
		Col:              chunk.Col,
		ParamVersion:     params.ParamVersion,
		AlgorithmVersion: params.AlgorithmVersion,
		Paths:            []acoustic.PathResult{},
		ListenerEnergies: []acoustic.ListenerEnergy{},
		ListenerArrivalQ: zeros(bandCount),
		ArrivalEnergyQ:   zeros(bandCount),
		AbsorbedQ:        zeros(bandCount),
		InboundQ:         zeros(bandCount),
		RetainedQ:        zeros(bandCount),
		OutboundSentQ:    zeros(bandCount),
		Transfers:        []acoustic.Transfer{},
		Dependencies:     []string{},
	}

	result.ContentHash = ContentHash(chunk, allSources, allListeners)
	result.LocalCacheKey = acoustic.HashValue("local-cache:", struct {
		ContentHash      string
		ParamVersion     string
		AlgorithmVersion string
	}{result.ContentHash, params.ParamVersion, params.AlgorithmVersion})

	listenerEnergyByID := map[string][]int64{}
	for _, l := range listeners {
		listenerEnergyByID[l.ID] = zeros(bandCount)
	}

	for _, source := range sources {
		pathsForSource := []*acoustic.PathResult{}
		for _, listener := range listeners {
			paths, err := enumeratePair(chunk.ID, len(chunk.Bands), source.Position, listener, surfaces, params.MaxBounces, pathBufferCap)
			if err != nil {
				return nil, err
			}
			for i := range paths {
				p := paths[i]
				pathsForSource = append(pathsForSource, &p)
			}
		}
		sort.Slice(pathsForSource, func(i, j int) bool {
			if pathsForSource[i].ListenerID != pathsForSource[j].ListenerID {
				return pathsForSource[i].ListenerID < pathsForSource[j].ListenerID
			}
			return pathsForSource[i].Key < pathsForSource[j].Key
		})
		if len(pathsForSource) > pathBufferCap {
			return nil, acoustic.NewError(acoustic.CodeBudget, chunk.ID, "path_buffer", "path buffer limit %d exceeded", pathBufferCap)
		}

		for band := 0; band < bandCount; band++ {
			emission := source.EmissionQ[band]
			pathCount := int64(len(pathsForSource))
			denominator := pathCount + int64(params.BoundaryWeight)
			share := emission / denominator
			boundaryBudget := emission - share*pathCount
			result.Quantization = append(result.Quantization, acoustic.QuantRecord{
				BandID:       band,
				Stage:        "energy_share_div",
				Location:     "source:" + source.ID,
				InputQ:       emission,
				NumeratorQ:   emission,
				DenominatorQ: denominator,
				TruncatedQ:   share,
				ResidualQ:    emission % denominator,
			})
			result.OutboundSentQ[band] += boundaryBudget

			for _, path := range pathsForSource {
				pre := share
				for bounce, surfaceID := range path.Sequence {
					alpha := absorptionForSurface(chunk, surfaceID, band)
					numerator := pre * (acoustic.EnergyScale - alpha)
					reflected := numerator / acoustic.EnergyScale
					residualNumerator := numerator % acoustic.EnergyScale
					absorbed := pre - reflected
					path.AbsorbedQ[band] += absorbed
					path.BandEnergyQ[band] = reflected
					path.Quantization = append(path.Quantization, acoustic.QuantRecord{
						PathID:       path.Key,
						BandID:       band,
						Stage:        "absorption_div",
						Location:     fmt.Sprintf("bounce:%d:%s", bounce, surfaceID),
						InputQ:       pre,
						NumeratorQ:   numerator,
						DenominatorQ: acoustic.EnergyScale,
						TruncatedQ:   reflected,
						ResidualQ:    residualNumerator,
					})
					result.AbsorbedQ[band] += absorbed
					pre = reflected
				}
				if path.Bounces == 0 {
					path.BandEnergyQ[band] = pre
				}
				result.ListenerArrivalQ[band] += path.BandEnergyQ[band]
				listenerEnergyByID[path.ListenerID][band] += path.BandEnergyQ[band]
			}
		}

		for _, p := range pathsForSource {
			result.Paths = append(result.Paths, *p)
		}
	}

	for _, listener := range listeners {
		result.ListenerEnergies = append(result.ListenerEnergies, acoustic.ListenerEnergy{
			ListenerID:   listener.ID,
			BandArrivalQ: listenerEnergyByID[listener.ID],
		})
	}
	sort.Slice(result.ListenerEnergies, func(i, j int) bool {
		return result.ListenerEnergies[i].ListenerID < result.ListenerEnergies[j].ListenerID
	})
	result.PathBufferUsed = len(result.Paths)
	result.PathDigest = acoustic.HashValue("path-digest:", pathDigestInput(result.Paths))
	result.Dependencies = buildDependencies(chunk, sources, listeners)
	result.DependencyRecords = buildDependencyRecords(chunk, sources, listeners)
	return result, nil
}

func zeros(n int) []int64 { return make([]int64, n) }

func absorptionForSurface(chunk acoustic.Chunk, _ string, band int) int64 {
	if band < 0 || band >= len(chunk.Bands) {
		return 0
	}
	return chunk.Bands[band].AlphaQ
}

func pathKey(sequence []acoustic.Surface) string {
	if len(sequence) == 0 {
		return "P00-direct"
	}
	ids := make([]string, 0, len(sequence))
	for _, s := range sequence {
		ids = append(ids, s.ID)
	}
	return fmt.Sprintf("P%02d-%s", len(sequence), strings.Join(ids, "__"))
}

func enumeratePair(chunkID string, bandCount int, source acoustic.Point, listener acoustic.Listener, surfaces []acoustic.Surface, maxBounces, bufferCap int) ([]acoustic.PathResult, error) {
	paths := make([]acoustic.PathResult, 0, 2)

	direct, ok := tracePath(source, listener.Position, nil, surfaces)
	if ok {
		direct.ListenerID = listener.ID
		direct.Key = pathKey(nil)
		direct.Sequence = []string{}
		direct.BandEnergyQ = zeros(bandCount)
		direct.AbsorbedQ = zeros(bandCount)
		paths = append(paths, direct)
	}

	sequences := enumerateSequences(len(surfaces), maxBounces)
	for _, indexes := range sequences {
		if len(paths) >= bufferCap {
			return nil, acoustic.NewError(acoustic.CodeBudget, chunkID, "path_buffer", "path buffer limit %d exceeded while screening", bufferCap)
		}
		seqSurfaces := make([]acoustic.Surface, 0, len(indexes))
		seqIDs := make([]string, 0, len(indexes))
		for _, idx := range indexes {
			seqSurfaces = append(seqSurfaces, surfaces[idx])
			seqIDs = append(seqIDs, surfaces[idx].ID)
		}
		traced, ok := tracePath(source, listener.Position, seqSurfaces, surfaces)
		if !ok {
			continue
		}
		traced.ListenerID = listener.ID
		traced.Key = pathKey(seqSurfaces)
		traced.Sequence = seqIDs
		traced.Bounces = len(seqSurfaces)
		traced.BandEnergyQ = zeros(bandCount)
		traced.AbsorbedQ = zeros(bandCount)
		paths = append(paths, traced)
	}
	return paths, nil
}

func enumerateSequences(surfaceCount, maxBounces int) [][]int {
	var out [][]int
	var rec func(prefix []int, depth int)
	rec = func(prefix []int, depth int) {
		if depth >= maxBounces {
			return
		}
		for i := 0; i < surfaceCount; i++ {
			if len(prefix) > 0 && prefix[len(prefix)-1] == i {
				continue
			}
			next := append(append([]int(nil), prefix...), i)
			out = append(out, next)
			rec(next, depth+1)
		}
	}
	rec(nil, 0)
	return out
}

func tracePath(srcQ acoustic.Point, dstQ acoustic.Point, sequence []acoustic.Surface, allSurfaces []acoustic.Surface) (acoustic.PathResult, bool) {
	src := toF(srcQ)
	dst := toF(dstQ)

	virtual := dst
	for i := len(sequence) - 1; i >= 0; i-- {
		virtual = reflectPoint(virtual, sequence[i])
	}

	type located struct {
		f         fpoint
		q         acoustic.Point
		surfaceID string
	}
	bounces := make([]located, 0, len(sequence))
	prev := src
	dir := sub(virtual, src)
	if mathHypot(dir.x, dir.y) < 1e-9 {
		return acoustic.PathResult{}, false
	}

	for i, surface := range sequence {
		hit, t, _, ok := raySegment(prev, dir, surface)
		if !ok {
			return acoustic.PathResult{}, false
		}
		if i == 0 && (t <= 1e-8 || t > 1+1e-7) {
			return acoustic.PathResult{}, false
		}
		q := quantize(hit)
		bounces = append(bounces, located{f: hit, q: q, surfaceID: surface.ID})
		dir = reflectVector(dir, surface)
		prev = hit
	}

	toDst := sub(dst, prev)
	dirNorm := dir.x*dir.x + dir.y*dir.y
	if dirNorm < 1e-12 {
		return acoustic.PathResult{}, false
	}
	finalT := (toDst.x*dir.x + toDst.y*dir.y) / dirNorm
	if finalT <= 1e-7 {
		return acoustic.PathResult{}, false
	}
	closest := add(prev, scale(dir, finalT))
	if mathHypot(closest.x-dst.x, closest.y-dst.y) > 1.25 {
		return acoustic.PathResult{}, false
	}

	floatPoints := []fpoint{src}
	quantPoints := []acoustic.Point{srcQ}
	for _, b := range bounces {
		floatPoints = append(floatPoints, b.f)
		quantPoints = append(quantPoints, b.q)
	}
	floatPoints = append(floatPoints, dst)
	quantPoints = append(quantPoints, dstQ)

	for leg := 0; leg+1 < len(floatPoints); leg++ {
		allowedStart := map[string]bool{}
		allowedEnd := map[string]bool{}
		if leg > 0 {
			allowedStart[sequence[leg-1].ID] = true
		}
		if leg < len(sequence) {
			allowedEnd[sequence[leg].ID] = true
		}
		if segmentBlocked(floatPoints[leg], floatPoints[leg+1], allSurfaces, allowedStart, allowedEnd) {
			return acoustic.PathResult{}, false
		}
	}

	length, segments, quant := pointsToLength(pathKey(sequence), pathKey(sequence), quantPoints)
	return acoustic.PathResult{
		LengthQ:         length,
		SegmentLengthsQ: segments,
		Quantization:    quant,
	}, true
}

func mathHypot(x, y float64) float64 {
	return sqrtFloat(x*x + y*y)
}

func sqrtFloat(v float64) float64 {
	if v <= 0 {
		return 0
	}
	z := v
	for i := 0; i < 24; i++ {
		z = (z + v/z) / 2
	}
	return z
}

type digestPath struct {
	Key             string
	ListenerID      string
	Sequence        []string
	Bounces         int
	LengthQ         int64
	SegmentLengthsQ []int64
	BandEnergyQ     []int64
	AbsorbedQ       []int64
}

func pathDigestInput(paths []acoustic.PathResult) []digestPath {
	out := make([]digestPath, 0, len(paths))
	for _, p := range paths {
		out = append(out, digestPath{
			Key: p.Key, ListenerID: p.ListenerID, Sequence: p.Sequence, Bounces: p.Bounces,
			LengthQ: p.LengthQ, SegmentLengthsQ: p.SegmentLengthsQ,
			BandEnergyQ: p.BandEnergyQ, AbsorbedQ: p.AbsorbedQ,
		})
	}
	return out
}

func buildDependencies(chunk acoustic.Chunk, sources []acoustic.Source, listeners []acoustic.Listener) []string {
	deps := []string{
		"chunk:" + chunk.ID,
		"content_hash:" + chunk.ID,
	}
	for _, b := range chunk.Bands {
		deps = append(deps, fmt.Sprintf("absorption:%s:%d", chunk.ID, b.ID))
	}
	for _, s := range chunk.Surfaces {
		deps = append(deps, "surface:"+chunk.ID+":"+s.ID+":geometry")
	}
	for _, s := range sources {
		deps = append(deps, "source:"+s.ID+":emission", "source:"+s.ID+":position")
	}
	for _, l := range listeners {
		deps = append(deps, "listener:"+l.ID+":position")
	}
	sort.Strings(deps)
	return deps
}

func buildDependencyRecords(chunk acoustic.Chunk, sources []acoustic.Source, listeners []acoustic.Listener) []acoustic.Dependency {
	records := []acoustic.Dependency{{Key: "chunk:" + chunk.ID, Kind: "chunk", ChunkID: chunk.ID, Field: "content"}}
	for _, b := range chunk.Bands {
		records = append(records, acoustic.Dependency{
			Key: fmt.Sprintf("absorption:%s:%d", chunk.ID, b.ID), Kind: "absorption", ChunkID: chunk.ID, Field: fmt.Sprintf("bands[%d].alpha_q", b.ID),
		})
	}
	for _, s := range chunk.Surfaces {
		records = append(records, acoustic.Dependency{
			Key: "surface:" + chunk.ID + ":" + s.ID + ":geometry", Kind: "surface_geometry", ChunkID: chunk.ID, Field: "surfaces." + s.ID,
		})
	}
	for _, s := range sources {
		records = append(records,
			acoustic.Dependency{Key: "source:" + s.ID + ":emission", Kind: "source_emission", ChunkID: chunk.ID, Field: s.ID + ".emission_q"},
			acoustic.Dependency{Key: "source:" + s.ID + ":position", Kind: "source_position", ChunkID: chunk.ID, Field: s.ID + ".position"},
		)
	}
	for _, l := range listeners {
		records = append(records, acoustic.Dependency{Key: "listener:" + l.ID + ":position", Kind: "listener_position", ChunkID: chunk.ID, Field: l.ID + ".position"})
	}
	return records
}

// ContentHash returns the deterministic hash of a chunk and its local samples.
func ContentHash(chunk acoustic.Chunk, allSources []acoustic.Source, allListeners []acoustic.Listener) string {
	sources := make([]acoustic.Source, 0)
	for _, source := range allSources {
		if source.ChunkID == chunk.ID {
			sources = append(sources, source)
		}
	}
	listeners := make([]acoustic.Listener, 0)
	for _, listener := range allListeners {
		if listener.ChunkID == chunk.ID {
			listeners = append(listeners, listener)
		}
	}
	sort.Slice(sources, func(i, j int) bool { return sources[i].ID < sources[j].ID })
	sort.Slice(listeners, func(i, j int) bool { return listeners[i].ID < listeners[j].ID })
	envelope := struct {
		Chunk     acoustic.Chunk
		Sources   []acoustic.Source
		Listeners []acoustic.Listener
	}{chunk, sources, listeners}
	return acoustic.HashValue("chunk-content:", envelope)
}
