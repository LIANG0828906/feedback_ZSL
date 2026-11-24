package render

import (
	"encoding/binary"
	"math"
	"sort"

	"cadpipe/internal/cad"
	"cadpipe/internal/tessellate"
)

type PatchView struct {
	Patch           *cad.Patch
	Mesh            *tessellate.Mesh
	MaterialVersion int64
}

type LightStats struct {
	Background int `json:"background"`
	Shadow     int `json:"shadow"`
	Mid        int `json:"mid"`
	Highlight  int `json:"highlight"`
}

type TileResult struct {
	TileID           int                `json:"tile_id"`
	X                int                `json:"x"`
	Y                int                `json:"y"`
	Width            int                `json:"width"`
	Height           int                `json:"height"`
	Color            []byte             `json:"-"`
	Depth            []float64          `json:"-"`
	ColorDigest      string             `json:"color_digest"`
	DepthDigest      string             `json:"depth_digest"`
	GeometryHash     string             `json:"geometry_hash"`
	ContentHash      string             `json:"content_hash"`
	CacheKey         string             `json:"cache_key"`
	Coverage         float64            `json:"coverage"`
	OutlineCoverage  float64            `json:"outline_coverage"`
	PatchCoverage    map[string]float64 `json:"patch_coverage"`
	LightStats       LightStats         `json:"light_stats"`
	SceneVersion     int64              `json:"scene_structure_version"`
	CameraVersion    int64              `json:"camera_version"`
	AlgorithmVersion string             `json:"algorithm_version"`
	DependencyHash   string             `json:"-"`
}

type TileBuffer struct {
	Color []byte
	Depth []float64
}

func NewTileBuffer(pixels int) *TileBuffer {
	color := make([]byte, pixels*4)
	for i := 0; i < pixels; i++ {
		color[i*4+0] = 18
		color[i*4+1] = 22
		color[i*4+2] = 28
		color[i*4+3] = 255
	}
	depth := make([]float64, pixels)
	for i := range depth {
		depth[i] = math.MaxFloat64
	}
	return &TileBuffer{Color: color, Depth: depth}
}

func (b *TileBuffer) Clone() *TileBuffer {
	return &TileBuffer{Color: append([]byte(nil), b.Color...), Depth: append([]float64(nil), b.Depth...)}
}

type screenTriangle struct {
	vertices [3]ProjectedVertex
}

type hashVertex struct {
	SX float64 `json:"sx"`
	SY float64 `json:"sy"`
	D  float64 `json:"d"`
	NX float64 `json:"nx"`
	NY float64 `json:"ny"`
	NZ float64 `json:"nz"`
	U  float64 `json:"u"`
	V  float64 `json:"v"`
}

type hashPatch struct {
	ID           string         `json:"id"`
	Triangles    []hashTriangle `json:"triangles"`
	ClipMaskHash string         `json:"clip_mask_hash"`
	OutlineHash  string         `json:"outline_hash"`
}

type hashTriangle struct {
	V [3]hashVertex `json:"v"`
}

type geometryHashDocument struct {
	Algorithm string      `json:"algorithm"`
	TileX     int         `json:"tile_x"`
	TileY     int         `json:"tile_y"`
	Patches   []hashPatch `json:"patches"`
}

type materialRef struct {
	ID              string `json:"id"`
	MaterialVersion int64  `json:"material_version"`
	Fingerprint     string `json:"fingerprint"`
}

type contentHashDocument struct {
	GeometryHash string        `json:"geometry_hash"`
	Materials    []materialRef `json:"materials"`
}

type cacheKeyDocument struct {
	TileID                int    `json:"tile_id"`
	ContentHash           string `json:"content_hash"`
	SceneStructureVersion int64  `json:"scene_structure_version"`
	CameraVersion         int64  `json:"camera_version"`
	AlgorithmVersion      string `json:"algorithm_version"`
}

func BuildCacheKey(tileID int, contentHash string, sceneVersion, cameraVersion int64) string {
	return cad.HashJSON(cacheKeyDocument{
		TileID: tileID, ContentHash: contentHash,
		SceneStructureVersion: sceneVersion, CameraVersion: cameraVersion,
		AlgorithmVersion: tessellate.AlgorithmVersion,
	})
}

func sortedViews(views []PatchView) []PatchView {
	out := append([]PatchView(nil), views...)
	sort.SliceStable(out, func(i, j int) bool { return out[i].Patch.ID < out[j].Patch.ID })
	return out
}

func RenderTile(cam *Camera, tileID int, views []PatchView, sceneVersion int64, buffer *TileBuffer) (*TileResult, error) {
	views = sortedViews(views)
	tx, ty := cam.TileXY(tileID)
	x0, y0, x1, y1 := cam.TileRect(tileID)
	rectW := x1 - x0
	pixelCount := cam.TileWidth * cam.TileHeight
	if buffer == nil {
		buffer = NewTileBuffer(pixelCount)
	}

	result := &TileResult{
		TileID: tileID, X: tx, Y: ty, Width: cam.TileWidth, Height: cam.TileHeight,
		Color: buffer.Color, Depth: buffer.Depth,
		PatchCoverage: make(map[string]float64),
		SceneVersion:  sceneVersion, CameraVersion: cam.Version,
		AlgorithmVersion: tessellate.AlgorithmVersion,
	}
	surfaceMask := make([]bool, pixelCount)
	outlineMask := make([]bool, pixelCount)
	geoDoc := geometryHashDocument{Algorithm: tessellate.AlgorithmVersion, TileX: tx, TileY: ty}
	contentDoc := contentHashDocument{}

	for _, view := range views {
		patch := view.Patch
		projected, triangles := projectPatch(cam, view.Mesh)
		visibleTris := trianglesForTile(triangles, projected, x0, y0, x1, y1)
		if len(visibleTris) == 0 {
			continue
		}
		patchPixels := make([]bool, pixelCount)
		patchOutline := make([]bool, pixelCount)
		clipState := make([]byte, pixelCount)
		hashPatch := hashPatch{ID: patch.ID}

		for _, triIndex := range visibleTris {
			tri := triangles[triIndex]
			hashPatch.Triangles = append(hashPatch.Triangles, hashTriangle{V: [3]hashVertex{
				hashVertexFrom(tri.vertices[0]), hashVertexFrom(tri.vertices[1]), hashVertexFrom(tri.vertices[2]),
			}})
			rasterTriangle(cam, tri, x0, y0, x1, y1, rectW, buffer, patch, patchPixels, clipState, surfaceMask)
		}
		markOutline(cam, projected, view.Mesh, x0, y0, x1, y1, rectW, patchOutline, outlineMask)

		covered := 0
		for i, hit := range patchPixels {
			if hit {
				covered++
				_ = i
			}
		}
		coverage := float64(covered) / float64(pixelCount)
		if coverage > 0 {
			result.PatchCoverage[patch.ID] = coverage
		}
		hashPatch.ClipMaskHash = cad.SHA256Hex(clipState)
		hashPatch.OutlineHash = cad.SHA256Hex(outlineMaskBytes(patchOutline))
		geoDoc.Patches = append(geoDoc.Patches, hashPatch)
		contentDoc.Materials = append(contentDoc.Materials, materialRef{
			ID: patch.ID, MaterialVersion: view.MaterialVersion,
			Fingerprint: cad.HashJSON(patch.Material),
		})
	}

	coveredAll := 0
	for _, hit := range surfaceMask {
		if hit {
			coveredAll++
		}
	}
	outlineAll := 0
	for _, hit := range outlineMask {
		if hit {
			outlineAll++
		}
	}
	result.Coverage = float64(coveredAll) / float64(pixelCount)
	result.OutlineCoverage = float64(outlineAll) / float64(pixelCount)
	result.LightStats = calculateLightStats(buffer.Color, surfaceMask, pixelCount)
	result.ColorDigest = cad.SHA256Hex(buffer.Color)
	result.DepthDigest = depthDigest(buffer.Depth)
	result.GeometryHash = cad.HashJSON(geoDoc)
	contentDoc.GeometryHash = result.GeometryHash
	result.ContentHash = cad.HashJSON(contentDoc)
	result.CacheKey = BuildCacheKey(tileID, result.ContentHash, sceneVersion, cam.Version)
	return result, nil
}

func outlineMaskBytes(mask []bool) []byte {
	out := make([]byte, len(mask))
	for i, v := range mask {
		if v {
			out[i] = 1
		}
	}
	return out
}

func hashVertexFrom(p ProjectedVertex) hashVertex {
	return hashVertex{
		SX: p.ScreenX, SY: p.ScreenY, D: p.Depth,
		NX: p.Normal.X, NY: p.Normal.Y, NZ: p.Normal.Z,
		U: p.UV.U, V: p.UV.V,
	}
}

func depthDigest(depth []float64) string {
	buf := make([]byte, 8*len(depth))
	for i, d := range depth {
		binary.BigEndian.PutUint64(buf[i*8:i*8+8], math.Float64bits(d))
	}
	return cad.SHA256Hex(buf)
}

func DepthDigest(depth []float64) string { return depthDigest(depth) }

func projectPatch(cam *Camera, mesh *tessellate.Mesh) ([]ProjectedVertex, []screenTriangle) {
	projected := make([]ProjectedVertex, len(mesh.Vertices))
	for i, v := range mesh.Vertices {
		p, ok := cam.Project(v)
		if !ok {
			p = ProjectedVertex{Depth: math.MaxFloat64}
		}
		projected[i] = p
	}
	tris := make([]screenTriangle, 0, len(mesh.Triangles))
	for _, idx := range mesh.Triangles {
		if projected[idx[0]].Depth == math.MaxFloat64 ||
			projected[idx[1]].Depth == math.MaxFloat64 ||
			projected[idx[2]].Depth == math.MaxFloat64 {
			continue
		}
		tris = append(tris, screenTriangle{vertices: [3]ProjectedVertex{
			projected[idx[0]], projected[idx[1]], projected[idx[2]],
		}})
	}
	return projected, tris
}

func trianglesForTile(tris []screenTriangle, _ []ProjectedVertex, x0, y0, x1, y1 int) []int {
	var out []int
	for i, tri := range tris {
		minX := math.Min(math.Min(tri.vertices[0].ScreenX, tri.vertices[1].ScreenX), tri.vertices[2].ScreenX)
		maxX := math.Max(math.Max(tri.vertices[0].ScreenX, tri.vertices[1].ScreenX), tri.vertices[2].ScreenX)
		minY := math.Min(math.Min(tri.vertices[0].ScreenY, tri.vertices[1].ScreenY), tri.vertices[2].ScreenY)
		maxY := math.Max(math.Max(tri.vertices[0].ScreenY, tri.vertices[1].ScreenY), tri.vertices[2].ScreenY)
		if maxX < float64(x0) || minX >= float64(x1) || maxY < float64(y0) || minY >= float64(y1) {
			continue
		}
		out = append(out, i)
	}
	return out
}

func rasterTriangle(
	cam *Camera,
	tri screenTriangle,
	x0, y0, x1, y1, stride int,
	buffer *TileBuffer,
	patch *cad.Patch,
	patchPixels []bool,
	clipState []byte,
	surfaceMask []bool,
) {
	a, b, c := tri.vertices[0], tri.vertices[1], tri.vertices[2]
	minPX := maxInt(x0, int(math.Floor(math.Min(math.Min(a.ScreenX, b.ScreenX), c.ScreenX))))
	maxPX := minInt(x1-1, int(math.Ceil(math.Max(math.Max(a.ScreenX, b.ScreenX), c.ScreenX))))
	minPY := maxInt(y0, int(math.Floor(math.Min(math.Min(a.ScreenY, b.ScreenY), c.ScreenY))))
	maxPY := minInt(y1-1, int(math.Ceil(math.Max(math.Max(a.ScreenY, b.ScreenY), c.ScreenY))))

	for py := minPY; py <= maxPY; py++ {
		for px := minPX; px <= maxPX; px++ {
			q := cad.Vec2{U: float64(px) + 0.5, V: float64(py) + 0.5}
			alpha, beta, gamma, ok := barycentric(q,
				cad.Vec2{U: a.ScreenX, V: a.ScreenY},
				cad.Vec2{U: b.ScreenX, V: b.ScreenY},
				cad.Vec2{U: c.ScreenX, V: c.ScreenY})
			if !ok {
				continue
			}
			uv := cad.Vec2{
				U: alpha*a.UV.U + beta*b.UV.U + gamma*c.UV.U,
				V: alpha*a.UV.V + beta*b.UV.V + gamma*c.UV.V,
			}
			inside := cad.PointInsideClip(patch.Clip, uv)
			localX := px - x0
			localY := py - y0
			idx := localY*stride + localX
			if idx < 0 || idx >= len(patchPixels) {
				continue
			}
			if inside {
				clipState[idx] = 1
				patchPixels[idx] = true
			} else if clipState[idx] == 0 {
				clipState[idx] = 2
			}
			if !inside {
				continue
			}
			surfaceMask[idx] = true
			depth := alpha*a.Depth + beta*b.Depth + gamma*c.Depth
			if depth >= buffer.Depth[idx] {
				continue
			}
			normal := cad.Vec3{
				X: alpha*a.Normal.X + beta*b.Normal.X + gamma*c.Normal.X,
				Y: alpha*a.Normal.Y + beta*b.Normal.Y + gamma*c.Normal.Y,
				Z: alpha*a.Normal.Z + beta*b.Normal.Z + gamma*c.Normal.Z,
			}
			color := shade(patch.Material, normal, a.World)
			buffer.Depth[idx] = depth
			o := idx * 4
			buffer.Color[o+0] = color[0]
			buffer.Color[o+1] = color[1]
			buffer.Color[o+2] = color[2]
			buffer.Color[o+3] = 255
		}
	}
}

func barycentric(p, a, b, c cad.Vec2) (float64, float64, float64, bool) {
	v0 := cad.Vec2{U: b.U - a.U, V: b.V - a.V}
	v1 := cad.Vec2{U: c.U - a.U, V: c.V - a.V}
	v2 := cad.Vec2{U: p.U - a.U, V: p.V - a.V}
	d00 := v0.U*v0.U + v0.V*v0.V
	d01 := v0.U*v1.U + v0.V*v1.V
	d11 := v1.U*v1.U + v1.V*v1.V
	d20 := v2.U*v0.U + v2.V*v0.V
	d21 := v2.U*v1.U + v2.V*v1.V
	den := d00*d11 - d01*d01
	if math.Abs(den) < 1e-18 {
		return 0, 0, 0, false
	}
	beta := (d11*d20 - d01*d21) / den
	gamma := (d00*d21 - d01*d20) / den
	alpha := 1 - beta - gamma
	const eps = -1e-9
	if alpha < eps || beta < eps || gamma < eps {
		return 0, 0, 0, false
	}
	return alpha, beta, gamma, true
}

var lightDirection = normalize3(cad.Vec3{X: -0.35, Y: 0.45, Z: 0.82})
var viewDirection = cad.Vec3{X: 0, Y: 0, Z: 1}

func shade(material *cad.Material, normal cad.Vec3, world cad.Vec3) [3]byte {
	if material == nil {
		material = &cad.Material{Color: cad.RGB{R: 0.6, G: 0.7, B: 0.85}, Roughness: 0.6}
	}
	n := normalize3(normal)
	if dot3(n, viewDirection) < 0 {
		n = scale3(n, -1)
	}
	diffuse := math.Max(dot3(n, lightDirection), 0)
	ambient := 0.24 + 0.08*material.Metallic
	intensity := ambient + 0.72*diffuse
	base := material.Color
	r := base.R*intensity + 0.05*material.Metallic
	g := base.G*intensity + 0.05*material.Metallic
	b := base.B*intensity + 0.06*material.Metallic

	half := normalize3(add3(lightDirection, viewDirection))
	specPower := 10 + 120*(1-math.Max(0, math.Min(1, material.Roughness)))
	spec := math.Pow(math.Max(dot3(n, half), 0), specPower) * (0.12 + 0.5*material.Metallic)
	r += spec
	g += spec
	b += spec
	return [3]byte{toByte(r), toByte(g), toByte(b)}
}

func toByte(v float64) byte {
	v = math.Max(0, math.Min(1, v))
	return byte(math.Round(v * 255))
}

func calculateLightStats(color []byte, mask []bool, pixelCount int) LightStats {
	stats := LightStats{Background: pixelCount}
	for i, hit := range mask {
		if !hit {
			continue
		}
		stats.Background--
		o := i * 4
		lum := (0.2126*float64(color[o]) + 0.7152*float64(color[o+1]) + 0.0722*float64(color[o+2])) / 255
		switch {
		case lum < 0.34:
			stats.Shadow++
		case lum < 0.72:
			stats.Mid++
		default:
			stats.Highlight++
		}
	}
	return stats
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
