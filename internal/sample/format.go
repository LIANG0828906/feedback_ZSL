package sample

import (
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"math"
	"os"
	"path/filepath"

	"hdrproof/internal/apperr"
	"hdrproof/internal/model"
)

const (
	sequenceMagic  = "HDRSEQ01"
	frameMagic     = "FRM1"
	fileHeaderLen  = 32
	frameHeaderLen = 16
	trailerLen     = 4
)

func BytesPerChannel(encoding string) int {
	switch encoding {
	case model.EncodingLinearF32:
		return 4
	case model.EncodingRGB10LE:
		return 2
	default:
		return 0
	}
}

func FramePayloadSize(r model.Route) int {
	return r.Width * r.Height * r.Channels * BytesPerChannel(r.Encoding)
}

func EncodePayload(linear []float64, encoding string) ([]byte, error) {
	switch encoding {
	case model.EncodingLinearF32:
		out := make([]byte, len(linear)*4)
		for i, v := range linear {
			if !model.Finite(v) {
				return nil, fmt.Errorf("non-finite sample value at index %d", i)
			}
			binary.LittleEndian.PutUint32(out[i*4:], math.Float32bits(float32(v)))
		}
		return out, nil
	case model.EncodingRGB10LE:
		out := make([]byte, len(linear)*2)
		for i, v := range linear {
			if !model.Finite(v) || v < 0 || v > 1 {
				return nil, fmt.Errorf("rgb10le value at index %d is outside [0,1]: %v", i, v)
			}
			binary.LittleEndian.PutUint16(out[i*2:], uint16(math.Round(v*1023)))
		}
		return out, nil
	default:
		return nil, fmt.Errorf("unsupported encoding %q", encoding)
	}
}

func DecodePayload(payload []byte, r model.Route) ([]float64, error) {
	expected := FramePayloadSize(r)
	if len(payload) != expected {
		return nil, apperr.New(apperr.CodeDecode, r.ID, -1, -1, "frame_payload",
			fmt.Sprintf("%d bytes", expected), fmt.Sprintf("received %d bytes", len(payload)), nil)
	}
	out := make([]float64, r.Width*r.Height*r.Channels)
	switch r.Encoding {
	case model.EncodingLinearF32:
		for i := range out {
			v := float64(math.Float32frombits(binary.LittleEndian.Uint32(payload[i*4:])))
			if !model.Finite(v) || v < 0 {
				return nil, apperr.New(apperr.CodeDecode, r.ID, -1, -1, "frame_payload",
					"finite non-negative float32", fmt.Sprintf("invalid sample at channel index %d: %v", i, v), nil)
			}
			out[i] = v
		}
	case model.EncodingRGB10LE:
		for i := range out {
			v := binary.LittleEndian.Uint16(payload[i*2:])
			if v > 1023 {
				return nil, apperr.New(apperr.CodeDecode, r.ID, -1, -1, "frame_payload",
					"10-bit value <= 1023", fmt.Sprintf("invalid sample at channel index %d: %d", i, v), nil)
			}
			out[i] = float64(v) / 1023.0
		}
	default:
		return nil, apperr.New(apperr.CodeMeta, r.ID, -1, -1, "encoding",
			model.EncodingLinearF32+"|"+model.EncodingRGB10LE, "unsupported encoding", nil)
	}
	return out, nil
}

func WriteSequence(path string, r model.Route, frames []model.RawFrame) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()

	header := make([]byte, fileHeaderLen)
	copy(header[0:8], sequenceMagic)
	binary.LittleEndian.PutUint32(header[8:12], 1)
	binary.LittleEndian.PutUint32(header[12:16], uint32(r.Width))
	binary.LittleEndian.PutUint32(header[16:20], uint32(r.Height))
	binary.LittleEndian.PutUint32(header[20:24], uint32(r.Channels))
	binary.LittleEndian.PutUint32(header[24:28], uint32(BytesPerChannel(r.Encoding)))
	binary.LittleEndian.PutUint32(header[28:32], uint32(len(frames)))
	if _, err := f.Write(header); err != nil {
		return err
	}
	for _, frame := range frames {
		fh := make([]byte, frameHeaderLen)
		copy(fh[0:4], frameMagic)
		binary.LittleEndian.PutUint64(fh[4:12], uint64(frame.TimestampMS))
		binary.LittleEndian.PutUint32(fh[12:16], uint32(len(frame.Payload)))
		if _, err := f.Write(fh); err != nil {
			return err
		}
		if _, err := f.Write(frame.Payload); err != nil {
			return err
		}
		var trailer [4]byte
		binary.LittleEndian.PutUint32(trailer[:], crc32.ChecksumIEEE(frame.Payload))
		if _, err := f.Write(trailer[:]); err != nil {
			return err
		}
	}
	return f.Sync()
}

func ReadSequence(path string, r model.Route) ([]model.RawFrame, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, apperr.New(apperr.CodeDecode, r.ID, -1, -1, "frame_file",
			"readable HDR sequence", err.Error(), err)
	}
	if len(data) < fileHeaderLen {
		return nil, apperr.New(apperr.CodeDecode, r.ID, -1, -1, "frame_file",
			"at least 32-byte header", "truncated sequence header", nil)
	}
	if string(data[0:8]) != sequenceMagic {
		return nil, apperr.New(apperr.CodeDecode, r.ID, -1, -1, "frame_file",
			sequenceMagic, fmt.Sprintf("received magic %q", string(data[0:8])), nil)
	}
	version := binary.LittleEndian.Uint32(data[8:12])
	if version != 1 {
		return nil, apperr.New(apperr.CodeMeta, r.ID, -1, -1, "schema_version", "1",
			fmt.Sprintf("sequence version %d is unsupported", version), nil)
	}
	file := model.Route{
		ID:       r.ID,
		Width:    int(binary.LittleEndian.Uint32(data[12:16])),
		Height:   int(binary.LittleEndian.Uint32(data[16:20])),
		Channels: int(binary.LittleEndian.Uint32(data[20:24])),
	}
	bpc := int(binary.LittleEndian.Uint32(data[24:28]))
	count := int(binary.LittleEndian.Uint32(data[28:32]))
	for _, item := range []struct {
		field    string
		actual   int
		expected int
	}{
		{"width", file.Width, r.Width},
		{"height", file.Height, r.Height},
		{"channels", file.Channels, r.Channels},
		{"frame_count", count, r.FrameCount},
		{"bytes_per_channel", bpc, BytesPerChannel(r.Encoding)},
	} {
		if item.actual != item.expected {
			return nil, apperr.New(apperr.CodeMeta, r.ID, -1, -1, item.field,
				fmt.Sprint(item.expected), fmt.Sprintf("received %d", item.actual), nil)
		}
	}
	payloadSize := FramePayloadSize(r)
	frames := make([]model.RawFrame, 0, count)
	off := fileHeaderLen
	var previous int64 = -1
	for i := 0; i < count; i++ {
		if len(data)-off < frameHeaderLen+trailerLen {
			return nil, apperr.New(apperr.CodeDecode, r.ID, -1, i, "frame_payload",
				"complete frame block", "truncated frame block", nil)
		}
		if string(data[off:off+4]) != frameMagic {
			return nil, apperr.New(apperr.CodeDecode, r.ID, -1, i, "frame_magic",
				frameMagic, fmt.Sprintf("received %q", string(data[off:off+4])), nil)
		}
		timestamp := int64(binary.LittleEndian.Uint64(data[off+4 : off+12]))
		payloadLen := int(binary.LittleEndian.Uint32(data[off+12 : off+16]))
		if payloadLen != payloadSize {
			return nil, apperr.New(apperr.CodeDecode, r.ID, -1, i, "frame_payload",
				fmt.Sprintf("%d bytes", payloadSize), fmt.Sprintf("received %d bytes", payloadLen), nil)
		}
		off += frameHeaderLen
		if len(data)-off < payloadLen+trailerLen {
			return nil, apperr.New(apperr.CodeDecode, r.ID, -1, i, "frame_payload",
				"complete payload and CRC", "truncated payload", nil)
		}
		payload := append([]byte(nil), data[off:off+payloadLen]...)
		off += payloadLen
		storedCRC := binary.LittleEndian.Uint32(data[off : off+4])
		off += trailerLen
		if actual := crc32.ChecksumIEEE(payload); actual != storedCRC {
			return nil, apperr.New(apperr.CodeDecode, r.ID, -1, i, "frame_crc32",
				fmt.Sprintf("%08x", storedCRC), fmt.Sprintf("received %08x", actual), nil)
		}
		if i < len(r.TimestampsMS) && timestamp != r.TimestampsMS[i] {
			return nil, apperr.New(apperr.CodeMeta, r.ID, -1, i, "timestamps_ms",
				fmt.Sprint(r.TimestampsMS[i]), fmt.Sprintf("received %d in frame header", timestamp), nil)
		}
		if timestamp < 0 || timestamp <= previous {
			return nil, apperr.New(apperr.CodeMeta, r.ID, -1, i, "timestamps_ms",
				"strictly increasing", fmt.Sprintf("received %d after %d", timestamp, previous), nil)
		}
		previous = timestamp
		frames = append(frames, model.RawFrame{Index: i, TimestampMS: timestamp, Payload: payload})
	}
	if off != len(data) {
		return nil, apperr.New(apperr.CodeDecode, r.ID, -1, -1, "frame_file",
			"exact frame_count blocks", fmt.Sprintf("%d trailing bytes", len(data)-off), nil)
	}
	return frames, nil
}
