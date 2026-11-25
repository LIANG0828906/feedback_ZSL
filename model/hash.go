package model

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"math"
	"sort"
)

type hashWriter struct {
	h [32]byte
	b []byte
}

func newHashWriter() *hashWriter {
	return &hashWriter{b: make([]byte, 8)}
}

func (w *hashWriter) string(s string) {
	binary.LittleEndian.PutUint64(w.b, uint64(len(s)))
	w.h = sha256.Sum256(append(w.h[:], append(w.b, []byte(s)...)...))
}

func (w *hashWriter) float(f float64) {
	binary.LittleEndian.PutUint64(w.b, math.Float64bits(f))
	w.h = sha256.Sum256(append(w.h[:], w.b...))
}

func (w *hashWriter) integer(i int64) {
	binary.LittleEndian.PutUint64(w.b, uint64(i))
	w.h = sha256.Sum256(append(w.h[:], w.b...))
}

func (w *hashWriter) sum() string {
	return hex.EncodeToString(w.h[:])
}

// ChunkHeightHash hashes only the discretized height content of a chunk.
func ChunkHeightHash(c Chunk) string {
	w := newHashWriter()
	w.string("chunk-height-v1")
	w.string(c.ID)
	w.integer(int64(c.ResX))
	w.integer(int64(c.ResZ))
	for _, h := range c.Heights {
		w.float(h)
	}
	return w.sum()
}

// ChunkContentHash hashes heights plus material and occluder references.
func ChunkContentHash(c Chunk) string {
	w := newHashWriter()
	w.string("chunk-content-v1")
	w.string(ChunkHeightHash(c))
	for _, a := range c.Albedo {
		w.float(a)
	}
	refs := append([]string(nil), c.Occluders...)
	sort.Strings(refs)
	for _, ref := range refs {
		w.string(ref)
	}
	return w.sum()
}

// SunHash hashes one solar sample.
func SunHash(s SunSample) string {
	w := newHashWriter()
	w.string("sun-v1")
	w.integer(int64(s.Time))
	w.float(s.AzimuthDeg)
	w.float(s.ElevationDeg)
	for _, r := range s.Radiance {
		w.float(r)
	}
	return w.sum()
}

// SkySetHash hashes the complete ordered sky-light sample set.
func SkySetHash(sky []SkySample) string {
	w := newHashWriter()
	w.string("sky-set-v1")
	w.integer(int64(len(sky)))
	for _, s := range sky {
		w.string(s.ID)
		w.float(s.AzimuthDeg)
		w.float(s.ElevationDeg)
		w.float(s.Weight)
		for _, r := range s.Radiance {
			w.float(r)
		}
	}
	return w.sum()
}

// OccluderSetHash hashes the complete occluder geometry by stable ID.
func OccluderSetHash(occluders []Occluder) string {
	ordered := append([]Occluder(nil), occluders...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].ID < ordered[j].ID })
	w := newHashWriter()
	w.string("occluder-set-v1")
	w.integer(int64(len(ordered)))
	for _, o := range ordered {
		w.string(o.ID)
		w.float(o.Center.X)
		w.float(o.Center.Z)
		w.float(o.Radius)
		w.float(o.Base)
		w.float(o.Height)
	}
	return w.sum()
}

// ParamsHash hashes the numeric bake contract.
func ParamsHash(p BakeParams) string {
	w := newHashWriter()
	w.string("params-v1")
	w.integer(p.FixedScale)
	w.integer(p.MaxChannelFixed)
	w.integer(int64(p.ShadowStepBudget))
	w.float(p.RayStep)
	w.float(p.RayEpsilon)
	w.float(p.IrradianceTolerance)
	w.integer(int64(p.ParameterVersion))
	w.string(p.AlgorithmVersion)
	return w.sum()
}

// ProjectIdentity identifies layout and immutable parameters, deliberately
// excluding editable heights, albedo, occluders, and solar values.
func ProjectIdentity(p Project) string {
	w := newHashWriter()
	w.string("project-identity-v1")
	w.integer(p.Seed)
	w.float(p.Bounds.MinX)
	w.float(p.Bounds.MinZ)
	w.float(p.Bounds.MaxX)
	w.float(p.Bounds.MaxZ)
	w.integer(int64(len(p.Chunks)))
	for _, c := range p.Chunks {
		w.string(c.ID)
		w.integer(int64(c.GridX))
		w.integer(int64(c.GridZ))
		w.float(c.Bounds.MinX)
		w.float(c.Bounds.MinZ)
		w.float(c.Bounds.MaxX)
		w.float(c.Bounds.MaxZ)
		w.integer(int64(c.ResX))
		w.integer(int64(c.ResZ))
	}
	w.string(ParamsHash(p.Params))
	return w.sum()
}

// HashStrings hashes an ordered string list.
func HashStrings(parts ...string) string {
	w := newHashWriter()
	w.string("strings-v1")
	for _, p := range parts {
		w.string(p)
	}
	return w.sum()
}

// CombineCacheKey builds the public cache key contract.
func CombineCacheKey(chunkID, heightHash string, neighborHeightHashes []string, sunTime int, sunHash string, parameterVersion int, algorithmVersion string, upstream []string) string {
	w := newHashWriter()
	w.string("cache-key-v1")
	w.string(chunkID)
	w.string(heightHash)
	neighbors := append([]string(nil), neighborHeightHashes...)
	sort.Strings(neighbors)
	for _, h := range neighbors {
		w.string(h)
	}
	w.integer(int64(sunTime))
	w.string(sunHash)
	w.integer(int64(parameterVersion))
	w.string(algorithmVersion)
	keys := append([]string(nil), upstream...)
	sort.Strings(keys)
	for _, k := range keys {
		w.string(k)
	}
	return w.sum()
}

// EdgeID returns a stable, orientation-independent shared edge identifier.
func EdgeID(a, b string) string {
	if b < a {
		a, b = b, a
	}
	return fmt.Sprintf("edge:%s|%s", a, b)
}
