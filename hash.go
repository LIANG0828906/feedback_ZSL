package acousticfield

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
)

func ChunkContentHash(c *Chunk) string {
	clone := *c
	clone.Absorption = append([]Absorption(nil), c.Absorption...)
	clone.Reflectors = append([]Reflector(nil), c.Reflectors...)
	sortAbsorption(clone.Absorption)
	sortReflectors(clone.Reflectors)
	return canonicalHash(clone)
}

func LevelContentHash(level *Level) string {
	clone := *level
	clone.Bands = append([]Band(nil), level.Bands...)
	clone.Chunks = append([]Chunk(nil), level.Chunks...)
	clone.Sources = append([]Source(nil), level.Sources...)
	clone.Listeners = append([]Listener(nil), level.Listeners...)
	sort.Slice(clone.Bands, func(i, j int) bool { return clone.Bands[i].ID < clone.Bands[j].ID })
	sort.Slice(clone.Chunks, func(i, j int) bool { return clone.Chunks[i].ID < clone.Chunks[j].ID })
	for i := range clone.Chunks {
		clone.Chunks[i].Absorption = append([]Absorption(nil), clone.Chunks[i].Absorption...)
		clone.Chunks[i].Reflectors = append([]Reflector(nil), clone.Chunks[i].Reflectors...)
		sortAbsorption(clone.Chunks[i].Absorption)
		sortReflectors(clone.Chunks[i].Reflectors)
	}
	sort.Slice(clone.Sources, func(i, j int) bool { return clone.Sources[i].ID < clone.Sources[j].ID })
	for i := range clone.Sources {
		clone.Sources[i].Emission = append([]BandEnergy(nil), clone.Sources[i].Emission...)
		sortBandEnergy(clone.Sources[i].Emission)
	}
	sort.Slice(clone.Listeners, func(i, j int) bool { return clone.Listeners[i].ID < clone.Listeners[j].ID })
	return canonicalHash(clone)
}

func PathDigest(paths []Path) string {
	return canonicalHash(paths)
}

func transferDigest(t Transfer) string {
	return canonicalHash(t)
}

func sourceEnergyDigest(v []SourceEnergy) string {
	return canonicalHash(v)
}

func resultFieldDigest(res ChunkResult) string {
	type field struct {
		ChunkID       int            `json:"chunk_id"`
		ArrivalEnergy []BandEnergy   `json:"arrival_energy"`
		SourceEnergy  []SourceEnergy `json:"source_energy"`
		Paths         []Path         `json:"paths"`
		Incoming      []Transfer     `json:"incoming"`
		Outgoing      []Transfer     `json:"outgoing"`
		Escapes       []Escape       `json:"escapes"`
	}
	return canonicalHash(field{
		ChunkID: res.ChunkID, ArrivalEnergy: res.ArrivalEnergy, SourceEnergy: res.SourceEnergy,
		Paths: res.Paths, Incoming: res.Incoming, Outgoing: res.Outgoing, Escapes: res.Escapes,
	})
}

func SnapshotID(level *Level, options Options) string {
	sum := sha256.Sum256([]byte(level.ID + "|" + options.Version() + "|" + AlgorithmVersion))
	return hex.EncodeToString(sum[:16])
}

func marshalStable(v any) []byte {
	b, _ := json.Marshal(v)
	return b
}
