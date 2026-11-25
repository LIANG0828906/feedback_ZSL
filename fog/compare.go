package fog

import (
	"fmt"
	"math"
)

type FrameComparison struct {
	SameCoverage        bool      `json:"same_coverage"`
	SameSnapshotVersion bool      `json:"same_media_snapshot"`
	SameCameraVersion   bool      `json:"same_camera_version"`
	Tolerance           float64   `json:"tolerance"`
	MaxDifference       float64   `json:"max_difference"`
	MeanDifference      float64   `json:"mean_difference"`
	ComparedPixels      int       `json:"compared_pixels"`
	FirstMismatch       *PixelRef `json:"first_mismatch,omitempty"`
	FirstDifference     float64   `json:"first_difference"`
	Diverged            bool      `json:"diverged"`
}

func CompareFrames(a, b *Frame, tolerance float64) (*FrameComparison, *Fault) {
	if a == nil || b == nil {
		return nil, &Fault{Code: EPara, Path: "frames", Message: "both frames are required"}
	}
	c := &FrameComparison{
		SameCoverage:        a.Width == b.Width && a.Height == b.Height && len(a.Pixels) == len(b.Pixels),
		SameSnapshotVersion: a.MediaSnapshotVersion == b.MediaSnapshotVersion,
		SameCameraVersion:   a.CameraVersion == b.CameraVersion,
		Tolerance:           tolerance,
	}
	if !c.SameSnapshotVersion || !c.SameCameraVersion {
		return c, &Fault{Code: EDiverge, Path: "frame.version", Actual: []string{a.MediaSnapshotVersion, a.CameraVersion}, Expected: []string{b.MediaSnapshotVersion, b.CameraVersion}, Message: "preview and exact frames use different snapshot or camera versions"}
	}
	if !c.SameCoverage {
		c.Diverged = true
		px := &PixelRef{}
		for i := 0; i < minInt(len(a.Pixels), len(b.Pixels)); i++ {
			if a.Pixels[i].X != b.Pixels[i].X || a.Pixels[i].Y != b.Pixels[i].Y {
				px.X, px.Y = a.Pixels[i].X, a.Pixels[i].Y
				break
			}
		}
		return c, &Fault{Code: EDiverge, Path: "frame.coverage", Pixel: px, Actual: len(a.Pixels), Expected: len(b.Pixels), Message: "preview and exact coverage differ"}
	}
	sum := 0.0
	for i := range a.Pixels {
		pa, pb := a.Pixels[i], b.Pixels[i]
		if pa.X != pb.X || pa.Y != pb.Y {
			c.SameCoverage = false
			c.Diverged = true
			ref := &PixelRef{X: pa.X, Y: pa.Y}
			return c, &Fault{Code: EDiverge, Path: "frame.coverage", Pixel: ref, Actual: []int{pa.X, pa.Y}, Expected: []int{pb.X, pb.Y}, Message: "pixel coverage coordinate mismatch"}
		}
		d := math.Abs(pa.Radiance - pb.Radiance)
		if pa.Missing != pb.Missing {
			d = math.MaxFloat64
		}
		if math.IsNaN(d) || math.IsInf(d, 1) {
			d = math.MaxFloat64
		}
		sum += d
		c.ComparedPixels++
		if d > c.MaxDifference {
			c.MaxDifference = d
		}
		if c.FirstMismatch == nil && d > tolerance {
			ref := PixelRef{X: pa.X, Y: pa.Y}
			c.FirstMismatch = &ref
			c.FirstDifference = d
		}
	}
	if c.ComparedPixels > 0 {
		c.MeanDifference = sum / float64(c.ComparedPixels)
	}
	if c.FirstMismatch != nil {
		c.Diverged = true
		return c, &Fault{
			Code: EDiverge, Path: "frame.pixel", Pixel: c.FirstMismatch,
			Actual: c.FirstDifference, Expected: tolerance,
			Message: fmt.Sprintf("first pixel exceeds tolerance; max difference %.9g", c.MaxDifference),
		}
	}
	return c, nil
}
