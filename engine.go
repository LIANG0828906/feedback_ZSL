package colortrace

import (
	"fmt"
	"sort"
)

// StagePlaneDimensions returns a plane's output dimensions after stage idx.
func StagePlaneDimensions(project *Project, idx int, planeName string) (int, int, error) {
	plane := project.Plane(planeName)
	if plane == nil {
		return 0, 0, schemaError("$.planes", "unknown plane %q", planeName)
	}
	w, h := plane.Width, plane.Height
	for i := 0; i <= idx; i++ {
		if i >= len(project.Stages) {
			return 0, 0, schemaError("$.stages", "stage %d missing", i)
		}
		stage := project.Stages[i]
		if w%stage.FactorX != 0 || h%stage.FactorY != 0 {
			return 0, 0, schemaError(fmt.Sprintf("$.stages[%d]", i),
				"plane %q dims %dx%d incompatible with factor %dx%d",
				planeName, w, h, stage.FactorX, stage.FactorY)
		}
		w /= stage.FactorX
		h /= stage.FactorY
	}
	return w, h, nil
}

// ProcessChunk deterministically evaluates one stage/chunk. It reads only
// immutable previous output and therefore is safe for concurrent use.
func ProcessChunk(project *Project, stageIdx int, input *Sample, prevChunks map[string]*ChunkResult, chunk ChunkCoord) (*ChunkResult, error) {
	if project == nil {
		return nil, schemaError("$", "nil project")
	}
	if stageIdx < 0 || stageIdx >= len(project.Stages) {
		return nil, paraError(stageIdx, 0, len(project.Stages)-1, "stage index out of range")
	}
	cfg := project.Stages[stageIdx]
	prevBits := project.BaseBits
	if stageIdx > 0 {
		prevBits = project.Stages[stageIdx-1].Bits
	}
	maxBits := MaxQuantBits
	if prevBits < maxBits {
		maxBits = prevBits
	}
	if cfg.Bits < MinQuantBits || cfg.Bits > maxBits {
		return nil, paraError(stageIdx, MinQuantBits, maxBits,
			"quantization bits %d outside execution range", cfg.Bits)
	}
	chunkID := ChunkID(chunk)
	out := &ChunkResult{
		ProjectID:  project.ProjectID,
		Stage:      stageIdx,
		Chunk:      chunk,
		ChunkID:    chunkID,
		Planes:     map[string]*PlaneChunk{},
		PlaneNames: append([]string{}, project.PlaneNames()...),
	}

	firstPlane := project.Planes[0]
	outW, outH, err := StagePlaneDimensions(project, stageIdx, firstPlane.Name)
	if err != nil {
		return nil, err
	}
	out.Region = ChunkRegion(chunk, outW, outH, project.ChunkSize)
	if out.Region.Samples() == 0 {
		return nil, schemaError("chunk", "chunk %s is outside stage %d output", chunkID, stageIdx)
	}

	expanded := expandedInputRegion(cfg, out.Region, firstPlane.Width, firstPlane.Height)
	upstreamIDs := collectUpstreamChunks(expanded, project.ChunkSize)
	upstreamKeys := make([]string, 0, len(upstreamIDs))
	for _, id := range upstreamIDs {
		if stageIdx == 0 {
			upstreamKeys = append(upstreamKeys, "source:"+SHA256Hex(projectSourceBytes(project)))
			continue
		}
		if prevChunks != nil && prevChunks[id] != nil && prevChunks[id].CacheKey != "" {
			upstreamKeys = append(upstreamKeys, prevChunks[id].CacheKey)
		} else {
			upstreamKeys = append(upstreamKeys, "uncommitted:"+id)
		}
	}
	sort.Strings(upstreamKeys)
	out.UpstreamKeys = uniqueStrings(upstreamKeys)

	planeHashes := map[string]string{}
	for _, planeDesc := range project.Planes {
		inW, inH := planeDesc.Width, planeDesc.Height
		if stageIdx > 0 {
			var err error
			inW, inH, err = StagePlaneDimensions(project, stageIdx-1, planeDesc.Name)
			if err != nil {
				return nil, err
			}
		}
		amplitudes, err := baseAmplitudes(project, stageIdx, planeDesc.Name, input)
		if err != nil {
			return nil, err
		}
		planeRegion := expandedInputRegion(cfg, out.Region, inW, inH)
		planeHashes[planeDesc.Name] = InputRegionHash(stageIdx, planeDesc.Name, planeRegion, amplitudes, inW)
	}
	out.InputHash = HashCanonical("chunk-input-v1", stageIdx, out.Region, planeHashes)
	out.CacheKey = ComputeCacheKey(out.InputHash, cfg, out.UpstreamKeys)

	for _, planeDesc := range project.Planes {
		pc, err := processPlaneChunk(project, cfg, planeDesc, out.Region, chunk, chunkID, out.CacheKey, input)
		if err != nil {
			return nil, err
		}
		out.Planes[planeDesc.Name] = pc
	}
	for _, pc := range out.Planes {
		for i := range pc.Ledgers {
			pc.Ledgers[i].ContentHash = HashCanonical("point-output-v1", pc.Ledgers[i].Code, pc.Ledgers[i].OutputFixed, pc.Ledgers[i].Breakdown)
		}
	}
	out.ContentHash = hashChunkOutput(out)
	return out, nil
}

func processPlaneChunk(project *Project, cfg StageConfig, planeDesc *Plane, region Region, chunk ChunkCoord, chunkID, cacheKey string, input *Sample) (*PlaneChunk, error) {
	stageIdx := cfg.Index
	inW, inH := planeDesc.Width, planeDesc.Height
	if stageIdx > 0 {
		var err error
		inW, inH, err = StagePlaneDimensions(project, stageIdx-1, planeDesc.Name)
		if err != nil {
			return nil, err
		}
	}
	amplitudes, err := baseAmplitudes(project, stageIdx, planeDesc.Name, input)
	if err != nil {
		return nil, err
	}
	outW := region.Width()
	outH := region.Height()
	pc := &PlaneChunk{
		Plane: planeDesc.Name, Width: outW, Height: outH, Region: region,
		Values:  make([]int64, 0, region.Samples()),
		Codes:   make([]int64, 0, region.Samples()),
		Ledgers: make([]PointLedger, 0, region.Samples()),
	}
	quantLSBBase := int64(1) << uint(project.BaseBits-cfg.Bits)
	grid := quantLSBBase * cfg.AccumScale

	for y := region.MinY; y < region.MaxY; y++ {
		for x := region.MinX; x < region.MaxX; x++ {
			anchor := Coord{X: x*cfg.FactorX + cfg.PhaseX, Y: y*cfg.FactorY + cfg.PhaseY}
			accC := int64(0)
			accR := int64(0)
			pointTruncs := []TruncationRecord{}
			pointInputVals := make([]int64, 0, len(cfg.Kernel.Taps))
			pointInputCoords := make([]Coord, 0, len(cfg.Kernel.Taps))
			for _, tap := range cfg.Kernel.Taps {
				ix := clampInt(anchor.X+tap.DX, 0, inW-1)
				iy := clampInt(anchor.Y+tap.DY, 0, inH-1)
				idx := iy*inW + ix
				rawFixed := amplitudes[idx] * cfg.AccumScale
				chromaFixed := rawFixed
				if planeDesc.Chroma && cfg.ChromaKernel != nil {
					cValue, cTrunc := applyChromaKernel(cfg, *cfg.ChromaKernel, amplitudes, inW, inH, ix, iy, rawFixed)
					chromaFixed = cValue
					if cTrunc != nil {
						cTrunc.Stage = stageIdx
						cTrunc.Plane = planeDesc.Name
						cTrunc.Coord = Coord{X: x, Y: y}
						cTrunc.ChunkID = chunkID
						pointTruncs = append(pointTruncs, *cTrunc)
						pc.Truncations = append(pc.Truncations, *cTrunc)
					}
				}
				accC += tap.W * chromaFixed
				accR += tap.W * rawFixed
				pointInputVals = append(pointInputVals, rawFixed)
				pointInputCoords = append(pointInputCoords, Coord{X: ix, Y: iy})
			}
			weightSum := cfg.Kernel.WeightSum()
			normScale := cfg.Kernel.NormScale
			rawReference := Rat(accR, weightSum)
			chromaPart := SubFrac(Rat(accC, weightSum), rawReference)
			normPart := SubFrac(Rat(accC, normScale), Rat(accC, weightSum))
			quot := accC / normScale
			rem := accC % normScale
			accumPart := Rat(-rem, normScale)
			preQuant := quot
			code := int64(0)
			if preQuant >= 0 {
				code = (preQuant + grid/2) / grid
			} else {
				code = -((-preQuant + grid/2) / grid)
			}
			maxCode := int64(1)<<uint(cfg.Bits) - 1
			if code < 0 {
				code = 0
			}
			if code > maxCode {
				code = maxCode
			}
			outputFixed := code * grid
			bitPart := I64(outputFixed - preQuant)
			breakdown := OperatorBreakdown{
				ResampleNorm: normPart,
				AccumTrunc:   accumPart,
				BitRound:     bitPart,
				Chroma:       chromaPart,
			}
			total := breakdown.Sum()
			propagated := I64(0)
			if stageIdx > 0 && input != nil {
				prevScale := Rat(cfg.AccumScale, project.Stages[stageIdx-1].AccumScale)
				prevPlane := input.Planes[planeDesc.Name]
				for tapIndex, tap := range cfg.Kernel.Taps {
					coord := pointInputCoords[tapIndex]
					prevLedger := prevPlane.LedgerAt(coord.X, coord.Y)
					if prevLedger == nil {
						return nil, schemaError("input.ledger", "missing prior ledger at %s (%d,%d)", planeDesc.Name, coord.X, coord.Y)
					}
					scaled := MulFrac(prevLedger.CumulativeDeviation, prevScale)
					propagated = AddFrac(propagated, MulFrac(scaled, Rat(tap.W, weightSum)))
				}
			}
			cumulative := AddFrac(propagated, total)
			mainTrunc := TruncationRecord{
				Stage: stageIdx, Plane: planeDesc.Name, Coord: Coord{X: x, Y: y}, ChunkID: chunkID,
				Kind: "accumulation", Position: "weighted_sum / kernel.norm_scale",
				RawValue: accC, Divisor: normScale, Quotient: quot, Remainder: rem,
				Discarded: Rat(rem, normScale),
			}
			bitTrunc := TruncationRecord{
				Stage: stageIdx, Plane: planeDesc.Name, Coord: Coord{X: x, Y: y}, ChunkID: chunkID,
				Kind: "bit_depth_round", Position: "round(fixed / quant_grid)",
				RawValue: preQuant, Divisor: grid, Quotient: code,
				GridLSB: grid, RoundingDelta: outputFixed - preQuant,
				Discarded: I64(outputFixed - preQuant),
			}
			pointTruncs = append(pointTruncs, mainTrunc, bitTrunc)
			pc.Truncations = append(pc.Truncations, mainTrunc, bitTrunc)
			pointHash := HashCanonical("point-input-v1", pointInputCoords, pointInputVals)
			ledger := PointLedger{
				Stage: stageIdx, Plane: planeDesc.Name, Coord: Coord{X: x, Y: y},
				Chunk: chunk, ChunkID: chunkID, InputHash: pointHash, CacheKey: cacheKey,
				Anchor: anchor, AccumScale: cfg.AccumScale, FactorX: cfg.FactorX, FactorY: cfg.FactorY,
				Bits: cfg.Bits, KernelRadiusX: cfg.Kernel.RadiusX, KernelRadiusY: cfg.Kernel.RadiusY,
				WeightSum: weightSum, NormScale: normScale, RawWeightedSum: accC, Remainder: rem,
				PreQuantFixed: preQuant, QuantGrid: grid, Code: code, OutputFixed: outputFixed,
				RawReferenceFixed: rawReference, TotalDeviation: total,
				CumulativeDeviation: cumulative, Breakdown: breakdown,
				Truncations: pointTruncs,
			}
			pc.Values = append(pc.Values, outputFixed)
			pc.Codes = append(pc.Codes, code)
			pc.Ledgers = append(pc.Ledgers, ledger)
		}
	}
	return pc, nil
}

func applyChromaKernel(stage StageConfig, kernel Kernel, amplitudes []int64, width, height int, x, y int, nearestRawFixed int64) (int64, *TruncationRecord) {
	acc := int64(0)
	for _, tap := range kernel.Taps {
		ix := clampInt(x+tap.DX, 0, width-1)
		iy := clampInt(y+tap.DY, 0, height-1)
		acc += tap.W * amplitudes[iy*width+ix] * stage.AccumScale
	}
	q := acc / kernel.NormScale
	rem := acc % kernel.NormScale
	delta := q - nearestRawFixed
	record := &TruncationRecord{
		Kind: "chroma_resample", Position: "chroma_weighted_sum / chroma.norm_scale",
		RawValue: acc, Divisor: kernel.NormScale, Quotient: q, Remainder: rem,
		Discarded: Rat(rem, kernel.NormScale), RoundingDelta: delta,
	}
	if rem == 0 {
		record = nil
	}
	return q, record
}

func baseAmplitudes(project *Project, stageIdx int, planeName string, input *Sample) ([]int64, error) {
	plane := project.Plane(planeName)
	if plane == nil {
		return nil, schemaError("plane", "unknown plane %q", planeName)
	}
	if stageIdx == 0 {
		out := append([]int64{}, plane.Values...)
		return out, nil
	}
	if input == nil {
		return nil, schemaError("input", "stage %d requires previous sample", stageIdx)
	}
	prev := input.Planes[planeName]
	if prev == nil {
		return nil, schemaError("input", "plane %q missing in previous sample", planeName)
	}
	prevBits := project.Stages[stageIdx-1].Bits
	shift := uint(project.BaseBits - prevBits)
	out := make([]int64, len(prev.Codes))
	for i, code := range prev.Codes {
		out[i] = code << shift
	}
	return out, nil
}

func expandedInputRegion(cfg StageConfig, out Region, inW, inH int) Region {
	r := Region{
		MinX: out.MinX*cfg.FactorX + cfg.PhaseX - cfg.Kernel.RadiusX,
		MinY: out.MinY*cfg.FactorY + cfg.PhaseY - cfg.Kernel.RadiusY,
		MaxX: (out.MaxX-1)*cfg.FactorX + cfg.PhaseX + cfg.Kernel.RadiusX + 1,
		MaxY: (out.MaxY-1)*cfg.FactorY + cfg.PhaseY + cfg.Kernel.RadiusY + 1,
	}
	r.MinX = clampInt(r.MinX, 0, inW)
	r.MinY = clampInt(r.MinY, 0, inH)
	r.MaxX = clampInt(r.MaxX, 0, inW)
	r.MaxY = clampInt(r.MaxY, 0, inH)
	return r
}

func collectUpstreamChunks(r Region, chunkSize int) []string {
	set := map[string]struct{}{}
	for y := r.MinY; y < r.MaxY; y++ {
		for x := r.MinX; x < r.MaxX; x++ {
			set[ChunkID(ChunkCoordAt(x, y, chunkSize))] = struct{}{}
		}
	}
	out := make([]string, 0, len(set))
	for id := range set {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

func uniqueStrings(values []string) []string {
	set := map[string]struct{}{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		if _, ok := set[value]; ok {
			continue
		}
		set[value] = struct{}{}
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func projectSourceBytes(project *Project) []byte {
	data, err := CanonicalJSON(project.Planes)
	if err != nil {
		panic(err)
	}
	return data
}

func hashChunkOutput(result *ChunkResult) string {
	plain := *result
	plain.ContentHash = ""
	plain.StartedAt = result.StartedAt
	plain.FinishedAt = result.FinishedAt
	return HashCanonical("chunk-result-v1", plain)
}

// AssembleChunks copies committed chunk products into a full stage sample.
func AssembleChunks(project *Project, stageIdx int, chunks map[string]*ChunkResult) (*Sample, error) {
	sample := &Sample{Stage: stageIdx, Planes: map[string]*PlaneSample{}}
	for _, planeDesc := range project.Planes {
		w, h, err := StagePlaneDimensions(project, stageIdx, planeDesc.Name)
		if err != nil {
			return nil, err
		}
		ps := &PlaneSample{
			Name: planeDesc.Name, Width: w, Height: h,
			Values: make([]int64, w*h), Codes: make([]int64, w*h),
			Ledgers: make([]PointLedger, w*h),
		}
		for _, chunk := range chunks {
			if chunk.Stage != stageIdx {
				continue
			}
			pc := chunk.Planes[planeDesc.Name]
			if pc == nil {
				continue
			}
			for ly := 0; ly < pc.Height; ly++ {
				for lx := 0; lx < pc.Width; lx++ {
					x := pc.Region.MinX + lx
					y := pc.Region.MinY + ly
					src := ly*pc.Width + lx
					dst := y*w + x
					ps.Values[dst] = pc.Values[src]
					ps.Codes[dst] = pc.Codes[src]
					ps.Ledgers[dst] = pc.Ledgers[src]
				}
			}
		}
		sample.Planes[planeDesc.Name] = ps
	}
	return sample, nil
}
