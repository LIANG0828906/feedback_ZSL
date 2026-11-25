package bake

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"sort"

	"terrainkernel/internal/kerr"
)

type Pixel struct {
	PX        int
	PY        int
	HeightMM  int64
	SlopeURad int64
	Weights   []int64
	Dominant  int
	Blend     int64
}

type ChunkResult struct {
	ChunkID      int
	Size         int
	ParamVersion string
	WeightsHash  string
	BlendHash    string
	ContentHash  string
	Dominants    []int
	Blends       []int64
	WeightRows   [][]int64
	Dependencies []string
}

func (r *ChunkResult) ComputeHashes() error {
	if r == nil {
		return kerr.New(kerr.CodeSchema, "chunk", "nil chunk result")
	}
	r.WeightsHash = WeightSummary(r.WeightRows)
	r.BlendHash = BlendSummary(r.Blends)
	r.ContentHash = ContentHash(r.WeightRows, r.Dominants, r.Blends)
	return nil
}

func WeightSummary(rows [][]int64) string {
	h := sha256.New()
	var buf [8]byte
	for _, row := range rows {
		binary.BigEndian.PutUint64(buf[:], uint64(len(row)))
		h.Write(buf[:])
		for _, v := range row {
			binary.BigEndian.PutUint64(buf[:], uint64(v))
			h.Write(buf[:])
		}
	}
	return hex.EncodeToString(h.Sum(nil))
}

func BlendSummary(blends []int64) string {
	h := sha256.New()
	var buf [8]byte
	for _, v := range blends {
		binary.BigEndian.PutUint64(buf[:], uint64(v))
		h.Write(buf[:])
	}
	return hex.EncodeToString(h.Sum(nil))
}

func ContentHash(rows [][]int64, dominants []int, blends []int64) string {
	h := sha256.New()
	var buf [8]byte
	binary.BigEndian.PutUint64(buf[:], uint64(len(rows)))
	h.Write(buf[:])
	for _, row := range rows {
		binary.BigEndian.PutUint64(buf[:], uint64(len(row)))
		h.Write(buf[:])
		for _, v := range row {
			binary.BigEndian.PutUint64(buf[:], uint64(v))
			h.Write(buf[:])
		}
	}
	binary.BigEndian.PutUint64(buf[:], uint64(len(dominants)))
	h.Write(buf[:])
	for _, v := range dominants {
		binary.BigEndian.PutUint64(buf[:], uint64(int64(v)))
		h.Write(buf[:])
	}
	binary.BigEndian.PutUint64(buf[:], uint64(len(blends)))
	h.Write(buf[:])
	for _, v := range blends {
		binary.BigEndian.PutUint64(buf[:], uint64(v))
		h.Write(buf[:])
	}
	return hex.EncodeToString(h.Sum(nil))
}

type DiffPoint struct {
	ChunkID int
	PX      int
	PY      int
	Field   string
	A       int64
	B       int64
}

func (d *DiffPoint) Error() string {
	if d == nil {
		return "<nil>"
	}
	return fmt.Sprintf("%s at chunk %d pixel (%d,%d) field %s: %d != %d",
		kerr.CodeDiff, d.ChunkID, d.PX, d.PY, d.Field, d.A, d.B)
}

func CompareResults(a, b map[int]*ChunkResult) error {
	ids := make([]int, 0, len(a)+len(b))
	seen := make(map[int]struct{}, len(a)+len(b))
	for id := range a {
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	for id := range b {
		if _, ok := seen[id]; !ok {
			ids = append(ids, id)
		}
	}
	sort.Ints(ids)
	for _, id := range ids {
		ra, oka := a[id]
		rb, okb := b[id]
		if !oka || !okb {
			return &DiffPoint{ChunkID: id, PX: -1, PY: -1, Field: "missing_chunk", A: boolInt(oka), B: boolInt(okb)}
		}
		if err := compareChunk(ra, rb); err != nil {
			return err
		}
	}
	return nil
}

func CompareCommitSequences(a, b []int) error {
	if len(a) != len(b) {
		return &DiffPoint{ChunkID: -1, PX: -1, PY: -1, Field: "commit_sequence_length", A: int64(len(a)), B: int64(len(b))}
	}
	for i := range a {
		if a[i] != b[i] {
			return &DiffPoint{ChunkID: a[i], PX: -1, PY: -1, Field: "commit_sequence", A: int64(a[i]), B: int64(b[i])}
		}
	}
	return nil
}

func compareChunk(a, b *ChunkResult) error {
	if a.ChunkID != b.ChunkID {
		return &DiffPoint{ChunkID: a.ChunkID, PX: -1, PY: -1, Field: "chunk_id", A: int64(a.ChunkID), B: int64(b.ChunkID)}
	}
	n := len(a.WeightRows)
	if n != len(b.WeightRows) {
		return &DiffPoint{ChunkID: a.ChunkID, PX: -1, PY: -1, Field: "row_count", A: int64(n), B: int64(len(b.WeightRows))}
	}
	size := chunkWidth(a)
	for index := 0; index < n; index++ {
		if len(a.WeightRows[index]) != len(b.WeightRows[index]) {
			return &DiffPoint{ChunkID: a.ChunkID, PX: index % size, PY: index / size, Field: "layer_count", A: int64(len(a.WeightRows[index])), B: int64(len(b.WeightRows[index]))}
		}
		for layerIndex := range a.WeightRows[index] {
			if a.WeightRows[index][layerIndex] != b.WeightRows[index][layerIndex] {
				return &DiffPoint{ChunkID: a.ChunkID, PX: index % size, PY: index / size, Field: "weights", A: a.WeightRows[index][layerIndex], B: b.WeightRows[index][layerIndex]}
			}
		}
	}
	if len(a.Dominants) != len(b.Dominants) {
		return &DiffPoint{ChunkID: a.ChunkID, PX: -1, PY: -1, Field: "dominant_count", A: int64(len(a.Dominants)), B: int64(len(b.Dominants))}
	}
	for i := range a.Dominants {
		if a.Dominants[i] != b.Dominants[i] {
			return &DiffPoint{ChunkID: a.ChunkID, PX: i % chunkWidth(a), PY: i / chunkWidth(a), Field: "dominant", A: int64(a.Dominants[i]), B: int64(b.Dominants[i])}
		}
	}
	if len(a.Blends) != len(b.Blends) {
		return &DiffPoint{ChunkID: a.ChunkID, PX: -1, PY: -1, Field: "blend_count", A: int64(len(a.Blends)), B: int64(len(b.Blends))}
	}
	for i := range a.Blends {
		if a.Blends[i] != b.Blends[i] {
			return &DiffPoint{ChunkID: a.ChunkID, PX: i % chunkWidth(a), PY: i / chunkWidth(a), Field: "blend", A: a.Blends[i], B: b.Blends[i]}
		}
	}
	return nil
}

func chunkWidth(r *ChunkResult) int {
	if r != nil && r.Size > 0 {
		return r.Size
	}
	return 1
}

func boolInt(v bool) int64 {
	if v {
		return 1
	}
	return 0
}
