package sample

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"image"
	"image/color"
	"image/png"
	"math"
	"math/rand"
	"os"
	"path/filepath"

	"iconbench/internal/imaging"
	"iconbench/internal/model"
)

const FixedSeed int64 = 0x42A8691E

type shapeKind string

const (
	shapeEllipse   shapeKind = "ellipse"
	shapeRoundRect shapeKind = "round_rect"
	shapeTriangle  shapeKind = "triangle"
)

type shapeSpec struct {
	Kind   shapeKind `json:"kind"`
	CX     float64   `json:"cx"`
	CY     float64   `json:"cy"`
	RX     float64   `json:"rx,omitempty"`
	RY     float64   `json:"ry,omitempty"`
	HalfW  float64   `json:"half_w,omitempty"`
	HalfH  float64   `json:"half_h,omitempty"`
	Radius float64   `json:"radius,omitempty"`
	Size   float64   `json:"size,omitempty"`
	HoleRX float64   `json:"hole_rx,omitempty"`
	HoleRY float64   `json:"hole_ry,omitempty"`
	Color  [3]uint8  `json:"color"`
}

type imageSpec struct {
	ID     string
	Width  int
	Height int
	Bg0    [3]uint8
	Bg1    [3]uint8
	Glow   [3]uint8
	Shapes []shapeSpec
}

func DefaultBudget() model.Budget {
	return model.Budget{
		TileSize:             64,
		WorkerThreads:        4,
		PixelBufferPerThread: 65536,
		PreviewDelayMS:       250,
		Seed:                 FixedSeed,
	}
}

func DefaultTolerances() model.Tolerances {
	return model.Tolerances{
		MaskIoUMin:           0.93,
		MaskMismatchRatioMax: 0.08,
		CentroidPx:           2.5,
		AreaRel:              0.08,
		HoleCount:            0,
		PreviewRegionCount:   0,
		PreviewCentroidPx:    3.5,
		PreviewAreaRel:       0.14,
	}
}

func specs() []imageSpec {
	indigo := [3]uint8{67, 56, 202}
	teal := [3]uint8{13, 148, 136}
	rose := [3]uint8{190, 18, 60}
	amber := [3]uint8{180, 83, 9}
	violet := [3]uint8{124, 58, 237}
	blue := [3]uint8{29, 78, 216}
	green := [3]uint8{21, 128, 61}
	orange := [3]uint8{194, 65, 12}

	return []imageSpec{
		{
			ID: "icon_grid_128", Width: 128, Height: 128,
			Bg0: [3]uint8{238, 242, 255}, Bg1: [3]uint8{199, 210, 254}, Glow: indigo,
			Shapes: []shapeSpec{
				{Kind: shapeEllipse, CX: 32, CY: 32, RX: 19, RY: 16, Color: indigo},
				{Kind: shapeRoundRect, CX: 96, CY: 32, HalfW: 19, HalfH: 15, Radius: 8, Color: teal},
				{Kind: shapeTriangle, CX: 32, CY: 97, Size: 20, Color: rose},
				{Kind: shapeEllipse, CX: 96, CY: 96, RX: 21, RY: 19, HoleRX: 9, HoleRY: 8, Color: amber},
			},
		},
		{
			ID: "icon_wide_192x128", Width: 192, Height: 128,
			Bg0: [3]uint8{255, 247, 237}, Bg1: [3]uint8{254, 215, 170}, Glow: orange,
			Shapes: []shapeSpec{
				{Kind: shapeRoundRect, CX: 32, CY: 32, HalfW: 20, HalfH: 16, Radius: 7, Color: orange},
				{Kind: shapeEllipse, CX: 96, CY: 32, RX: 18, RY: 20, Color: rose},
				{Kind: shapeTriangle, CX: 160, CY: 33, Size: 19, Color: violet},
				{Kind: shapeEllipse, CX: 32, CY: 96, RX: 20, RY: 18, HoleRX: 8, HoleRY: 7, Color: blue},
				{Kind: shapeRoundRect, CX: 160, CY: 96, HalfW: 18, HalfH: 18, Radius: 10, Color: green},
			},
		},
		{
			ID: "icon_tall_128x192", Width: 128, Height: 192,
			Bg0: [3]uint8{236, 254, 255}, Bg1: [3]uint8{165, 243, 252}, Glow: teal,
			Shapes: []shapeSpec{
				{Kind: shapeTriangle, CX: 32, CY: 33, Size: 19, Color: teal},
				{Kind: shapeEllipse, CX: 96, CY: 32, RX: 19, RY: 17, Color: blue},
				{Kind: shapeEllipse, CX: 32, CY: 96, RX: 21, RY: 18, HoleRX: 10, HoleRY: 7, Color: violet},
				{Kind: shapeRoundRect, CX: 96, CY: 96, HalfW: 18, HalfH: 19, Radius: 9, Color: rose},
				{Kind: shapeRoundRect, CX: 32, CY: 160, HalfW: 20, HalfH: 15, Radius: 6, Color: green},
				{Kind: shapeTriangle, CX: 96, CY: 161, Size: 20, Color: amber},
			},
		},
		{
			ID: "icon_strip_256x128", Width: 256, Height: 128,
			Bg0: [3]uint8{250, 245, 255}, Bg1: [3]uint8{221, 214, 254}, Glow: violet,
			Shapes: []shapeSpec{
				{Kind: shapeEllipse, CX: 32, CY: 32, RX: 17, RY: 19, Color: violet},
				{Kind: shapeTriangle, CX: 96, CY: 33, Size: 18, Color: indigo},
				{Kind: shapeRoundRect, CX: 160, CY: 32, HalfW: 20, HalfH: 15, Radius: 8, Color: blue},
				{Kind: shapeEllipse, CX: 224, CY: 32, RX: 18, RY: 17, HoleRX: 7, HoleRY: 8, Color: rose},
				{Kind: shapeRoundRect, CX: 96, CY: 96, HalfW: 17, HalfH: 18, Radius: 9, Color: teal},
				{Kind: shapeEllipse, CX: 160, CY: 96, RX: 20, RY: 16, Color: amber},
				{Kind: shapeTriangle, CX: 224, CY: 97, Size: 19, Color: green},
			},
		},
	}
}

func Generate(dataDir string) (*model.Manifest, error) {
	if err := ensureWritable(dataDir); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(dataDir, "images"), 0o755); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(dataDir, "masks"), 0o755); err != nil {
		return nil, err
	}

	manifest := &model.Manifest{
		Seed:             FixedSeed,
		GeneratorVersion: model.GeneratorVersion,
		AlgorithmVersion: model.AlgorithmVersion,
		DataDir:          dataDir,
		Budget:           DefaultBudget(),
		Tolerances:       DefaultTolerances(),
	}

	for imageIndex, sp := range specs() {
		mask := rasterTruth(sp)
		_, regions := imaging.ConnectedComponents(mask, sp.Width, sp.Height)
		img := renderImage(sp, mask, imageIndex)

		var imgBuf bytes.Buffer
		if err := png.Encode(&imgBuf, img); err != nil {
			return nil, err
		}
		imgBytes := imgBuf.Bytes()
		imgName := sp.ID + ".png"
		if err := os.WriteFile(filepath.Join(dataDir, "images", imgName), imgBytes, 0o644); err != nil {
			return nil, err
		}

		maskGray := image.NewGray(image.Rect(0, 0, sp.Width, sp.Height))
		for i, v := range mask {
			if v != 0 {
				maskGray.Pix[i] = 255
			}
		}
		var maskBuf bytes.Buffer
		if err := png.Encode(&maskBuf, maskGray); err != nil {
			return nil, err
		}
		maskName := sp.ID + ".mask.png"
		if err := os.WriteFile(filepath.Join(dataDir, "masks", maskName), maskBuf.Bytes(), 0o644); err != nil {
			return nil, err
		}

		imgSum := sha256.Sum256(imgBytes)
		maskSum := sha256.Sum256(mask)
		fp := sha256.New()
		fp.Write(maskSum[:])
		regionJSON, _ := json.Marshal(regions)
		fp.Write(regionJSON)

		manifest.Images = append(manifest.Images, model.ImageManifest{
			ID:                     sp.ID,
			File:                   filepath.ToSlash(filepath.Join("images", imgName)),
			MaskFile:               filepath.ToSlash(filepath.Join("masks", maskName)),
			Width:                  sp.Width,
			Height:                 sp.Height,
			Channels:               4,
			SHA256:                 hex.EncodeToString(imgSum[:]),
			MaskSHA256:             hex.EncodeToString(maskSum[:]),
			ReferenceMaskPNGBase64: base64.StdEncoding.EncodeToString(maskBuf.Bytes()),
			TruthFingerprint:       hex.EncodeToString(fp.Sum(nil)),
			Regions:                regions,
		})
	}

	b, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(dataDir, "manifest.json"), b, 0o644); err != nil {
		return nil, err
	}
	return manifest, nil
}

func ensureWritable(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".write-check-*")
	if err != nil {
		return &model.Error{Code: model.CodeResource, Field: "data_dir", Actual: dir, Message: "data directory is not writable: " + err.Error()}
	}
	name := f.Name()
	if _, err := f.WriteString("ok"); err != nil {
		f.Close()
		os.Remove(name)
		return &model.Error{Code: model.CodeResource, Field: "data_dir", Actual: dir, Message: "data directory write failed: " + err.Error()}
	}
	if err := f.Close(); err != nil {
		os.Remove(name)
		return err
	}
	return os.Remove(name)
}

func rasterTruth(sp imageSpec) []byte {
	mask := make([]byte, sp.Width*sp.Height)
	for y := 0; y < sp.Height; y++ {
		for x := 0; x < sp.Width; x++ {
			fx := float64(x) + 0.5
			fy := float64(y) + 0.5
			for _, sh := range sp.Shapes {
				if shapeContains(sh, fx, fy) && !holeContains(sh, fx, fy) {
					mask[y*sp.Width+x] = 1
					break
				}
			}
		}
	}
	return mask
}

func shapeContains(sh shapeSpec, x, y float64) bool {
	dx, dy := x-sh.CX, y-sh.CY
	switch sh.Kind {
	case shapeEllipse:
		v := (dx*dx)/(sh.RX*sh.RX) + (dy*dy)/(sh.RY*sh.RY)
		return v <= 1
	case shapeRoundRect:
		qx := math.Abs(dx) - (sh.HalfW - sh.Radius)
		qy := math.Abs(dy) - (sh.HalfH - sh.Radius)
		ox := math.Max(qx, 0)
		oy := math.Max(qy, 0)
		outside := math.Hypot(ox, oy)
		inside := math.Min(math.Max(qx, qy), 0)
		return outside+inside-sh.Radius <= 0
	case shapeTriangle:
		r := sh.Size
		a := [2]float64{sh.CX, sh.CY - r}
		b := [2]float64{sh.CX - r*0.92, sh.CY + r*0.72}
		c := [2]float64{sh.CX + r*0.92, sh.CY + r*0.72}
		return pointInTriangle(x, y, a, b, c)
	}
	return false
}

func holeContains(sh shapeSpec, x, y float64) bool {
	if sh.HoleRX <= 0 || sh.HoleRY <= 0 {
		return false
	}
	dx, dy := x-sh.CX, y-sh.CY
	return (dx*dx)/(sh.HoleRX*sh.HoleRX)+(dy*dy)/(sh.HoleRY*sh.HoleRY) <= 1
}

func sign(px, py float64, a, b [2]float64) float64 {
	return (px-b[0])*(a[1]-b[1]) - (a[0]-b[0])*(py-b[1])
}

func pointInTriangle(px, py float64, a, b, c [2]float64) bool {
	d1 := sign(px, py, a, b)
	d2 := sign(px, py, b, c)
	d3 := sign(px, py, c, a)
	hasNeg := d1 < 0 || d2 < 0 || d3 < 0
	hasPos := d1 > 0 || d2 > 0 || d3 > 0
	return !(hasNeg && hasPos)
}

func renderImage(sp imageSpec, truth []byte, imageIndex int) *image.NRGBA {
	rng := rand.New(rand.NewSource(FixedSeed + int64(imageIndex)*1009))
	img := image.NewNRGBA(image.Rect(0, 0, sp.Width, sp.Height))
	for y := 0; y < sp.Height; y++ {
		for x := 0; x < sp.Width; x++ {
			i := y*sp.Width + x
			t := 0.62*float64(x)/float64(sp.Width-1) + 0.38*float64(y)/float64(sp.Height-1)
			r := blend8(sp.Bg0[0], sp.Bg1[0], t)
			g := blend8(sp.Bg0[1], sp.Bg1[1], t)
			b := blend8(sp.Bg0[2], sp.Bg1[2], t)

			gdx := float64(x-sp.Width/3) / float64(sp.Width)
			gdy := float64(y-sp.Height/3) / float64(sp.Height)
			glow := math.Exp(-(gdx*gdx+gdy*gdy)*9) * 0.22
			r = lerp(r, float64(sp.Glow[0]), glow)
			g = lerp(g, float64(sp.Glow[1]), glow)
			b = lerp(b, float64(sp.Glow[2]), glow)

			n := (rng.Float64() - 0.5) * 12
			r += n
			g += n * 0.85
			b += n * 1.1

			if truth[i] != 0 {
				sh := shapeAt(sp, x, y)
				col := sh.Color
				shade := 0.92 + 0.12*float64(y)/float64(sp.Height)
				fn := (rng.Float64() - 0.5) * 10
				r = float64(col[0])*shade + fn
				g = float64(col[1])*shade + fn
				b = float64(col[2])*shade + fn*0.8
			}
			img.SetNRGBA(x, y, color.NRGBA{R: byte(clamp(r)), G: byte(clamp(g)), B: byte(clamp(b)), A: 255})
		}
	}

	// A one-pixel translucent inner stroke/highlight. Keeping the stroke
	// inside the analytical silhouette gives the reference mask a stable edge.
	for y := 0; y < sp.Height; y++ {
		for x := 0; x < sp.Width; x++ {
			i := y*sp.Width + x
			if truth[i] == 0 {
				continue
			}
			innerEdge := false
			for _, n := range neighbors4(x, y, sp.Width, sp.Height) {
				if truth[n] == 0 {
					innerEdge = true
					break
				}
			}
			if innerEdge {
				blendPixel(img, x, y, [3]uint8{15, 23, 42}, 0.18)
			}
		}
	}

	// Embedded deterministic speckle noise.
	speckRng := rand.New(rand.NewSource(FixedSeed + int64(imageIndex)*7919 + 17))
	speckles := sp.Width * sp.Height / 260
	for k := 0; k < speckles; k++ {
		x := speckRng.Intn(sp.Width)
		y := speckRng.Intn(sp.Height)
		light := speckRng.Intn(2) == 0
		if light {
			blendPixel(img, x, y, [3]uint8{255, 255, 255}, 0.35)
		} else {
			blendPixel(img, x, y, [3]uint8{15, 23, 42}, 0.22)
		}
	}
	return img
}

func shapeAt(sp imageSpec, x, y int) *shapeSpec {
	fx, fy := float64(x)+0.5, float64(y)+0.5
	for i := range sp.Shapes {
		sh := &sp.Shapes[i]
		if shapeContains(*sh, fx, fy) || nearShapeEdge(*sh, fx, fy, 1.4) {
			return sh
		}
	}
	return nil
}

func nearShapeEdge(sh shapeSpec, x, y, slack float64) bool {
	switch sh.Kind {
	case shapeEllipse:
		dx, dy := x-sh.CX, y-sh.CY
		v := math.Abs((dx*dx)/(sh.RX*sh.RX) + (dy*dy)/(sh.RY*sh.RY) - 1)
		return v < slack*0.18
	case shapeRoundRect:
		qx := math.Abs(x-sh.CX) - (sh.HalfW - sh.Radius)
		qy := math.Abs(y-sh.CY) - (sh.HalfH - sh.Radius)
		outside := math.Hypot(math.Max(qx, 0), math.Max(qy, 0))
		inside := math.Min(math.Max(qx, qy), 0)
		d := math.Abs(outside + inside - sh.Radius)
		return d < slack
	default:
		return false
	}
}

func neighbors8(x, y, w, h int) []int {
	var out []int
	for dy := -1; dy <= 1; dy++ {
		for dx := -1; dx <= 1; dx++ {
			if dx == 0 && dy == 0 {
				continue
			}
			nx, ny := x+dx, y+dy
			if nx >= 0 && nx < w && ny >= 0 && ny < h {
				out = append(out, ny*w+nx)
			}
		}
	}
	return out
}

func neighbors4(x, y, w, h int) []int {
	var out []int
	if x > 0 {
		out = append(out, y*w+x-1)
	}
	if x+1 < w {
		out = append(out, y*w+x+1)
	}
	if y > 0 {
		out = append(out, (y-1)*w+x)
	}
	if y+1 < h {
		out = append(out, (y+1)*w+x)
	}
	return out
}

func blendPixel(img *image.NRGBA, x, y int, col [3]uint8, alpha float64) {
	c := img.NRGBAAt(x, y)
	c.R = byte(lerp(float64(c.R), float64(col[0]), alpha))
	c.G = byte(lerp(float64(c.G), float64(col[1]), alpha))
	c.B = byte(lerp(float64(c.B), float64(col[2]), alpha))
	img.SetNRGBA(x, y, c)
}

func blend8(a, b uint8, t float64) float64 {
	return lerp(float64(a), float64(b), clamp01(t))
}

func lerp(a, b, t float64) float64 {
	return a + (b-a)*t
}

func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}

func clamp(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 255 {
		return 255
	}
	return v
}
