package editorpick

import "math"

// Camera is a deterministic orthographic editor camera looking toward +Z.
// Screen coordinates use top-left pixel origin.
type Camera struct {
	Width      int   `json:"width"`
	Height     int   `json:"height"`
	ViewHeight Fixed `json:"view_height"`
	Center     Vec3  `json:"center"`
	EyeZ       Fixed `json:"eye_z"`
}

func DefaultCamera() Camera {
	return Camera{
		Width:      800,
		Height:     600,
		ViewHeight: FixedFromFloat(16),
		Center:     Vec3{},
		EyeZ:       FixedFromFloat(-10),
	}
}

func (c Camera) ViewWidth() float64 {
	if c.Height == 0 {
		return 0
	}
	return c.ViewHeight.Float64() * float64(c.Width) / float64(c.Height)
}

// ScreenRay maps pixel coordinates to the same fixed-point ray representation
// used by Raycast. Coordinates outside the viewport are accepted because
// editors commonly issue picks while the cursor is near a viewport edge.
func (c Camera) ScreenRay(id string, screenX, screenY float64) RayQuery {
	viewWidth := c.ViewWidth()
	width := float64(c.Width)
	height := float64(c.Height)
	if width == 0 || height == 0 {
		width, height = 1, 1
	}
	worldX := c.Center.X.Float64() + (screenX/width-0.5)*viewWidth
	worldY := c.Center.Y.Float64() + (0.5-screenY/height)*c.ViewHeight.Float64()
	return RayQuery{
		ID:     id,
		Screen: [2]float64{screenX, screenY},
		Origin: Vec3FromFloat(worldX, worldY, c.EyeZ.Float64()),
		Dir:    Vec3FromFloat(0, 0, 1),
	}
}

func (c Camera) WorldToScreen(x, y float64) [2]float64 {
	viewWidth := c.ViewWidth()
	screenX := (x-c.Center.X.Float64())/viewWidth*float64(c.Width) + 0.5*float64(c.Width)
	screenY := (0.5 - (y-c.Center.Y.Float64())/c.ViewHeight.Float64()) * float64(c.Height)
	round := func(v float64) float64 { return math.Round(v*1000) / 1000 }
	return [2]float64{round(screenX), round(screenY)}
}
