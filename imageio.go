package textureconv

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
)

var ximgMagic = [8]byte{'T', 'X', 'I', 'M', 'G', '0', '1', 0}
var stageMagic = [8]byte{'T', 'X', 'S', 'T', 'G', '0', '1', 0}

const ximgHeaderSize = 32
const stageHeaderSize = 32

// SaveImageFile writes the deterministic XIMG container. The format stores a
// small binary header followed by little-endian uint16 components.
func SaveImageFile(path string, im *Image) (string, error) {
	if err := im.Validate(); err != nil {
		return "", err
	}
	buf := make([]byte, ximgHeaderSize+len(im.Pixels)*2)
	copy(buf[:8], ximgMagic[:])
	binary.LittleEndian.PutUint32(buf[8:12], uint32(im.Width))
	binary.LittleEndian.PutUint32(buf[12:16], uint32(im.Height))
	binary.LittleEndian.PutUint16(buf[16:18], uint16(im.BitDepth))
	binary.LittleEndian.PutUint16(buf[18:20], 4)
	binary.LittleEndian.PutUint16(buf[20:22], im.Layout.Code())
	binary.LittleEndian.PutUint16(buf[22:24], 0)
	binary.LittleEndian.PutUint64(buf[24:32], uint64(len(im.Pixels)*2))
	for i, v := range im.Pixels {
		binary.LittleEndian.PutUint16(buf[ximgHeaderSize+i*2:], v)
	}
	if err := os.WriteFile(path, buf, 0o644); err != nil {
		return "", ioErr("writeImage", err)
	}
	sum := sha256.Sum256(buf)
	return hex.EncodeToString(sum[:]), nil
}

// LoadImageFile opens and validates an XIMG source image.
func LoadImageFile(path string) (*Image, string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, "", ioErr("readImage", err)
	}
	sum := sha256.Sum256(data)
	if len(data) < ximgHeaderSize {
		return nil, "", schemaErr(0, "image", "file is shorter than XIMG header")
	}
	if !bytes.Equal(data[:8], ximgMagic[:]) {
		return nil, "", schemaErr(0, "image", "bad XIMG magic")
	}
	w := int(binary.LittleEndian.Uint32(data[8:12]))
	h := int(binary.LittleEndian.Uint32(data[12:16]))
	depth := BitDepth(binary.LittleEndian.Uint16(data[16:18]))
	channels := int(binary.LittleEndian.Uint16(data[18:20]))
	layoutCode := binary.LittleEndian.Uint16(data[20:22])
	payloadLen := binary.LittleEndian.Uint64(data[24:32])
	layout, ok := layoutFromCode(layoutCode)
	if !ok {
		return nil, "", paramErr(0, "layout", "unknown layout code %d", layoutCode)
	}
	if channels != 4 {
		return nil, "", paramErr(0, "channels", "source images must have 4 channels, got %d", channels)
	}
	if payloadLen != uint64(len(data)-ximgHeaderSize) {
		return nil, "", schemaErr(0, "image", "payload length mismatch: header=%d file=%d", payloadLen, len(data)-ximgHeaderSize)
	}
	if payloadLen%2 != 0 {
		return nil, "", schemaErr(0, "image", "payload is not uint16 aligned")
	}
	components := int(payloadLen / 2)
	if w <= 0 || h <= 0 || w > 1<<30 || h > 1<<30 {
		return nil, "", schemaErr(0, "geometry", "invalid dimensions %dx%d", w, h)
	}
	if int64(components) != int64(w)*int64(h)*int64(channels) {
		return nil, "", schemaErr(0, "image", "component count %d does not match %dx%dx%d", components, w, h, channels)
	}
	pixels := make([]uint16, components)
	for i := range pixels {
		pixels[i] = binary.LittleEndian.Uint16(data[ximgHeaderSize+i*2:])
	}
	im := &Image{Width: w, Height: h, BitDepth: depth, Layout: layout, Pixels: pixels}
	if err := im.Validate(); err != nil {
		return nil, "", err
	}
	return im, hex.EncodeToString(sum[:]), nil
}

// ImageSummary is a compact audit representation of a stage image.
type ImageSummary struct {
	Width      int              `json:"width"`
	Height     int              `json:"height"`
	BitDepth   BitDepth         `json:"bitDepth"`
	Channels   int              `json:"channels"`
	AlphaIndex int              `json:"alphaIndex"`
	Hash       string           `json:"hash"`
	Dropped    []DroppedChannel `json:"dropped,omitempty"`
}

func summarizeStage(im *StageImage) ImageSummary {
	return ImageSummary{
		Width: im.Width, Height: im.Height, BitDepth: im.BitDepth,
		Channels: im.Channels, AlphaIndex: im.AlphaIndex,
		Hash: HashStageImage(im), Dropped: append([]DroppedChannel(nil), im.Dropped...),
	}
}

// HashStageImage hashes all header properties and component bytes.
func HashStageImage(im *StageImage) string {
	h := sha256.New()
	writeHashHeader(h, im.Width, im.Height, im.BitDepth, im.Channels, im.AlphaIndex)
	var b [2]byte
	for _, v := range im.Pixels {
		binary.LittleEndian.PutUint16(b[:], v)
		_, _ = h.Write(b[:])
	}
	for _, d := range im.Dropped {
		fmt.Fprintf(h, "%d:%d;", d.Entry, d.Channel)
	}
	return hex.EncodeToString(h.Sum(nil))
}

func writeHashHeader(w io.Writer, width, height int, depth BitDepth, channels, alpha int) {
	var b [12]byte
	binary.LittleEndian.PutUint32(b[0:4], uint32(width))
	binary.LittleEndian.PutUint32(b[4:8], uint32(height))
	b[8] = byte(depth)
	b[9] = byte(channels)
	b[10] = byte(alpha + 1)
	_, _ = w.Write(b[:])
}

func sourceTileHash(im *Image, t Tile) string {
	h := sha256.New()
	writeHashHeader(h, t.W, t.H, im.BitDepth, 4, 3)
	var b [2]byte
	for y := t.Y; y < t.Y+t.H; y++ {
		row := im.Pixels[(y*im.Width+t.X)*4 : (y*im.Width+t.X+t.W)*4]
		for _, v := range row {
			binary.LittleEndian.PutUint16(b[:], v)
			_, _ = h.Write(b[:])
		}
	}
	return hex.EncodeToString(h.Sum(nil))
}

func stageTileHash(im *StageImage, t Tile) string {
	h := sha256.New()
	writeHashHeader(h, im.Width, im.Height, im.BitDepth, im.Channels, im.AlphaIndex)
	var b [2]byte
	for y := 0; y < im.Height; y++ {
		off := y * im.Width * im.Channels
		for _, v := range im.Pixels[off : off+im.Width*im.Channels] {
			binary.LittleEndian.PutUint16(b[:], v)
			_, _ = h.Write(b[:])
		}
	}
	for _, d := range im.Dropped {
		fmt.Fprintf(h, "drop:%d:%d;", d.Entry, d.Channel)
	}
	_ = t
	return hex.EncodeToString(h.Sum(nil))
}

func bytesToPixels(b []byte) ([]uint16, error) {
	if len(b)%2 != 0 {
		return nil, schemaErr(0, "pixels", "pixel blob is not uint16 aligned")
	}
	out := make([]uint16, len(b)/2)
	for i := range out {
		out[i] = binary.LittleEndian.Uint16(b[i*2:])
	}
	return out, nil
}

func pixelsToBytes(p []uint16) []byte {
	b := make([]byte, len(p)*2)
	for i, v := range p {
		binary.LittleEndian.PutUint16(b[i*2:], v)
	}
	return b
}

// SaveStageFile exports a converted stage with a self-describing header.
func SaveStageFile(path string, im *StageImage) error {
	if err := im.Validate(); err != nil {
		return err
	}
	dropped, err := json.Marshal(im.Dropped)
	if err != nil {
		return ioErr("marshalDropped", err)
	}
	bodyLen := 4 + len(dropped) + len(im.Pixels)*2
	buf := make([]byte, stageHeaderSize+bodyLen)
	copy(buf[:8], stageMagic[:])
	binary.LittleEndian.PutUint32(buf[8:12], uint32(im.Width))
	binary.LittleEndian.PutUint32(buf[12:16], uint32(im.Height))
	binary.LittleEndian.PutUint16(buf[16:18], uint16(im.BitDepth))
	binary.LittleEndian.PutUint16(buf[18:20], uint16(im.Channels))
	binary.LittleEndian.PutUint16(buf[20:22], uint16(int16(im.AlphaIndex)))
	binary.LittleEndian.PutUint16(buf[22:24], 0)
	binary.LittleEndian.PutUint64(buf[24:32], uint64(bodyLen))
	binary.LittleEndian.PutUint32(buf[stageHeaderSize:stageHeaderSize+4], uint32(len(dropped)))
	copy(buf[stageHeaderSize+4:], dropped)
	pixelStart := stageHeaderSize + 4 + len(dropped)
	for i, v := range im.Pixels {
		binary.LittleEndian.PutUint16(buf[pixelStart+i*2:], v)
	}
	if err := os.WriteFile(path, buf, 0o644); err != nil {
		return ioErr("writeStage", err)
	}
	return nil
}

func LoadStageFile(path string) (*StageImage, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, ioErr("readStage", err)
	}
	if len(data) < stageHeaderSize {
		return nil, schemaErr(0, "stage", "file is shorter than stage header")
	}
	if !bytes.Equal(data[:8], stageMagic[:]) {
		return nil, schemaErr(0, "stage", "bad stage magic")
	}
	w := int(binary.LittleEndian.Uint32(data[8:12]))
	h := int(binary.LittleEndian.Uint32(data[12:16]))
	depth := BitDepth(binary.LittleEndian.Uint16(data[16:18]))
	channels := int(binary.LittleEndian.Uint16(data[18:20]))
	alpha := int(int16(binary.LittleEndian.Uint16(data[20:22])))
	payloadLen := binary.LittleEndian.Uint64(data[24:32])
	if payloadLen != uint64(len(data)-stageHeaderSize) {
		return nil, schemaErr(0, "stage", "payload length mismatch")
	}
	body := data[stageHeaderSize:]
	if len(body) < 4 {
		return nil, schemaErr(0, "stage", "stage body is too short")
	}
	droppedLen := int(binary.LittleEndian.Uint32(body[:4]))
	if droppedLen < 0 || 4+droppedLen > len(body) {
		return nil, schemaErr(0, "stage", "dropped metadata length is invalid")
	}
	var dropped []DroppedChannel
	if droppedLen > 0 {
		if err := json.Unmarshal(body[4:4+droppedLen], &dropped); err != nil {
			return nil, schemaErr(0, "stage", "invalid dropped metadata: %v", err)
		}
	}
	pix, err := bytesToPixels(body[4+droppedLen:])
	if err != nil {
		return nil, err
	}
	im := &StageImage{Width: w, Height: h, BitDepth: depth, Channels: channels, AlphaIndex: alpha, Pixels: pix, Dropped: dropped}
	if err := im.Validate(); err != nil {
		return nil, err
	}
	return im, nil
}
