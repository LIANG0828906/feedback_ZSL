package pipeline

import (
	"fmt"
	"sort"

	"glyphbrush/internal/errs"
	"glyphbrush/internal/raster"
)

type Diff struct {
	Kind         string `json:"kind"`
	PixelX       int    `json:"pixelX"`
	PixelY       int    `json:"pixelY"`
	SegmentID    int    `json:"segmentId"`
	ContourIndex int    `json:"contourIndex"`
	SamplesIndex int    `json:"samplesIndex,omitempty"`
	Message      string `json:"message"`
}

// Compare performs row-major pixel comparison followed by sample-by-sample
// density comparison. The returned error is always E_DIFF when a mismatch is
// found and includes the first differing pixel and owning segment.
func Compare(incremental, full *RunResult) (*Diff, error) {
	if incremental == nil || full == nil {
		return nil, errs.New(errs.CodeDiff, "both render results are required")
	}
	if incremental.Mask.Width != full.Mask.Width || incremental.Mask.Height != full.Mask.Height {
		diff := &Diff{
			Kind:      "mask-dimensions",
			SegmentID: -1,
			Message: fmt.Sprintf("mask dimensions differ: incremental=%dx%d full=%dx%d",
				incremental.Mask.Width, incremental.Mask.Height, full.Mask.Width, full.Mask.Height),
		}
		return diff, diff.err()
	}
	x, y, found := raster.FirstDifference(incremental.Mask, full.Mask)
	if found {
		segment := ownerAt(incremental, x, y)
		if segment < 0 {
			segment = ownerAt(full, x, y)
		}
		diff := &Diff{
			Kind:      "pixel",
			PixelX:    x,
			PixelY:    y,
			SegmentID: segment,
			Message: fmt.Sprintf("first pixel mismatch at (%d,%d), incremental=%d full=%d",
				x, y, incremental.Mask.At(x, y), full.Mask.At(x, y)),
		}
		diff.ContourIndex = contourForSegment(incremental, segment)
		return diff, diff.err()
	}
	index, found := raster.DensityDifference(incremental.Densities, full.Densities)
	if found {
		var sample raster.DensitySample
		if index < len(incremental.Densities) {
			sample = incremental.Densities[index]
		} else if index < len(full.Densities) {
			sample = full.Densities[index]
		}
		segment := sample.SegmentID
		if segment == 0 {
			segment = segmentForDensity(full, sample)
		}
		diff := &Diff{
			Kind:         "density",
			PixelX:       sample.PixelX,
			PixelY:       sample.PixelY,
			SegmentID:    segment,
			ContourIndex: sample.ContourIndex,
			SamplesIndex: index,
			Message: fmt.Sprintf("first density mismatch at sample %d pixel (%d,%d)",
				index, sample.PixelX, sample.PixelY),
		}
		return diff, diff.err()
	}
	return nil, nil
}

func (d *Diff) err() *errs.Error {
	e := errs.New(errs.CodeDiff, "%s", d.Message).
		WithPixel(d.PixelX, d.PixelY)
	if d.SegmentID > 0 {
		e = e.WithSegment(d.SegmentID)
	}
	if d.ContourIndex >= 0 && d.SegmentID > 0 {
		e = e.WithContour(d.ContourIndex)
	}
	return e
}

func ownerAt(result *RunResult, x, y int) int {
	segments := append([]SegmentResult(nil), result.Segments...)
	sort.SliceStable(segments, func(i, j int) bool { return segments[i].SegmentID < segments[j].SegmentID })
	for _, segment := range segments {
		if segment.Status != StatusValid {
			continue
		}
		if segment.Mask.At(x, y) != 0 {
			return segment.SegmentID
		}
	}
	return -1
}

func contourForSegment(result *RunResult, segmentID int) int {
	for _, segment := range result.Segments {
		if segment.SegmentID == segmentID {
			return segment.ContourIndex
		}
	}
	return -1
}

func segmentForDensity(result *RunResult, sample raster.DensitySample) int {
	for _, segment := range result.Segments {
		if segment.ContourIndex == sample.ContourIndex && segment.CurveIndex == sample.CurveIndex {
			return segment.SegmentID
		}
	}
	return -1
}
