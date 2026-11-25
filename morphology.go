package iconmask

import "math"

const (
	ShapeSquare  = "square"
	ShapeDisk    = "disk"
	ShapeDiamond = "diamond"
	ShapeCross   = "cross"
)

var LegalShapes = []string{ShapeSquare, ShapeDisk, ShapeDiamond, ShapeCross}

type Offset struct {
	X int
	Y int
}

type StructuringElement struct {
	Shape   string
	Radius  int
	Offsets []Offset
}

func NewStructuringElement(imageID, shape string, radius int) (StructuringElement, *Error) {
	if !legalShape(shape) {
		return StructuringElement{}, parameterError(
			imageID, "shape", "unsupported structuring element shape", LegalShapes...,
		)
	}
	if radius <= 0 {
		return StructuringElement{}, parameterError(
			imageID, "radius", "structuring element radius must be a positive integer",
			"positive integer",
		)
	}
	offsets := make([]Offset, 0, (2*radius+1)*(2*radius+1))
	for dy := -radius; dy <= radius; dy++ {
		for dx := -radius; dx <= radius; dx++ {
			inside := false
			switch shape {
			case ShapeSquare:
				inside = true
			case ShapeDisk:
				inside = dx*dx+dy*dy <= radius*radius
			case ShapeDiamond:
				inside = int(math.Abs(float64(dx))+math.Abs(float64(dy))) <= radius
			case ShapeCross:
				inside = dx == 0 || dy == 0
			}
			if inside {
				offsets = append(offsets, Offset{X: dx, Y: dy})
			}
		}
	}
	return StructuringElement{Shape: shape, Radius: radius, Offsets: offsets}, nil
}

func legalShape(shape string) bool {
	for _, candidate := range LegalShapes {
		if shape == candidate {
			return true
		}
	}
	return false
}

func Dilate(mask Mask, element StructuringElement) Mask {
	out := NewMask(mask.Width, mask.Height)
	for y := 0; y < mask.Height; y++ {
		for x := 0; x < mask.Width; x++ {
			if mask.At(x, y) == 0 {
				continue
			}
			for _, offset := range element.Offsets {
				nx, ny := x+offset.X, y+offset.Y
				if nx >= 0 && ny >= 0 && nx < mask.Width && ny < mask.Height {
					out.Data[ny*mask.Width+nx] = 1
				}
			}
		}
	}
	return out
}

func Erode(mask Mask, element StructuringElement) Mask {
	out := NewMask(mask.Width, mask.Height)
	for y := 0; y < mask.Height; y++ {
		for x := 0; x < mask.Width; x++ {
			keep := true
			for _, offset := range element.Offsets {
				nx, ny := x+offset.X, y+offset.Y
				if nx < 0 || ny < 0 || nx >= mask.Width || ny >= mask.Height || mask.Data[ny*mask.Width+nx] == 0 {
					keep = false
					break
				}
			}
			if keep {
				out.Data[y*mask.Width+x] = 1
			}
		}
	}
	return out
}

func MorphologicalGradient(mask Mask, element StructuringElement) Mask {
	dilated := Dilate(mask, element)
	eroded := Erode(mask, element)
	out := NewMask(mask.Width, mask.Height)
	for i := range out.Data {
		if dilated.Data[i] != 0 && eroded.Data[i] == 0 {
			out.Data[i] = 1
		}
	}
	return out
}

func Close(mask Mask, element StructuringElement) Mask {
	return Erode(Dilate(mask, element), element)
}

func Open(mask Mask, element StructuringElement) Mask {
	return Dilate(Erode(mask, element), element)
}
