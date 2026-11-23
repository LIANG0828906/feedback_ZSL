package ortho

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"image"
	"image/png"
	"io"
	"os"
)

// DecodeImage 解码 PNG 输入，失败返回 E_DECODE。
// 支持单通道 *image.Gray 与三通道 *image.RGBA（其他类型统一转为 RGBA）。
func DecodeImage(r io.Reader) (image.Image, *Error) {
	img, err := png.Decode(r)
	if err != nil {
		return nil, &Error{Code: EDecode, Msg: "input image is not decodable: " + err.Error()}
	}
	return NormalizeImage(img), nil
}

// LoadImageFile 从磁盘读取并解码图像。
func LoadImageFile(path string) (image.Image, *Error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, &Error{Code: EDecode, Msg: "cannot open input: " + err.Error()}
	}
	defer f.Close()
	return DecodeImage(f)
}

// NormalizeImage 将任意图像规整为组件支持的栅格类型。
func NormalizeImage(img image.Image) image.Image {
	switch img.(type) {
	case *image.Gray, *image.RGBA:
		return img
	}
	b := img.Bounds()
	out := image.NewRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
	for y := 0; y < b.Dy(); y++ {
		for x := 0; x < b.Dx(); x++ {
			r, g, bl, _ := img.At(b.Min.X+x, b.Min.Y+y).RGBA()
			i := out.PixOffset(x, y)
			out.Pix[i+0] = uint8(r >> 8)
			out.Pix[i+1] = uint8(g >> 8)
			out.Pix[i+2] = uint8(bl >> 8)
			out.Pix[i+3] = 255
		}
	}
	return out
}

// Channels 返回栅格通道数（1 或 3；RGBA 的 alpha 视为不透明填充）。
func Channels(img image.Image) int {
	if _, ok := img.(*image.Gray); ok {
		return 1
	}
	return 3
}

// NewOutputLike 创建与输入通道数一致、尺寸为 w*h 的输出栅格。
func NewOutputLike(src image.Image, w, h int) image.Image {
	if Channels(src) == 1 {
		return image.NewGray(image.Rect(0, 0, w, h))
	}
	out := image.NewRGBA(image.Rect(0, 0, w, h))
	for i := 3; i < len(out.Pix); i += 4 {
		out.Pix[i] = 255
	}
	return out
}

// EncodePNG 将图像编码为 PNG 字节。
func EncodePNG(img image.Image) ([]byte, error) {
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// ContentHash 计算图像内容哈希（颜色模型、尺寸与全部像素）。
func ContentHash(img image.Image) string {
	h := sha256.New()
	h.Write([]byte{byte(Channels(img))})
	b := img.Bounds()
	var dim [8]byte
	binary.BigEndian.PutUint32(dim[0:4], uint32(b.Dx()))
	binary.BigEndian.PutUint32(dim[4:8], uint32(b.Dy()))
	h.Write(dim[:])
	switch im := img.(type) {
	case *image.Gray:
		h.Write(im.Pix)
	case *image.RGBA:
		h.Write(im.Pix)
	default:
		for y := b.Min.Y; y < b.Max.Y; y++ {
			for x := b.Min.X; x < b.Max.X; x++ {
				r, g, bl, a := img.At(x, y).RGBA()
				var px [4]byte
				binary.BigEndian.PutUint16(px[0:2], uint16(r))
				_ = g
				_ = bl
				_ = a
				h.Write(px[:])
			}
		}
	}
	return hex.EncodeToString(h.Sum(nil))
}

// HashBytes 计算任意字节的十六进制哈希。
func HashBytes(parts ...[]byte) string {
	h := sha256.New()
	for _, p := range parts {
		var ln [4]byte
		binary.BigEndian.PutUint32(ln[:], uint32(len(p)))
		h.Write(ln[:])
		h.Write(p)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// getChannel 读取 (x,y) 处第 c 通道的值（0-255）。
func getChannel(img image.Image, x, y, c int) uint8 {
	switch im := img.(type) {
	case *image.Gray:
		return im.Pix[y*im.Stride+x]
	case *image.RGBA:
		return im.Pix[y*im.Stride+x*4+c]
	}
	return 0
}

// setChannel 写入 (x,y) 处第 c 通道的值。
func setChannel(img image.Image, x, y, c int, v uint8) {
	switch im := img.(type) {
	case *image.Gray:
		im.Pix[y*im.Stride+x] = v
	case *image.RGBA:
		im.Pix[y*im.Stride+x*4+c] = v
	}
}

// sampleNearest 以 0 阶（最近邻）插值采样第 c 通道。
func sampleNearest(img image.Image, fx, fy float64, c int) uint8 {
	w := img.Bounds().Dx()
	h := img.Bounds().Dy()
	x := int(fx + 0.5)
	y := int(fy + 0.5)
	x = clampInt(x, 0, w-1)
	y = clampInt(y, 0, h-1)
	return getChannel(img, x, y, c)
}

// sampleBilinear 以 1 阶（双线性）插值采样第 c 通道。
func sampleBilinear(img image.Image, fx, fy float64, c int) uint8 {
	w := img.Bounds().Dx()
	h := img.Bounds().Dy()
	if fx < 0 {
		fx = 0
	}
	if fy < 0 {
		fy = 0
	}
	if fx > float64(w-1) {
		fx = float64(w - 1)
	}
	if fy > float64(h-1) {
		fy = float64(h - 1)
	}
	x0 := int(fx)
	y0 := int(fy)
	x1 := x0 + 1
	y1 := y0 + 1
	if x1 > w-1 {
		x1 = w - 1
	}
	if y1 > h-1 {
		y1 = h - 1
	}
	ax := fx - float64(x0)
	ay := fy - float64(y0)
	v00 := float64(getChannel(img, x0, y0, c))
	v10 := float64(getChannel(img, x1, y0, c))
	v01 := float64(getChannel(img, x0, y1, c))
	v11 := float64(getChannel(img, x1, y1, c))
	v := v00*(1-ax)*(1-ay) + v10*ax*(1-ay) + v01*(1-ax)*ay + v11*ax*ay
	if v < 0 {
		v = 0
	}
	if v > 255 {
		v = 255
	}
	return uint8(v + 0.5)
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
