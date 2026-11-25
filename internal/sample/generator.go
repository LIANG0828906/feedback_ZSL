package sample

import (
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"math"
	"os"
	"path/filepath"
	"sort"

	"iconseg/internal/core"
)

type shapeKind string

const (
	shapeRoundedRect shapeKind = "rounded_rect"
	shapeEllipse     shapeKind = "ellipse"
)

type holeSpec struct {
	CX, CY, R float64
}

type shapeSpec struct {
	Kind            shapeKind
	CX, CY, RX, RY  float64
	Corner          float64
	Holes           []holeSpec
	ForegroundSeedX int
	ForegroundSeedY int
}

type iconSpec struct {
	Name   string
	Width  int
	Height int
	Shapes []shapeSpec
}

type deterministicNoise struct {
	state uint64
}

func newNoise(seed uint64) *deterministicNoise {
	return &deterministicNoise{state: seed ^ 0x9e3779b97f4a7c15}
}

func (n *deterministicNoise) next() uint64 {
	n.state += 0x9e3779b97f4a7c15
	z := n.state
	z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9
	z = (z ^ (z >> 27)) * 0x94d049bb133111eb
	return z ^ (z >> 31)
}

func (n *deterministicNoise) signed(amplitude int) int {
	if amplitude == 0 {
		return 0
	}
	return int(n.next()%uint64(2*amplitude+1)) - amplitude
}

// DefaultSpecs returns the fixed construction program used by the local
// sample generator. Shape placement is intentionally explicit so that the
// truth geometry has a simple, reviewable analytic definition.
func DefaultSpecs() []iconSpec {
	return []iconSpec{
		{
			Name: "icon_ring_96", Width: 96, Height: 96,
			Shapes: []shapeSpec{{
				Kind: shapeRoundedRect, CX: 48, CY: 48, RX: 27, RY: 23, Corner: 9,
				Holes:           []holeSpec{{CX: 48, CY: 48, R: 8.5}},
				ForegroundSeedX: 31, ForegroundSeedY: 48,
			}},
		},
		{
			Name: "icon_pair_112x88", Width: 112, Height: 88,
			Shapes: []shapeSpec{
				{
					Kind: shapeEllipse, CX: 31, CY: 44, RX: 18, RY: 24,
					ForegroundSeedX: 31, ForegroundSeedY: 34,
				},
				{
					Kind: shapeRoundedRect, CX: 79, CY: 44, RX: 20, RY: 24, Corner: 8,
					Holes:           []holeSpec{{CX: 79, CY: 44, R: 7}},
					ForegroundSeedX: 68, ForegroundSeedY: 44,
				},
			},
		},
		{
			Name: "icon_triad_128x120", Width: 128, Height: 120,
			Shapes: []shapeSpec{
				{
					Kind: shapeRoundedRect, CX: 31, CY: 34, RX: 18, RY: 27, Corner: 7,
					Holes:           []holeSpec{{CX: 31, CY: 34, R: 5.5}},
					ForegroundSeedX: 31, ForegroundSeedY: 23,
				},
				{
					Kind: shapeEllipse, CX: 89, CY: 31, RX: 24, RY: 20,
					ForegroundSeedX: 89, ForegroundSeedY: 31,
				},
				{
					Kind: shapeRoundedRect, CX: 57, CY: 92, RX: 38, RY: 16, Corner: 6,
					Holes:           []holeSpec{{CX: 57, CY: 92, R: 8}},
					ForegroundSeedX: 44, ForegroundSeedY: 92,
				},
			},
		},
	}
}

func shapeContains(s shapeSpec, x, y float64) bool {
	var inside bool
	switch s.Kind {
	case shapeEllipse:
		dx := (x - s.CX) / s.RX
		dy := (y - s.CY) / s.RY
		inside = dx*dx+dy*dy <= 1
	default:
		dx := math.Abs(x - s.CX)
		dy := math.Abs(y - s.CY)
		if dx > s.RX || dy > s.RY {
			return false
		}
		corner := s.Corner
		if corner <= 0 {
			inside = true
			break
		}
		qx := dx - (s.RX - corner)
		qy := dy - (s.RY - corner)
		if qx <= 0 || qy <= 0 {
			inside = true
		} else {
			inside = qx*qx+qy*qy <= corner*corner
		}
	}
	if !inside {
		return false
	}
	for _, h := range s.Holes {
		dx := x - h.CX
		dy := y - h.CY
		if dx*dx+dy*dy <= h.R*h.R {
			return false
		}
	}
	return true
}

func rasterize(spec iconSpec) []byte {
	mask := make([]byte, spec.Width*spec.Height)
	for y := 0; y < spec.Height; y++ {
		for x := 0; x < spec.Width; x++ {
			px := float64(x) + 0.5
			py := float64(y) + 0.5
			for _, shape := range spec.Shapes {
				if shapeContains(shape, px, py) {
					mask[y*spec.Width+x] = 1
					break
				}
			}
		}
	}
	return mask
}

func boundaryNear(mask []byte, w, h, x, y int) bool {
	at := func(xx, yy int) bool {
		if xx < 0 || yy < 0 || xx >= w || yy >= h {
			return false
		}
		return mask[yy*w+xx] != 0
	}
	center := at(x, y)
	for _, d := range [][2]int{{0, 0}, {1, 0}, {-1, 0}, {0, 1}, {0, -1}} {
		if at(x+d[0], y+d[1]) != center {
			return true
		}
	}
	return false
}

func clampByte(v int) byte {
	if v < 0 {
		return 0
	}
	if v > 255 {
		return 255
	}
	return byte(v)
}

func buildImage(spec iconSpec, mask []byte, iconSeed int64) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, spec.Width, spec.Height))
	noise := newNoise(uint64(iconSeed) ^ uint64(spec.Width)<<17 ^ uint64(spec.Height)<<3)
	for y := 0; y < spec.Height; y++ {
		for x := 0; x < spec.Width; x++ {
			// The background is deliberately close to the foreground hue.
			r := 118 + (42*x)/max(1, spec.Width-1) + (12*y)/max(1, spec.Height-1)
			g := 54 + (18*y)/max(1, spec.Height-1) + (9*x)/max(1, spec.Width-1)
			b := 102 + (30*(x+y))/max(1, spec.Width+spec.Height-2)
			if mask[y*spec.Width+x] != 0 {
				r, g, b = 207, 91, 156
			}
			alpha := 255
			if boundaryNear(mask, spec.Width, spec.Height, x, y) {
				// Semi-transparent highlight on the construction outline. A
				// non-opaque alpha also keeps the generated PNG explicitly RGBA.
				r = (r*65 + 250*35) / 100
				g = (g*65 + 226*35) / 100
				b = (b*65 + 244*35) / 100
				alpha = 208
			}
			r += noise.signed(6)
			g += noise.signed(6)
			b += noise.signed(6)
			img.SetNRGBA(x, y, color.NRGBA{R: clampByte(r), G: clampByte(g), B: clampByte(b), A: uint8(alpha)})
		}
	}
	return img
}

func backgroundSeeds(spec iconSpec) []core.Seed {
	seeds := []core.Seed{
		{X: 3, Y: 3, Kind: core.SeedBackground},
		{X: spec.Width - 4, Y: 3, Kind: core.SeedBackground},
		{X: 3, Y: spec.Height - 4, Kind: core.SeedBackground},
		{X: spec.Width - 4, Y: spec.Height - 4, Kind: core.SeedBackground},
	}
	for _, shape := range spec.Shapes {
		for _, hole := range shape.Holes {
			seeds = append(seeds, core.Seed{
				X: int(math.Round(hole.CX)), Y: int(math.Round(hole.CY)), Kind: core.SeedBackground,
			})
		}
	}
	return seeds
}

type componentStat struct {
	CentroidX, CentroidY float64
	Area                 int
	BBox                 core.Rect
	Holes                int
	Pixels               []int
}

func components(mask []byte, w, h int) []componentStat {
	seen := make([]bool, len(mask))
	var out []componentStat
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			i := y*w + x
			if mask[i] == 0 || seen[i] {
				continue
			}
			stack := []int{i}
			seen[i] = true
			pixels := make([]int, 0, 32)
			minX, minY, maxX, maxY := x, y, x, y
			for len(stack) > 0 {
				p := stack[len(stack)-1]
				stack = stack[:len(stack)-1]
				pixels = append(pixels, p)
				px, py := p%w, p/w
				if px < minX {
					minX = px
				}
				if px > maxX {
					maxX = px
				}
				if py < minY {
					minY = py
				}
				if py > maxY {
					maxY = py
				}
				for _, d := range [][2]int{{1, 0}, {-1, 0}, {0, 1}, {0, -1}} {
					nx, ny := px+d[0], py+d[1]
					if nx < 0 || ny < 0 || nx >= w || ny >= h {
						continue
					}
					ni := ny*w + nx
					if mask[ni] != 0 && !seen[ni] {
						seen[ni] = true
						stack = append(stack, ni)
					}
				}
			}
			var sx, sy float64
			for _, p := range pixels {
				sx += float64(p%w) + 0.5
				sy += float64(p/w) + 0.5
			}
			out = append(out, componentStat{
				CentroidX: sx / float64(len(pixels)), CentroidY: sy / float64(len(pixels)),
				Area: len(pixels), BBox: core.Rect{X: minX, Y: minY, W: maxX - minX + 1, H: maxY - minY + 1},
				Holes:  holeCount(mask, w, h, minX-1, minY-1, maxX+1, maxY+1),
				Pixels: pixels,
			})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].BBox.Y != out[j].BBox.Y {
			return out[i].BBox.Y < out[j].BBox.Y
		}
		if out[i].BBox.X != out[j].BBox.X {
			return out[i].BBox.X < out[j].BBox.X
		}
		return out[i].Area < out[j].Area
	})
	return out
}

func holeCount(mask []byte, w, h, minX, minY, maxX, maxY int) int {
	if minX < 0 {
		minX = 0
	}
	if minY < 0 {
		minY = 0
	}
	if maxX >= w {
		maxX = w - 1
	}
	if maxY >= h {
		maxY = h - 1
	}
	ww, hh := maxX-minX+1, maxY-minY+1
	seen := make([]bool, ww*hh)
	queue := make([]int, 0, ww*hh)
	for x := 0; x < ww; x++ {
		queue = append(queue, x, (hh-1)*ww+x)
	}
	for y := 0; y < hh; y++ {
		queue = append(queue, y*ww, y*ww+ww-1)
	}
	for len(queue) > 0 {
		p := queue[len(queue)-1]
		queue = queue[:len(queue)-1]
		if seen[p] {
			continue
		}
		seen[p] = true
		x, y := p%ww, p/ww
		if mask[(minY+y)*w+minX+x] != 0 {
			continue
		}
		for _, d := range [][2]int{{1, 0}, {-1, 0}, {0, 1}, {0, -1}} {
			nx, ny := x+d[0], y+d[1]
			if nx < 0 || ny < 0 || nx >= ww || ny >= hh {
				continue
			}
			np := ny*ww + nx
			if !seen[np] {
				queue = append(queue, np)
			}
		}
	}
	holes := 0
	for y := 0; y < hh; y++ {
		for x := 0; x < ww; x++ {
			p := y*ww + x
			if seen[p] || mask[(minY+y)*w+minX+x] != 0 {
				continue
			}
			holes++
			stack := []int{p}
			seen[p] = true
			for len(stack) > 0 {
				q := stack[len(stack)-1]
				stack = stack[:len(stack)-1]
				qx, qy := q%ww, q/ww
				for _, d := range [][2]int{{1, 0}, {-1, 0}, {0, 1}, {0, -1}} {
					nx, ny := qx+d[0], qy+d[1]
					if nx < 0 || ny < 0 || nx >= ww || ny >= hh {
						continue
					}
					np := ny*ww + nx
					if !seen[np] && mask[(minY+ny)*w+minX+nx] == 0 {
						seen[np] = true
						stack = append(stack, np)
					}
				}
			}
		}
	}
	return holes
}

// Generate creates the deterministic in-memory icon set. No sample files are
// required in the repository.
func Generate(seed int64) []core.Icon {
	specs := DefaultSpecs()
	icons := make([]core.Icon, 0, len(specs))
	for idx, spec := range specs {
		mask := rasterize(spec)
		img := buildImage(spec, mask, seed+int64(idx*104729))
		comps := components(mask, spec.Width, spec.Height)
		regions := make([]core.ComponentTruth, 0, len(comps))
		for i, c := range comps {
			regions = append(regions, core.ComponentTruth{
				ID: i + 1, CentroidX: c.CentroidX, CentroidY: c.CentroidY,
				Area: c.Area, BBox: c.BBox, HoleCount: c.Holes,
			})
		}
		seeds := backgroundSeeds(spec)
		for i, shape := range spec.Shapes {
			seeds = append(seeds, core.Seed{
				X: shape.ForegroundSeedX, Y: shape.ForegroundSeedY,
				Kind: core.SeedForeground, Component: i + 1,
			})
		}
		sort.SliceStable(seeds, func(i, j int) bool {
			if seeds[i].Kind != seeds[j].Kind {
				return seeds[i].Kind < seeds[j].Kind
			}
			if seeds[i].Component != seeds[j].Component {
				return seeds[i].Component < seeds[j].Component
			}
			if seeds[i].Y != seeds[j].Y {
				return seeds[i].Y < seeds[j].Y
			}
			return seeds[i].X < seeds[j].X
		})
		manifest := core.TruthManifest{
			SchemaVersion:             core.SchemaVersion,
			AlgorithmVersion:          core.AlgorithmVersion,
			ImageID:                   spec.Name,
			File:                      spec.Name + ".png",
			MaskFile:                  spec.Name + "_mask.png",
			Seed:                      seed,
			Width:                     spec.Width,
			Height:                    spec.Height,
			Channels:                  4,
			BlockSize:                 32,
			WorkerBudget:              4,
			PerThreadPixelBuffer:      spec.Width * spec.Height,
			PreviewLatencyMS:          60,
			MaskMismatchTolerance:     0.035,
			HoleCountTolerance:        0,
			PreviewCentroidTolerance:  2.0,
			PreviewAreaRelTolerance:   0.08,
			PreviewDiffPixelTolerance: 0.05,
			Regions:                   regions,
			Seeds:                     seeds,
			ReferenceMask:             core.CloneBytes(mask),
			ReferenceMaskHash:         core.MaskHash(mask),
		}
		manifest.ReferenceFingerprint = core.FingerprintTruth(manifest)
		icons = append(icons, core.Icon{Manifest: manifest, Image: img})
	}
	return icons
}

func encodeMaskPNG(mask []byte, w, h int) *image.Gray {
	img := image.NewGray(image.Rect(0, 0, w, h))
	for i, v := range mask {
		if v != 0 {
			img.Pix[i] = 255
		}
	}
	return img
}

func writePNG(path string, img image.Image) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	encodeErr := png.Encode(f, img)
	closeErr := f.Close()
	if encodeErr != nil {
		return encodeErr
	}
	return closeErr
}

// WriteDataset materializes the generated samples and truth manifests. The
// generator remains the only source of sample data.
func WriteDataset(dir string, icons []core.Icon) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for _, icon := range icons {
		imageDir := filepath.Join(dir, icon.Manifest.ImageID)
		if err := os.MkdirAll(imageDir, 0o755); err != nil {
			return err
		}
		if err := writePNG(filepath.Join(imageDir, icon.Manifest.File), icon.Image); err != nil {
			return err
		}
		if err := writePNG(filepath.Join(imageDir, icon.Manifest.MaskFile),
			encodeMaskPNG(icon.Manifest.ReferenceMask, icon.Manifest.Width, icon.Manifest.Height)); err != nil {
			return err
		}
		body, err := json.MarshalIndent(icon.Manifest, "", "  ")
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(imageDir, "manifest.json"), body, 0o644); err != nil {
			return err
		}
	}
	return nil
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func Show(icon core.Icon) string {
	return fmt.Sprintf("%s %dx%d components=%d", icon.Manifest.ImageID, icon.Manifest.Width,
		icon.Manifest.Height, len(icon.Manifest.Regions))
}
