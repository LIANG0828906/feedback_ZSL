// Package exporter materializes stored SDR frames, optional HDR previews and
// proof records as portable files for manual review.
package exporter

import (
	"context"
	"encoding/json"
	"fmt"
	"image"
	stdcolor "image/color"
	"image/png"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"hdrproof/internal/color"
	"hdrproof/internal/model"
	"hdrproof/internal/sample"
	"hdrproof/internal/snapshot"
)

type Summary struct {
	Frames      int    `json:"frames"`
	Proofs      int    `json:"proofs"`
	HDRPreviews int    `json:"hdr_previews"`
	OutputDir   string `json:"output_dir"`
}

type entry struct {
	RouteID    string `json:"route_id"`
	Frame      int    `json:"frame"`
	Segment    int    `json:"segment"`
	PNG        string `json:"png"`
	HDRPreview string `json:"hdr_preview,omitempty"`
	Proof      string `json:"proof"`
}

func Run(ctx context.Context, dbPath, outDir, samplesDir string) (*Summary, error) {
	store, err := snapshot.OpenReadOnly(dbPath)
	if err != nil {
		return nil, err
	}
	defer store.Close()
	routes, err := store.RouteJSON(ctx)
	if err != nil {
		return nil, err
	}
	_, frames, err := store.ReadAll(ctx)
	if err != nil {
		return nil, err
	}
	var dataset *model.Dataset
	if samplesDir != "" {
		dataset, err = sample.Load(samplesDir)
		if err != nil {
			return nil, err
		}
	}
	summary := &Summary{OutputDir: outDir}
	var index []entry
	for _, route := range routes {
		routeDir := filepath.Join(outDir, safeName(route.ID))
		if err := os.MkdirAll(routeDir, 0o755); err != nil {
			return nil, err
		}
		var ordered []snapshot.StoredFrame
		for key, frame := range frames {
			if key.RouteID == route.ID {
				ordered = append(ordered, frame)
			}
		}
		sort.Slice(ordered, func(i, j int) bool { return ordered[i].FrameIndex < ordered[j].FrameIndex })
		for _, frame := range ordered {
			pngPath := filepath.Join(routeDir, fmt.Sprintf("frame-%04d.png", frame.FrameIndex))
			if err := writePNG(pngPath, frame.Width, frame.Height, frame.Pixels); err != nil {
				return nil, err
			}
			proofPath := filepath.Join(routeDir, fmt.Sprintf("proof-%04d.json", frame.FrameIndex))
			var proof interface{}
			if err := json.Unmarshal(frame.ProofJSON, &proof); err != nil {
				return nil, err
			}
			if err := writeJSON(proofPath, proof); err != nil {
				return nil, err
			}
			item := entry{RouteID: route.ID, Frame: frame.FrameIndex, Segment: frame.Segment,
				PNG: pngPath, Proof: proofPath}
			if dataset != nil {
				if hdrPath, ok := writeHDRPreview(dataset, route.ID, frame.FrameIndex, routeDir); ok {
					item.HDRPreview = hdrPath
					summary.HDRPreviews++
				}
			}
			index = append(index, item)
			summary.Frames++
			summary.Proofs++
		}
	}
	if err := writeJSON(filepath.Join(outDir, "index.json"), index); err != nil {
		return nil, err
	}
	return summary, nil
}

func writePNG(path string, width, height int, pixels []byte) error {
	if len(pixels) != width*height*3 {
		return fmt.Errorf("pixel buffer length %d does not match %dx%dx3", len(pixels), width, height)
	}
	img := image.NewNRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			i := (y*width + x) * 3
			img.SetNRGBA(x, y, stdcolor.NRGBA{R: pixels[i], G: pixels[i+1], B: pixels[i+2], A: 255})
		}
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return png.Encode(f, img)
}

func writeHDRPreview(ds *model.Dataset, routeID string, frameIndex int, routeDir string) (string, bool) {
	for _, rd := range ds.Routes {
		if rd.Route.ID != routeID || frameIndex < 0 || frameIndex >= len(rd.DecodedFrames) {
			continue
		}
		frame := rd.DecodedFrames[frameIndex]
		img := image.NewNRGBA(image.Rect(0, 0, rd.Route.Width, rd.Route.Height))
		matrix := color.PrimariesToTarget(rd.Route.ColorSpace.Primaries)
		for y := 0; y < rd.Route.Height; y++ {
			for x := 0; x < rd.Route.Width; x++ {
				i := (y*rd.Route.Width + x) * 3
				src := [3]float64{frame.LinearRGB[i], frame.LinearRGB[i+1], frame.LinearRGB[i+2]}
				target := [3]float64{
					matrix[0][0]*src[0] + matrix[0][1]*src[1] + matrix[0][2]*src[2],
					matrix[1][0]*src[0] + matrix[1][1]*src[1] + matrix[1][2]*src[2],
					matrix[2][0]*src[0] + matrix[2][1]*src[1] + matrix[2][2]*src[2],
				}
				target = color.Desaturate(target)
				img.SetNRGBA(x, y, stdcolor.NRGBA{
					R: color.EncodeSRGBByte(color.ACES(target[0])),
					G: color.EncodeSRGBByte(color.ACES(target[1])),
					B: color.EncodeSRGBByte(color.ACES(target[2])),
					A: 255,
				})
			}
		}
		path := filepath.Join(routeDir, fmt.Sprintf("hdr-frame-%04d.png", frameIndex))
		f, err := os.Create(path)
		if err != nil {
			return "", false
		}
		err = png.Encode(f, img)
		f.Close()
		return path, err == nil
	}
	return "", false
}

func writeJSON(path string, v interface{}) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	return os.WriteFile(path, b, 0o644)
}

func safeName(name string) string {
	name = strings.ReplaceAll(name, "/", "_")
	name = strings.ReplaceAll(name, "\\", "_")
	if name == "" || name == "." || name == ".." {
		return "route"
	}
	return name
}
