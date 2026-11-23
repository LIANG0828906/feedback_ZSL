package morph

import (
	"bufio"
	"encoding/csv"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"math"
	"os"
	"path/filepath"
	"strconv"
)

func WritePGM(path string, gray []uint8, width, height int) error {
	if len(gray) != width*height {
		return withField(newError(CodeDecode, "bitmap length mismatch"), "gray")
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	w := bufio.NewWriter(f)
	if _, err := fmt.Fprintf(w, "P5\n%d %d\n255\n", width, height); err != nil {
		f.Close()
		return err
	}
	if _, err := w.Write(gray); err != nil {
		f.Close()
		return err
	}
	if err := w.Flush(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func WriteFramePNG(path string, gray []uint8, width, height int) error {
	if len(gray) != width*height {
		return withField(newError(CodeDecode, "bitmap length mismatch"), "gray")
	}
	img := image.NewGray(image.Rect(0, 0, width, height))
	copy(img.Pix, gray)
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	if err := png.Encode(f, img); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func WriteDiffPNG(path string, a, b *SampleResult) error {
	if a == nil || b == nil || a.Width != b.Width || a.Height != b.Height {
		return withField(newError(CodeDiverge, "diff frames are not comparable"), "diff")
	}
	img := image.NewRGBA(image.Rect(0, 0, a.Width, a.Height))
	for i := range a.Gray {
		d := math.Abs(float64(a.Gray[i])-float64(b.Gray[i])) / 255.0
		c := heatColor(d)
		img.SetRGBA(i%a.Width, i/a.Width, c)
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	if err := png.Encode(f, img); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

func heatColor(v float64) color.RGBA {
	if v <= 0 {
		return color.RGBA{0, 0, 0, 255}
	}
	if v < 0.25 {
		t := v / 0.25
		return color.RGBA{0, uint8(80*t + 20), 255, 255}
	}
	if v < 0.5 {
		t := (v - 0.25) / 0.25
		return color.RGBA{0, 255, uint8(255*(1-t) + 40*t), 255}
	}
	if v < 0.75 {
		t := (v - 0.5) / 0.25
		return color.RGBA{uint8(255 * t), 255, 40, 255}
	}
	t := (v - 0.75) / 0.25
	return color.RGBA{255, uint8(255 * (1 - t)), 0, 255}
}

func ExportResults(dir string, results []*SampleResult) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	csvFile, err := os.Create(filepath.Join(dir, "coverage_stats.csv"))
	if err != nil {
		return err
	}
	cw := csv.NewWriter(csvFile)
	if err := cw.Write([]string{"sample", "time", "path_id", "segment_count", "interpolation_count", "coverage_area", "mean_coverage", "pixel_area", "bbox_x0", "bbox_y0", "bbox_x1", "bbox_y1", "pixel_hash"}); err != nil {
		csvFile.Close()
		return err
	}
	for _, r := range results {
		if r == nil {
			continue
		}
		if err := WritePGM(filepath.Join(dir, fmt.Sprintf("frame_%04d.pgm", r.Index)), r.Gray, r.Width, r.Height); err != nil {
			csvFile.Close()
			return err
		}
		if err := WriteFramePNG(filepath.Join(dir, fmt.Sprintf("frame_%04d.png", r.Index)), r.Gray, r.Width, r.Height); err != nil {
			csvFile.Close()
			return err
		}
		for _, s := range r.PathStats {
			if err := cw.Write([]string{
				strconv.Itoa(r.Index), strconv.FormatFloat(r.Time, 'g', 17, 64), s.PathID,
				strconv.Itoa(s.SegmentCount), strconv.Itoa(s.InterpolationCount),
				strconv.FormatFloat(s.CoverageArea, 'g', 17, 64), strconv.FormatFloat(s.MeanCoverage, 'g', 17, 64),
				strconv.Itoa(s.PixelArea), strconv.Itoa(s.CoverageBBox.X0), strconv.Itoa(s.CoverageBBox.Y0),
				strconv.Itoa(s.CoverageBBox.X1), strconv.Itoa(s.CoverageBBox.Y1), r.PixelHash,
			}); err != nil {
				csvFile.Close()
				return err
			}
		}
	}
	cw.Flush()
	if err := cw.Error(); err != nil {
		csvFile.Close()
		return err
	}
	return csvFile.Close()
}
