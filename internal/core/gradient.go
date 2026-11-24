package core

import "math"

func GradientMagnitude(src Image, operator string) (Image, error) {
	if err := src.Validate(); err != nil {
		return Image{}, err
	}
	var kx, ky [9]int
	switch operator {
	case "sobel":
		kx = [9]int{-1, 0, 1, -2, 0, 2, -1, 0, 1}
		ky = [9]int{-1, -2, -1, 0, 0, 0, 1, 2, 1}
	case "prewitt":
		kx = [9]int{-1, 0, 1, -1, 0, 1, -1, 0, 1}
		ky = [9]int{-1, -1, -1, 0, 0, 0, 1, 1, 1}
	case "central":
		kx = [9]int{0, 0, 0, -1, 0, 1, 0, 0, 0}
		ky = [9]int{0, -1, 0, 0, 0, 0, 0, 1, 0}
	default:
		return Image{}, NewError(CodeParam, "operator", "operator must be sobel, prewitt, or central")
	}
	out := NewImage(src.Width, src.Height, src.Channels)
	for c := 0; c < src.Channels; c++ {
		for y := 0; y < src.Height; y++ {
			for x := 0; x < src.Width; x++ {
				gx, gy := 0, 0
				k := 0
				for dy := -1; dy <= 1; dy++ {
					yy := clamp(y+dy, 0, src.Height-1)
					for dx := -1; dx <= 1; dx++ {
						xx := clamp(x+dx, 0, src.Width-1)
						v := int(src.At(xx, yy, c))
						gx += v * kx[k]
						gy += v * ky[k]
						k++
					}
				}
				var magnitude float64
				switch operator {
				case "sobel":
					magnitude = (math.Abs(float64(gx)) + math.Abs(float64(gy))) / 4.0
				case "prewitt":
					magnitude = (math.Abs(float64(gx)) + math.Abs(float64(gy))) / 3.0
				default:
					magnitude = math.Hypot(float64(gx), float64(gy))
				}
				out.Set(x, y, c, byte(clamp(int(math.Round(magnitude)), 0, 255)))
			}
		}
	}
	return out, nil
}

func DualThresholdHysteresis(magnitude Image, low, high int) (Image, error) {
	if low < 0 || low > 255 {
		return Image{}, NewError(CodeParam, "lowThreshold", "low threshold must be in [0,255]")
	}
	if high < 0 || high > 255 {
		return Image{}, NewError(CodeParam, "highThreshold", "high threshold must be in [0,255]")
	}
	if low > high {
		return Image{}, NewError(CodeParam, "lowThreshold", "low threshold must not exceed high threshold")
	}
	if err := magnitude.Validate(); err != nil {
		return Image{}, err
	}
	out := NewImage(magnitude.Width, magnitude.Height, magnitude.Channels)
	for c := 0; c < magnitude.Channels; c++ {
		strong := make([]bool, magnitude.Width*magnitude.Height)
		weak := make([]bool, magnitude.Width*magnitude.Height)
		queue := make([]int, 0, magnitude.Width*magnitude.Height/16)
		for y := 0; y < magnitude.Height; y++ {
			for x := 0; x < magnitude.Width; x++ {
				i := y*magnitude.Width + x
				v := int(magnitude.At(x, y, c))
				if v >= high && (high > 0 || v >= low) {
					strong[i] = true
					queue = append(queue, i)
				} else if v >= low {
					weak[i] = true
				}
			}
		}
		if high == low {
			for i := range weak {
				strong[i] = true
				weak[i] = false
			}
			queue = queue[:0]
			for i := range strong {
				if strong[i] {
					queue = append(queue, i)
				}
			}
		}
		for head := 0; head < len(queue); head++ {
			i := queue[head]
			x, y := i%magnitude.Width, i/magnitude.Width
			for dy := -1; dy <= 1; dy++ {
				yy := y + dy
				if yy < 0 || yy >= magnitude.Height {
					continue
				}
				for dx := -1; dx <= 1; dx++ {
					if dx == 0 && dy == 0 {
						continue
					}
					xx := x + dx
					if xx < 0 || xx >= magnitude.Width {
						continue
					}
					j := yy*magnitude.Width + xx
					if weak[j] && !strong[j] {
						strong[j] = true
						queue = append(queue, j)
					}
				}
			}
		}
		for i, on := range strong {
			if on {
				out.Pix[i*magnitude.Channels+c] = 255
			}
		}
	}
	return out, nil
}
