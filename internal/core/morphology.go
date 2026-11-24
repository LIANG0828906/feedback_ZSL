package core

import "math"

func MorphOpen(src Image, radius int) (Image, error) {
	if radius < 0 || radius > 16 {
		return Image{}, NewError(CodeParam, "openRadius", "open radius must be in [0,16]")
	}
	if radius == 0 {
		return src.Clone(), nil
	}
	eroded := morphExtreme(src, radius, false)
	return morphExtreme(eroded, radius, true), nil
}

func MorphClose(src Image, radius int) (Image, error) {
	if radius < 0 || radius > 16 {
		return Image{}, NewError(CodeParam, "closeRadius", "close radius must be in [0,16]")
	}
	if radius == 0 {
		return src.Clone(), nil
	}
	dilated := morphExtreme(src, radius, true)
	return morphExtreme(dilated, radius, false), nil
}

// morphExtreme performs square morphology as separable horizontal/vertical
// passes. Pixels outside the image are treated as zero for both dilation and
// erosion, which keeps the result independent of traversal order.
func morphExtreme(src Image, radius int, dilate bool) Image {
	tmp := NewImage(src.Width, src.Height, src.Channels)
	out := NewImage(src.Width, src.Height, src.Channels)
	for c := 0; c < src.Channels; c++ {
		for y := 0; y < src.Height; y++ {
			for x := 0; x < src.Width; x++ {
				var best int
				if dilate {
					best = 0
				} else {
					best = 255
				}
				for dx := -radius; dx <= radius; dx++ {
					xx := x + dx
					if xx < 0 || xx >= src.Width {
						if !dilate {
							best = 0
						}
						continue
					}
					v := int(src.At(xx, y, c))
					if dilate {
						best = max(best, v)
					} else {
						best = min(best, v)
					}
				}
				tmp.Set(x, y, c, byte(best))
			}
		}
		for y := 0; y < src.Height; y++ {
			for x := 0; x < src.Width; x++ {
				var best int
				if dilate {
					best = 0
				} else {
					best = 255
				}
				for dy := -radius; dy <= radius; dy++ {
					yy := y + dy
					if yy < 0 || yy >= src.Height {
						if !dilate {
							best = 0
						}
						continue
					}
					v := int(tmp.At(x, yy, c))
					if dilate {
						best = max(best, v)
					} else {
						best = min(best, v)
					}
				}
				out.Set(x, y, c, byte(best))
			}
		}
	}
	return out
}

func Thin(src Image, iterations int) (Image, error) {
	if iterations < 0 || iterations > 32 {
		return Image{}, NewError(CodeParam, "thinIterations", "thin iterations must be in [0,32]")
	}
	out := src.Clone()
	if iterations == 0 {
		return out, nil
	}
	for c := 0; c < out.Channels; c++ {
		plane := channelPlane(out, c)
		for iter := 0; iter < iterations; iter++ {
			changed := false
			for step := 0; step < 2; step++ {
				marks := make([]int, 0, out.Width*out.Height/16)
				for y := 1; y < out.Height-1; y++ {
					for x := 1; x < out.Width-1; x++ {
						if plane[y*out.Width+x] == 0 {
							continue
						}
						p2 := bit(plane, out.Width, x, y-1)
						p3 := bit(plane, out.Width, x+1, y-1)
						p4 := bit(plane, out.Width, x+1, y)
						p5 := bit(plane, out.Width, x+1, y+1)
						p6 := bit(plane, out.Width, x, y+1)
						p7 := bit(plane, out.Width, x-1, y+1)
						p8 := bit(plane, out.Width, x-1, y)
						p9 := bit(plane, out.Width, x-1, y-1)
						b := p2 + p3 + p4 + p5 + p6 + p7 + p8 + p9
						if b < 2 || b > 6 {
							continue
						}
						a := transitions(p2, p3, p4, p5, p6, p7, p8, p9)
						if a != 1 {
							continue
						}
						if step == 0 {
							if p2*p4*p6 != 0 || p4*p6*p8 != 0 {
								continue
							}
						} else {
							if p2*p4*p8 != 0 || p2*p6*p8 != 0 {
								continue
							}
						}
						marks = append(marks, y*out.Width+x)
					}
				}
				if len(marks) > 0 {
					changed = true
				}
				for _, i := range marks {
					plane[i] = 0
				}
			}
			if !changed {
				break
			}
		}
		for y := 0; y < out.Height; y++ {
			for x := 0; x < out.Width; x++ {
				on := plane[y*out.Width+x] != 0
				if on {
					out.Set(x, y, c, 255)
				} else {
					out.Set(x, y, c, 0)
				}
			}
		}
	}
	return out, nil
}

func PruneShortComponents(src Image, maximumLength int) (Image, error) {
	if maximumLength < 0 || maximumLength > 256 {
		return Image{}, NewError(CodeParam, "pruneLength", "prune length must be in [0,256]")
	}
	out := src.Clone()
	if maximumLength == 0 {
		return out, nil
	}
	w, h := src.Width, src.Height
	for c := 0; c < src.Channels; c++ {
		plane := channelPlane(out, c)
		visited := make([]bool, w*h)
		for y := 0; y < h; y++ {
			for x := 0; x < w; x++ {
				start := y*w + x
				if plane[start] == 0 || visited[start] {
					continue
				}
				queue := []int{start}
				visited[start] = true
				component := []int{start}
				for head := 0; head < len(queue); head++ {
					i := queue[head]
					xx, yy := i%w, i/w
					for dy := -1; dy <= 1; dy++ {
						ny := yy + dy
						if ny < 0 || ny >= h {
							continue
						}
						for dx := -1; dx <= 1; dx++ {
							if dx == 0 && dy == 0 {
								continue
							}
							nx := xx + dx
							if nx < 0 || nx >= w {
								continue
							}
							j := ny*w + nx
							if plane[j] != 0 && !visited[j] {
								visited[j] = true
								queue = append(queue, j)
								component = append(component, j)
							}
						}
					}
				}
				if len(component) <= maximumLength {
					for _, i := range component {
						plane[i] = 0
					}
				}
			}
		}
		for y := 0; y < h; y++ {
			for x := 0; x < w; x++ {
				if plane[y*w+x] != 0 {
					out.Set(x, y, c, 255)
				} else {
					out.Set(x, y, c, 0)
				}
			}
		}
	}
	return out, nil
}

func channelPlane(im Image, channel int) []byte {
	plane := make([]byte, im.Width*im.Height)
	for i := range plane {
		plane[i] = im.Pix[i*im.Channels+channel]
	}
	return plane
}

func bit(plane []byte, width, x, y int) int {
	if plane[y*width+x] == 0 {
		return 0
	}
	return 1
}

func transitions(p2, p3, p4, p5, p6, p7, p8, p9 int) int {
	points := [9]int{p2, p3, p4, p5, p6, p7, p8, p9, p2}
	n := 0
	for i := 0; i < 8; i++ {
		if points[i] == 0 && points[i+1] == 1 {
			n++
		}
	}
	return n
}

func Downsample(src Image, factor int) Image {
	if factor <= 1 {
		return src.Clone()
	}
	w := int(math.Ceil(float64(src.Width) / float64(factor)))
	h := int(math.Ceil(float64(src.Height) / float64(factor)))
	out := NewImage(w, h, src.Channels)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			for c := 0; c < src.Channels; c++ {
				sum, count := 0, 0
				for dy := 0; dy < factor; dy++ {
					sy := y*factor + dy
					if sy >= src.Height {
						break
					}
					for dx := 0; dx < factor; dx++ {
						sx := x*factor + dx
						if sx >= src.Width {
							break
						}
						sum += int(src.At(sx, sy, c))
						count++
					}
				}
				out.Set(x, y, c, byte((sum+count/2)/max(1, count)))
			}
		}
	}
	return out
}

func UpsampleNearest(src Image, width, height int) Image {
	out := NewImage(width, height, src.Channels)
	for y := 0; y < height; y++ {
		sy := min(src.Height-1, y*src.Height/height)
		for x := 0; x < width; x++ {
			sx := min(src.Width-1, x*src.Width/width)
			for c := 0; c < src.Channels; c++ {
				out.Set(x, y, c, src.At(sx, sy, c))
			}
		}
	}
	return out
}
