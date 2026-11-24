package render

import (
	"context"
	"image"
	"math"
	"time"

	"cadlab/internal/geom"
	"cadlab/internal/model"
)

type RenderOptions struct {
	Context  context.Context
	FailTile int
}

var lightDir = model.Vec3{X: -0.48, Y: 0.76, Z: 0.44}.Normalize()

func RenderTile(scene *Scene, tileID int, opts RenderOptions) (TileResult, error) {
	if scene == nil {
		return TileResult{}, model.Diag(model.CodeStage, "nil render scene")
	}
	if opts.Context != nil {
		select {
		case <-opts.Context.Done():
			return TileResult{}, opts.Context.Err()
		default:
		}
	}
	if opts.FailTile >= 0 && opts.FailTile == tileID {
		return TileResult{}, model.DiagTile(model.CodeStage, -1, tileID, "tileRaster", "injected single-tile stage failure")
	}
	x0, y0, x1, y1 := scene.Tiles.Rect(tileID)
	w, h := x1-x0, y1-y0
	t := TileResult{
		ID: tileID, X: x0, Y: y0, W: w, H: h,
		Color: make([]uint8, w*h*4), Depth: make([]float32, w*h),
		Version: scene.Version, Path: scenePath(scene),
	}
	for i := range t.Depth {
		t.Depth[i] = float32(math.Inf(1))
	}
	start := time.Now()
	bg := backgroundFor(scene, x0, y0, w, h)
	copy(t.Color, bg)
	vb := newViewBasis(scene.Camera)
	if scene.LOD <= 12 {
		renderMeshes(scene, vb, &t, true)
	} else {
		renderMeshes(scene, vb, &t, false)
	}
	t.Duration = time.Since(start)
	t.Stats = statsFor(t.Color, t.Depth, w, h)
	if opts.Context != nil {
		select {
		case <-opts.Context.Done():
			return TileResult{}, opts.Context.Err()
		default:
		}
	}
	return t, nil
}

func scenePath(_ *Scene) string {
	return PathExact
}

type rasterTri struct {
	p [3]model.Vec3
	n [3]model.Vec3
}

type drawItem struct {
	mesh     *geom.SurfaceMesh
	subdiv   *model.SubdivMesh
	material model.Material
	patchID  int
	flat     bool
}

func renderMeshes(scene *Scene, vb viewBasis, tile *TileResult, preview bool) {
	for i := range scene.Meshes {
		item := drawItem{mesh: nil, subdiv: &scene.Meshes[i].Mesh, material: scene.Meshes[i].Material, patchID: -1, flat: preview}
		renderItem(vb, scene.Clip, tile, item)
	}
	for i := range scene.Patches {
		item := drawItem{mesh: scene.Patches[i].Mesh, material: scene.Patches[i].Material, patchID: scene.Patches[i].PatchID, flat: preview}
		renderItem(vb, scene.Clip, tile, item)
	}
}

func renderItem(vb viewBasis, clip model.ClipPlane, tile *TileResult, item drawItem) {
	if item.mesh != nil {
		for i := 0; i+2 < len(item.mesh.Indices); i += 3 {
			a := item.mesh.Vertices[item.mesh.Indices[i]]
			b := item.mesh.Vertices[item.mesh.Indices[i+1]]
			c := item.mesh.Vertices[item.mesh.Indices[i+2]]
			tri := rasterTri{
				p: [3]model.Vec3{a.Position, b.Position, c.Position},
				n: [3]model.Vec3{a.Normal, b.Normal, c.Normal},
			}
			rasterizeClipped(vb, clip, tile, tri, item.material, item.flat)
		}
		return
	}
	if item.subdiv != nil {
		m := item.subdiv
		for i := 0; i+2 < len(m.Indices); i += 3 {
			ia, ib, ic := m.Indices[i], m.Indices[i+1], m.Indices[i+2]
			na, nb, nc := faceNormal(m.Vertices[ia], m.Vertices[ib], m.Vertices[ic]),
				faceNormal(m.Vertices[ia], m.Vertices[ib], m.Vertices[ic]),
				faceNormal(m.Vertices[ia], m.Vertices[ib], m.Vertices[ic])
			if len(m.Normals) == len(m.Vertices) {
				na, nb, nc = m.Normals[ia], m.Normals[ib], m.Normals[ic]
			}
			tri := rasterTri{
				p: [3]model.Vec3{m.Vertices[ia], m.Vertices[ib], m.Vertices[ic]},
				n: [3]model.Vec3{na, nb, nc},
			}
			rasterizeClipped(vb, clip, tile, tri, item.material, item.flat)
		}
	}
}

func faceNormal(a, b, c model.Vec3) model.Vec3 {
	return b.Sub(a).Cross(c.Sub(a)).Normalize()
}

func rasterizeClipped(vb viewBasis, clip model.ClipPlane, tile *TileResult, tri rasterTri, mat model.Material, preview bool) {
	polys := clipTriangle(tri, clip)
	for _, poly := range polys {
		if len(poly) < 3 {
			continue
		}
		for i := 1; i+1 < len(poly); i++ {
			var projected [3]screenVertex
			ok := true
			for j, src := range []int{0, i, i + 1} {
				p, valid := vb.project(poly[src].p, poly[src].n)
				if !valid {
					ok = false
					break
				}
				projected[j] = p
			}
			if ok {
				rasterizeProjected(tile, projected, mat, preview)
			}
		}
	}
}

type clipVertex struct {
	p model.Vec3
	n model.Vec3
}

func clipTriangle(tri rasterTri, plane model.ClipPlane) [][]clipVertex {
	input := []clipVertex{{tri.p[0], tri.n[0]}, {tri.p[1], tri.n[1]}, {tri.p[2], tri.n[2]}}
	out := make([]clipVertex, 0, 4)
	for i := 0; i < len(input); i++ {
		a, b := input[i], input[(i+1)%len(input)]
		da, db := plane.Distance(a.p), plane.Distance(b.p)
		ain, bin := da >= 0, db >= 0
		if ain {
			out = append(out, a)
		}
		if ain != bin {
			t := da / (da - db)
			out = append(out, clipVertex{
				p: a.p.Lerp(b.p, t),
				n: a.n.Lerp(b.n, t).Normalize(),
			})
		}
	}
	if len(out) < 3 {
		return nil
	}
	return [][]clipVertex{out}
}

func rasterizeProjected(tile *TileResult, p [3]screenVertex, mat model.Material, preview bool) {
	area := edge(p[0].x, p[0].y, p[1].x, p[1].y, p[2].x, p[2].y)
	if math.Abs(area) < 1e-10 {
		return
	}
	minX := int(math.Floor(math.Min(p[0].x, math.Min(p[1].x, p[2].x))))
	maxX := int(math.Ceil(math.Max(p[0].x, math.Max(p[1].x, p[2].x))))
	minY := int(math.Floor(math.Min(p[0].y, math.Min(p[1].y, p[2].y))))
	maxY := int(math.Ceil(math.Max(p[0].y, math.Max(p[1].y, p[2].y))))
	minX = maxInt(minX, tile.X)
	maxX = minInt(maxX, tile.X+tile.W)
	minY = maxInt(minY, tile.Y)
	maxY = minInt(maxY, tile.Y+tile.H)
	for y := minY; y < maxY; y++ {
		for x := minX; x < maxX; x++ {
			px, py := float64(x)+0.5, float64(y)+0.5
			w0 := edge(p[1].x, p[1].y, p[2].x, p[2].y, px, py) / area
			w1 := edge(p[2].x, p[2].y, p[0].x, p[0].y, px, py) / area
			w2 := 1 - w0 - w1
			if w0 < -1e-9 || w1 < -1e-9 || w2 < -1e-9 {
				continue
			}
			depth := p[0].z*w0 + p[1].z*w1 + p[2].z*w2
			lx, ly := x-tile.X, y-tile.Y
			idx := ly*tile.W + lx
			if depth >= float64(tile.Depth[idx]) || depth < 0 || depth > 1 {
				continue
			}
			normal := p[0].normal.Mul(w0).Add(p[1].normal.Mul(w1)).Add(p[2].normal.Mul(w2)).Normalize()
			if preview {
				normal = p[0].normal.Add(p[1].normal).Add(p[2].normal).Normalize()
			}
			if normal.Dot(model.Vec3{X: 0, Y: 1, Z: 0}) < 0 {
				normal = normal.Mul(-1)
			}
			shade := shadePixel(normal, mat, depth, preview)
			tile.Depth[idx] = float32(depth)
			o := idx * 4
			tile.Color[o] = shade.R
			tile.Color[o+1] = shade.G
			tile.Color[o+2] = shade.B
			tile.Color[o+3] = 255
		}
	}
}

func edge(ax, ay, bx, by, px, py float64) float64 {
	return (bx-ax)*(py-ay) - (by-ay)*(px-ax)
}

func shadePixel(n model.Vec3, mat model.Material, depth float64, preview bool) model.Color {
	ndl := math.Max(0, n.Dot(lightDir))
	ambient := mat.Ambient
	if ambient <= 0 {
		ambient = 0.22
	}
	diffuse := mat.Diffuse
	if diffuse <= 0 {
		diffuse = 0.74
	}
	value := ambient + diffuse*ndl
	if !preview {
		view := model.Vec3{X: 0, Y: 0, Z: 1}
		half := view.Add(lightDir).Normalize()
		spec := math.Pow(math.Max(0, n.Dot(half)), math.Max(1, mat.Shininess))
		value += mat.Specular * spec
		fresnel := math.Pow(1-math.Max(0, n.Dot(view)), 4) * 0.16
		value += fresnel
	}
	value *= 0.84 + 0.16*(1-depth)
	value = math.Max(0, math.Min(1.35, value))
	r := clampByte(float64(mat.BaseColor.R) * value)
	g := clampByte(float64(mat.BaseColor.G) * value)
	b := clampByte(float64(mat.BaseColor.B) * value)
	if preview {
		r = clampByte((float64(r) + 14) * 1.01)
		g = clampByte((float64(g) + 14) * 1.01)
		b = clampByte((float64(b) + 14) * 1.01)
	}
	return model.Color{R: r, G: g, B: b}
}

func clampByte(v float64) uint8 {
	if v <= 0 {
		return 0
	}
	if v >= 255 {
		return 255
	}
	return uint8(math.Round(v))
}

func backgroundFor(scene *Scene, x0, y0, w, h int) []uint8 {
	pix := make([]uint8, w*h*4)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			gx, gy := x0+x, y0+y
			base := 13.0 + 11.0*float64(gy)/math.Max(1, float64(scene.Camera.Height))
			grid := 0.0
			if gx%48 == 0 || gy%48 == 0 {
				grid = 4
			}
			o := (y*w + x) * 4
			pix[o] = uint8(math.Min(255, base+grid+3))
			pix[o+1] = uint8(math.Min(255, base+grid+8))
			pix[o+2] = uint8(math.Min(255, base+grid+12))
			pix[o+3] = 255
		}
	}
	return pix
}

func statsFor(colorBuf []uint8, depth []float32, w, h int) TileStats {
	st := TileStats{MinDepth: float32(math.Inf(1)), MaxDepth: 0}
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			idx := y*w + x
			d := depth[idx]
			if math.IsInf(float64(d), 1) {
				continue
			}
			st.SurfacePixels++
			if d < st.MinDepth {
				st.MinDepth = d
			}
			if d > st.MaxDepth {
				st.MaxDepth = d
			}
			o := idx * 4
			lum := 0.2126*float64(colorBuf[o]) + 0.7152*float64(colorBuf[o+1]) + 0.0722*float64(colorBuf[o+2])
			bin := int(lum / 16)
			if bin < 0 {
				bin = 0
			}
			if bin > 15 {
				bin = 15
			}
			st.LuminanceBins[bin]++
		}
	}
	st.Coverage = float64(st.SurfacePixels) / float64(w*h)
	if st.SurfacePixels == 0 {
		st.MinDepth = 0
	}
	return st
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func newBlankImage(w, h int) *image.NRGBA { return image.NewNRGBA(image.Rect(0, 0, w, h)) }
