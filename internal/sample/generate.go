package sample

import (
	"crypto/sha256"
	"encoding/hex"
	"image"
	"image/png"
	"os"

	"maskpipe/internal/model"
)

type shapeKind string

const (
	shapeRect    shapeKind = "rect"
	shapeEllipse shapeKind = "ellipse"
)

type shapeSpec struct {
	label string
	kind  shapeKind
	rect  image.Rectangle
	rgb   [3]int
}

type sceneSpec struct {
	id        string
	sceneType string
	w, h      int
	tw, th    int
	top       [3]int
	bottom    [3]int
	noiseAmp  int
	shapes    []shapeSpec
	params    model.ParameterSet
}

func defaultScenes() []sceneSpec {
	return []sceneSpec{
		{
			id: "blocks", sceneType: "color_blocks", w: 192, h: 128, tw: 64, th: 64,
			top: [3]int{28, 34, 66}, bottom: [3]int{76, 58, 104}, noiseAmp: 10,
			shapes: []shapeSpec{
				{label: "coral-rect", kind: shapeRect, rect: image.Rect(14, 18, 50, 50), rgb: [3]int{226, 76, 82}},
				{label: "teal-circle", kind: shapeEllipse, rect: image.Rect(141, 13, 179, 51), rgb: [3]int{54, 205, 184}},
				{label: "amber-rect", kind: shapeRect, rect: image.Rect(78, 78, 116, 112), rgb: [3]int{244, 196, 72}},
			},
			params: model.ParameterSet{
				ColorTolerance: 64, GradientThreshold: 48, StructureSize: 3,
				BinaryThreshold: 0.12, MinRegionArea: 80, MaxIterations: 20000,
			},
		},
		{
			id: "low-contrast", sceneType: "gradient_low_contrast", w: 240, h: 160, tw: 80, th: 80,
			top: [3]int{214, 221, 230}, bottom: [3]int{174, 186, 202}, noiseAmp: 5,
			shapes: []shapeSpec{
				{label: "mist-ellipse", kind: shapeEllipse, rect: image.Rect(18, 22, 62, 58), rgb: [3]int{164, 178, 202}},
				{label: "slate-rect", kind: shapeRect, rect: image.Rect(101, 20, 141, 58), rgb: [3]int{151, 166, 194}},
				{label: "blue-ellipse", kind: shapeEllipse, rect: image.Rect(178, 101, 222, 139), rgb: [3]int{132, 151, 188}},
			},
			params: model.ParameterSet{
				ColorTolerance: 38, GradientThreshold: 34, StructureSize: 3,
				BinaryThreshold: 0.10, MinRegionArea: 80, MaxIterations: 30000,
			},
		},
		{
			id: "embedded-noise", sceneType: "noise_embedded_targets", w: 256, h: 192, tw: 64, th: 64,
			top: [3]int{34, 42, 48}, bottom: [3]int{58, 70, 72}, noiseAmp: 18,
			shapes: []shapeSpec{
				{label: "lime-circle", kind: shapeEllipse, rect: image.Rect(14, 14, 50, 50), rgb: [3]int{142, 220, 92}},
				{label: "violet-rect", kind: shapeRect, rect: image.Rect(78, 78, 114, 114), rgb: [3]int{158, 112, 235}},
				{label: "orange-circle", kind: shapeEllipse, rect: image.Rect(142, 14, 178, 50), rgb: [3]int{246, 142, 58}},
				{label: "cyan-ellipse", kind: shapeEllipse, rect: image.Rect(205, 143, 243, 177), rgb: [3]int{72, 202, 232}},
			},
			params: model.ParameterSet{
				ColorTolerance: 76, GradientThreshold: 70, StructureSize: 3,
				BinaryThreshold: 0.10, MinRegionArea: 70, MaxIterations: 40000,
			},
		},
	}
}

func insideShape(s shapeSpec, x, y int) bool {
	switch s.kind {
	case shapeRect:
		return x >= s.rect.Min.X && x < s.rect.Max.X && y >= s.rect.Min.Y && y < s.rect.Max.Y
	case shapeEllipse:
		cx := float64(s.rect.Min.X+s.rect.Max.X) / 2
		cy := float64(s.rect.Min.Y+s.rect.Max.Y) / 2
		rx := float64(s.rect.Dx()) / 2
		ry := float64(s.rect.Dy()) / 2
		dx := (float64(x) + 0.5 - cx) / rx
		dy := (float64(y) + 0.5 - cy) / ry
		return dx*dx+dy*dy <= 1
	}
	return false
}

func clampByte(v int) uint8 {
	if v < 0 {
		return 0
	}
	if v > 255 {
		return 255
	}
	return uint8(v)
}

func writePNG(path string, img image.Image) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return png.Encode(f, img)
}

func fingerprintMask(mask []byte) string {
	sum := sha256.Sum256(mask)
	return hex.EncodeToString(sum[:])
}
