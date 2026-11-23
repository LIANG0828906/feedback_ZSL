package tex

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"os"
	"path/filepath"
)

var imageMagic = [8]byte{'T', 'W', 'B', 'I', 'M', 'G', '0', '1'}

const (
	imageFormatVersion = 1
	imageHeaderSize    = 24
)

var layoutCodes = map[Layout]byte{
	LayoutGray: 1,
	LayoutRGB:  2,
	LayoutRGBA: 3,
	LayoutBGR:  4,
	LayoutBGRA: 5,
}

var codeLayouts = map[byte]Layout{
	1: LayoutGray,
	2: LayoutRGB,
	3: LayoutRGBA,
	4: LayoutBGR,
	5: LayoutBGRA,
}

func EncodeImage(w io.Writer, img *Image) error {
	if err := img.Validate(); err != nil {
		return err
	}
	header := make([]byte, imageHeaderSize)
	copy(header[0:8], imageMagic[:])
	binary.BigEndian.PutUint16(header[8:10], imageFormatVersion)
	binary.BigEndian.PutUint32(header[12:16], uint32(img.Width))
	binary.BigEndian.PutUint32(header[16:20], uint32(img.Height))
	header[20] = byte(img.BitDepth)
	header[21] = layoutCodes[img.Layout]
	header[22] = byte(img.Channels())
	if _, err := w.Write(header); err != nil {
		return err
	}
	buffer := make([]byte, 32*1024)
	offset := 0
	for offset < len(img.Pixels) {
		n := 0
		for n < len(buffer)/2 && offset+n < len(img.Pixels) {
			binary.BigEndian.PutUint16(buffer[n*2:n*2+2], img.Pixels[offset+n])
			n++
		}
		if _, err := w.Write(buffer[:n*2]); err != nil {
			return err
		}
		offset += n
	}
	return nil
}

func DecodeImage(r io.Reader) (*Image, error) {
	reader := bufio.NewReader(r)
	header := make([]byte, imageHeaderSize)
	if _, err := io.ReadFull(reader, header); err != nil {
		return nil, ErrDecode("file is shorter than the image header")
	}
	if string(header[0:8]) != string(imageMagic[:]) {
		return nil, ErrDecode("invalid image magic")
	}
	version := binary.BigEndian.Uint16(header[8:10])
	if version != imageFormatVersion {
		return nil, ErrDecode(fmt.Sprintf("unsupported image format version %d", version))
	}
	width := int(binary.BigEndian.Uint32(header[12:16]))
	height := int(binary.BigEndian.Uint32(header[16:20]))
	bitDepth := int(header[20])
	layout, ok := codeLayouts[header[21]]
	if !ok {
		return nil, ErrDecode(fmt.Sprintf("unknown layout code %d", header[21]))
	}
	if bitDepth != 8 && bitDepth != 16 {
		return nil, ErrDecode(fmt.Sprintf("invalid bit depth %d", bitDepth))
	}
	if width <= 0 || height <= 0 {
		return nil, ErrDecode("image dimensions must be positive")
	}
	if width > 100000 || height > 100000 {
		return nil, ErrDecode("image dimensions exceed safety limit")
	}
	channels := int(header[22])
	if channels != layout.ChannelCount() {
		return nil, ErrDecode(fmt.Sprintf("layout %s requires %d channels, header says %d", layout, layout.ChannelCount(), channels))
	}
	count64 := int64(width) * int64(height) * int64(channels)
	if count64 <= 0 || count64 > int64(^uint(0)>>1)/2 {
		return nil, ErrDecode("pixel buffer size overflow")
	}
	pixels := make([]uint16, int(count64))
	var pair [2]byte
	max := uint16(255)
	if bitDepth == 16 {
		max = 65535
	}
	for i := range pixels {
		if _, err := io.ReadFull(reader, pair[:]); err != nil {
			return nil, ErrDecode(fmt.Sprintf("truncated pixel data at sample %d", i))
		}
		value := binary.BigEndian.Uint16(pair[:])
		if value > max {
			return nil, ErrDecode(fmt.Sprintf("sample %d=%d exceeds %d-bit range", i, value, bitDepth))
		}
		pixels[i] = value
	}
	if _, err := reader.ReadByte(); err == nil {
		return nil, ErrDecode("trailing bytes after pixel data")
	} else if err != io.EOF {
		return nil, ErrDecode(fmt.Sprintf("cannot inspect trailing bytes: %v", err))
	}
	img := &Image{
		Width:    width,
		Height:   height,
		BitDepth: bitDepth,
		Layout:   layout,
		Pixels:   pixels,
	}
	img.EnsureHash()
	return img, nil
}

func DecodeImageFile(path string) (*Image, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, ErrDecode(fmt.Sprintf("cannot open image: %v", err))
	}
	defer file.Close()
	var header [8]byte
	if _, err := io.ReadFull(file, header[:]); err != nil {
		return nil, ErrDecode("file is shorter than an image signature")
	}
	if string(header[:]) == string(imageMagic[:]) {
		if _, err := file.Seek(0, io.SeekStart); err != nil {
			return nil, ErrDecode(fmt.Sprintf("cannot rewind image: %v", err))
		}
		img, err := DecodeImage(file)
		if err != nil {
			return nil, err
		}
		img.Path = path
		if img.ID == "" {
			img.ID = filepath.Base(path)
		}
		img.EnsureHash()
		return img, nil
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return nil, ErrDecode(fmt.Sprintf("cannot rewind image: %v", err))
	}
	src, err := png.Decode(file)
	if err != nil {
		return nil, ErrDecode(fmt.Sprintf("unsupported or corrupt image: %v", err))
	}
	img := imageFromDecoded(src, path)
	if err := img.Validate(); err != nil {
		return nil, err
	}
	return img, nil
}

// EncodePNG writes the target channel layout as a visually correct PNG while
// preserving the target bit depth. BGR/BGRA samples are reordered to canonical
// PNG RGB/RGBA channel order only in the export representation.
func EncodePNG(w io.Writer, data *Converted, force8Bit bool) error {
	if data.Width <= 0 || data.Height <= 0 {
		return ErrPara("image", "cannot encode empty image")
	}
	channels := data.Layout.ChannelCount()
	if len(data.Pixels) != data.Width*data.Height*channels {
		return ErrPara("pixels", "pixel buffer does not match exported dimensions and layout")
	}
	depth := data.BitDepth
	if force8Bit {
		depth = 8
	}
	colorAt := func(x, y int, name string) uint16 {
		index := data.Layout.IndexOf(name)
		if index < 0 {
			return 0
		}
		value := data.Pixels[(y*data.Width+x)*channels+index]
		if force8Bit && data.BitDepth == 16 {
			value = uint16((uint32(value)*255 + 32767) / 65535)
		}
		return value
	}
	if data.Layout == LayoutGray {
		if depth == 8 {
			out := image.NewGray(image.Rect(0, 0, data.Width, data.Height))
			for y := 0; y < data.Height; y++ {
				for x := 0; x < data.Width; x++ {
					out.SetGray(x, y, color.Gray{Y: uint8(colorAt(x, y, "Y"))})
				}
			}
			return png.Encode(w, out)
		}
		out := image.NewGray16(image.Rect(0, 0, data.Width, data.Height))
		for y := 0; y < data.Height; y++ {
			for x := 0; x < data.Width; x++ {
				out.SetGray16(x, y, color.Gray16{Y: colorAt(x, y, "Y")})
			}
		}
		return png.Encode(w, out)
	}
	if depth == 8 {
		out := image.NewNRGBA(image.Rect(0, 0, data.Width, data.Height))
		for y := 0; y < data.Height; y++ {
			for x := 0; x < data.Width; x++ {
				a := uint8(255)
				if data.Layout.HasAlpha() {
					a = uint8(colorAt(x, y, "A"))
				}
				out.SetNRGBA(x, y, color.NRGBA{
					R: uint8(colorAt(x, y, "R")),
					G: uint8(colorAt(x, y, "G")),
					B: uint8(colorAt(x, y, "B")),
					A: a,
				})
			}
		}
		return png.Encode(w, out)
	}
	out := image.NewNRGBA64(image.Rect(0, 0, data.Width, data.Height))
	for y := 0; y < data.Height; y++ {
		for x := 0; x < data.Width; x++ {
			a := uint16(65535)
			if data.Layout.HasAlpha() {
				a = colorAt(x, y, "A")
			}
			out.SetNRGBA64(x, y, color.NRGBA64{
				R: colorAt(x, y, "R"),
				G: colorAt(x, y, "G"),
				B: colorAt(x, y, "B"),
				A: a,
			})
		}
	}
	return png.Encode(w, out)
}

func DecodePNG(path string) (*Image, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, ErrDecode(fmt.Sprintf("cannot open PNG: %v", err))
	}
	defer file.Close()
	src, err := png.Decode(file)
	if err != nil {
		return nil, ErrDecode(fmt.Sprintf("cannot decode PNG: %v", err))
	}
	img := imageFromDecoded(src, path)
	if err := img.Validate(); err != nil {
		return nil, err
	}
	return img, nil
}

func imageFromDecoded(src image.Image, path string) *Image {
	bounds := src.Bounds()
	width, height := bounds.Dx(), bounds.Dy()
	if width <= 0 || height <= 0 {
		return &Image{ID: filepath.Base(path), Path: path}
	}
	layout := LayoutRGBA
	bitDepth := 8
	switch src.(type) {
	case *image.Gray:
		layout = LayoutGray
	case *image.Gray16:
		layout = LayoutGray
		bitDepth = 16
	case *image.NRGBA64, *image.RGBA64:
		bitDepth = 16
	}
	channels := layout.ChannelCount()
	pixels := make([]uint16, width*height*channels)
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			offset := (y*width + x) * channels
			if bitDepth == 8 {
				c := color.NRGBAModel.Convert(src.At(bounds.Min.X+x, bounds.Min.Y+y)).(color.NRGBA)
				if layout == LayoutGray {
					pixels[offset] = uint16(c.R)
				} else {
					pixels[offset] = uint16(c.R)
					pixels[offset+1] = uint16(c.G)
					pixels[offset+2] = uint16(c.B)
					pixels[offset+3] = uint16(c.A)
				}
			} else {
				c := color.NRGBA64Model.Convert(src.At(bounds.Min.X+x, bounds.Min.Y+y)).(color.NRGBA64)
				if layout == LayoutGray {
					pixels[offset] = c.R
				} else {
					pixels[offset] = c.R
					pixels[offset+1] = c.G
					pixels[offset+2] = c.B
					pixels[offset+3] = c.A
				}
			}
		}
	}
	img := &Image{
		ID:       filepath.Base(path),
		Path:     path,
		Width:    width,
		Height:   height,
		BitDepth: bitDepth,
		Layout:   layout,
		Pixels:   pixels,
	}
	img.EnsureHash()
	return img
}
