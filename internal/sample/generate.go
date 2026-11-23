package sample

import (
	"bytes"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"sort"

	"maskpipeline/internal/model"
)

type Resolution struct {
	Width      int
	Height     int
	TileSize   int
	DeadlineMS int
}

type GenerateOptions struct {
	Seed        int64
	OutDir      string
	Resolutions []Resolution
}

type Generated struct {
	SampleID string `json:"sample_id"`
	Dir      string `json:"dir"`
	Width    int    `json:"width"`
	Height   int    `json:"height"`
	Regions  int    `json:"regions"`
}

func DefaultResolutions() []Resolution {
	return []Resolution{
		{Width: 384, Height: 256, TileSize: 64, DeadlineMS: 250},
		{Width: 512, Height: 320, TileSize: 64, DeadlineMS: 300},
		{Width: 640, Height: 384, TileSize: 64, DeadlineMS: 350},
	}
}

type shape struct {
	id     int
	name   string
	kind   string
	x0, y0 int
	x1, y1 int
	cx, cy int
	rx, ry int
	color  model.RGB
}

func Generate(opts GenerateOptions) ([]Generated, error) {
	if opts.OutDir == "" {
		opts.OutDir = "samples"
	}
	if len(opts.Resolutions) == 0 {
		opts.Resolutions = DefaultResolutions()
	}
	if err := os.MkdirAll(opts.OutDir, 0o755); err != nil {
		return nil, err
	}
	out := make([]Generated, 0, len(opts.Resolutions))
	for i, res := range opts.Resolutions {
		if res.Width <= 0 || res.Height <= 0 || res.TileSize <= 0 || res.DeadlineMS <= 0 {
			return nil, fmt.Errorf("invalid resolution at index %d", i)
		}
		g, err := generateOne(opts, res, i)
		if err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, nil
}

func generateOne(opts GenerateOptions, res Resolution, index int) (Generated, error) {
	sampleID := fmt.Sprintf("seed-%d-%dx%d", opts.Seed, res.Width, res.Height)
	dir := filepath.Join(opts.OutDir, sampleID)
	if err := os.RemoveAll(dir); err != nil {
		return Generated{}, err
	}
	if err := os.MkdirAll(filepath.Join(dir, "masks"), 0o755); err != nil {
		return Generated{}, err
	}
	rng := rand.New(rand.NewSource(opts.Seed + int64(index)*1009))
	im := makeBackground(res.Width, res.Height)
	shapes := makeShapes(res.Width, res.Height, im)

	regionMasks := make([][]bool, len(shapes))
	for i, s := range shapes {
		mask := make([]bool, res.Width*res.Height)
		switch s.kind {
		case "rect":
			fillRect(mask, res.Width, s.x0, s.y0, s.x1, s.y1)
		case "ellipse":
			fillEllipse(mask, res.Width, res.Height, s.cx, s.cy, s.rx, s.ry)
		}
		regionMasks[i] = mask
		for y := 0; y < res.Height; y++ {
			for x := 0; x < res.Width; x++ {
				if mask[y*res.Width+x] {
					setRGB(im, x, y, s.color)
				}
			}
		}
	}
	embedNoise(im, rng, shapes)
	raw := EncodeImage(im)
	imagePath := ImageName
	if err := os.WriteFile(filepath.Join(dir, imagePath), raw, 0o644); err != nil {
		return Generated{}, err
	}

	labels := make([]byte, res.Width*res.Height)
	regions := make([]RegionTruth, 0, len(shapes))
	for i, s := range shapes {
		mask := regionMasks[i]
		area := 0
		var sumX, sumY int64
		minX, minY, maxX, maxY := res.Width, res.Height, -1, -1
		for y := 0; y < res.Height; y++ {
			for x := 0; x < res.Width; x++ {
				if !mask[y*res.Width+x] {
					continue
				}
				labels[y*res.Width+x] = byte(s.id)
				area++
				sumX += int64(x)
				sumY += int64(y)
				if x < minX {
					minX = x
				}
				if y < minY {
					minY = y
				}
				if x > maxX {
					maxX = x
				}
				if y > maxY {
					maxY = y
				}
			}
		}
		if area == 0 {
			return Generated{}, fmt.Errorf("generated empty shape %s", s.name)
		}
		rel := filepath.ToSlash(filepath.Join("masks", fmt.Sprintf("region_%02d.png", s.id)))
		maskPath := filepath.Join(dir, filepath.FromSlash(rel))
		maskBytes, err := encodeRegionMask(mask, res.Width, res.Height)
		if err != nil {
			return Generated{}, err
		}
		if err := os.WriteFile(maskPath, maskBytes, 0o644); err != nil {
			return Generated{}, err
		}
		regions = append(regions, RegionTruth{
			ID:         s.id,
			Name:       s.name,
			Seed:       model.Point{X: s.cx, Y: s.cy},
			RGB:        im.RGBAt(s.cx, s.cy),
			Area:       area,
			Centroid:   model.PointF{X: float64(sumX) / float64(area), Y: float64(sumY) / float64(area)},
			BBox:       model.Rect{X: minX, Y: minY, W: maxX - minX + 1, H: maxY - minY + 1},
			MaskPath:   rel,
			MaskSHA256: model.HashBytes(maskBytes),
		})
	}
	sort.Slice(regions, func(i, j int) bool { return regions[i].ID < regions[j].ID })
	combinedRel := "masks/truth_labels.png"
	combinedBytes, err := encodeLabelMask(labels, res.Width, res.Height)
	if err != nil {
		return Generated{}, err
	}
	if err := os.WriteFile(filepath.Join(dir, "masks", "truth_labels.png"), combinedBytes, 0o644); err != nil {
		return Generated{}, err
	}
	m := Manifest{
		SchemaVersion:     SchemaVersion,
		SampleID:          sampleID,
		Seed:              opts.Seed,
		Width:             res.Width,
		Height:            res.Height,
		Channels:          3,
		TileSize:          res.TileSize,
		PreviewDeadlineMS: res.DeadlineMS,
		AlgorithmVersion:  model.AlgorithmVersion,
		Tolerances: Tolerances{
			RegionCount: 0,
			CentroidPx:  1.50,
			AreaRel:     0.05,
		},
		DefaultParams: ParameterDefaults{
			ColorTolerance:       26,
			GradientThreshold:    6,
			StructureElementSize: 5,
		},
		Image: ImageRef{Path: imagePath, SHA256: model.HashBytes(raw)},
		Truth: TruthRef{
			RegionCount: len(regions),
			MaskPath:    combinedRel,
			MaskSHA256:  model.HashBytes(combinedBytes),
		},
		Regions: regions,
	}
	m.Truth.Fingerprint = m.Fingerprint()
	manifestBytes, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return Generated{}, err
	}
	manifestBytes = append(manifestBytes, '\n')
	if err := os.WriteFile(filepath.Join(dir, ManifestName), manifestBytes, 0o644); err != nil {
		return Generated{}, err
	}
	if _, err := Load(dir); err != nil {
		return Generated{}, fmt.Errorf("generated sample failed validation: %w", err)
	}
	return Generated{
		SampleID: sampleID,
		Dir:      filepath.ToSlash(filepath.Clean(dir)),
		Width:    res.Width,
		Height:   res.Height,
		Regions:  len(regions),
	}, nil
}

func makeBackground(width, height int) *model.Image {
	im := &model.Image{Width: width, Height: height, Channels: 3, Pix: make([]byte, width*height*3)}
	for y := 0; y < height; y++ {
		gy := float64(y) / math.Max(1, float64(height-1))
		for x := 0; x < width; x++ {
			gx := float64(x) / math.Max(1, float64(width-1))
			r := clamp8(int(24 + 86*gx + 4*gy))
			g := clamp8(int(38 + 58*gy + 10*gx))
			b := clamp8(int(86 + 34*gx + 48*gy))
			setRGB(im, x, y, model.RGB{r, g, b})
		}
	}
	return im
}

func makeShapes(width, height int, im *model.Image) []shape {
	rect := shape{
		id: 1, name: "high_contrast_rectangle", kind: "rect",
		x0: width / 10, x1: width * 34 / 100,
		y0: height * 18 / 100, y1: height * 52 / 100,
		color: model.RGB{232, 72, 61},
	}
	rect.cx = (rect.x0 + rect.x1) / 2
	rect.cy = (rect.y0 + rect.y1) / 2

	lowCX, lowCY := width*64/100, height*38/100
	bg := im.RGBAt(lowCX, lowCY)
	low := shape{
		id: 2, name: "low_contrast_ellipse", kind: "ellipse",
		cx: lowCX, cy: lowCY, rx: width * 13 / 100, ry: height * 20 / 100,
		color: model.RGB{
			clamp8(int(bg[0]) + 24),
			clamp8(int(bg[1]) + 21),
			clamp8(int(bg[2]) + 18),
		},
	}
	circle := shape{
		id: 3, name: "medium_contrast_circle", kind: "ellipse",
		cx: width * 63 / 100, cy: height * 77 / 100,
		rx: height * 13 / 100, ry: height * 13 / 100,
		color: model.RGB{59, 187, 149},
	}
	return []shape{rect, low, circle}
}

func embedNoise(im *model.Image, rng *rand.Rand, shapes []shape) {
	count := im.Width * im.Height / 180
	seeds := make([]model.Point, len(shapes))
	for i, s := range shapes {
		seeds[i] = model.Point{X: s.cx, Y: s.cy}
	}
	for i := 0; i < count; i++ {
		x := rng.Intn(im.Width)
		y := rng.Intn(im.Height)
		skip := false
		for _, s := range seeds {
			dx, dy := x-s.X, y-s.Y
			if dx*dx+dy*dy <= 9 {
				skip = true
				break
			}
		}
		if skip {
			continue
		}
		p := im.RGBAt(x, y)
		delta := []int{-72, -51, 47, 68}
		d := delta[rng.Intn(len(delta))]
		setRGB(im, x, y, model.RGB{
			clamp8(int(p[0]) + d),
			clamp8(int(p[1]) + d/2),
			clamp8(int(p[2]) - d/3),
		})
	}
}

func setRGB(im *model.Image, x, y int, c model.RGB) {
	i := (y*im.Width + x) * im.Channels
	im.Pix[i] = c[0]
	im.Pix[i+1] = c[1]
	im.Pix[i+2] = c[2]
}

func fillRect(mask []bool, width, x0, y0, x1, y1 int) {
	for y := y0; y < y1; y++ {
		for x := x0; x < x1; x++ {
			mask[y*width+x] = true
		}
	}
}

func fillEllipse(mask []bool, width, height, cx, cy, rx, ry int) {
	if rx <= 0 || ry <= 0 {
		return
	}
	for y := max(0, cy-ry); y <= min(height-1, cy+ry); y++ {
		for x := max(0, cx-rx); x <= min(width-1, cx+rx); x++ {
			dx := float64(x-cx) / float64(rx)
			dy := float64(y-cy) / float64(ry)
			if dx*dx+dy*dy <= 1.0 {
				mask[y*width+x] = true
			}
		}
	}
}

func encodeRegionMask(mask []bool, width, height int) ([]byte, error) {
	g := image.NewGray(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			if mask[y*width+x] {
				g.SetGray(x, y, color.Gray{Y: 255})
			}
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, g); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func encodeLabelMask(labels []byte, width, height int) ([]byte, error) {
	g := image.NewGray(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			g.SetGray(x, y, color.Gray{Y: labels[y*width+x]})
		}
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, g); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func clamp8(v int) uint8 {
	if v < 0 {
		return 0
	}
	if v > 255 {
		return 255
	}
	return uint8(v)
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
