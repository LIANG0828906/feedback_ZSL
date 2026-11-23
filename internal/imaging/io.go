package imaging

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"
	"math"
	"os"
	"path/filepath"
	"strings"

	"stitchworkbench/internal/errs"
	"stitchworkbench/internal/model"
)

type LoadedImage struct {
	Index       int
	Truth       model.ImageTruth
	Path        string
	ContentHash string
	Width       int
	Height      int
	Channels    int
	Pix         []uint8
	Homography  model.Matrix3
}

type Set struct {
	Dir          string
	Manifest     model.Manifest
	ManifestHash string
	Images       []*LoadedImage
	ContentHash  string
	ByFile       map[string]*LoadedImage
}

func ReadManifest(dir string) (model.Manifest, string, error) {
	path := filepath.Join(dir, "manifest.json")
	b, err := os.ReadFile(path)
	if err != nil {
		return model.Manifest{}, "", errs.Wrap(errs.EDecode, "manifest missing or unreadable", err)
	}
	var m model.Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return model.Manifest{}, "", errs.Wrap(errs.EDecode, "manifest is not valid JSON", err)
	}
	if len(m.Images) == 0 {
		return model.Manifest{}, "", errs.Field(errs.EMeta, "manifest has no images", "images")
	}
	if m.Canvas.Width <= 0 || m.Canvas.Height <= 0 {
		return model.Manifest{}, "", errs.Field(errs.EMeta, "theoretical canvas must be positive", "canvas")
	}
	for i, it := range m.Images {
		h, ok := model.MatrixFromSlice(it.HomographyToBase)
		if !ok {
			return model.Manifest{}, "", errs.Field(errs.EMeta, "truth homography is not finite", fmt.Sprintf("images[%d].homography_to_base", i))
		}
		if _, inv := h.Inverse(); !inv {
			return model.Manifest{}, "", errs.Field(errs.EMeta, "truth homography is not invertible", fmt.Sprintf("images[%d].homography_to_base", i))
		}
	}
	sum := sha256.Sum256(b)
	return m, hex.EncodeToString(sum[:]), nil
}

func LoadSet(dir string) (*Set, error) {
	m, mh, err := ReadManifest(dir)
	if err != nil {
		return nil, err
	}
	s := &Set{Dir: dir, Manifest: m, ManifestHash: mh, ByFile: make(map[string]*LoadedImage)}
	contentHash := sha256.New()
	for i, it := range m.Images {
		path := filepath.Join(dir, filepath.FromSlash(it.File))
		img, ch, err := decodeFile(path)
		if err != nil {
			return nil, err
		}
		b := img.Bounds()
		w, h := b.Dx(), b.Dy()
		if w != it.Width {
			return nil, errs.Field(errs.EMeta, fmt.Sprintf("image width %d differs from manifest %d", w, it.Width), fmt.Sprintf("images[%d].width", i))
		}
		if h != it.Height {
			return nil, errs.Field(errs.EMeta, fmt.Sprintf("image height %d differs from manifest %d", h, it.Height), fmt.Sprintf("images[%d].height", i))
		}
		if ch != it.Channels {
			return nil, errs.Field(errs.EMeta, fmt.Sprintf("image channels %d differs from manifest %d", ch, it.Channels), fmt.Sprintf("images[%d].channels", i))
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return nil, errs.Wrap(errs.EDecode, "image cannot be read", err)
		}
		sum := sha256.Sum256(raw)
		hash := hex.EncodeToString(sum[:])
		contentHash.Write([]byte(hash))
		nrgba := ToNRGBA(img)
		hm, _ := model.MatrixFromSlice(it.HomographyToBase)
		li := &LoadedImage{
			Index: i, Truth: it, Path: path, ContentHash: hash,
			Width: w, Height: h, Channels: ch, Pix: nrgba.Pix,
			Homography: hm,
		}
		s.Images = append(s.Images, li)
		s.ByFile[it.File] = li
	}
	setHash := sha256.Sum256(contentHash.Sum(nil))
	s.ContentHash = hex.EncodeToString(setHash[:])
	return s, nil
}

func decodeFile(path string) (image.Image, int, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, errs.Wrap(errs.EDecode, "image missing or unreadable", err)
	}
	defer f.Close()
	ext := strings.ToLower(filepath.Ext(path))
	switch ext {
	case ".png":
		ch, err := pngChannels(path)
		if err != nil {
			return nil, 0, errs.Wrap(errs.EDecode, "invalid PNG header", err)
		}
		img, err := png.Decode(f)
		if err != nil {
			return nil, 0, errs.Wrap(errs.EDecode, "corrupt PNG image", err)
		}
		return img, ch, nil
	case ".jpg", ".jpeg":
		img, err := jpeg.Decode(f)
		if err != nil {
			return nil, 0, errs.Wrap(errs.EDecode, "corrupt JPEG image", err)
		}
		if _, ok := img.(*image.Gray); ok {
			return img, 1, nil
		}
		return img, 3, nil
	default:
		return nil, 0, errs.New(errs.EDecode, "unsupported image format")
	}
}

func pngChannels(path string) (int, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	if len(b) < 26 || string(b[:8]) != "\x89PNG\r\n\x1a\n" || string(b[12:16]) != "IHDR" {
		return 0, fmt.Errorf("missing PNG signature or IHDR")
	}
	_ = binary.BigEndian.Uint32(b[16:20])
	switch b[25] {
	case 0:
		return 1, nil
	case 2:
		return 3, nil
	case 3:
		return 3, nil // indexed color is exposed as RGB by image/png
	case 4:
		return 2, nil
	case 6:
		return 4, nil
	default:
		return 0, fmt.Errorf("unknown PNG color type %d", b[25])
	}
}

func PNGChannels(path string) (int, error) { return pngChannels(path) }

func ToNRGBA(img image.Image) *image.NRGBA {
	b := img.Bounds()
	out := image.NewNRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	for y := 0; y < b.Dy(); y++ {
		for x := 0; x < b.Dx(); x++ {
			r, g, bl, a := img.At(b.Min.X+x, b.Min.Y+y).RGBA()
			i := out.PixOffset(x, y)
			out.Pix[i] = uint8(r >> 8)
			out.Pix[i+1] = uint8(g >> 8)
			out.Pix[i+2] = uint8(bl >> 8)
			out.Pix[i+3] = uint8(a >> 8)
		}
	}
	return out
}

func Grayscale(img *LoadedImage) []float32 {
	out := make([]float32, img.Width*img.Height)
	for i := 0; i < len(out); i++ {
		r := float32(img.Pix[i*4])
		g := float32(img.Pix[i*4+1])
		b := float32(img.Pix[i*4+2])
		out[i] = 0.2126*r + 0.7152*g + 0.0722*b
	}
	return out
}

func SampleRGB(img *LoadedImage, x, y float64) (float64, float64, float64, bool) {
	if x < 0 || y < 0 || x > float64(img.Width-1) || y > float64(img.Height-1) {
		return 0, 0, 0, false
	}
	x0, y0 := int(math.Floor(x)), int(math.Floor(y))
	x1, y1 := x0+1, y0+1
	if x1 >= img.Width {
		x1 = img.Width - 1
	}
	if y1 >= img.Height {
		y1 = img.Height - 1
	}
	fx, fy := x-float64(x0), y-float64(y0)
	sample := func(c int) float64 {
		v00 := float64(img.Pix[(y0*img.Width+x0)*4+c])
		v10 := float64(img.Pix[(y0*img.Width+x1)*4+c])
		v01 := float64(img.Pix[((y1)*img.Width+x0)*4+c])
		v11 := float64(img.Pix[(y1*img.Width+x1)*4+c])
		v0 := v00*(1-fx) + v10*fx
		v1 := v01*(1-fx) + v11*fx
		return v0*(1-fy) + v1*fy
	}
	return sample(0), sample(1), sample(2), true
}
