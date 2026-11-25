package colortrace

import "fmt"

// PipelineResult contains assembled samples, chunk products and band report.
type PipelineResult struct {
	Samples []*Sample      `json:"samples"`
	Stages  []*StageResult `json:"stages"`
	Band    *BandReport    `json:"band,omitempty"`
}

// ExecuteSequential is the deterministic one-shot reference orchestration over
// the production chunk executor. It is useful as the full-recompute path.
func ExecuteSequential(project *Project) (*PipelineResult, error) {
	result := &PipelineResult{}
	var previous *Sample
	var previousChunks map[string]*ChunkResult
	for stageIdx := range project.Stages {
		first := project.Planes[0]
		w, h, err := StagePlaneDimensions(project, stageIdx, first.Name)
		if err != nil {
			return nil, err
		}
		chunks := AllChunks(w, h, project.ChunkSize)
		chunkMap := map[string]*ChunkResult{}
		for _, chunk := range chunks {
			chunkResult, err := ProcessChunk(project, stageIdx, previous, previousChunks, chunk)
			if err != nil {
				return nil, err
			}
			chunkMap[chunkResult.ChunkID] = chunkResult
		}
		sample, err := AssembleChunks(project, stageIdx, chunkMap)
		if err != nil {
			return nil, err
		}
		result.Samples = append(result.Samples, sample)
		result.Stages = append(result.Stages, &StageResult{
			Config: project.Stages[stageIdx], Sample: sample, Chunks: chunkMap,
		})
		previous = sample
		previousChunks = chunkMap
	}
	band, err := AnalyzeFirstBand(project, result.Samples)
	if err != nil {
		if ce, ok := AsError(err); ok && ce.Code == EAmbig {
			result.Band = band
			return result, err
		}
		return result, err
	}
	result.Band = band
	return result, nil
}

// CompareResults locates the first differing sample field or commit sequence.
func CompareResults(project *Project, a, b *PipelineResult, commitsA, commitsB []CommitEntry) *Mismatch {
	if len(a.Samples) != len(b.Samples) {
		return &Mismatch{Field: "stage_count", Expected: len(a.Samples), Actual: len(b.Samples)}
	}
	for stage := range a.Samples {
		for _, desc := range project.Planes {
			pa := a.Samples[stage].Planes[desc.Name]
			pb := b.Samples[stage].Planes[desc.Name]
			if pa == nil || pb == nil {
				return &Mismatch{Stage: stage, Plane: desc.Name, Field: "plane", Expected: pa != nil, Actual: pb != nil}
			}
			for i := range pa.Values {
				coord := Coord{X: i % pa.Width, Y: i / pa.Width}
				chunk := ChunkCoordAt(coord.X, coord.Y, project.ChunkSize)
				chunkID := ChunkID(chunk)
				if i >= len(pb.Values) {
					return &Mismatch{Stage: stage, ChunkID: chunkID, Plane: desc.Name, Coord: &coord, Field: "value_count", Expected: len(pa.Values), Actual: len(pb.Values)}
				}
				if pa.Codes[i] != pb.Codes[i] {
					return &Mismatch{Stage: stage, ChunkID: chunkID, Plane: desc.Name, Coord: &coord, Field: "code", Expected: pa.Codes[i], Actual: pb.Codes[i]}
				}
				if pa.Values[i] != pb.Values[i] {
					return &Mismatch{Stage: stage, ChunkID: chunkID, Plane: desc.Name, Coord: &coord, Field: "output_fixed", Expected: pa.Values[i], Actual: pb.Values[i]}
				}
				la, lb := pa.Ledgers[i], pb.Ledgers[i]
				if la.Remainder != lb.Remainder {
					return &Mismatch{Stage: stage, ChunkID: chunkID, Plane: desc.Name, Coord: &coord, Field: "remainder", Expected: la.Remainder, Actual: lb.Remainder}
				}
				if la.CacheKey != lb.CacheKey {
					return &Mismatch{Stage: stage, ChunkID: chunkID, Plane: desc.Name, Coord: &coord, Field: "cache_key", Expected: la.CacheKey, Actual: lb.CacheKey}
				}
			}
		}
	}
	if mismatch := compareBand(a.Band, b.Band); mismatch != nil {
		return mismatch
	}
	if len(commitsA) != len(commitsB) {
		return &Mismatch{Field: "commit_sequence_length", Expected: len(commitsA), Actual: len(commitsB)}
	}
	for i := range commitsA {
		if commitsA[i].Stage != commitsB[i].Stage || commitsA[i].ChunkID != commitsB[i].ChunkID || commitsA[i].CacheKey != commitsB[i].CacheKey {
			return &Mismatch{
				Stage: commitsA[i].Stage, ChunkID: commitsA[i].ChunkID, Field: fmt.Sprintf("commit_sequence[%d]", i),
				Expected: commitsA[i], Actual: commitsB[i],
			}
		}
	}
	return nil
}

func compareBand(a, b *BandReport) *Mismatch {
	if a == nil || b == nil {
		return &Mismatch{Field: "band_presence", Expected: a != nil, Actual: b != nil}
	}
	if a.Stage != b.Stage {
		return &Mismatch{Stage: a.Stage, Field: "band.stage", Expected: a.Stage, Actual: b.Stage}
	}
	if a.Plane != b.Plane {
		return &Mismatch{Stage: a.Stage, Plane: a.Plane, Field: "band.plane", Expected: a.Plane, Actual: b.Plane}
	}
	if a.Coord != b.Coord {
		return &Mismatch{Stage: a.Stage, Plane: a.Plane, Coord: &a.Coord, Field: "band.coord", Expected: a.Coord, Actual: b.Coord}
	}
	if a.DominantOperator != b.DominantOperator {
		return &Mismatch{Stage: a.Stage, Plane: a.Plane, Coord: &a.Coord, Field: "band.dominant_operator", Expected: a.DominantOperator, Actual: b.DominantOperator}
	}
	return nil
}
