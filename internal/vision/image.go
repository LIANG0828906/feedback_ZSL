package vision

import "image"

type nrgbaImage = image.NRGBA

func NewGrayMask(w, h int) []byte {
	return make([]byte, w*h)
}
