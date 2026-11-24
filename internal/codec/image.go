package codec

import (
	"image"
	_ "image/jpeg"
	_ "image/png"
	"os"
)

type normalizedImage struct {
	Width, Height int
	Channels      int
	Image         image.Image
	MinX, MinY    int
}

func decodeNormalizedImage(path string, declaredChannels int) (*normalizedImage, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	img, _, err := image.Decode(f)
	if err != nil {
		return nil, err
	}
	b := img.Bounds()
	w, h := b.Dx(), b.Dy()
	if w <= 0 || h <= 0 {
		return nil, errInvalid("image has empty bounds")
	}
	out := &normalizedImage{
		Width: w, Height: h, Channels: declaredChannels,
		Image: img, MinX: b.Min.X, MinY: b.Min.Y,
	}
	return out, nil
}

func resizeBlock(src *normalizedImage, dstW, dstH, y0, y1, outChannels int) []uint16 {
	out := make([]uint16, dstW*(y1-y0)*outChannels)
	sw, sh := src.Width, src.Height
	for dy := y0; dy < y1; dy++ {
		sy0 := dy * sh / dstH
		sy1 := (dy + 1) * sh / dstH
		if sy1 <= sy0 {
			sy1 = sy0 + 1
		}
		for dx := 0; dx < dstW; dx++ {
			sx0 := dx * sw / dstW
			sx1 := (dx + 1) * sw / dstW
			if sx1 <= sx0 {
				sx1 = sx0 + 1
			}
			var sum [4]int64
			var count int64
			for sy := sy0; sy < sy1; sy++ {
				for sx := sx0; sx < sx1; sx++ {
					r, g, b, a := src.Image.At(src.MinX+sx, src.MinY+sy).RGBA()
					sum[0] += int64(r)
					sum[1] += int64(g)
					sum[2] += int64(b)
					sum[3] += int64(a)
					count++
				}
			}
			dst := ((dy-y0)*dstW + dx) * outChannels
			for ch := 0; ch < outChannels; ch++ {
				out[dst+ch] = uint16((sum[ch] + count/2) / count)
			}
		}
	}
	return out
}

func applyChromaSampling(pix []uint16, width, height, channels int, sampling string) {
	if sampling == "4:4:4" || width == 0 || height == 0 || channels < 3 {
		return
	}
	n := width * height
	y := make([]uint16, n)
	cb := make([]uint16, n)
	cr := make([]uint16, n)
	for i := 0; i < n; i++ {
		yi, cbi, cri := rgbToYCbCr(pix[i*channels], pix[i*channels+1], pix[i*channels+2])
		y[i], cb[i], cr[i] = yi, cbi, cri
	}
	blockW, blockH := 2, 1
	if sampling == "4:2:0" {
		blockH = 2
	}
	for by := 0; by < height; by += blockH {
		for bx := 0; bx < width; bx += blockW {
			var cbSum, crSum uint64
			var count uint64
			for yy := by; yy < by+blockH && yy < height; yy++ {
				for xx := bx; xx < bx+blockW && xx < width; xx++ {
					i := yy*width + xx
					cbSum += uint64(cb[i])
					crSum += uint64(cr[i])
					count++
				}
			}
			cbAvg := uint16((cbSum + count/2) / count)
			crAvg := uint16((crSum + count/2) / count)
			for yy := by; yy < by+blockH && yy < height; yy++ {
				for xx := bx; xx < bx+blockW && xx < width; xx++ {
					i := yy*width + xx
					r, g, b := yCbCrToRGB(y[i], cbAvg, crAvg)
					pix[i*channels] = r
					pix[i*channels+1] = g
					pix[i*channels+2] = b
				}
			}
		}
	}
}

func rgbToYCbCr(r, g, b uint16) (uint16, uint16, uint16) {
	ri, gi, bi := int64(r), int64(g), int64(b)
	yi := (19595*ri + 38470*gi + 7471*bi + 32768) >> 16
	cbi := ((-11059*ri - 21709*gi + 32768*bi + (32768 << 16)) >> 16)
	cri := ((32768*ri - 27439*gi - 5329*bi + (32768 << 16)) >> 16)
	return clamp16(yi), clamp16(cbi), clamp16(cri)
}

func yCbCrToRGB(y, cb, cr uint16) (uint16, uint16, uint16) {
	yi := int64(y)
	cbi := int64(cb) - 32768
	cri := int64(cr) - 32768
	ri := yi + (91881*cri)>>16
	gi := yi - (22554*cbi+46802*cri)>>16
	bi := yi + (116130*cbi)>>16
	return clamp16(ri), clamp16(gi), clamp16(bi)
}

func clamp16(v int64) uint16 {
	if v < 0 {
		return 0
	}
	if v > 65535 {
		return 65535
	}
	return uint16(v)
}

func quantize(v uint16, bits int) uint16 {
	max := int64((1 << bits) - 1)
	return uint16((int64(v)*max + 32767) / 65535)
}

type invalidError string

func (e invalidError) Error() string { return string(e) }

func errInvalid(s string) error { return invalidError(s) }
