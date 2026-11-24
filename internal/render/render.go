package render

import (
	"bytes"
	"encoding/base64"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"math"
	"sort"
	"time"

	"strata/internal/model"
)

type Camera struct {
	Yaw      float64 `json:"yaw"`
	Pitch    float64 `json:"pitch"`
	Distance float64 `json:"distance"`
	TargetX  float64 `json:"targetX"`
	TargetY  float64 `json:"targetY"`
	TargetZ  float64 `json:"targetZ"`
	FOV      float64 `json:"fov"`
	Width    int     `json:"width"`
	Height   int     `json:"height"`
}

func DefaultCamera() Camera {
	return Camera{Yaw: -1.4, Pitch: 0.8, Distance: 10, TargetX: 0, TargetY: 0, TargetZ: -0.3, FOV: 37, Width: 720, Height: 480}
}

func (c Camera) Validate() error {
	if math.IsNaN(c.Yaw) || c.Pitch < -1.45 || c.Pitch > 1.45 {
		return model.NewError(model.EPARA, "camera pitch must be in [-1.45, 1.45]", "camera.pitch", 0, map[string]any{"value": c.Pitch})
	}
	if c.Distance < 0.2 || c.Distance > 100 {
		return model.NewError(model.EPARA, "camera distance must be in [0.2, 100]", "camera.distance", 0, map[string]any{"value": c.Distance})
	}
	if c.FOV < 10 || c.FOV > 110 {
		return model.NewError(model.EPARA, "camera fov must be in [10, 110]", "camera.fov", 0, map[string]any{"value": c.FOV})
	}
	if c.Width < 240 || c.Width > 1600 || c.Height < 180 || c.Height > 1200 {
		return model.NewError(model.EPARA, "camera viewport must be within 240x180 and 1600x1200", "camera.viewport", 0, nil)
	}
	return nil
}

type ClipPlane struct {
	Enabled   bool    `json:"enabled"`
	Axis      string  `json:"axis"`
	Value     float64 `json:"value"`
	KeepAbove bool    `json:"keepAbove"`
}

func (c ClipPlane) Validate() error {
	if !c.Enabled {
		return nil
	}
	switch c.Axis {
	case "x", "y", "z":
	default:
		return model.NewError(model.EPARA, "clip axis must be x, y or z", "clip.axis", 0, map[string]any{"value": c.Axis})
	}
	if math.Abs(c.Value) > 1000 {
		return model.NewError(model.EPARA, "clip value is outside the scene range", "clip.value", 0, map[string]any{"value": c.Value})
	}
	return nil
}

type RenderOptions struct {
	Mode       string
	SampleRate float64
	FlatShade  bool
	FrameID    string
	Version    int
}

type ProjectedBounds struct {
	MinX float64 `json:"minX"`
	MinY float64 `json:"minY"`
	MaxX float64 `json:"maxX"`
	MaxY float64 `json:"maxY"`
}

type Frame struct {
	ID              string          `json:"id"`
	Version         int             `json:"version"`
	Mode            string          `json:"mode"`
	AlgorithmMode   string          `json:"algorithmMode"`
	RenderMode      string          `json:"renderMode"`
	SampleRate      float64         `json:"sampleRate"`
	Width           int             `json:"width"`
	Height          int             `json:"height"`
	Camera          Camera          `json:"camera"`
	Clip            ClipPlane       `json:"clip"`
	ChunkBounds     []model.Bounds  `json:"chunkBounds"`
	Coverage        float64         `json:"coverage"`
	BBoxProjection  ProjectedBounds `json:"bboxProjection"`
	NormalHistogram []float64       `json:"normalHistogram"`
	LatencyMs       float64         `json:"latencyMs"`
	WithinBudget    bool            `json:"withinBudget"`
	Message         string          `json:"message,omitempty"`
	ImageBase64     string          `json:"imageBase64,omitempty"`
	Mask            []byte          `json:"-"`
	Labels          []int32         `json:"-"`
	ChunkIDs        []string        `json:"-"`
	RGBA            *image.RGBA     `json:"-"`
}

type PixelMismatch struct {
	X       int    `json:"x"`
	Y       int    `json:"y"`
	ChunkID string `json:"chunkId"`
}

type Tolerances struct {
	Coverage float64 `json:"coverage"`
	BBox     float64 `json:"bbox"`
	Normal   float64 `json:"normal"`
}

func DefaultTolerances() Tolerances {
	return Tolerances{Coverage: 0.02, BBox: 0.01, Normal: 0.18}
}

type Comparison struct {
	Passed              bool           `json:"passed"`
	CoverageDeviation   float64        `json:"coverageDeviation"`
	BBoxProjectionDelta float64        `json:"bboxProjectionDelta"`
	NormalHistogramL1   float64        `json:"normalHistogramL1"`
	Tolerances          Tolerances     `json:"tolerances"`
	PreviewVersion      int            `json:"previewVersion"`
	ExactVersion        int            `json:"exactVersion"`
	PreviewFrameID      string         `json:"previewFrameId"`
	ExactFrameID        string         `json:"exactFrameId"`
	FirstMismatch       *PixelMismatch `json:"firstMismatch,omitempty"`
}

func Render(mesh *model.Mesh, camera Camera, clip ClipPlane, opts RenderOptions) (*Frame, error) {
	if mesh == nil {
		return nil, model.NewError(model.ESTATE, "no mesh is available for rendering", "mesh", 0, nil)
	}
	if err := camera.Validate(); err != nil {
		return nil, err
	}
	if err := clip.Validate(); err != nil {
		return nil, err
	}
	start := time.Now()
	scene := newScene(camera)
	w, h := camera.Width, camera.Height
	mask := make([]byte, w*h)
	labels := make([]int32, w*h)
	rgba := image.NewRGBA(image.Rect(0, 0, w, h))
	zbuf := make([]float64, w*h)
	for i := range zbuf {
		zbuf[i] = math.Inf(1)
	}
	chunkIDs := make([]string, len(mesh.Chunks)+1)
	for i, chunk := range mesh.Chunks {
		chunkIDs[i+1] = chunk.ID
	}
	covered := 0
	for _, face := range mesh.Faces {
		a, b, c := mesh.Vertices[face.A], mesh.Vertices[face.B], mesh.Vertices[face.C]
		tri := []rasterVertex{
			{Position: a.Position, Normal: a.Normal, Color: a.Color, Intensity: a.Intensity},
			{Position: b.Position, Normal: b.Normal, Color: b.Color, Intensity: b.Intensity},
			{Position: c.Position, Normal: c.Normal, Color: c.Color, Intensity: c.Intensity},
		}
		front := a.Position.Sub(b.Position).Cross(c.Position.Sub(b.Position))
		if front.Dot(scene.view) > 0 {
			// Keep both winding directions drawable; exact and preview share it.
		}
		poly := clipPolygon(tri, clip)
		if len(poly) < 3 {
			continue
		}
		labelIndex := int32(0)
		for i := 1; i < len(chunkIDs); i++ {
			if chunkIDs[i] == face.Chunk {
				labelIndex = int32(i)
				break
			}
		}
		for i := 1; i < len(poly)-1; i++ {
			if rasterizeTriangle(scene, []rasterVertex{poly[0], poly[i], poly[i+1]}, camera, clip, opts, mask, labels, labelIndex, rgba, zbuf) {
				// Count unique covered pixels after the face pass below.
			}
		}
	}
	for _, v := range mask {
		if v != 0 {
			covered++
		}
	}
	bbox := projectedBBox(mesh.Bounds, scene, camera)
	frame := &Frame{
		ID: opts.FrameID, Version: opts.Version, Mode: opts.Mode,
		AlgorithmMode: opts.Mode, RenderMode: renderMode(opts), SampleRate: opts.SampleRate,
		Width: w, Height: h, Camera: camera, Clip: clip,
		ChunkBounds: chunkBounds(mesh), Coverage: float64(covered) / float64(w*h),
		BBoxProjection: bbox, NormalHistogram: NormalHistogram(mesh),
		Mask: mask, Labels: labels, ChunkIDs: chunkIDs, RGBA: rgba,
	}
	frame.LatencyMs = float64(time.Since(start).Microseconds()) / 1000
	frame.WithinBudget = !opts.FlatShade || frame.LatencyMs < 50
	var pngBuf bytes.Buffer
	if err := png.Encode(&pngBuf, rgba); err == nil {
		frame.ImageBase64 = base64.StdEncoding.EncodeToString(pngBuf.Bytes())
	}
	return frame, nil
}

type rasterVertex struct {
	Position  model.Vec3
	Normal    model.Vec3
	Color     [3]byte
	Intensity float64
}

func renderMode(opts RenderOptions) string {
	if opts.FlatShade {
		return "flat-normal+reduced-shading"
	}
	return "smooth-normal+pbr"
}

type sceneCamera struct {
	camera model.Vec3
	view   model.Vec3
	right  model.Vec3
	up     model.Vec3
	focal  float64
	w, h   int
}

func newScene(c Camera) sceneCamera {
	cp := math.Cos(c.Pitch)
	dir := model.Vec3{X: cp * math.Cos(c.Yaw), Y: cp * math.Sin(c.Yaw), Z: math.Sin(c.Pitch)}
	target := model.Vec3{X: c.TargetX, Y: c.TargetY, Z: c.TargetZ}
	camera := target.Add(dir.Mul(c.Distance))
	view := target.Sub(camera).Normalize()
	right := view.Cross(model.Vec3{Z: 1}).Normalize()
	up := right.Cross(view).Normalize()
	focal := 0.5 * float64(c.Height) / math.Tan(c.FOV*math.Pi/360)
	return sceneCamera{camera: camera, view: view, right: right, up: up, focal: focal, w: c.Width, h: c.Height}
}

func project(scene sceneCamera, p model.Vec3) (float64, float64, float64, bool) {
	rel := p.Sub(scene.camera)
	z := rel.Dot(scene.view)
	if z <= 0.01 {
		return 0, 0, z, false
	}
	x := rel.Dot(scene.right)
	y := rel.Dot(scene.up)
	return float64(scene.w)*0.5 + scene.focal*x/z, float64(scene.h)*0.5 - scene.focal*y/z, z, true
}

func clipPolygon(poly []rasterVertex, clip ClipPlane) []rasterVertex {
	if len(poly) == 0 {
		return nil
	}
	// Near-plane clipping prevents extreme projections.
	poly = clipAgainst(poly, func(v rasterVertex) float64 {
		return 0.02
	}, func(v rasterVertex) float64 {
		return v.Position.Z
	}, true)
	if !clip.Enabled || len(poly) == 0 {
		return poly
	}
	sign := 1.0
	if !clip.KeepAbove {
		sign = -1
	}
	axis := func(v rasterVertex) float64 {
		switch clip.Axis {
		case "x":
			return v.Position.X
		case "y":
			return v.Position.Y
		default:
			return v.Position.Z
		}
	}
	return clipAgainst(poly, func(v rasterVertex) float64 { return clip.Value }, axis, sign > 0)
}

func clipAgainst(poly []rasterVertex, planeAt func(rasterVertex) float64, value func(rasterVertex) float64, keepPositive bool) []rasterVertex {
	if len(poly) == 0 {
		return nil
	}
	inside := func(v rasterVertex) bool {
		d := value(v) - planeAt(v)
		if keepPositive {
			return d >= 0
		}
		return d <= 0
	}
	out := make([]rasterVertex, 0, len(poly)+2)
	prev := poly[len(poly)-1]
	prevIn := inside(prev)
	for _, cur := range poly {
		curIn := inside(cur)
		if curIn != prevIn {
			d0 := value(prev) - planeAt(prev)
			d1 := value(cur) - planeAt(cur)
			denom := d0 - d1
			t := 0.5
			if math.Abs(denom) > 1e-12 {
				t = d0 / denom
			}
			out = append(out, lerpVertex(prev, cur, t))
		}
		if curIn {
			out = append(out, cur)
		}
		prev, prevIn = cur, curIn
	}
	return out
}

func lerpVertex(a, b rasterVertex, t float64) rasterVertex {
	return rasterVertex{
		Position: a.Position.Add(b.Position.Sub(a.Position).Mul(t)),
		Normal:   a.Normal.Add(b.Normal.Sub(a.Normal).Mul(t)).Normalize(),
		Color: [3]byte{
			uint8(float64(a.Color[0]) + (float64(b.Color[0])-float64(a.Color[0]))*t),
			uint8(float64(a.Color[1]) + (float64(b.Color[1])-float64(a.Color[1]))*t),
			uint8(float64(a.Color[2]) + (float64(b.Color[2])-float64(a.Color[2]))*t),
		},
		Intensity: a.Intensity + (b.Intensity-a.Intensity)*t,
	}
}

func rasterizeTriangle(scene sceneCamera, tri []rasterVertex, camera Camera, clip ClipPlane, opts RenderOptions, mask []byte, labels []int32, label int32, rgba *image.RGBA, zbuf []float64) bool {
	type screenVertex struct {
		x, y, z float64
		v       rasterVertex
	}
	sv := make([]screenVertex, len(tri))
	for i, v := range tri {
		x, y, z, ok := project(scene, v.Position)
		if !ok {
			return false
		}
		sv[i] = screenVertex{x: x, y: y, z: z, v: v}
	}
	area := (sv[1].x-sv[0].x)*(sv[2].y-sv[0].y) - (sv[1].y-sv[0].y)*(sv[2].x-sv[0].x)
	if math.Abs(area) < 1e-9 {
		return false
	}
	minX := maxInt(0, int(math.Floor(math.Min(sv[0].x, math.Min(sv[1].x, sv[2].x)))))
	maxX := minInt(camera.Width-1, int(math.Ceil(math.Max(sv[0].x, math.Max(sv[1].x, sv[2].x)))))
	minY := maxInt(0, int(math.Floor(math.Min(sv[0].y, math.Min(sv[1].y, sv[2].y)))))
	maxY := minInt(camera.Height-1, int(math.Ceil(math.Max(sv[0].y, math.Max(sv[1].y, sv[2].y)))))
	if minX > maxX || minY > maxY {
		return false
	}
	faceNormal := tri[0].Position.Sub(tri[1].Position).Cross(tri[2].Position.Sub(tri[1].Position)).Normalize()
	faceColor := [3]float64{
		(float64(tri[0].Color[0]) + float64(tri[1].Color[0]) + float64(tri[2].Color[0])) / 3,
		(float64(tri[0].Color[1]) + float64(tri[1].Color[1]) + float64(tri[2].Color[1])) / 3,
		(float64(tri[0].Color[2]) + float64(tri[1].Color[2]) + float64(tri[2].Color[2])) / 3,
	}
	light := model.Vec3{X: -0.4, Y: -0.55, Z: 0.74}.Normalize()
	invArea := 1 / area
	for py := minY; py <= maxY; py++ {
		for px := minX; px <= maxX; px++ {
			cx := float64(px) + 0.5
			cy := float64(py) + 0.5
			w0 := ((sv[1].x-cx)*(sv[2].y-cy) - (sv[1].y-cy)*(sv[2].x-cx)) * invArea
			w1 := ((sv[2].x-cx)*(sv[0].y-cy) - (sv[2].y-cy)*(sv[0].x-cx)) * invArea
			w2 := 1 - w0 - w1
			if w0 < -1e-8 || w1 < -1e-8 || w2 < -1e-8 {
				continue
			}
			z := w0*sv[0].z + w1*sv[1].z + w2*sv[2].z
			idx := py*camera.Width + px
			if z >= zbuf[idx] {
				continue
			}
			zbuf[idx] = z
			mask[idx] = 1
			labels[idx] = label
			var r, g, b, shade float64
			if opts.FlatShade {
				shade = 0.38 + 0.62*math.Abs(faceNormal.Dot(light))
				r, g, b = faceColor[0], faceColor[1], faceColor[2]
			} else {
				n := tri[0].Normal.Mul(w0).Add(tri[1].Normal.Mul(w1)).Add(tri[2].Normal.Mul(w2)).Normalize()
				shade = 0.29 + 0.71*math.Abs(n.Dot(light))
				r = float64(tri[0].Color[0])*w0 + float64(tri[1].Color[0])*w1 + float64(tri[2].Color[0])*w2
				g = float64(tri[0].Color[1])*w0 + float64(tri[1].Color[1])*w1 + float64(tri[2].Color[1])*w2
				b = float64(tri[0].Color[2])*w0 + float64(tri[1].Color[2])*w1 + float64(tri[2].Color[2])*w2
			}
			rgba.SetRGBA(px, py, color.RGBA{
				R: uint8(clamp(r*shade, 0, 255)),
				G: uint8(clamp(g*shade, 0, 255)),
				B: uint8(clamp(b*shade, 0, 255)),
				A: 255,
			})
		}
	}
	return true
}

func Compare(preview, exact *Frame) (*Comparison, error) {
	if preview == nil || exact == nil {
		return nil, model.NewError(model.ESTATE, "both preview and exact frames are required", "comparison.frames", 0, nil)
	}
	if preview.Width != exact.Width || preview.Height != exact.Height {
		return nil, model.NewError(model.ESTATE, "preview and exact viewports differ", "comparison.viewport", 0, nil)
	}
	tol := DefaultTolerances()
	cmp := &Comparison{
		PreviewVersion: preview.Version, ExactVersion: exact.Version,
		PreviewFrameID: preview.ID, ExactFrameID: exact.ID, Tolerances: tol,
	}
	different := 0
	union := 0
	var first *PixelMismatch
	for i := range preview.Mask {
		p, e := preview.Mask[i], exact.Mask[i]
		if p != 0 || e != 0 {
			union++
		}
		if p != e {
			different++
			if first == nil {
				x, y := i%exact.Width, i/exact.Width
				label := int32(0)
				if i < len(exact.Labels) {
					label = exact.Labels[i]
				}
				chunkID := ""
				if int(label) < len(exact.ChunkIDs) {
					chunkID = exact.ChunkIDs[label]
				}
				if chunkID == "" {
					chunkID = nearestChunk(exact, x, y)
				}
				first = &PixelMismatch{X: x, Y: y, ChunkID: chunkID}
			}
		}
	}
	if union > 0 {
		cmp.CoverageDeviation = float64(different) / float64(union)
	} else {
		cmp.CoverageDeviation = math.Abs(preview.Coverage - exact.Coverage)
	}
	cmp.BBoxProjectionDelta = bboxDelta(preview.BBoxProjection, exact.BBoxProjection)
	cmp.NormalHistogramL1 = histogramL1(preview.NormalHistogram, exact.NormalHistogram)
	cmp.Passed = cmp.CoverageDeviation <= tol.Coverage &&
		cmp.BBoxProjectionDelta <= tol.BBox &&
		cmp.NormalHistogramL1 <= tol.Normal
	if !cmp.Passed {
		cmp.FirstMismatch = first
		return cmp, model.NewError(model.EDIVERGE, "preview and exact reconstruction diverged", "comparison", 0, map[string]any{
			"coverageDeviation": cmp.CoverageDeviation, "bboxProjectionDelta": cmp.BBoxProjectionDelta,
			"normalHistogramL1": cmp.NormalHistogramL1, "firstMismatch": cmp.FirstMismatch,
			"tolerances": tol,
		})
	}
	return cmp, nil
}

func NormalHistogram(mesh *model.Mesh) []float64 {
	hist := make([]float64, 18)
	if mesh == nil {
		return hist
	}
	for _, f := range mesh.Faces {
		a, b, c := mesh.Vertices[f.A].Position, mesh.Vertices[f.B].Position, mesh.Vertices[f.C].Position
		n := b.Sub(a).Cross(c.Sub(a))
		area := n.Length()
		if area < 1e-12 {
			continue
		}
		n = n.Mul(1 / area)
		angle := math.Atan2(n.Y, n.X)
		if angle < 0 {
			angle += 2 * math.Pi
		}
		bin := int(angle/(2*math.Pi)*18) % 18
		hist[bin] += area
	}
	total := 0.0
	for _, v := range hist {
		total += v
	}
	if total > 0 {
		for i := range hist {
			hist[i] /= total
		}
	}
	return hist
}

func histogramL1(a, b []float64) float64 {
	n := min(len(a), len(b))
	total := 0.0
	for i := 0; i < n; i++ {
		total += math.Abs(a[i] - b[i])
	}
	return total
}

func bboxDelta(a, b ProjectedBounds) float64 {
	vals := []float64{
		math.Abs(a.MinX - b.MinX), math.Abs(a.MinY - b.MinY),
		math.Abs(a.MaxX - b.MaxX), math.Abs(a.MaxY - b.MaxY),
	}
	sort.Float64s(vals)
	return vals[len(vals)-1]
}

func projectedBBox(bounds model.Bounds, scene sceneCamera, camera Camera) ProjectedBounds {
	corners := []model.Vec3{
		{X: bounds.Min.X, Y: bounds.Min.Y, Z: bounds.Min.Z}, {X: bounds.Min.X, Y: bounds.Min.Y, Z: bounds.Max.Z},
		{X: bounds.Min.X, Y: bounds.Max.Y, Z: bounds.Min.Z}, {X: bounds.Min.X, Y: bounds.Max.Y, Z: bounds.Max.Z},
		{X: bounds.Max.X, Y: bounds.Min.Y, Z: bounds.Min.Z}, {X: bounds.Max.X, Y: bounds.Min.Y, Z: bounds.Max.Z},
		{X: bounds.Max.X, Y: bounds.Max.Y, Z: bounds.Min.Z}, {X: bounds.Max.X, Y: bounds.Max.Y, Z: bounds.Max.Z},
	}
	out := ProjectedBounds{MinX: math.Inf(1), MinY: math.Inf(1), MaxX: math.Inf(-1), MaxY: math.Inf(-1)}
	for _, p := range corners {
		x, y, _, ok := project(scene, p)
		if !ok {
			continue
		}
		out.MinX = math.Min(out.MinX, x/float64(camera.Width))
		out.MaxX = math.Max(out.MaxX, x/float64(camera.Width))
		out.MinY = math.Min(out.MinY, y/float64(camera.Height))
		out.MaxY = math.Max(out.MaxY, y/float64(camera.Height))
	}
	return out
}

func chunkBounds(mesh *model.Mesh) []model.Bounds {
	out := make([]model.Bounds, 0, len(mesh.Chunks))
	for _, c := range mesh.Chunks {
		out = append(out, c.Bounds)
	}
	return out
}

func nearestChunk(frame *Frame, x, y int) string {
	if len(frame.ChunkIDs) <= 1 {
		return ""
	}
	idx := maxInt(1, minInt(len(frame.ChunkIDs)-1, len(frame.ChunkIDs)-1))
	return frame.ChunkIDs[idx]
}

func clamp(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
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

func (f *Frame) String() string {
	return fmt.Sprintf("%s mode=%s coverage=%.5f latency=%.3fms", f.ID, f.Mode, f.Coverage, f.LatencyMs)
}
