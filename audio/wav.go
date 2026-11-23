package audio

import (
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"path/filepath"

	"loudnessworkbench/apperr"
)

const (
	formatPCM        = 1
	formatIEEEFFloat = 3
	formatExtensible = 0xFFFE
)

type wavFormat struct {
	format        uint16
	channels      int
	sampleRate    int
	blockAlign    int
	bitsPerSample int
}

func ReadWAV(path string) (Signal, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Signal{}, apperr.Wrap(apperr.CodeDecode, "cannot read audio sample", err).WithField("audio")
	}
	s, err := DecodeWAV(data)
	if err != nil {
		if e, ok := apperr.As(err); ok {
			e.Field = "audio"
			if e.Path == "" {
				e.Path = filepath.ToSlash(path)
			}
		}
		return Signal{}, err
	}
	return s, nil
}

func DecodeWAV(data []byte) (Signal, error) {
	if len(data) < 12 || string(data[0:4]) != "RIFF" || string(data[8:12]) != "WAVE" {
		return Signal{}, apperr.New(apperr.CodeDecode, "invalid RIFF/WAVE header")
	}
	var (
		format    *wavFormat
		audioData []byte
		dataSeen  bool
		fmtSeen   bool
	)
	offset := 12
	for offset+8 <= len(data) {
		id := string(data[offset : offset+4])
		size := int(binary.LittleEndian.Uint32(data[offset+4 : offset+8]))
		offset += 8
		if size < 0 || offset+size > len(data) {
			return Signal{}, apperr.New(apperr.CodeDecode, "truncated WAV chunk").WithField("chunks").WithActual(id)
		}
		chunk := data[offset : offset+size]
		switch id {
		case "fmt ":
			parsed, err := parseWAVFormat(chunk)
			if err != nil {
				return Signal{}, err
			}
			format = &parsed
			fmtSeen = true
		case "data":
			if !fmtSeen {
				return Signal{}, apperr.New(apperr.CodeDecode, "WAV data chunk precedes fmt chunk")
			}
			audioData = chunk
			dataSeen = true
		}
		offset += size
		if size%2 == 1 {
			offset++
		}
	}
	if format == nil || !dataSeen {
		return Signal{}, apperr.New(apperr.CodeDecode, "WAV is missing fmt or data chunk")
	}
	if format.channels != 1 && format.channels != 2 {
		return Signal{}, apperr.New(apperr.CodeDecode, "WAV channel count is not supported").
			WithField("channels").WithActual(format.channels)
	}
	if format.sampleRate <= 0 {
		return Signal{}, apperr.New(apperr.CodeDecode, "WAV sample rate is invalid").WithField("sample_rate_hz").WithActual(format.sampleRate)
	}
	if format.blockAlign != format.channels*((format.bitsPerSample+7)/8) {
		return Signal{}, apperr.New(apperr.CodeDecode, "WAV block alignment is inconsistent").WithField("block_align")
	}
	if len(audioData)%format.blockAlign != 0 {
		return Signal{}, apperr.New(apperr.CodeDecode, "WAV data is not frame aligned").WithField("data")
	}
	if !supportedDecodeFormat(format.format, format.bitsPerSample) {
		return Signal{}, apperr.New(apperr.CodeDecode, "unsupported WAV encoding").
			WithField("format").WithActual(fmt.Sprintf("format=%d bits=%d", format.format, format.bitsPerSample))
	}

	frames := len(audioData) / format.blockAlign
	out := New(format.sampleRate, format.channels, frames)
	bytesPerSample := format.bitsPerSample / 8
	pos := 0
	for frame := 0; frame < frames; frame++ {
		for channel := 0; channel < format.channels; channel++ {
			sampleBytes := audioData[pos : pos+bytesPerSample]
			pos += bytesPerSample
			var value float64
			switch format.format {
			case formatPCM:
				switch format.bitsPerSample {
				case 8:
					value = (float64(sampleBytes[0]) - 128.0) / 128.0
				case 16:
					value = float64(int16(binary.LittleEndian.Uint16(sampleBytes))) / 32768.0
				case 24:
					raw := int32(sampleBytes[0]) | int32(sampleBytes[1])<<8 | int32(sampleBytes[2])<<16
					if raw&0x800000 != 0 {
						raw |= ^0xFFFFFF
					}
					value = float64(raw) / 8388608.0
				case 32:
					value = float64(int32(binary.LittleEndian.Uint32(sampleBytes))) / 2147483648.0
				}
			case formatIEEEFFloat:
				if format.bitsPerSample == 32 {
					value = float64(math.Float32frombits(binary.LittleEndian.Uint32(sampleBytes)))
				} else {
					value = math.Float64frombits(binary.LittleEndian.Uint64(sampleBytes))
				}
			}
			if math.IsNaN(value) || math.IsInf(value, 0) {
				return Signal{}, apperr.New(apperr.CodeDecode, "WAV contains a non-finite sample").
					WithField("data").WithActual(frame)
			}
			out.Data[frame*format.channels+channel] = value
		}
	}
	return out, nil
}

func parseWAVFormat(chunk []byte) (wavFormat, error) {
	if len(chunk) < 16 {
		return wavFormat{}, apperr.New(apperr.CodeDecode, "WAV fmt chunk is too short")
	}
	f := wavFormat{
		format:        binary.LittleEndian.Uint16(chunk[0:2]),
		channels:      int(binary.LittleEndian.Uint16(chunk[2:4])),
		sampleRate:    int(binary.LittleEndian.Uint32(chunk[4:8])),
		blockAlign:    int(binary.LittleEndian.Uint16(chunk[12:14])),
		bitsPerSample: int(binary.LittleEndian.Uint16(chunk[14:16])),
	}
	if f.format == formatExtensible {
		if len(chunk) < 40 {
			return wavFormat{}, apperr.New(apperr.CodeDecode, "extensible WAV fmt chunk is too short")
		}
		f.format = binary.LittleEndian.Uint16(chunk[24:26])
	}
	return f, nil
}

func supportedDecodeFormat(format uint16, bits int) bool {
	if format == formatIEEEFFloat {
		return bits == 32 || bits == 64
	}
	return format == formatPCM && (bits == 8 || bits == 16 || bits == 24 || bits == 32)
}

func WriteWAV(path string, s Signal) error {
	if err := s.Validate(); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return apperr.Wrap(apperr.CodeDecode, "cannot create audio directory", err)
	}
	blockAlign := s.Channels * 4
	dataSize := len(s.Data) * 4
	bytes := make([]byte, 44+dataSize)
	copy(bytes[0:4], "RIFF")
	binary.LittleEndian.PutUint32(bytes[4:8], uint32(36+dataSize))
	copy(bytes[8:12], "WAVE")
	copy(bytes[12:16], "fmt ")
	binary.LittleEndian.PutUint32(bytes[16:20], 16)
	binary.LittleEndian.PutUint16(bytes[20:22], formatIEEEFFloat)
	binary.LittleEndian.PutUint16(bytes[22:24], uint16(s.Channels))
	binary.LittleEndian.PutUint32(bytes[24:28], uint32(s.SampleRateHz))
	binary.LittleEndian.PutUint32(bytes[28:32], uint32(s.SampleRateHz*blockAlign))
	binary.LittleEndian.PutUint16(bytes[32:34], uint16(blockAlign))
	binary.LittleEndian.PutUint16(bytes[34:36], 32)
	copy(bytes[36:40], "data")
	binary.LittleEndian.PutUint32(bytes[40:44], uint32(dataSize))
	pos := 44
	for _, v := range s.Data {
		binary.LittleEndian.PutUint32(bytes[pos:pos+4], math.Float32bits(float32(v)))
		pos += 4
	}
	if err := os.WriteFile(path, bytes, 0o644); err != nil {
		return apperr.Wrap(apperr.CodeDecode, "cannot write WAV sample", err).WithPath(path)
	}
	return nil
}
