package terrainbake

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
)

func digestVertices(vertices []Vertex) string {
	h := sha256.New()
	var buf [8]byte
	for _, v := range vertices {
		binary.LittleEndian.PutUint32(buf[:4], uint32(v.ID))
		h.Write(buf[:4])
		binary.LittleEndian.PutUint64(buf[:8], uint64(v.X))
		h.Write(buf[:8])
		binary.LittleEndian.PutUint64(buf[:8], uint64(v.Y))
		h.Write(buf[:8])
		binary.LittleEndian.PutUint64(buf[:8], uint64(v.HeightMM))
		h.Write(buf[:8])
	}
	return hex.EncodeToString(h.Sum(nil))
}

func digestTriangles(triangles []Triangle) string {
	h := sha256.New()
	var buf [8]byte
	for _, t := range triangles {
		for _, v := range []int{t.A, t.B, t.C} {
			binary.LittleEndian.PutUint32(buf[:4], uint32(v))
			h.Write(buf[:4])
		}
		binary.LittleEndian.PutUint64(buf[:8], uint64(t.HeightErrorUM))
		h.Write(buf[:8])
		if t.Boundary {
			h.Write([]byte{1})
		} else {
			h.Write([]byte{0})
		}
	}
	return hex.EncodeToString(h.Sum(nil))
}

func digestJSON(v any) string {
	data, _ := json.Marshal(v)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func DigestMesh(m *Mesh) string {
	if m == nil {
		return ""
	}
	return digestJSON(struct {
		BlockID      string     `json:"block_id"`
		Vertices     []Vertex   `json:"vertices"`
		Triangles    []Triangle `json:"triangles"`
		Error        int64      `json:"error"`
		Boundary     int64      `json:"boundary_error"`
		Undersampled bool       `json:"undersampled"`
	}{
		BlockID: m.BlockID, Vertices: m.Vertices, Triangles: m.Triangles,
		Error: m.HeightErrorMaxUM, Boundary: m.BoundaryErrorMaxUM,
		Undersampled: m.Undersampled,
	})
}

func hashParts(parts ...string) string {
	h := sha256.New()
	for _, p := range parts {
		var n [8]byte
		binary.LittleEndian.PutUint64(n[:], uint64(len(p)))
		h.Write(n[:])
		h.Write([]byte(p))
	}
	return hex.EncodeToString(h.Sum(nil))
}

func contentHash(block BlockSpec, height MeshHeightFunc) string {
	h := sha256.New()
	h.Write([]byte(block.ID))
	// A fixed lattice keeps hashing cheap while covering the whole block.
	const stride = int64(8)
	h.Write([]byte{0})
	for y := block.Rect.MinY; y <= block.Rect.MaxY; y += stride {
		for x := block.Rect.MinX; x <= block.Rect.MaxX; x += stride {
			var buf [16]byte
			binary.LittleEndian.PutUint64(buf[:8], uint64(x))
			binary.LittleEndian.PutUint64(buf[8:], uint64(y))
			h.Write(buf[:])
			binary.LittleEndian.PutUint64(buf[:8], uint64(height(x, y)))
			h.Write(buf[:8])
		}
	}
	return hex.EncodeToString(h.Sum(nil))
}

func adjacencyVersion(p *Project) string {
	type edge struct {
		ID, A, B, Axis             string
		Coord, Start, End, Spacing int64
		AngleLimit                 float64
	}
	edges := make([]edge, 0, len(p.Seams))
	for _, s := range p.Seams {
		edges = append(edges, edge{
			ID: s.ID, A: s.A, B: s.B, Axis: string(s.Axis),
			Coord: s.Coordinate, Start: s.Start, End: s.End, Spacing: s.SampleSpacing,
			AngleLimit: s.AngleLimitDeg,
		})
	}
	sort.Slice(edges, func(i, j int) bool { return edges[i].ID < edges[j].ID })
	return digestJSON(edges)
}

func parameterVersion(b BlockSpec, seams []SeamSpec) string {
	type param struct {
		Budget        Budget
		ToleranceUM   int64
		Rounding      string
		Scale         string
		SpacingByEdge map[string]int64
	}
	spacing := map[string]int64{}
	for _, s := range seams {
		if s.A == b.ID || s.B == b.ID {
			spacing[s.ID] = s.SampleSpacing
		}
	}
	return digestJSON(param{
		Budget: b.Budget, ToleranceUM: b.ToleranceUM,
		Rounding: b.ToleranceRounding, Scale: b.ToleranceScale,
		SpacingByEdge: spacing,
	})
}

func cacheKey(content, adjVersion, paramVersion, algorithm string, upstream []string) string {
	up := append([]string(nil), upstream...)
	sort.Strings(up)
	parts := []string{"cache-v1", content, adjVersion, paramVersion, algorithm}
	parts = append(parts, up...)
	return hashParts(parts...)
}

func fmtValue(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case nil:
		return "<nil>"
	default:
		data, err := json.Marshal(x)
		if err != nil {
			return fmt.Sprintf("%v", x)
		}
		if len(data) > 240 {
			return string(data[:240]) + "..."
		}
		return string(data)
	}
}
