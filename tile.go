package grid

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"sort"
)

type Camera struct {
	Offset        Vec2  `json:"offset"`
	UnitsPerPixel int64 `json:"unitsPerPixel"`
	ViewWidth     int   `json:"viewWidth"`
	ViewHeight    int   `json:"viewHeight"`
	TilePixels    int   `json:"tilePixels"`
	CameraVersion int   `json:"cameraVersion"`
}

func DefaultCamera() Camera {
	return Camera{
		Offset: Vec2{}, UnitsPerPixel: 250 * PositionScale, // 250 mm per pixel
		ViewWidth: 128, ViewHeight: 64, TilePixels: PixelSize, CameraVersion: 1,
	}
}

func (c Camera) Cols() int { return (c.ViewWidth + c.TilePixels - 1) / c.TilePixels }
func (c Camera) Rows() int { return (c.ViewHeight + c.TilePixels - 1) / c.TilePixels }

// ProjectQ returns fixed-point screen pixels with 1/1024 pixel precision.
func (c Camera) ProjectQ(p Vec2) (int64, int64) {
	const q = 1024
	x := (p.X - c.Offset.X) * q / c.UnitsPerPixel
	y := (p.Y - c.Offset.Y) * q / c.UnitsPerPixel
	return x, y
}

type Tile struct {
	ID                TileID   `json:"id"`
	Seq               int      `json:"seq"`
	ContentHash       string   `json:"contentHash"`
	GeometryHash      string   `json:"geometryHash"`
	CacheKey          string   `json:"cacheKey,omitempty"`
	ColorDigest       string   `json:"colorDigest"`
	DepthDigest       string   `json:"depthDigest"`
	ObstacleCoverage  int      `json:"obstacleCoveragePixels"`
	PolyCoverage      int      `json:"polyCoveragePixels"`
	CoveragePixels    int      `json:"coveragePixels"`
	CoveragePermille  int      `json:"coveragePermille"`
	PolyCoverageRatio float64  `json:"polyCoverage"`
	ObstacleIDs       []string `json:"obstacleIds,omitempty"`
	RequestIDs        []string `json:"requestIds,omitempty"`
	Color             []byte   `json:"-"`
	Depth             []uint32 `json:"-"`
}

type SceneTiles struct {
	Camera        Camera           `json:"camera"`
	Tiles         []*Tile          `json:"tiles"`
	Results       []*PredictResult `json:"-"`
	RequestErrors []*Error         `json:"requestErrors,omitempty"`
	HitStats      map[string]int   `json:"hitStats"`
}

type segmentMM struct {
	ax, ay, bx, by, radius int64
}

type rasterTile struct {
	id               TileID
	obsMask          []bool
	polyMask         []bool
	bounceMask       []bool
	obstacleSeq      []int
	obstacleMat      []int
	obstacleIDs      map[string]bool
	obstacleMaterial map[string]int
	requestIDs       map[string]bool
	geomRecords      []string
}

// ProjectScene predicts every request, rasterizes geometry and produces
// content-addressed tiles.
func ProjectScene(level *Level, cam Camera) (*SceneTiles, error) {
	return ProjectSceneWithPool(level, cam, nil)
}

func ProjectSceneWithPool(level *Level, cam Camera, pool *RayPool) (*SceneTiles, error) {
	if cam.TilePixels <= 0 {
		cam.TilePixels = PixelSize
	}
	if cam.UnitsPerPixel <= 0 {
		cam = DefaultCamera()
	}
	scene := &SceneTiles{Camera: cam, HitStats: map[string]int{}}
	for _, req := range level.Requests {
		launch := level.LaunchByID(req.LaunchID)
		if launch == nil {
			continue
		}
		result, err := Predict(level, req.RequestID, *launch)
		if err != nil {
			if ge, ok := AsError(err, ""); ok {
				scene.RequestErrors = append(scene.RequestErrors, ge)
			}
		}
		if result != nil {
			scene.Results = append(scene.Results, result)
			for k, v := range result.HitStats {
				scene.HitStats[k] += v
			}
		}
	}

	tileCount := cam.Cols() * cam.Rows()
	scene.Tiles = make([]*Tile, tileCount)
	if pool == nil {
		seq := 0
		for row := 0; row < cam.Rows(); row++ {
			for col := 0; col < cam.Cols(); col++ {
				scene.Tiles[seq] = buildOneTile(TileID{Col: col, Row: row}, seq, level, scene.Results, cam)
				seq++
			}
		}
		return scene, nil
	}

	jobs := make([]TileJob, 0, tileCount)
	for seq := 0; seq < tileCount; seq++ {
		tileSeq := seq
		row := seq / cam.Cols()
		col := seq % cam.Cols()
		tileID := TileID{Col: col, Row: row}
		jobs = append(jobs, func(ctx context.Context, workerID int) error {
			rt := newRasterTile(tileID, cam.TilePixels)
			rasterizeObstacles(rt, level.Obstacles, cam)
			for _, result := range scene.Results {
				rasterizePath(rt, result, cam)
				for rayIndex := 0; rayIndex < 2; rayIndex++ {
					ray := Ray{Tile: tileID, Index: rayIndex, Name: "projection-sweep-row"}
					if err := pool.SubmitRay(ctx, workerID, ray); err != nil {
						return err
					}
				}
			}
			pool.Flush(workerID)
			scene.Tiles[tileSeq] = rt.buildTile(tileSeq, cam)
			return nil
		})
	}
	if err := pool.RunJobs(context.Background(), jobs); err != nil {
		return scene, err
	}
	return scene, nil
}

func buildOneTile(id TileID, seq int, level *Level, results []*PredictResult, cam Camera) *Tile {
	rt := newRasterTile(id, cam.TilePixels)
	rasterizeObstacles(rt, level.Obstacles, cam)
	for _, result := range results {
		rasterizePath(rt, result, cam)
	}
	return rt.buildTile(seq, cam)
}

func newRasterTile(id TileID, pixels int) *rasterTile {
	n := pixels * pixels
	return &rasterTile{
		id: id, obsMask: make([]bool, n), polyMask: make([]bool, n), bounceMask: make([]bool, n),
		obstacleSeq: make([]int, n), obstacleMat: make([]int, n),
		obstacleIDs: map[string]bool{}, obstacleMaterial: map[string]int{}, requestIDs: map[string]bool{},
	}
}

func (rt *rasterTile) pixelCenterMM(idx int, cam Camera) (int64, int64) {
	px := idx % cam.TilePixels
	py := idx / cam.TilePixels
	mmPerPixel := cam.UnitsPerPixel / PositionScale
	ox := cam.Offset.X / PositionScale
	oy := cam.Offset.Y / PositionScale
	x := ox + int64(rt.id.Col*cam.TilePixels+px)*mmPerPixel + mmPerPixel/2
	y := oy + int64(rt.id.Row*cam.TilePixels+py)*mmPerPixel + mmPerPixel/2
	return x, y
}

func rasterizeObstacles(rt *rasterTile, obstacles []Obstacle, cam Camera) {
	radius := cam.UnitsPerPixel / PositionScale / 2
	if radius < 1 {
		radius = 1
	}
	for _, o := range obstacles {
		seg := segmentMM{
			ax: o.A.X / PositionScale, ay: o.A.Y / PositionScale,
			bx: o.B.X / PositionScale, by: o.B.Y / PositionScale, radius: radius,
		}
		touched := false
		for i := range rt.obsMask {
			if rt.obsMask[i] && rt.obstacleSeq[i] != 0 && rt.obstacleSeq[i] <= o.Seq {
				// Still check this obstacle for overlap; depth choice happens below.
			}
			x, y := rt.pixelCenterMM(i, cam)
			if pointNearSegment(x, y, seg) {
				rt.obsMask[i] = true
				if rt.obstacleSeq[i] == 0 || o.Seq < rt.obstacleSeq[i] {
					rt.obstacleSeq[i] = o.Seq
					rt.obstacleMat[i] = o.MaterialVer
				}
				touched = true
			}
		}
		if touched {
			rt.obstacleIDs[o.ID] = true
			rt.obstacleMaterial[o.ID] = o.MaterialVer
			qx, qy := cam.ProjectQ(o.A)
			ex, ey := cam.ProjectQ(o.B)
			rt.geomRecords = append(rt.geomRecords,
				fmt.Sprintf("obstacle id=%s seq=%d material=%d aq=(%d,%d) bq=(%d,%d) restitution=%d normal=(%d,%d)",
					o.ID, o.Seq, o.MaterialVer, qx, qy, ex, ey, o.Restitution, o.Normal.X, o.Normal.Y))
		}
	}
}

func rasterizePath(rt *rasterTile, result *PredictResult, cam Camera) {
	if result == nil || len(result.Path) < 2 {
		return
	}
	radius := cam.UnitsPerPixel / PositionScale / 3
	if radius < 1 {
		radius = 1
	}
	touched := false
	for i := 1; i < len(result.Path); i++ {
		a := result.Path[i-1]
		b := result.Path[i]
		seg := segmentMM{
			ax: a.X / PositionScale, ay: a.Y / PositionScale,
			bx: b.X / PositionScale, by: b.Y / PositionScale, radius: radius,
		}
		for idx := range rt.polyMask {
			x, y := rt.pixelCenterMM(idx, cam)
			if pointNearSegment(x, y, seg) {
				rt.polyMask[idx] = true
				touched = true
			}
		}
	}
	bounceRadius := radius * 3 / 2
	if bounceRadius < 2 {
		bounceRadius = 2
	}
	for _, b := range result.Bounces {
		centerX := b.Point.X / PositionScale
		centerY := b.Point.Y / PositionScale
		for idx := range rt.bounceMask {
			x, y := rt.pixelCenterMM(idx, cam)
			dx := x - centerX
			dy := y - centerY
			if dx*dx+dy*dy <= bounceRadius*bounceRadius {
				rt.bounceMask[idx] = true
				touched = true
			}
		}
	}
	if touched {
		rt.requestIDs[result.RequestID] = true
		rt.geomRecords = append(rt.geomRecords, "path request="+result.RequestID)
	}
}

func pointNearSegment(px, py int64, s segmentMM) bool {
	vx := s.bx - s.ax
	vy := s.by - s.ay
	wx := px - s.ax
	wy := py - s.ay
	lengthSq := vx*vx + vy*vy
	if lengthSq == 0 {
		dx := px - s.ax
		dy := py - s.ay
		return dx*dx+dy*dy <= s.radius*s.radius
	}
	dot := wx*vx + wy*vy
	if dot <= 0 {
		return wx*wx+wy*wy <= s.radius*s.radius
	}
	if dot >= lengthSq {
		dx := px - s.bx
		dy := py - s.by
		return dx*dx+dy*dy <= s.radius*s.radius
	}
	cross := vx*wy - vy*wx
	r2 := s.radius * s.radius
	return cross*cross <= r2*lengthSq
}

func (rt *rasterTile) buildTile(seq int, cam Camera) *Tile {
	n := cam.TilePixels * cam.TilePixels
	obsCount := 0
	polyCount := 0
	allCount := 0
	for i := 0; i < n; i++ {
		if rt.obsMask[i] {
			obsCount++
		}
		if rt.polyMask[i] {
			polyCount++
		}
		if rt.obsMask[i] || rt.polyMask[i] || rt.bounceMask[i] {
			allCount++
		}
	}
	obsIDs := sortedStringSet(rt.obstacleIDs)
	reqIDs := sortedStringSet(rt.requestIDs)

	geometryHash := hashGeometry(rt)
	contentHash := hashContent(rt, geometryHash, obsIDs)
	color, depth := renderBuffers(rt, cam.TilePixels)
	t := &Tile{
		ID: rt.id, Seq: seq, ContentHash: contentHash, GeometryHash: geometryHash,
		ColorDigest: digestBytes(color), DepthDigest: digestDepth(depth),
		ObstacleCoverage: obsCount, PolyCoverage: polyCount, CoveragePixels: allCount,
		CoveragePermille: allCount * 1000 / n, PolyCoverageRatio: float64(polyCount) / float64(n),
		ObstacleIDs: obsIDs, RequestIDs: reqIDs, Color: color, Depth: depth,
	}
	t.CacheKey = CacheKeyForTile(t.ContentHash, CurrentVersions, cam.CameraVersion)
	return t
}

func sortedStringSet(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func maskBytes(mask []bool) []byte {
	out := make([]byte, (len(mask)+7)/8)
	for i, v := range mask {
		if v {
			out[i/8] |= 1 << uint(i%8)
		}
	}
	return out
}

func hashGeometry(rt *rasterTile) string {
	h := sha256.New()
	fmt.Fprintf(h, "geometry-algorithm=%s\n", AlgorithmVersion)
	fmt.Fprintf(h, "tile=%d,%d\n", rt.id.Col, rt.id.Row)
	fmt.Fprintf(h, "obstacle-mask=%x\n", maskBytes(rt.obsMask))
	fmt.Fprintf(h, "poly-mask=%x\n", maskBytes(rt.polyMask))
	fmt.Fprintf(h, "bounce-mask=%x\n", maskBytes(rt.bounceMask))
	records := append([]string(nil), rt.geomRecords...)
	sort.Strings(records)
	for _, r := range records {
		fmt.Fprintf(h, "record=%s\n", r)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func hashContent(rt *rasterTile, geometryHash string, obstacleIDs []string) string {
	h := sha256.New()
	fmt.Fprintf(h, "content-v1 tile=%d,%d\n", rt.id.Col, rt.id.Row)
	fmt.Fprintf(h, "geometry=%s\n", geometryHash)
	// Material versions are intentionally explicit: a material bump changes
	// content even if projected coverage is unchanged.
	for _, id := range obstacleIDs {
		fmt.Fprintf(h, "material=%s:%d\n", id, rt.obstacleMaterial[id])
	}
	return hex.EncodeToString(h.Sum(nil))
}

func renderBuffers(rt *rasterTile, pixels int) ([]byte, []uint32) {
	n := pixels * pixels
	color := make([]byte, n*4)
	depth := make([]uint32, n)
	for y := 0; y < pixels; y++ {
		for x := 0; x < pixels; x++ {
			i := y*pixels + x
			r, g, b := byte(244), byte(247), byte(250)
			if (x/4+y/4)%2 == 0 {
				r, g, b = 236, 241, 246
			}
			if x == 0 || y == 0 || x == pixels-1 || y == pixels-1 {
				r, g, b = 203, 213, 225
			}
			d := uint32(1000)
			if seq := rt.obstacleSeq[i]; seq > 0 {
				switch rt.obstacleMat[i] {
				case 4:
					r, g, b = 234, 88, 12
				default:
					r, g, b = 37, 99, 235
				}
				d = uint32(100 + seq)
			}
			if rt.polyMask[i] {
				r, g, b = 22, 163, 74
				d = 50
			}
			if rt.bounceMask[i] {
				r, g, b = 220, 38, 38
				d = 10
			}
			color[i*4] = r
			color[i*4+1] = g
			color[i*4+2] = b
			color[i*4+3] = 255
			depth[i] = d
		}
	}
	return color, depth
}

func digestBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func digestDepth(depth []uint32) string {
	b := make([]byte, len(depth)*4)
	for i, d := range depth {
		binary.LittleEndian.PutUint32(b[i*4:], d)
	}
	return digestBytes(b)
}

func DepthBytes(depth []uint32) []byte {
	b := make([]byte, len(depth)*4)
	for i, d := range depth {
		binary.LittleEndian.PutUint32(b[i*4:], d)
	}
	return b
}

func ParseDepthBytes(b []byte) []uint32 {
	out := make([]uint32, len(b)/4)
	for i := range out {
		out[i] = binary.LittleEndian.Uint32(b[i*4:])
	}
	return out
}

// ValidateCoverage enforces the declared per-tile coverage ceiling.
func (s *SceneTiles) ValidateCoverage(ceilingPermille int) error {
	if ceilingPermille <= 0 {
		ceilingPermille = 1000
	}
	for _, t := range s.Tiles {
		if t.CoveragePermille > ceilingPermille {
			ge := &Error{
				Code: CodeCover, Tile: &t.ID, Field: "coveragePermille",
				Measured: fmt.Sprintf("%d", t.CoveragePermille), Expected: fmt.Sprintf("<=%d", ceilingPermille),
				Message: "tile coverage exceeds declared ceiling",
			}
			return ge
		}
	}
	return nil
}
