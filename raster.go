package maskqc

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
)

type ChunkGrid struct {
	Width      int `json:"width"`
	Height     int `json:"height"`
	TileWidth  int `json:"tile_width"`
	TileHeight int `json:"tile_height"`
	Cols       int `json:"cols"`
	Rows       int `json:"rows"`
	Count      int `json:"count"`
}

type Chunk struct {
	ID     int `json:"id"`
	X      int `json:"x"`
	Y      int `json:"y"`
	Width  int `json:"width"`
	Height int `json:"height"`
}

func NewChunkGrid(width, height, tileW, tileH int) ChunkGrid {
	if tileW <= 0 {
		tileW = DefaultTileSize
	}
	if tileH <= 0 {
		tileH = tileW
	}
	cols := (width + tileW - 1) / tileW
	rows := (height + tileH - 1) / tileH
	return ChunkGrid{Width: width, Height: height, TileWidth: tileW, TileHeight: tileH, Cols: cols, Rows: rows, Count: cols * rows}
}

func (g ChunkGrid) Chunk(id int) (Chunk, bool) {
	if id < 0 || id >= g.Count {
		return Chunk{}, false
	}
	cy := id / g.Cols
	cx := id % g.Cols
	x := cx * g.TileWidth
	y := cy * g.TileHeight
	w := minInt(g.TileWidth, g.Width-x)
	h := minInt(g.TileHeight, g.Height-y)
	if w <= 0 || h <= 0 {
		return Chunk{}, false
	}
	return Chunk{ID: id, X: x, Y: y, Width: w, Height: h}, true
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func (g ChunkGrid) ChunkAt(x, y int) int {
	if x < 0 || y < 0 || x >= g.Width || y >= g.Height {
		return -1
	}
	return (y/g.TileHeight)*g.Cols + x/g.TileWidth
}

// AffectedChunks returns chunks intersecting the expanded stroke bounding box.
func (g ChunkGrid) AffectedChunks(s Stroke) []int {
	radius := maxInt(1, (s.Width+1)/2)
	minX := minInt(s.Start.X, s.End.X) - radius
	maxX := maxInt(s.Start.X, s.End.X) + radius
	minY := minInt(s.Start.Y, s.End.Y) - radius
	maxY := maxInt(s.Start.Y, s.End.Y) + radius
	if maxX < 0 || maxY < 0 || minX >= g.Width || minY >= g.Height {
		return nil
	}
	if minX < 0 {
		minX = 0
	}
	if minY < 0 {
		minY = 0
	}
	if maxX >= g.Width {
		maxX = g.Width - 1
	}
	if maxY >= g.Height {
		maxY = g.Height - 1
	}
	idSet := map[int]bool{}
	for y := minY; y <= maxY; y++ {
		for x := minX; x <= maxX; x++ {
			id := g.ChunkAt(x, y)
			if id >= 0 {
				idSet[id] = true
			}
		}
	}
	ids := make([]int, 0, len(idSet))
	for id := range idSet {
		ids = append(ids, id)
	}
	sort.Ints(ids)
	return ids
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func ValidateStrokeBounds(s Stroke, width, height int, g ChunkGrid) error {
	if s.Start.X < 0 || s.Start.Y < 0 || s.End.X < 0 || s.End.Y < 0 ||
		s.Start.X >= width || s.Start.Y >= height || s.End.X >= width || s.End.Y >= height {
		chunkID := g.ChunkAt(s.Start.X, s.Start.Y)
		if chunkID < 0 {
			affected := g.AffectedChunks(s)
			if len(affected) > 0 {
				chunkID = affected[0]
			} else {
				chunkID = 0
			}
		}
		field := "start"
		badX := s.Start.X < 0 || s.Start.X >= width
		badY := s.Start.Y < 0 || s.Start.Y >= height
		if s.End.X < 0 || s.End.X >= width || s.End.Y < 0 || s.End.Y >= height {
			if !badX && !badY {
				field = "end"
			}
		}
		return &BizError{
			Code: CodeRange, Message: "stroke coordinate outside canvas",
			Field: field, StrokeID: s.ID, ObjectID: s.ID, ChunkID: chunkID,
			Want: fmt.Sprintf("x in [0,%d), y in [0,%d)", width, height),
			Got:  fmt.Sprintf("start=%+v end=%+v", s.Start, s.End),
		}
	}
	return nil
}

// RasterStrokeChunk returns alpha coverage (0..255) for one chunk. All geometry
// comparisons use integer squared distances.
func RasterStrokeChunk(s Stroke, c Chunk) ([]byte, error) {
	if err := ValidateStrokeParameters(s); err != nil {
		if be, ok := AsBizError(err); ok {
			be.ChunkID = c.ID
			return nil, be
		}
		return nil, err
	}
	out := make([]byte, c.Width*c.Height)
	radius := maxInt(1, (s.Width+1)/2)
	outer2Scaled := int64(4 * radius * radius)
	hard := s.HardnessPct
	innerRadiusScaled := int64(2 * radius * hard / 100)
	inner2Scaled := innerRadiusScaled * innerRadiusScaled

	// Coordinates are doubled so integer pixel centers are odd.
	ax := int64(2*s.Start.X + 1)
	ay := int64(2*s.Start.Y + 1)
	bx := int64(2*s.End.X + 1)
	by := int64(2*s.End.Y + 1)
	vx := bx - ax
	vy := by - ay
	l2 := vx*vx + vy*vy

	for ly := 0; ly < c.Height; ly++ {
		for lx := 0; lx < c.Width; lx++ {
			gx := c.X + lx
			gy := c.Y + ly
			qx := int64(2*gx + 1)
			qy := int64(2*gy + 1)
			wx := qx - ax
			wy := qy - ay
			var d2, denominator int64
			insideInner := false
			outsideOuter := false
			if l2 == 0 {
				d2 = wx*wx + wy*wy
				denominator = 1
				insideInner = d2 <= inner2Scaled
				outsideOuter = d2 >= outer2Scaled
			} else {
				dot := wx*vx + wy*vy
				switch {
				case dot <= 0:
					d2 = wx*wx + wy*wy
					denominator = 1
					insideInner = d2 <= inner2Scaled
					outsideOuter = d2 >= outer2Scaled
				case dot >= l2:
					dx := qx - bx
					dy := qy - by
					d2 = dx*dx + dy*dy
					denominator = 1
					insideInner = d2 <= inner2Scaled
					outsideOuter = d2 >= outer2Scaled
				default:
					cross := wx*vy - wy*vx
					cross2 := cross * cross
					insideInner = cross2 <= inner2Scaled*l2
					outsideOuter = cross2 >= outer2Scaled*l2
					if !insideInner && !outsideOuter {
						num := outer2Scaled*l2 - cross2
						den := (outer2Scaled - inner2Scaled) * l2
						if den <= 0 {
							insideInner = true
						} else {
							alpha := int64(255) * num / den
							alpha = applyWeight(alpha, s.WeightMyriad)
							if alpha < 0 {
								alpha = 0
							}
							if alpha > 255 {
								alpha = 255
							}
							out[ly*c.Width+lx] = byte(alpha)
							continue
						}
					}
				}
			}
			if outsideOuter {
				continue
			}
			alpha := int64(255)
			if !insideInner && outer2Scaled > inner2Scaled && denominator != 0 {
				// Endpoint path uses squared-radius interpolation.
				num := outer2Scaled - d2
				den := outer2Scaled - inner2Scaled
				alpha = int64(255) * num / den
			}
			alpha = applyWeight(alpha, s.WeightMyriad)
			if alpha < 0 {
				alpha = 0
			}
			if alpha > 255 {
				alpha = 255
			}
			out[ly*c.Width+lx] = byte(alpha)
		}
	}
	return out, nil
}

func applyWeight(alpha int64, weightMyriad int) int64 {
	return alpha * int64(weightMyriad) / 10000
}

type StrokeComponent struct {
	StrokeID    string `json:"stroke_id"`
	ChunkID     int    `json:"chunk_id"`
	Coverage    []byte `json:"coverage"`
	CoverageSHA string `json:"coverage_sha"`
	ContentHash string `json:"content_hash"`
	CacheKey    string `json:"cache_key"`
}

func HashBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func ComponentContentHash(s Stroke, c Chunk) string {
	return stableJSONHash(map[string]any{
		"stroke": s, "chunk": c, "algorithm": AlgorithmVersion,
	})
}

func ComponentCacheKey(s Stroke, c Chunk, chunkInputHash, upstreamKey string) string {
	return stableJSONHash(map[string]any{
		"chunk_input":   chunkInputHash,
		"stroke_id":     s.ID,
		"stroke_seq":    s.Seq,
		"stroke_hash":   ComponentContentHash(s, c),
		"order_version": OrderVersion,
		"convert_ver":   ConvertVersion,
		"upstream":      upstreamKey,
	})
}

// CompositeChunk performs source-over alpha accumulation. The component order
// must already be the globally adjudicated order.
func CompositeChunk(destination []byte, components []StrokeComponent, order []string, width, height int) ([]byte, error) {
	expected := width * height
	if len(destination) != expected {
		return nil, &BizError{
			Code: CodeSize, Message: "candidate chunk buffer size mismatch",
			Want: fmt.Sprint(expected), Got: fmt.Sprint(len(destination)), ChunkID: -1,
		}
	}
	out := append([]byte(nil), destination...)
	byID := map[string]StrokeComponent{}
	for _, comp := range components {
		if len(comp.Coverage) != expected {
			return nil, &BizError{
				Code: CodeSize, Message: "coverage chunk buffer size mismatch",
				StrokeID: comp.StrokeID, ChunkID: comp.ChunkID,
				Want: fmt.Sprint(expected), Got: fmt.Sprint(len(comp.Coverage)),
			}
		}
		byID[comp.StrokeID] = comp
	}
	for _, id := range order {
		comp, ok := byID[id]
		if !ok {
			continue
		}
		for i := 0; i < expected; i++ {
			src := int(comp.Coverage[i])
			dst := int(out[i])
			out[i] = byte(src + dst*(255-src)/255)
		}
	}
	return out, nil
}

type OrderDecision struct {
	ChunkID   int      `json:"chunk_id"`
	StrokeIDs []string `json:"stroke_ids"`
	Reasons   []string `json:"reasons"`
	Basis     string   `json:"basis"`
}

func PlanOrders(p *Project, g ChunkGrid) map[int]OrderDecision {
	ordered := p.SortedStrokes()
	out := map[int]OrderDecision{}
	for id := 0; id < g.Count; id++ {
		d := OrderDecision{ChunkID: id, Basis: "priority ascending, then seq ascending; " + OrderVersion}
		for _, s := range ordered {
			ids := g.AffectedChunks(s)
			for _, cid := range ids {
				if cid == id {
					d.StrokeIDs = append(d.StrokeIDs, s.ID)
					d.Reasons = append(d.Reasons, fmt.Sprintf("%s(priority=%d,seq=%d)", s.ID, s.Priority, s.Seq))
					break
				}
			}
		}
		out[id] = d
	}
	return out
}

func CandidateChunk(p *Project, c Chunk) ([]byte, error) {
	if p.Candidate == nil {
		return make([]byte, c.Width*c.Height), nil
	}
	if p.Candidate.Width != p.Width || p.Candidate.Height != p.Height {
		return nil, &BizError{
			Code: CodeSize, Message: "candidate dimensions do not match project canvas",
			Field: "candidate", ObjectID: p.ID, ChunkID: c.ID,
			Want: fmt.Sprintf("%dx%d", p.Width, p.Height),
			Got:  fmt.Sprintf("%dx%d", p.Candidate.Width, p.Candidate.Height),
		}
	}
	buf := make([]byte, c.Width*c.Height)
	for y := 0; y < c.Height; y++ {
		srcStart := (c.Y+y)*p.Candidate.Width + c.X
		copy(buf[y*c.Width:(y+1)*c.Width], p.Candidate.Pix[srcStart:srcStart+c.Width])
	}
	return buf, nil
}

func AssembleMask(g ChunkGrid, chunks map[int][]byte) *GrayMask {
	m := NewGrayMask(g.Width, g.Height)
	for id, data := range chunks {
		c, ok := g.Chunk(id)
		if !ok || len(data) != c.Width*c.Height {
			continue
		}
		for y := 0; y < c.Height; y++ {
			dst := (c.Y+y)*g.Width + c.X
			src := y * c.Width
			copy(m.Pix[dst:dst+c.Width], data[src:src+c.Width])
		}
	}
	return m
}
