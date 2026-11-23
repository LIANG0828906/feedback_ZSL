// Package export writes portable PNG diagnostics and JSON summaries.
package export

import (
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"os"
	"path/filepath"
	"sort"

	"glyphbrush/internal/errs"
	"glyphbrush/internal/loader"
	"glyphbrush/internal/pipeline"
	"glyphbrush/internal/raster"
	"glyphbrush/internal/stroke"
	"glyphbrush/internal/viz"
)

type SegmentSummary struct {
	SegmentID    int                    `json:"segmentId"`
	ContourIndex int                    `json:"contourIndex"`
	CurveIndex   int                    `json:"curveIndex"`
	ArcStart     float64                `json:"arcStart"`
	ArcEnd       float64                `json:"arcEnd"`
	Status       pipeline.SegmentStatus `json:"status"`
	BandArea     float64                `json:"bandArea,omitempty"`
	BandPoints   int                    `json:"bandPoints,omitempty"`
	MaskPixels   int                    `json:"maskPixels"`
	DensityCount int                    `json:"densityCount"`
	Error        *errs.Error            `json:"error,omitempty"`
}

type Summary struct {
	GlyphID       string                       `json:"glyphId"`
	Params        stroke.Params                `json:"params"`
	ParamVersion  string                       `json:"paramVersion"`
	GlyphHash     string                       `json:"glyphHash"`
	Algorithm     string                       `json:"algorithmVersion"`
	CompositeHash string                       `json:"compositeHash"`
	Grid          *raster.Grid                 `json:"grid"`
	Redrawn       []int                        `json:"redrawn"`
	Reused        []int                        `json:"reused"`
	Invalidation  *pipeline.InvalidationReport `json:"invalidation,omitempty"`
	Segments      []SegmentSummary             `json:"segments"`
	Errors        []*errs.Error                `json:"errors,omitempty"`
}

// WriteGlyph writes all diagnostics for one glyph. The result grid determines
// image dimensions.
func WriteGlyph(dir string, glyph loader.Glyph, result *pipeline.RunResult) (*Summary, error) {
	if result == nil || result.Grid == nil {
		return nil, fmt.Errorf("export: render result and grid are required")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	files := map[string]image.Image{
		"outline.png": viz.Outline(glyph, result),
		"bands.png":   viz.Bands(glyph, result),
		"mask.png":    viz.MaskImage(result.Mask),
		"density.png": viz.Heatmap(result),
	}
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if err := writePNG(filepath.Join(dir, name), files[name]); err != nil {
			return nil, err
		}
	}
	summary := buildSummary(result)
	if err := writeJSON(filepath.Join(dir, "summary.json"), summary); err != nil {
		return nil, err
	}
	if err := writeJSON(filepath.Join(dir, "densities.json"), result.Densities); err != nil {
		return nil, err
	}
	if result.Validation != nil {
		if err := writeJSON(filepath.Join(dir, "invalidation.json"), result.Validation); err != nil {
			return nil, err
		}
	}
	var statuses []SegmentSummary
	for _, segment := range summary.Segments {
		statuses = append(statuses, segment)
	}
	if err := writeJSON(filepath.Join(dir, "segments.json"), statuses); err != nil {
		return nil, err
	}
	return summary, nil
}

func buildSummary(result *pipeline.RunResult) *Summary {
	summary := &Summary{
		GlyphID:       result.GlyphID,
		Params:        result.Params,
		ParamVersion:  result.ParamVersion,
		GlyphHash:     result.GlyphHash,
		Algorithm:     result.Algorithm,
		CompositeHash: result.CompositeHash,
		Grid:          result.Grid,
		Redrawn:       result.Redrawn,
		Reused:        result.Reused,
		Invalidation:  result.Validation,
		Errors:        result.Errors,
	}
	for _, segment := range result.Segments {
		item := SegmentSummary{
			SegmentID:    segment.SegmentID,
			ContourIndex: segment.ContourIndex,
			CurveIndex:   segment.CurveIndex,
			ArcStart:     segment.ArcStart,
			ArcEnd:       segment.ArcEnd,
			Status:       segment.Status,
			MaskPixels:   countNonZero(segment.Mask),
			DensityCount: len(segment.Densities),
			Error:        segment.Error,
		}
		if segment.Band != nil {
			item.BandArea = segment.Band.Area
			item.BandPoints = len(segment.Band.Polygon)
		}
		summary.Segments = append(summary.Segments, item)
	}
	return summary
}

func countNonZero(m raster.Mask) int {
	n := 0
	for _, v := range m.Data {
		if v != 0 {
			n++
		}
	}
	return n
}

func writePNG(path string, img image.Image) error {
	return atomicFile(path, func(f *os.File) error { return png.Encode(f, img) })
}

func writeJSON(path string, v any) error {
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	return atomicFile(path, func(f *os.File) error {
		_, err := f.Write(raw)
		return err
	})
}

func atomicFile(path string, write func(*os.File) error) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, ".export-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	ok := false
	defer func() {
		_ = f.Close()
		if !ok {
			_ = os.Remove(tmp)
		}
	}()
	if err := write(f); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	ok = true
	return nil
}
