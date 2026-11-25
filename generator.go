package iconmask

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"math"
	"math/rand"
	"sort"
)

const (
	GeneratorVersion = "iconmask-samples/v1"
	ProjectFormat    = "iconmask/project/v1"
	segmentsPerLoop  = 8
	pointsPerSegment = 5
)

type iconSpec struct {
	id     string
	width  int
	height int
}

var deterministicIcons = []iconSpec{
	{id: "icon-96", width: 96, height: 96},
	{id: "icon-128", width: 128, height: 128},
	{id: "icon-160", width: 160, height: 160},
}

type shapeKind int

const (
	shapeEllipse shapeKind = iota
	shapeAnnulus
)

type analyticShape struct {
	id        string
	kind      shapeKind
	cx        float64
	cy        float64
	rx        float64
	ry        float64
	hole      float64
	foreColor color.RGBA
}

// GenerateProject produces all samples solely from the supplied seed. No
// wall-clock value, map iteration order, or external asset participates.
func GenerateProject(seed int64) *Project {
	project := &Project{
		Format:           ProjectFormat,
		Generator:        "iconmask-deterministic",
		GeneratorVersion: GeneratorVersion,
		AlgorithmVersion: AlgorithmVersion,
		Seed:             seed,
		Images:           make([]ImageSample, 0, len(deterministicIcons)),
	}
	for i, spec := range deterministicIcons {
		imageSeed := seed + int64(i+1)*1009
		project.Images = append(project.Images, generateIcon(spec, imageSeed))
	}
	return project
}

func generateIcon(spec iconSpec, seed int64) ImageSample {
	rng := rand.New(rand.NewSource(seed))
	width, height := spec.width, spec.height
	baseHue := 35 + rng.Intn(160)
	shapes := []analyticShape{
		{
			id:        "c0",
			kind:      shapeEllipse,
			cx:        float64(width) * 0.28,
			cy:        float64(height) * 0.42,
			rx:        float64(width) * 0.155,
			ry:        float64(height) * 0.27,
			foreColor: rgbaFromHue(baseHue, 205, 222),
		},
		{
			id:        "c1",
			kind:      shapeAnnulus,
			cx:        float64(width) * 0.69,
			cy:        float64(height) * 0.36,
			rx:        float64(width) * 0.145,
			ry:        float64(width) * 0.145,
			hole:      float64(width) * 0.058,
			foreColor: rgbaFromHue(baseHue+58, 226, 210),
		},
		{
			id:        "c2",
			kind:      shapeEllipse,
			cx:        float64(width) * 0.69,
			cy:        float64(height) * 0.73,
			rx:        float64(width) * 0.125,
			ry:        float64(height) * 0.105,
			foreColor: rgbaFromHue(baseHue+112, 242, 232),
		},
	}

	reference := NewMask(width, height)
	componentMasks := make([]Mask, len(shapes))
	for i := range componentMasks {
		componentMasks[i] = NewMask(width, height)
	}
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			px, py := float64(x)+0.5, float64(y)+0.5
			for i, shape := range shapes {
				if shapeContains(shape, px, py) {
					idx := y*width + x
					reference.Data[idx] = 1
					componentMasks[i].Data[idx] = 1
				}
			}
		}
	}

	components := make([]ComponentTruth, 0, len(shapes))
	for i, shape := range shapes {
		stats := AnalyzeMask(componentMasks[i])
		if stats.Count != 1 {
			continue
		}
		component := stats.Regions[0]
		component.ID = shape.id
		components = append(components, component)
	}
	sort.Slice(components, func(i, j int) bool { return components[i].ID < components[j].ID })

	segments := generateSegments(shapes)
	initial := generateRoughMask(reference, shapes, componentMasks, rng, seed)
	rgba := renderIcon(width, height, reference, shapes, rng, baseHue)

	return ImageSample{
		ID:          spec.id,
		Width:       width,
		Height:      height,
		Seed:        seed,
		ImagePNG:    encodePNG(rgba, width, height),
		InitialMask: initial,
		Truth: Truth{
			ReferenceMask: reference,
			Components:    components,
			Segments:      segments,
		},
	}
}

func shapeContains(shape analyticShape, x, y float64) bool {
	dx := x - shape.cx
	dy := y - shape.cy
	outer := (dx*dx)/(shape.rx*shape.rx) + (dy*dy)/(shape.ry*shape.ry)
	if outer > 1 {
		return false
	}
	if shape.kind == shapeAnnulus && shape.hole > 0 {
		return dx*dx+dy*dy > shape.hole*shape.hole
	}
	return true
}

func generateSegments(shapes []analyticShape) []BoundarySegment {
	segments := make([]BoundarySegment, 0, len(shapes)*2*segmentsPerLoop)
	for _, shape := range shapes {
		segments = append(segments, generateLoopSegments(shape, "outer", shape.rx, shape.ry)...)
		if shape.kind == shapeAnnulus && shape.hole > 0 {
			segments = append(segments, generateLoopSegments(shape, "hole-0", shape.hole, shape.hole)...)
		}
	}
	return segments
}

func generateLoopSegments(shape analyticShape, loop string, rx, ry float64) []BoundarySegment {
	segments := make([]BoundarySegment, 0, segmentsPerLoop)
	for i := 0; i < segmentsPerLoop; i++ {
		start := float64(i) / segmentsPerLoop
		end := float64(i+1) / segmentsPerLoop
		mid := 2 * math.Pi * (start + end) / 2
		curvature := analyticCurvature(rx, ry, mid)
		points := make([]Point, 0, pointsPerSegment)
		for j := 0; j < pointsPerSegment; j++ {
			u := start + (end-start)*(float64(j)+0.5)/pointsPerSegment
			t := 2 * math.Pi * u
			points = append(points, Point{
				X: shape.cx + rx*math.Cos(t),
				Y: shape.cy + ry*math.Sin(t),
			})
		}
		segments = append(segments, BoundarySegment{
			ID:          shape.id + "-" + loop + "-s" + itoa(i),
			ComponentID: shape.id,
			Loop:        loop,
			Start:       start,
			End:         end,
			Curvature:   curvature,
			Points:      points,
		})
	}
	return segments
}

func analyticCurvature(rx, ry, t float64) float64 {
	if rx <= 0 || ry <= 0 {
		return 0
	}
	sinT, cosT := math.Sin(t), math.Cos(t)
	numerator := rx * ry
	denominator := math.Pow(rx*rx*sinT*sinT+ry*ry*cosT*cosT, 1.5)
	if denominator == 0 {
		return 0
	}
	return numerator / denominator
}

func generateRoughMask(reference Mask, shapes []analyticShape, componentMasks []Mask, rng *rand.Rand, seed int64) Mask {
	rough := reference.Clone()
	// Every image intentionally exhibits all three defect families: inward
	// bites, outward growth, and enclosed holes/specks.
	for i, shape := range shapes {
		if i >= len(componentMasks) {
			continue
		}
		outward := boundaryPoint(shape, "outer", 0.08+0.10*float64(i%3))
		inward := boundaryPoint(shape, "outer", 0.58+0.07*float64(i%3))
		normalOut := outwardNormal(shape, "outer", 0.08+0.10*float64(i%3))
		normalIn := outwardNormal(shape, "outer", 0.58+0.07*float64(i%3))
		carveDisk(&rough, outward.X+normalOut.X*1.2, outward.Y+normalOut.Y*1.2, 2.0, 0)
		addDisk(&rough, inward.X+normalIn.X*1.1, inward.Y+normalIn.Y*1.1, 2.0)
		addDisk(&rough, outward.X+normalOut.X*5.5, outward.Y+normalOut.Y*5.5, 1.0)

		inside := boundaryPoint(shape, "outer", 0.35+0.05*float64(i))
		insideNormal := outwardNormal(shape, "outer", 0.35+0.05*float64(i))
		holeX := inside.X - insideNormal.X*6.0
		holeY := inside.Y - insideNormal.Y*6.0
		if reference.At(int(holeX), int(holeY)) != 0 {
			carveDisk(&rough, holeX, holeY, 1.2, 0)
		}
	}
	// The PRNG decides one extra micro-speck so dimensions and seeds remain
	// visible in deterministic test samples without changing defect families.
	extraX := 6 + rng.Intn(maxInt(1, reference.Width-12))
	extraY := 6 + rng.Intn(maxInt(1, reference.Height-12))
	if reference.At(extraX, extraY) == 0 {
		addDisk(&rough, float64(extraX), float64(extraY), 1.0)
	}
	// Seed is mixed into a final deterministic polarity jitter only outside the
	// reference to avoid perturbing analytic truth.
	jitter := 3 + int(seed%5)
	for k := 0; k < jitter; k++ {
		x := (int(seed/7) + k*17) % maxInt(1, reference.Width)
		y := (int(seed/13) + k*29) % maxInt(1, reference.Height)
		if reference.At(x, y) == 0 && rough.At(x, y) == 0 {
			rough.Set(x, y, 1)
		}
	}
	return rough
}

func boundaryPoint(shape analyticShape, loop string, u float64) Point {
	rx, ry := shape.rx, shape.ry
	if loop != "outer" && shape.hole > 0 {
		rx, ry = shape.hole, shape.hole
	}
	t := 2 * math.Pi * u
	return Point{X: shape.cx + rx*math.Cos(t), Y: shape.cy + ry*math.Sin(t)}
}

func outwardNormal(shape analyticShape, loop string, u float64) Point {
	rx, ry := shape.rx, shape.ry
	if loop != "outer" && shape.hole > 0 {
		rx, ry = shape.hole, shape.hole
	}
	t := 2 * math.Pi * u
	nx := math.Cos(t) / rx
	ny := math.Sin(t) / ry
	length := math.Hypot(nx, ny)
	if length == 0 {
		return Point{}
	}
	return Point{X: nx / length, Y: ny / length}
}

func addDisk(mask *Mask, cx, cy, radius float64) {
	r2 := radius * radius
	minX := maxInt(0, int(math.Floor(cx-radius)))
	maxX := minInt(mask.Width-1, int(math.Ceil(cx+radius)))
	minY := maxInt(0, int(math.Floor(cy-radius)))
	maxY := minInt(mask.Height-1, int(math.Ceil(cy+radius)))
	for y := minY; y <= maxY; y++ {
		for x := minX; x <= maxX; x++ {
			dx, dy := float64(x)+0.5-cx, float64(y)+0.5-cy
			if dx*dx+dy*dy <= r2 {
				mask.Set(x, y, 1)
			}
		}
	}
}

func carveDisk(mask *Mask, cx, cy, radius float64, value byte) {
	r2 := radius * radius
	minX := maxInt(0, int(math.Floor(cx-radius)))
	maxX := minInt(mask.Width-1, int(math.Ceil(cx+radius)))
	minY := maxInt(0, int(math.Floor(cy-radius)))
	maxY := minInt(mask.Height-1, int(math.Ceil(cy+radius)))
	for y := minY; y <= maxY; y++ {
		for x := minX; x <= maxX; x++ {
			dx, dy := float64(x)+0.5-cx, float64(y)+0.5-cy
			if dx*dx+dy*dy <= r2 {
				mask.Set(x, y, value)
			}
		}
	}
}

func renderIcon(width, height int, reference Mask, shapes []analyticShape, rng *rand.Rand, hue int) []byte {
	rgba := make([]byte, width*height*4)
	bgA := rgbaFromHue(hue+18, 42, 54)
	bgB := rgbaFromHue(hue+24, 60, 72)
	nearFg := rgbaFromHue(hue-10, 92, 110)
	stroke := rgbaFromHue(hue+8, 182, 190)
	for y := 0; y < height; y++ {
		t := float64(y) / float64(maxInt(1, height-1))
		for x := 0; x < width; x++ {
			bg := mixRGBA(bgA, bgB, t)
			bg = mixRGBA(bg, nearFg, 0.16)
			pixel := bg
			idx := y*width + x
			if reference.At(x, y) != 0 {
				pixel = foregroundAt(shapes, float64(x)+0.5, float64(y)+0.5, bg)
				if boundaryDistance(reference, x, y, 2) {
					pixel = mixRGBA(pixel, stroke, 0.58)
				}
			} else if boundaryDistance(reference, x, y, 2) {
				pixel = mixRGBA(bg, stroke, 0.42)
				pixel.A = 150
			}
			noise := rng.Intn(15) - 7
			pixel.R = clampByte(int(pixel.R) + noise)
			pixel.G = clampByte(int(pixel.G) + noise/2)
			pixel.B = clampByte(int(pixel.B) - noise)
			rgba[idx*4] = pixel.R
			rgba[idx*4+1] = pixel.G
			rgba[idx*4+2] = pixel.B
			rgba[idx*4+3] = pixel.A
		}
	}
	return rgba
}

func foregroundAt(shapes []analyticShape, x, y float64, fallback color.RGBA) color.RGBA {
	for _, shape := range shapes {
		if shapeContains(shape, x, y) {
			return shape.foreColor
		}
	}
	return fallback
}

func boundaryDistance(mask Mask, x, y, radius int) bool {
	for dy := -radius; dy <= radius; dy++ {
		for dx := -radius; dx <= radius; dx++ {
			nx, ny := x+dx, y+dy
			if nx < 0 || ny < 0 || nx >= mask.Width || ny >= mask.Height {
				if dx*dx+dy*dy <= radius*radius {
					return true
				}
				continue
			}
			if mask.At(nx, ny) != mask.At(x, y) && dx*dx+dy*dy <= radius*radius {
				return true
			}
		}
	}
	return false
}

func encodePNG(rgba []byte, width, height int) []byte {
	img := image.NewNRGBA(image.Rect(0, 0, width, height))
	copy(img.Pix, rgba)
	var buffer bytes.Buffer
	encoder := png.Encoder{CompressionLevel: png.BestSpeed}
	if err := encoder.Encode(&buffer, img); err != nil {
		return nil
	}
	return buffer.Bytes()
}

func rgbaFromHue(hue, saturation, value int) color.RGBA {
	hue = ((hue % 360) + 360) % 360
	s := float64(saturation) / 255
	v := float64(value) / 255
	c := v * s
	x := c * (1 - math.Abs(math.Mod(float64(hue)/60, 2)-1))
	m := v - c
	var r, g, b float64
	switch {
	case hue < 60:
		r, g = c, x
	case hue < 120:
		r, g = x, c
	case hue < 180:
		g, b = c, x
	case hue < 240:
		g, b = x, c
	case hue < 300:
		r, b = x, c
	default:
		r, b = c, x
	}
	return color.RGBA{
		R: clampByte(int((r + m) * 255)),
		G: clampByte(int((g + m) * 255)),
		B: clampByte(int((b + m) * 255)),
		A: 255,
	}
}

func mixRGBA(a, b color.RGBA, t float64) color.RGBA {
	return color.RGBA{
		R: clampByte(int(float64(a.R)*(1-t) + float64(b.R)*t)),
		G: clampByte(int(float64(a.G)*(1-t) + float64(b.G)*t)),
		B: clampByte(int(float64(a.B)*(1-t) + float64(b.B)*t)),
		A: clampByte(int(float64(a.A)*(1-t) + float64(b.A)*t)),
	}
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

func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	var digits [8]byte
	pos := len(digits)
	for v > 0 {
		pos--
		digits[pos] = byte('0' + v%10)
		v /= 10
	}
	return string(digits[pos:])
}
