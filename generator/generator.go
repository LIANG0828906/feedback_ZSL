package generator

import (
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"

	"iconflow/model"
)

const FixedSeed int64 = 42042

type RNG struct{ state uint64 }

func NewRNG(seed int64) *RNG {
	return &RNG{state: uint64(seed) + 0x9e3779b97f4a7c15}
}

func (r *RNG) next() uint64 {
	z := r.state
	z += 0x9e3779b97f4a7c15
	r.state = z
	z = (z ^ (z >> 30)) * 0xbf58476d1ce4e5b9
	z = (z ^ (z >> 27)) * 0x94d049bb133111eb
	return z ^ (z >> 31)
}

func (r *RNG) Intn(n int) int {
	if n <= 0 {
		return 0
	}
	return int(r.next() % uint64(n))
}

type Spec struct {
	ID     string
	Width  int
	Height int
}

func DefaultSpecs() []Spec {
	return []Spec{
		{ID: "icon-32x32", Width: 32, Height: 32},
		{ID: "icon-48x32", Width: 48, Height: 32},
		{ID: "icon-64x48", Width: 64, Height: 48},
	}
}

func GenerateProject(seed int64) *model.Project {
	if seed == 0 {
		seed = FixedSeed
	}
	rng := NewRNG(seed)
	project := &model.Project{
		SchemaVersion:      model.SchemaV3,
		SourceVersion:      "generator-canonical",
		ProjectID:          fmt.Sprintf("icon-flow-%d", seed),
		Seed:               seed,
		BlockWidth:         16,
		BlockHeight:        16,
		ThresholdPermyriad: 8500,
		ChannelOrder:       []string{"R", "G", "B", "A"},
		ChannelLayout:      model.LayoutGrouped,
		ColorTolerance:     intPointer(12),
		Quant: model.QuantSpec{
			Step: 6, Rounding: model.RoundFloor, ExtentDivisible: false,
		},
		ParamVersion: "param-2026-11-27",
		Rules: model.Rules{
			GradientOperator:  "sobel",
			OperatorVersion:   "sobel-v1",
			QuantilePermyriad: 8500,
			MinContrast:       96,
			ThresholdMin:      24,
			ThresholdMax:      1200,
			MinComponentArea:  6,
			Connectivity:      8,
			FillHoles:         true,
			Tolerances: model.Tolerances{
				AreaAbs:          72,
				CentroidMilliAbs: 1750,
			},
		},
		Truth: map[string]*model.BlockTruth{},
	}

	for _, spec := range DefaultSpecs() {
		img, truth := generateImage(rng, spec, project)
		project.Images = append(project.Images, img)
		for key, blockTruth := range truth {
			project.Truth[key] = blockTruth
		}
	}

	for _, img := range project.Images {
		project.Panels = append(project.Panels, model.Panel{
			ID:     "panel-" + img.ID,
			Blocks: blockKeys(project, img.ID),
		})
	}

	coords := project.SortedBlockCoords()
	if len(coords) >= 3 {
		project.Dirty = []model.DirtyRecord{
			{PanelID: "panel-" + coords[0].ImageID, Coord: coords[0].Key(), Fields: []string{"threshold_scale"}, Reason: "reviewer found weak contour"},
			{PanelID: "panel-" + coords[1].ImageID, Coord: coords[1].Key(), Fields: []string{"channel_order"}, Reason: "export swizzle suspect"},
			{PanelID: "panel-" + coords[len(coords)-1].ImageID, Coord: coords[len(coords)-1].Key(), Fields: []string{"threshold_scale", "quant_step"}, Reason: "quantized edge audit"},
		}
	}
	return project
}

func intPointer(v int) *int { return &v }

func blockKeys(p *model.Project, imageID string) []string {
	var keys []string
	for _, coord := range p.SortedBlockCoords() {
		if coord.ImageID == imageID {
			keys = append(keys, coord.Key())
		}
	}
	return keys
}

func generateImage(rng *RNG, spec Spec, project *model.Project) (*model.Image, map[string]*model.BlockTruth) {
	channelCount := 4
	flat := make([]int, spec.Width*spec.Height*channelCount)
	planes := make([][]int, channelCount)
	for ch := range planes {
		planes[ch] = make([]int, spec.Width*spec.Height)
	}

	bg := [4]int{28 + rng.Intn(8), 34 + rng.Intn(8), 48 + rng.Intn(10), 255}
	fg := [4]int{224 + rng.Intn(28), 198 + rng.Intn(35), 104 + rng.Intn(55), 255}
	for y := 0; y < spec.Height; y++ {
		for x := 0; x < spec.Width; x++ {
			setPixel(flat, planes, spec.Width, x, y, bg)
		}
	}

	truth := map[string]*model.BlockTruth{}
	cols := spec.Width / project.BlockWidth
	rows := spec.Height / project.BlockHeight
	for ty := 0; ty < rows; ty++ {
		for tx := 0; tx < cols; tx++ {
			bx := tx * project.BlockWidth
			by := ty * project.BlockHeight
			margin := 3 + rng.Intn(2)
			rw := project.BlockWidth - margin*2 - rng.Intn(2)
			rh := project.BlockHeight - margin*2 - rng.Intn(2)
			x0 := bx + margin
			y0 := by + margin
			x1 := x0 + rw - 1
			y1 := y0 + rh - 1

			blockMask := make([]bool, project.BlockWidth*project.BlockHeight)
			for y := y0; y <= y1; y++ {
				for x := x0; x <= x1; x++ {
					color := fg
					if rng.Intn(17) == 0 {
						color = [4]int{min(255, fg[0]+12), min(255, fg[1]+12), min(255, fg[2]+12), 255}
					}
					setPixel(flat, planes, spec.Width, x, y, color)
					blockMask[(y-by)*project.BlockWidth+(x-bx)] = true
				}
			}

			coord := model.BlockCoord{
				ImageID: spec.ID, TX: tx, TY: ty,
				OriginX: bx, OriginY: by,
				Width: project.BlockWidth, Height: project.BlockHeight,
			}
			truth[coord.Key()] = truthFromMask(coord, blockMask)
		}
	}

	raster := &model.Raster{
		Width: spec.Width, Height: spec.Height,
		Channels: []string{"R", "G", "B", "A"},
		Layout:   model.LayoutGrouped,
		Flat:     flat, Planes: planes,
	}
	return &model.Image{ID: spec.ID, Path: "images/" + spec.ID + ".png", Raster: raster}, truth
}

func setPixel(flat []int, planes [][]int, width, x, y int, rgba [4]int) {
	pixel := y*width + x
	for ch := 0; ch < 4; ch++ {
		flat[pixel*4+ch] = rgba[ch]
		planes[ch][pixel] = rgba[ch]
	}
}

func truthFromMask(coord model.BlockCoord, mask []bool) *model.BlockTruth {
	var count, sumX, sumY int64
	var pixels [][2]int
	for y := 0; y < coord.Height; y++ {
		for x := 0; x < coord.Width; x++ {
			if !mask[y*coord.Width+x] {
				continue
			}
			count++
			sumX += int64(x)
			sumY += int64(y)
			pixels = append(pixels, [2]int{x, y})
		}
	}
	if count == 0 {
		return &model.BlockTruth{Coord: coord}
	}
	return &model.BlockTruth{
		Coord:      coord,
		Foreground: int(count),
		Components: []model.ComponentTruth{{
			Area: int(count),
			Centroid: model.Centroid{
				XMilli: sumX * 1000 / count,
				YMilli: sumY * 1000 / count,
			},
		}},
	}
}

func WritePNGs(dir string, p *model.Project) error {
	imageDir := filepath.Join(dir, "images")
	if err := os.MkdirAll(imageDir, 0o755); err != nil {
		return err
	}
	for _, img := range p.Images {
		r := img.Raster
		out := image.NewRGBA(image.Rect(0, 0, r.Width, r.Height))
		for y := 0; y < r.Height; y++ {
			for x := 0; x < r.Width; x++ {
				i := y*r.Width + x
				out.SetRGBA(x, y, color.RGBA{
					R: uint8(r.Planes[0][i]),
					G: uint8(r.Planes[1][i]),
					B: uint8(r.Planes[2][i]),
					A: uint8(r.Planes[3][i]),
				})
			}
		}
		path := filepath.Join(dir, img.Path)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		file, err := os.Create(path)
		if err != nil {
			return err
		}
		err = png.Encode(file, out)
		closeErr := file.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
	}
	return nil
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
