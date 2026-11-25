package sample

import "channelpipe"

type rgbaColor struct {
	R, G, B, A int
}

func paintImage(spec ImageSpec, seed int64) *SourceImage {
	buffer := channelpipe.Buffer{
		Width:    spec.Width,
		Height:   spec.Height,
		Depth:    spec.Depth,
		Channels: append([]channelpipe.Channel(nil), spec.Layout...),
		Pixels:   make([]uint16, spec.Width*spec.Height*len(spec.Layout)),
	}
	blockColors := []rgbaColor{
		{R: 220, G: 30, B: 40, A: 255},
		{R: 35, G: 190, B: 70, A: 255},
		{R: 30, G: 90, B: 230, A: 255},
		{R: 245, G: 205, B: 40, A: 255},
	}
	_ = seed
	for y := 0; y < spec.Height; y++ {
		for x := 0; x < spec.Width; x++ {
			color := rgbaColor{R: 0, G: 0, B: 0, A: 255}
			switch {
			case y < spec.Height/4:
				block := x * 4 / spec.Width
				if block >= len(blockColors) {
					block = len(blockColors) - 1
				}
				color = blockColors[block]
			case y < spec.Height*3/4:
				bandHeight := spec.Height / 2
				bandY := y - spec.Height/4
				color.R = x * 255 / maxInt(1, spec.Width-1)
				color.G = bandY * 255 / maxInt(1, bandHeight-1)
				color.B = 128
				color.A = 255
			default:
				step := x * 5 / spec.Width
				if step > 4 {
					step = 4
				}
				color.R = 180
				color.G = 180
				color.B = 180
				color.A = step * 255 / 4
			}
			setNativePixel(&buffer, x, y, spec.Layout, color)
		}
	}
	return &SourceImage{ID: spec.ID, Buffer: buffer}
}

func setNativePixel(buffer *channelpipe.Buffer, x, y int, layout []channelpipe.Channel, color rgbaColor) {
	values := map[channelpipe.Channel]int{
		channelpipe.ChannelR: color.R,
		channelpipe.ChannelG: color.G,
		channelpipe.ChannelB: color.B,
		channelpipe.ChannelA: color.A,
	}
	base := (y*buffer.Width + x) * len(layout)
	for i, channel := range layout {
		buffer.Pixels[base+i] = scale8ToDepth(values[channel], buffer.Depth)
	}
}

func scale8ToDepth(value8, depth int) uint16 {
	if depth == 8 {
		if value8 < 0 {
			value8 = 0
		}
		if value8 > 255 {
			value8 = 255
		}
		return uint16(value8)
	}
	return uint16(uint64(value8) * channelpipe.DepthMax(16) / 255)
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
