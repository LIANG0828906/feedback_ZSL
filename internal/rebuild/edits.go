package rebuild

import (
	"acousticfield/internal/acoustic"
	"acousticfield/internal/deps"
)

func EditAbsorption(level *acoustic.Level, chunkID string, bandID int, alphaQ int64) (deps.Edit, error) {
	for i := range level.Chunks {
		if level.Chunks[i].ID != chunkID {
			continue
		}
		if bandID < 0 || bandID >= len(level.Chunks[i].Bands) {
			return deps.Edit{}, acoustic.SchemaError(chunkID, "bands.id", "band %d not found", bandID)
		}
		if !acoustic.IsQuantumAligned(alphaQ, acoustic.AbsorptionQuantum) || alphaQ > acoustic.EnergyScale {
			return deps.Edit{}, acoustic.SchemaError(chunkID, "bands.alpha_q", "alpha must be a positive declared-tick coefficient <= %d", acoustic.EnergyScale)
		}
		level.Chunks[i].Bands[bandID].AlphaQ = alphaQ
		return deps.Edit{Kind: "absorption", ChunkID: chunkID, BandID: bandID}, nil
	}
	return deps.Edit{}, acoustic.SchemaError(chunkID, "id", "chunk not found")
}

func EditSourceEmission(level *acoustic.Level, sourceID string, emission []int64) (deps.Edit, error) {
	for i := range level.Sources {
		if level.Sources[i].ID != sourceID {
			continue
		}
		if len(emission) != len(level.BandIDs) {
			return deps.Edit{}, acoustic.SchemaError(sourceID, "emission_q", "expected %d bands", len(level.BandIDs))
		}
		for _, v := range emission {
			if v <= 0 {
				return deps.Edit{}, acoustic.SchemaError(sourceID, "emission_q", "energy must be positive")
			}
		}
		level.Sources[i].EmissionQ = acoustic.CopyInt64s(emission)
		return deps.Edit{Kind: "source_emission", ChunkID: level.Sources[i].ChunkID, SourceID: sourceID}, nil
	}
	return deps.Edit{}, acoustic.SchemaError(sourceID, "id", "source not found")
}

func MoveSource(level *acoustic.Level, sourceID string, position acoustic.Point, newChunkID string) (deps.Edit, error) {
	for i := range level.Sources {
		if level.Sources[i].ID != sourceID {
			continue
		}
		oldChunk := level.Sources[i].ChunkID
		if newChunkID == "" {
			for _, c := range level.Chunks {
				if c.Bounds.Contains(position) {
					newChunkID = c.ID
					break
				}
			}
		}
		if newChunkID == "" {
			return deps.Edit{}, acoustic.SchemaError(sourceID, "position", "position is outside all chunks")
		}
		var chunk *acoustic.Chunk
		for j := range level.Chunks {
			if level.Chunks[j].ID == newChunkID {
				chunk = &level.Chunks[j]
				break
			}
		}
		if chunk == nil {
			return deps.Edit{}, acoustic.SchemaError(sourceID, "chunk_id", "chunk %s not found", newChunkID)
		}
		if !chunk.Bounds.Contains(position) {
			return deps.Edit{}, acoustic.SchemaError(sourceID, "position", "position outside chunk %s", newChunkID)
		}
		level.Sources[i].Position = position
		level.Sources[i].ChunkID = newChunkID
		return deps.Edit{
			Kind: "source_position", ChunkID: oldChunk, SourceID: sourceID, NewChunkID: newChunkID,
		}, nil
	}
	return deps.Edit{}, acoustic.SchemaError(sourceID, "id", "source not found")
}

func EditSurfaceGeometry(level *acoustic.Level, chunkID, surfaceID string, a, b acoustic.Point) (deps.Edit, error) {
	for ci := range level.Chunks {
		if level.Chunks[ci].ID != chunkID {
			continue
		}
		for si := range level.Chunks[ci].Surfaces {
			s := &level.Chunks[ci].Surfaces[si]
			if s.ID != surfaceID {
				continue
			}
			if !level.Chunks[ci].Bounds.Contains(a) || !level.Chunks[ci].Bounds.Contains(b) {
				return deps.Edit{}, acoustic.SchemaError(surfaceID, "endpoints", "surface must remain inside chunk")
			}
			if a == b {
				return deps.Edit{}, acoustic.SchemaError(surfaceID, "endpoints", "endpoints must differ")
			}
			s.A, s.B = a, b
			return deps.Edit{Kind: "surface_geometry", ChunkID: chunkID, SurfaceID: surfaceID}, nil
		}
		return deps.Edit{}, acoustic.SchemaError(surfaceID, "id", "surface not found in chunk %s", chunkID)
	}
	return deps.Edit{}, acoustic.SchemaError(chunkID, "id", "chunk not found")
}
