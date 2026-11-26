package quantforensics

import "fmt"

const DefaultSeed int64 = 0x51A7EED

func DefaultPalette() Palette {
	entries := []PaletteEntry{
		{Index: 0, ID: "ink", Name: "Ink", Group: "neutral", Color: RGB{R: 34, G: 40, B: 49}, GainPPM: 1_000_000},
		{Index: 1, ID: "panel", Name: "Panel", Group: "neutral", Color: RGB{R: 57, G: 62, B: 70}, GainPPM: 1_000_000},
		{Index: 2, ID: "cloud", Name: "Cloud", Group: "neutral", Color: RGB{R: 238, G: 238, B: 238}, GainPPM: 1_000_000},
		{Index: 3, ID: "red", Name: "Signal Red", Group: "warm", Color: RGB{R: 230, G: 57, B: 70}, GainPPM: 1_000_000},
		{Index: 4, ID: "orange", Name: "Amber", Group: "warm", Color: RGB{R: 244, G: 162, B: 97}, GainPPM: 1_000_000},
		{Index: 5, ID: "green", Name: "Teal", Group: "cool", Color: RGB{R: 42, G: 157, B: 143}, GainPPM: 1_000_000},
		{Index: 6, ID: "blue", Name: "Dock Blue", Group: "cool", Color: RGB{R: 69, G: 123, B: 157}, GainPPM: 1_000_000},
		{Index: 7, ID: "purple", Name: "Violet", Group: "cool", Color: RGB{R: 123, G: 44, B: 191}, GainPPM: 1_000_000},
	}
	return Palette{Version: "palette-seed-v1", Organization: "grouped", Entries: entries}
}

func blockKindForIndex(i int) BlockKind {
	switch i {
	case 3, 5, 10, 14:
		return KindGradient
	case 6, 7, 11:
		return KindStroke
	case 8, 9, 13:
		return KindNoise
	case 15:
		return KindMixed
	default:
		return KindForeground
	}
}

func blendOver(dst, src RGB, alpha int) RGB {
	if alpha <= 0 {
		return dst
	}
	if alpha >= 255 {
		return src
	}
	inv := 255 - alpha
	return RGB{
		R: uint8((int(dst.R)*inv + int(src.R)*alpha) / 255),
		G: uint8((int(dst.G)*inv + int(src.G)*alpha) / 255),
		B: uint8((int(dst.B)*inv + int(src.B)*alpha) / 255),
	}
}

func lerpRGB(a, b RGB, num, denom int) RGB {
	if denom <= 0 {
		return a
	}
	one := func(x, y uint8) uint8 {
		v := (int(x)*(denom-num) + int(y)*num) / denom
		return uint8(v)
	}
	return RGB{R: one(a.R, b.R), G: one(a.G, b.G), B: one(a.B, b.B)}
}

func drawBlock(img *Image, block Block, palette Palette, imageSalt uint64) {
	set := func(lx, ly int, c RGB) {
		if lx < 0 || ly < 0 || lx >= block.W || ly >= block.H {
			return
		}
		img.Pixels[img.Index(block.X+lx, block.Y+ly)] = c
	}
	pc := func(id int) RGB { return palette.Entries[id].Color }
	rngSeed := StableHash("sample", img.ID, block.ID, fmt.Sprint(imageSalt))

	switch block.Kind {
	case KindGradient:
		a, b := pc(4), pc(3)
		if block.Index%2 == 0 {
			a, b = pc(6), pc(5)
		}
		denom := block.W + block.H - 2
		for y := 0; y < block.H; y++ {
			for x := 0; x < block.W; x++ {
				draw := x + y
				if block.Index%3 == 0 {
					draw = x*2 + y
					denom = 2*(block.W-1) + block.H - 1
				}
				set(x, y, lerpRGB(a, b, draw, denom))
			}
		}
	case KindStroke:
		for y := 0; y < block.H; y++ {
			for x := 0; x < block.W; x++ {
				set(x, y, pc(2))
			}
		}
		for i := -2; i < block.W+2; i++ {
			x := i
			y := block.H - 1 - i
			for thickness := -1; thickness <= 1; thickness++ {
				xx, yy := x, y+thickness
				if xx >= 0 && xx < block.W && yy >= 0 && yy < block.H {
					set(xx, yy, pc(0))
				}
			}
		}
		for i := 0; i < block.W; i++ {
			x := i
			y := 2 + i/2
			if x < block.W && y < block.H {
				bg := img.At(block.X+x, block.Y+y)
				set(x, y, blendOver(bg, RGB{R: 120, G: 220, B: 255}, 150))
			}
		}
	case KindNoise:
		base := pc(6)
		if block.Index%2 == 1 {
			base = pc(1)
		}
		for y := 0; y < block.H; y++ {
			for x := 0; x < block.W; x++ {
				h := splitMix64(rngSeed ^ uint64(y*block.W+x))
				n := int(h%37) - 18
				set(x, y, clampRGB(int(base.R)+n, int(base.G)+n/2, int(base.B)-n/3))
			}
		}
	case KindMixed:
		for y := 0; y < block.H; y++ {
			for x := 0; x < block.W; x++ {
				set(x, y, pc(1))
			}
		}
		for y := 1; y < 6; y++ {
			for x := 1; x < 5; x++ {
				set(x, y, pc(7))
			}
		}
		for x := 5; x < 8; x++ {
			set(x, 2, pc(4))
			set(x, 3, pc(3))
		}
	default:
		bg := pc(1)
		fg := pc([]int{3, 5, 6, 7, 4}[block.Index%5])
		for y := 0; y < block.H; y++ {
			for x := 0; x < block.W; x++ {
				set(x, y, bg)
			}
		}
		for y := 1; y < 7; y++ {
			for x := 1; x < 7; x++ {
				inside := (x-3)*(x-3)+(y-4)*(y-4) <= 9 || (x >= 2 && x <= 5 && y >= 2 && y <= 6)
				if inside {
					set(x, y, fg)
				}
			}
		}
		if block.Index%4 == 0 {
			for x := 2; x < 6; x++ {
				set(x, 1, pc(2))
			}
		}
	}
}

// GenerateSampleSet creates the fixed-seed screenshot batch and construction
// truth. It uses only local integer operations and deterministic hashes.
func GenerateSampleSet(seed int64) SampleSet {
	if seed == 0 {
		seed = DefaultSeed
	}
	palette := DefaultPalette()
	const width, height, tile = 32, 32, 8
	imageIDs := []string{"shot-main", "shot-atlas", "shot-panel"}
	set := SampleSet{Seed: seed, ProjectID: "color-difference-rework"}

	for imgIdx, id := range imageIDs {
		im := Image{ID: id, Width: width, Height: height, Pixels: make([]RGB, width*height)}
		for i := range im.Pixels {
			im.Pixels[i] = palette.Entries[1].Color
		}
		for row := 0; row < height/tile; row++ {
			for col := 0; col < width/tile; col++ {
				idx := row*(width/tile) + col
				blockID := fmt.Sprintf("%s-b%02d", id, idx)
				b := Block{
					ID: blockID, ImageID: id, Index: idx, Col: col, Row: row,
					X: col * tile, Y: row * tile, W: tile, H: tile,
					Kind: blockKindForIndex(idx),
				}
				drawBlock(&im, b, palette, uint64(seed)+uint64(imgIdx)*1009)
				set.Blocks = append(set.Blocks, b)
			}
		}
		im.SHA256 = HashHex(pixelsBytes(im.Pixels))
		set.Images = append(set.Images, im)
	}

	imageByID := map[string]Image{}
	for _, im := range set.Images {
		imageByID[im.ID] = im
	}
	for _, b := range set.Blocks {
		im := imageByID[b.ImageID]
		truth := BlockTruth{BlockID: b.ID, ImageID: b.ImageID, Construction: string(b.Kind)}
		counts := make([]int, len(palette.Entries))
		truth.ExpectedIndices = make([]int, b.W*b.H)
		for y := 0; y < b.H; y++ {
			for x := 0; x < b.W; x++ {
				src := im.At(b.X+x, b.Y+y)
				idx, _ := NearestPaletteIndex(palette, src, MetricWeightedRGB)
				local := y*b.W + x
				truth.ExpectedIndices[local] = idx
				counts[idx]++
				entry, _ := palette.ByIndex(idx)
				got := entry.EffectiveColor().Ints()
				srcInts := src.Ints()
				perr := [3]int{srcInts[0] - got[0], srcInts[1] - got[1], srcInts[2] - got[2]}
				truth.Pixels = append(truth.Pixels, PixelTruth{X: x, Y: y, ExpectedIndex: idx, QuantError: perr})
			}
		}
		primary := 0
		for i, n := range counts {
			if n > counts[primary] {
				primary = i
			}
		}
		truth.ExpectedPrimaryIndex = primary
		ev, _, _ := EvaluateIndices(im, b, truth.ExpectedIndices, palette, MetricWeightedRGB, Tolerance{MaxComponentDeviation: 1 << 20, MaxWeightedColorDiff: 1 << 40})
		truth.ExpectedMaxDeviation = ev.MaxDeviation
		truth.ExpectedWeightedDiff = ev.WeightedColorDiff
		set.Truths = append(set.Truths, truth)
	}

	set.DirtyRecords = []DirtyRecord{
		{PanelID: "panel-palette", ImageID: imageIDs[0], BlockID: imageIDs[0] + "-b03", Reason: "palette_entry_edit", EditPath: "$.palette.entries[4].color"},
		{PanelID: "panel-palette", ImageID: imageIDs[0], BlockID: imageIDs[0] + "-b05", Reason: "palette_entry_edit", EditPath: "$.palette.entries[5].gain"},
		{PanelID: "panel-layout", ImageID: imageIDs[1], BlockID: imageIDs[1] + "-b09", Reason: "geometry_coordinate_edit", EditPath: "$.blocks[9].col"},
		{PanelID: "panel-dither", ImageID: imageIDs[2], BlockID: imageIDs[2] + "-b10", Reason: "dither_parameter_edit", EditPath: "$.blocks[10].ditherPermyriad"},
	}
	return set
}

func pixelsBytes(pixels []RGB) []byte {
	b := make([]byte, len(pixels)*3)
	for i, c := range pixels {
		b[i*3] = c.R
		b[i*3+1] = c.G
		b[i*3+2] = c.B
	}
	return b
}

// DefaultProject wires generated samples into the canonical internal model.
func DefaultProject() (*Project, SampleSet) {
	sample := GenerateSampleSet(DefaultSeed)
	p := &Project{
		Schema:       InternalSchema,
		SourceSchema: InternalSchema,
		ProjectID:    sample.ProjectID,
		Seed:         sample.Seed,
		OriginX:      0,
		OriginY:      0,
		TileW:        8,
		TileH:        8,
		Images:       sample.Images,
		Blocks:       sample.Blocks,
		Palette:      DefaultPalette(),
		Params: Params{
			Metric:             MetricWeightedRGB,
			QuantScalePPM:      1_000_000,
			GainPPM:            1_000_000,
			GlobalDitherPPM:    0,
			ParamVersion:       ParamFormatVersion,
			AlgoVersion:        AlgoVersion,
			CoordDeriveVersion: CoordDeriveVersion,
		},
		Tolerance: Tolerance{MaxComponentDeviation: 56, MaxWeightedColorDiff: 72},
	}
	return p, sample
}
