package imagecodec

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"os"

	"edgerefine/internal/errs"
	"edgerefine/internal/model"
)

func SaveGrayPNG(path string, im *model.GrayImage) error {
	gray := image.NewGray(image.Rect(0, 0, im.Width, im.Height))
	copy(gray.Pix, im.Pix)
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	enc := png.Encoder{CompressionLevel: png.BestSpeed}
	if err := enc.Encode(f, gray); err != nil {
		return err
	}
	return f.Close()
}

func PNGMetadata(path string) (width, height int, bitDepth, colorType byte, err error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, 0, 0, 0, err
	}
	if len(b) < 26 || bytes.Index(b, []byte("IHDR")) < 0 {
		return 0, 0, 0, 0, errs.Decode(path, png.FormatError("missing IHDR"))
	}
	pos := bytes.Index(b, []byte("IHDR"))
	if pos < 4 || pos+13 > len(b) {
		return 0, 0, 0, 0, errs.Decode(path, png.FormatError("short IHDR"))
	}
	d := pos + 4
	width = int(b[d])<<24 | int(b[d+1])<<16 | int(b[d+2])<<8 | int(b[d+3])
	height = int(b[d+4])<<24 | int(b[d+5])<<16 | int(b[d+6])<<8 | int(b[d+7])
	bitDepth = b[d+8]
	colorType = b[d+9]
	return width, height, bitDepth, colorType, nil
}

func colorTypeChannels(t byte) int {
	switch t {
	case 0:
		return 1
	case 2:
		return 3
	case 3:
		return 1
	case 4:
		return 2
	case 6:
		return 4
	default:
		return -1
	}
}

func LoadGrayPNG(path string, wantW, wantH, wantChannels, wantBitDepth int) (*model.GrayImage, error) {
	w, h, bd, ct, metaErr := PNGMetadata(path)
	if metaErr != nil {
		return nil, metaErr
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, errs.Decode(path, err)
	}
	defer f.Close()
	img, err := png.Decode(f)
	if err != nil {
		return nil, errs.Decode(path, err)
	}
	if w != wantW || h != wantH {
		return nil, errs.Para("resolution", [2]int{w, h}, [2]any{wantW, wantH})
	}
	gotChannels := colorTypeChannels(ct)
	if gotChannels != wantChannels {
		return nil, errs.Para("channels", gotChannels, [2]any{wantChannels, wantChannels})
	}
	if int(bd) != wantBitDepth {
		return nil, errs.Para("bit_depth", bd, [2]any{wantBitDepth, wantBitDepth})
	}
	b := img.Bounds()
	out := &model.GrayImage{Width: wantW, Height: wantH, Channels: wantChannels, BitDepth: int(bd), Pix: make([]uint8, wantW*wantH)}
	for y := 0; y < wantH; y++ {
		for x := 0; x < wantW; x++ {
			r, g, bl, _ := img.At(b.Min.X+x, b.Min.Y+y).RGBA()
			lum := color.Gray{Y: uint8((299*r + 587*g + 114*bl + 500) / 1000 >> 8)}
			out.Pix[y*wantW+x] = lum.Y
		}
	}
	return out, nil
}

func LoadAnyPNG(path string) (image.Image, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, errs.Decode(path, err)
	}
	defer f.Close()
	img, err := png.Decode(f)
	if err != nil {
		return nil, errs.Decode(path, err)
	}
	return img, nil
}
