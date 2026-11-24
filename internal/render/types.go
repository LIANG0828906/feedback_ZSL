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

	"cadlab/internal/geom"
	"cadlab/internal/model"
)

const AlgorithmVersion = "cadlab-render-cpu-v3"

const (
	PathPreview     = "preview"
	PathExact       = "exact"
	PathIncremental = "incremental"
	PathResume      = "resume"
)

type PatchDraw struct {
	PatchID  int                `json:"patchId"`
	Source   model.SurfacePatch `json:"source"`
	Mesh     *geom.SurfaceMesh  `json:"mesh"`
	Material model.Material     `json:"material"`
}

type MeshDraw struct {
	Mesh     model.SubdivMesh `json:"mesh"`
	Material model.Material   `json:"material"`
}

type Scene struct {
	Version          int64           `json:"version"`
	ContentHash      string          `json:"contentHash"`
	AlgorithmVersion string          `json:"algorithmVersion"`
	Camera           model.Camera    `json:"camera"`
	Clip             model.ClipPlane `json:"clipPlane"`
	Tiles            model.TileGrid  `json:"tiles"`
	LOD              int             `json:"lod"`
	Patches          []PatchDraw     `json:"patches"`
	Meshes           []MeshDraw      `json:"meshes"`
}

type TileStats struct {
	Coverage      float64 `json:"coverage"`
	SurfacePixels int     `json:"surfacePixels"`
	LuminanceBins [16]int `json:"luminanceBins"`
	MinDepth      float32 `json:"minDepth"`
	MaxDepth      float32 `json:"maxDepth"`
}

type TileResult struct {
	ID       int           `json:"id"`
	X        int           `json:"x"`
	Y        int           `json:"y"`
	W        int           `json:"w"`
	H        int           `json:"h"`
	Color    []uint8       `json:"-"`
	Depth    []float32     `json:"-"`
	Stats    TileStats     `json:"stats"`
	Duration time.Duration `json:"durationNs"`
	Version  int64         `json:"version"`
	Path     string        `json:"path"`
}

type Frame struct {
	Version     int64           `json:"version"`
	Path        string          `json:"path"`
	LOD         int             `json:"lod"`
	Width       int             `json:"width"`
	Height      int             `json:"height"`
	Grid        model.TileGrid  `json:"grid"`
	Image       *image.NRGBA    `json:"-"`
	Depth       []float32       `json:"-"`
	Stats       []TileStats     `json:"tileStats"`
	Tiles       []TileResult    `json:"tiles"`
	Duration    time.Duration   `json:"durationNs"`
	CacheHits   int             `json:"cacheHits"`
	Rendered    int             `json:"renderedTiles"`
	Algorithm   string          `json:"algorithmVersion"`
	ContentHash string          `json:"contentHash"`
	Camera      model.Camera    `json:"camera"`
	Clip        model.ClipPlane `json:"clipPlane"`
}

func (f *Frame) PNGBase64() string {
	if f == nil || f.Image == nil {
		return ""
	}
	var buf bytes.Buffer
	enc := png.Encoder{CompressionLevel: png.BestSpeed}
	if err := enc.Encode(&buf, f.Image); err != nil {
		return ""
	}
	return base64.StdEncoding.EncodeToString(buf.Bytes())
}

func NewFrame(scene *Scene, path string, duration time.Duration, tiles []TileResult) *Frame {
	f := &Frame{
		Version: scene.Version, Path: path, LOD: scene.LOD,
		Width: scene.Camera.Width, Height: scene.Camera.Height, Grid: scene.Tiles,
		Image:    image.NewNRGBA(image.Rect(0, 0, scene.Camera.Width, scene.Camera.Height)),
		Depth:    make([]float32, scene.Camera.Width*scene.Camera.Height),
		Duration: duration, Algorithm: scene.AlgorithmVersion, ContentHash: scene.ContentHash,
		Camera: scene.Camera, Clip: scene.Clip,
	}
	for i := range f.Depth {
		f.Depth[i] = float32(math.Inf(1))
	}
	f.Tiles = append([]TileResult(nil), tiles...)
	sort.Slice(f.Tiles, func(i, j int) bool { return f.Tiles[i].ID < f.Tiles[j].ID })
	f.Stats = make([]TileStats, 0, len(f.Tiles))
	for _, t := range f.Tiles {
		f.Stats = append(f.Stats, t.Stats)
		for y := 0; y < t.H; y++ {
			src := y * t.W * 4
			dst := (t.Y+y)*f.Width*4 + t.X*4
			copy(f.Image.Pix[dst:dst+t.W*4], t.Color[src:src+t.W*4])
			copy(f.Depth[(t.Y+y)*f.Width+t.X:(t.Y+y)*f.Width+t.X+t.W], t.Depth[y*t.W:(y+1)*t.W])
		}
	}
	return f
}

func (f *Frame) TileByID(id int) (TileResult, bool) {
	for _, t := range f.Tiles {
		if t.ID == id {
			return t, true
		}
	}
	return TileResult{}, false
}

func (f *Frame) CloneWithTiles(path string, tiles []TileResult) *Frame {
	scene := &Scene{
		Version: f.Version, ContentHash: f.ContentHash, AlgorithmVersion: f.Algorithm,
		Camera: f.Camera, Clip: f.Clip, Tiles: f.Grid, LOD: f.LOD,
	}
	out := NewFrame(scene, path, f.Duration, tiles)
	out.CacheHits = f.CacheHits
	out.Rendered = f.Rendered
	return out
}

func ColorFromMaterial(c model.Color) color.NRGBA {
	return color.NRGBA{R: c.R, G: c.G, B: c.B, A: 255}
}

func (s TileStats) Key() string {
	return fmt.Sprintf("%.9f/%d/%v", s.Coverage, s.SurfacePixels, s.LuminanceBins)
}
