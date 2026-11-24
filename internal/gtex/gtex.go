package gtex

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"texlab/internal/apperr"
	"texlab/internal/model"
)

var magic = [8]byte{'G', 'T', 'E', 'X', '0', '0', '0', '1'}

const headerSize = 8 + 4 + 4 + 1 + 1 + 1 + 1 + 32

type Header struct {
	Width    int    `json:"width"`
	Height   int    `json:"height"`
	Depth    int    `json:"depth"`
	Layout   string `json:"layout"`
	Channels int    `json:"channels"`
	SHA256   string `json:"sha256"`
}

func Write(path string, im model.Image) error {
	if im.Width <= 0 || im.Height <= 0 || (im.Depth != 8 && im.Depth != 16) || im.Channels != 4 {
		return apperr.At(apperr.EPara, 0, "image", "invalid image dimensions/depth/channels")
	}
	layout, err := layoutCode(im.Layout)
	if err != nil {
		return err
	}
	var payload bytes.Buffer
	var tmp [2]byte
	for i := 0; i < im.Width*im.Height; i++ {
		rgba := im.Pixels[i*4 : i*4+4]
		order := []int{0, 1, 2, 3}
		switch im.Layout {
		case "BGRA":
			order = []int{2, 1, 0, 3}
		case "ARGB":
			order = []int{3, 0, 1, 2}
		case "ABGR":
			order = []int{3, 2, 1, 0}
		}
		for _, idx := range order {
			v := rgba[idx]
			if im.Depth == 8 {
				if v > 255 {
					return apperr.At(apperr.EPara, 0, "pixels", "8-bit sample %d exceeds 255", v)
				}
				payload.WriteByte(byte(v))
			} else {
				binary.BigEndian.PutUint16(tmp[:], v)
				payload.Write(tmp[:])
			}
		}
	}
	sum := sha256.Sum256(payload.Bytes())

	var out bytes.Buffer
	out.Write(magic[:])
	binary.Write(&out, binary.BigEndian, uint32(im.Width))
	binary.Write(&out, binary.BigEndian, uint32(im.Height))
	out.WriteByte(byte(im.Depth))
	out.WriteByte(layout)
	out.WriteByte(byte(im.Channels))
	out.WriteByte(0)
	out.Write(sum[:])
	out.Write(payload.Bytes())
	return writeAtomic(path, out.Bytes())
}

func Read(path string) (model.Image, Header, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return model.Image{}, Header{}, &apperr.Error{Code: apperr.EDecode, Message: "read source image", Path: path, Details: err.Error()}
	}
	return Decode(raw)
}

func Decode(raw []byte) (model.Image, Header, error) {
	if len(raw) < headerSize {
		return model.Image{}, Header{}, apperr.New(apperr.EDecode, "source image shorter than GTEX header")
	}
	if !bytes.Equal(raw[:8], magic[:]) {
		return model.Image{}, Header{}, apperr.New(apperr.EDecode, "invalid GTEX magic")
	}
	width := int(binary.BigEndian.Uint32(raw[8:12]))
	height := int(binary.BigEndian.Uint32(raw[12:16]))
	depth := int(raw[16])
	layout, err := layoutName(raw[17])
	if err != nil {
		return model.Image{}, Header{}, &apperr.Error{Code: apperr.EDecode, Message: err.Error(), Field: "layout"}
	}
	channels := int(raw[18])
	if width <= 0 || height <= 0 || width > 32768 || height > 32768 || (depth != 8 && depth != 16) || channels != 4 {
		return model.Image{}, Header{}, apperr.New(apperr.EDecode, "invalid GTEX dimensions, depth, or channel count")
	}
	expected := width * height * channels
	if depth == 16 {
		expected *= 2
	}
	if len(raw) != headerSize+expected {
		return model.Image{}, Header{}, apperr.New(apperr.EDecode, "GTEX payload length mismatch: got %d want %d", len(raw)-headerSize, expected)
	}
	want := raw[20:52]
	got := sha256.Sum256(raw[headerSize:])
	if !bytes.Equal(want, got[:]) {
		return model.Image{}, Header{}, apperr.New(apperr.EDecode, "GTEX SHA-256 mismatch")
	}
	im := model.NewImage(width, height, depth, channels, layout, "straight")
	off := headerSize
	for i := 0; i < width*height; i++ {
		slots := [4]uint16{}
		for c := 0; c < channels; c++ {
			var v uint16
			if depth == 8 {
				v = uint16(raw[off])
				off++
			} else {
				v = binary.BigEndian.Uint16(raw[off : off+2])
				off += 2
			}
			slots[c] = v
		}
		rgba, err := layoutToSemantic(layout, slots)
		if err != nil {
			return model.Image{}, Header{}, &apperr.Error{Code: apperr.EDecode, Message: err.Error(), Field: "layout"}
		}
		copy(im.Pixels[i*4:i*4+4], rgba[:])
	}
	h := Header{Width: width, Height: height, Depth: depth, Layout: layout, Channels: channels, SHA256: hex.EncodeToString(got[:])}
	return im, h, nil
}

func FileHash(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", &apperr.Error{Code: apperr.EIO, Message: "open source", Path: path, Details: err.Error()}
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", &apperr.Error{Code: apperr.EIO, Message: "hash source", Path: path, Details: err.Error()}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func HeaderJSON(im model.Image, sha string) Header {
	return Header{Width: im.Width, Height: im.Height, Depth: im.Depth, Layout: im.Layout, Channels: im.Channels, SHA256: sha}
}

func MarshalHeader(header Header) ([]byte, error) {
	return json.MarshalIndent(header, "", "  ")
}

func layoutCode(layout string) (byte, error) {
	switch layout {
	case "RGBA":
		return 0, nil
	case "BGRA":
		return 1, nil
	case "ARGB":
		return 2, nil
	case "ABGR":
		return 3, nil
	}
	return 0, apperr.At(apperr.EPara, 0, "layout", "unsupported layout %q", layout)
}

func layoutName(v byte) (string, error) {
	switch v {
	case 0:
		return "RGBA", nil
	case 1:
		return "BGRA", nil
	case 2:
		return "ARGB", nil
	case 3:
		return "ABGR", nil
	}
	return "", fmt.Errorf("unknown layout code %d", v)
}

func layoutToSemantic(layout string, slots [4]uint16) ([4]uint16, error) {
	var out [4]uint16
	order := []int{0, 1, 2, 3}
	switch layout {
	case "RGBA":
	case "BGRA":
		order = []int{2, 1, 0, 3}
	case "ARGB":
		order = []int{1, 2, 3, 0}
	case "ABGR":
		order = []int{3, 2, 1, 0}
	default:
		return out, fmt.Errorf("unknown layout %q", layout)
	}
	for slot, semantic := range order {
		out[semantic] = slots[slot]
	}
	return out, nil
}

func writeAtomic(path string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return &apperr.Error{Code: apperr.EIO, Message: "create source directory", Path: path, Details: err.Error()}
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".gtex-*.tmp")
	if err != nil {
		return &apperr.Error{Code: apperr.EIO, Message: "create source temp", Path: path, Details: err.Error()}
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return &apperr.Error{Code: apperr.EIO, Message: "write source temp", Path: path, Details: err.Error()}
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return &apperr.Error{Code: apperr.EIO, Message: "sync source temp", Path: path, Details: err.Error()}
	}
	if err := tmp.Close(); err != nil {
		return &apperr.Error{Code: apperr.EIO, Message: "close source temp", Path: path, Details: err.Error()}
	}
	if err := os.Rename(tmpName, path); err != nil {
		return &apperr.Error{Code: apperr.EIO, Message: "replace source", Path: path, Details: err.Error()}
	}
	return nil
}
