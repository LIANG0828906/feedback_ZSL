package frameio

import (
	"encoding/binary"
	"os"

	"screenverify/internal/model"
	"screenverify/internal/rverr"
)

const HeaderSize = 24

func WriteFile(path string, width, height, channels int, pixels []byte) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()

	header := make([]byte, HeaderSize)
	copy(header[0:8], []byte(model.FrameMagic))
	binary.BigEndian.PutUint32(header[8:12], uint32(width))
	binary.BigEndian.PutUint32(header[12:16], uint32(height))
	binary.BigEndian.PutUint32(header[16:20], uint32(channels))
	binary.BigEndian.PutUint32(header[20:24], 0)
	if _, err := f.Write(header); err != nil {
		return err
	}
	if _, err := f.Write(pixels); err != nil {
		return err
	}
	return f.Close()
}

func ReadFile(path string) (*model.Frame, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, rverr.New(rverr.EDecode, "frame file missing: "+path)
		}
		return nil, rverr.Wrap(rverr.EDecode, "read frame file", err)
	}
	if len(b) < HeaderSize {
		return nil, rverr.New(rverr.EDecode, "truncated frame header: "+path)
	}
	if string(b[0:8]) != model.FrameMagic {
		return nil, rverr.New(rverr.EDecode, "bad frame magic: "+path)
	}
	width := int(binary.BigEndian.Uint32(b[8:12]))
	height := int(binary.BigEndian.Uint32(b[12:16]))
	channels := int(binary.BigEndian.Uint32(b[16:20]))
	if width <= 0 || height <= 0 || channels <= 0 || channels > 4 {
		return nil, rverr.Field(rverr.EDecode, "frame_header", "invalid frame header dimensions")
	}
	pixelCount := width * height * channels
	if len(b)-HeaderSize < pixelCount {
		return nil, rverr.Field(rverr.EDecode, "frame_data", "truncated pixel data: "+path)
	}
	if len(b)-HeaderSize > pixelCount {
		return nil, rverr.Field(rverr.EDecode, "frame_data", "trailing bytes in frame file: "+path)
	}
	pixels := make([]byte, pixelCount)
	copy(pixels, b[HeaderSize:HeaderSize+pixelCount])
	return &model.Frame{
		Width:    width,
		Height:   height,
		Channels: channels,
		Pixels:   pixels,
	}, nil
}
