package quantforensics

import (
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
)

func WriteImagePNG(im Image, path string) error {
	rgba := image.NewRGBA(image.Rect(0, 0, im.Width, im.Height))
	for y := 0; y < im.Height; y++ {
		for x := 0; x < im.Width; x++ {
			c := im.At(x, y)
			rgba.SetRGBA(x, y, color.RGBA{R: c.R, G: c.G, B: c.B, A: 255})
		}
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return png.Encode(f, rgba)
}

func WriteSamplePNGs(sample SampleSet, dir string) ([]string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	paths := make([]string, 0, len(sample.Images))
	for _, im := range sample.Images {
		name := im.ID + ".png"
		path := filepath.Join(dir, name)
		if err := WriteImagePNG(im, path); err != nil {
			return nil, err
		}
		paths = append(paths, filepath.ToSlash(filepath.Join("screenshots", name)))
	}
	return paths, nil
}
