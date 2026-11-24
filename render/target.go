package render

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"math"
	"os"

	"diffrender/errs"
)

const targetMagic = "DRTGT01\n"

// Image is an interleaved RGB, float64 camera image.
type Image struct {
	CameraID string
	Width    int
	Height   int
	Pixels   []float64
}

func (im Image) At(x, y int) (r, g, b float64) {
	i := (y*im.Width + x) * 3
	return im.Pixels[i], im.Pixels[i+1], im.Pixels[i+2]
}

func (im Image) Clone() Image {
	im.Pixels = append([]float64(nil), im.Pixels...)
	return im
}

// Validate checks image shape and payload values. It returns E_DECODE because
// images at this boundary are normally decoded task targets.
func (im Image) Validate(file string) error {
	if im.Width <= 0 || im.Height <= 0 || im.Width > 8192 || im.Height > 8192 {
		return errs.New(errs.CodeDecode, file, "target.resolution", "invalid dimensions %dx%d", im.Width, im.Height)
	}
	if len(im.Pixels) != im.Width*im.Height*3 {
		return errs.New(errs.CodeDecode, file, "target.pixels", "expected %d values, got %d", im.Width*im.Height*3, len(im.Pixels))
	}
	if im.CameraID == "" {
		return errs.New(errs.CodeDecode, file, "target.cameraId", "camera id is empty")
	}
	for i, v := range im.Pixels {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return errs.New(errs.CodeDecode, file, "target.pixels", "non-finite value at index %d", i)
		}
	}
	return nil
}

// EncodeTarget writes a deterministic, checksummed binary target image.
func EncodeTarget(im Image) ([]byte, error) {
	if err := im.Validate("<encode>"); err != nil {
		return nil, err
	}
	if len(im.CameraID) > math.MaxUint32 {
		return nil, errs.New(errs.CodeDecode, "<encode>", "target.cameraId", "camera id is too long")
	}
	var buf bytes.Buffer
	buf.WriteString(targetMagic)
	_ = binary.Write(&buf, binary.BigEndian, uint32(im.Width))
	_ = binary.Write(&buf, binary.BigEndian, uint32(im.Height))
	_ = binary.Write(&buf, binary.BigEndian, uint32(len(im.CameraID)))
	buf.WriteString(im.CameraID)
	for _, v := range im.Pixels {
		_ = binary.Write(&buf, binary.BigEndian, math.Float64bits(v))
	}
	sum := sha256.Sum256(buf.Bytes())
	buf.Write(sum[:])
	return buf.Bytes(), nil
}

// DecodeTarget verifies the checksum before decoding image content.
func DecodeTarget(data []byte, file string) (Image, error) {
	const fixedHeader = 8 + 4 + 4 + 4
	const checksumSize = sha256.Size
	if len(data) < fixedHeader+checksumSize {
		return Image{}, errs.New(errs.CodeDecode, file, "target.length", "target file is truncated")
	}
	if string(data[:8]) != targetMagic {
		return Image{}, errs.New(errs.CodeDecode, file, "target.magic", "invalid target magic")
	}
	payload := data[:len(data)-checksumSize]
	want := sha256.Sum256(payload)
	if !bytes.Equal(want[:], data[len(data)-checksumSize:]) {
		return Image{}, errs.New(errs.CodeDecode, file, "target.checksum", "target content checksum mismatch")
	}
	width := int(binary.BigEndian.Uint32(payload[8:12]))
	height := int(binary.BigEndian.Uint32(payload[12:16]))
	camLen := int(binary.BigEndian.Uint32(payload[16:20]))
	if width <= 0 || height <= 0 || width > 8192 || height > 8192 {
		return Image{}, errs.New(errs.CodeDecode, file, "target.resolution", "invalid dimensions %dx%d", width, height)
	}
	if camLen <= 0 || fixedHeader+camLen > len(payload) {
		return Image{}, errs.New(errs.CodeDecode, file, "target.cameraId", "invalid camera id length %d", camLen)
	}
	cameraID := string(payload[fixedHeader : fixedHeader+camLen])
	values := width * height * 3
	payloadStart := fixedHeader + camLen
	if len(payload)-payloadStart != values*8 {
		return Image{}, errs.New(errs.CodeDecode, file, "target.pixels", "expected %d bytes, got %d", values*8, len(payload)-payloadStart)
	}
	pixels := make([]float64, values)
	for i := range pixels {
		offset := payloadStart + i*8
		pixels[i] = math.Float64frombits(binary.BigEndian.Uint64(payload[offset : offset+8]))
	}
	im := Image{CameraID: cameraID, Width: width, Height: height, Pixels: pixels}
	if err := im.Validate(file); err != nil {
		return Image{}, err
	}
	return im, nil
}

// WriteTargetFile writes a target through a same-directory temporary file and
// rename, so readers never observe a partially written image.
func WriteTargetFile(path string, im Image) error {
	data, err := EncodeTarget(im)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return errs.Wrap(err, errs.CodeDecodeIO, path, "target", "cannot write target")
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return errs.Wrap(err, errs.CodeDecodeIO, path, "target", "cannot commit target")
	}
	return nil
}

// ReadTargetFile reads and verifies a target image.
func ReadTargetFile(path string) (Image, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Image{}, errs.Wrap(err, errs.CodeDecode, path, "target", "cannot read target")
	}
	return DecodeTarget(data, path)
}
