package codec

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
)

type PCMAudio struct {
	SampleRate int
	Channels   int
	BitDepth   int
	Samples    int64
	Data       []int32
}

func DecodeWAV(data []byte) (PCMAudio, error) {
	var out PCMAudio
	if len(data) < 12 || string(data[:4]) != "RIFF" || string(data[8:12]) != "WAVE" {
		return out, fmt.Errorf("not a RIFF/WAVE file")
	}
	pos := 12
	var format, bits, channels int
	var dataOff, dataLen int
	for pos+8 <= len(data) {
		id := string(data[pos : pos+4])
		n := int(binary.LittleEndian.Uint32(data[pos+4 : pos+8]))
		pos += 8
		if n < 0 || pos+n > len(data) {
			return out, fmt.Errorf("truncated WAV chunk %q", id)
		}
		body := data[pos : pos+n]
		switch id {
		case "fmt ":
			if len(body) < 16 {
				return out, fmt.Errorf("short fmt chunk")
			}
			format = int(binary.LittleEndian.Uint16(body[0:2]))
			channels = int(binary.LittleEndian.Uint16(body[2:4]))
			out.SampleRate = int(binary.LittleEndian.Uint32(body[4:8]))
			bits = int(binary.LittleEndian.Uint16(body[14:16]))
			if format == 0xfffe && len(body) >= 40 {
				format = int(binary.LittleEndian.Uint16(body[24:26]))
			}
		case "data":
			dataOff, dataLen = pos, n
		}
		pos += n
		if n%2 == 1 {
			pos++
		}
	}
	if format != 1 {
		return out, fmt.Errorf("unsupported WAV format %d (PCM required)", format)
	}
	if channels != 1 && channels != 2 {
		return out, fmt.Errorf("unsupported WAV channel count %d", channels)
	}
	if bits != 16 && bits != 24 && bits != 32 {
		return out, fmt.Errorf("unsupported WAV bit depth %d", bits)
	}
	if out.SampleRate <= 0 || dataLen <= 0 {
		return out, fmt.Errorf("missing fmt or data chunk")
	}
	bytesPer := bits / 8
	frame := channels * bytesPer
	if frame <= 0 || dataLen%frame != 0 {
		return out, fmt.Errorf("WAV data is not frame aligned")
	}
	out.Channels = channels
	out.BitDepth = bits
	out.Samples = int64(dataLen / frame)
	out.Data = make([]int32, int(out.Samples)*channels)
	raw := data[dataOff : dataOff+dataLen]
	for i := range out.Data {
		off := i * bytesPer
		var v int32
		switch bits {
		case 16:
			v = int32(int16(binary.LittleEndian.Uint16(raw[off:]))) << 16
		case 24:
			u := uint32(raw[off]) | uint32(raw[off+1])<<8 | uint32(raw[off+2])<<16
			if u&0x800000 != 0 {
				u |= 0xff000000
			}
			v = int32(u) << 8
		case 32:
			v = int32(binary.LittleEndian.Uint32(raw[off:]))
		}
		out.Data[i] = v
	}
	return out, nil
}

func EncodeWAVPCM16(sampleRate, channels int, samples []int16) ([]byte, error) {
	if sampleRate <= 0 || (channels != 1 && channels != 2) {
		return nil, fmt.Errorf("invalid WAV parameters")
	}
	dataLen := len(samples) * 2
	if dataLen%2 != 0 {
		dataLen++
	}
	var buf bytes.Buffer
	buf.Grow(44 + dataLen)
	buf.WriteString("RIFF")
	_ = binary.Write(&buf, binary.LittleEndian, uint32(36+dataLen))
	buf.WriteString("WAVEfmt ")
	_ = binary.Write(&buf, binary.LittleEndian, uint32(16))
	_ = binary.Write(&buf, binary.LittleEndian, uint16(1))
	_ = binary.Write(&buf, binary.LittleEndian, uint16(channels))
	_ = binary.Write(&buf, binary.LittleEndian, uint32(sampleRate))
	_ = binary.Write(&buf, binary.LittleEndian, uint32(sampleRate*channels*2))
	_ = binary.Write(&buf, binary.LittleEndian, uint16(channels*2))
	_ = binary.Write(&buf, binary.LittleEndian, uint16(16))
	buf.WriteString("data")
	_ = binary.Write(&buf, binary.LittleEndian, uint32(dataLen))
	for _, s := range samples {
		_ = binary.Write(&buf, binary.LittleEndian, s)
	}
	if dataLen > len(samples)*2 {
		buf.WriteByte(0)
	}
	return buf.Bytes(), nil
}

func outputSampleCount(srcSamples int64, srcRate, dstRate int) int64 {
	if srcSamples <= 0 {
		return 0
	}
	n := (srcSamples*int64(dstRate) + int64(srcRate)/2) / int64(srcRate)
	if n < 1 {
		n = 1
	}
	return n
}

func writeResampledAudio(dst io.Writer, in PCMAudio, dstRate, dstBits int, chunkSamples int64) (int64, error) {
	if in.Samples == 0 {
		return 0, nil
	}
	outN := outputSampleCount(in.Samples, in.SampleRate, dstRate)
	if chunkSamples <= 0 {
		chunkSamples = 65536
	}
	maxVal := int64(1) << (dstBits - 1)
	mask := uint32((int64(1) << dstBits) - 1)
	span := int64(1) << 32
	var buf [4]byte
	for start := int64(0); start < outN; start += chunkSamples {
		end := start + chunkSamples
		if end > outN {
			end = outN
		}
		for i := start; i < end; i++ {
			var pos uint64
			if outN == 1 {
				pos = 0
			} else {
				pos = uint64(i) * uint64(in.Samples-1) * uint64(span) / uint64(outN-1)
			}
			idx := int64(pos >> 32)
			frac := int64(pos & uint64(span-1))
			for ch := 0; ch < in.Channels; ch++ {
				s0 := in.Data[idx*int64(in.Channels)+int64(ch)]
				s1 := s0
				if idx+1 < in.Samples {
					s1 = in.Data[(idx+1)*int64(in.Channels)+int64(ch)]
				}
				v := int64(s0) + ((int64(s1)-int64(s0))*frac)/span
				q := v * maxVal / (int64(1) << 31)
				if q > maxVal-1 {
					q = maxVal - 1
				}
				if q < -maxVal {
					q = -maxVal
				}
				u := uint32(q) & mask
				binary.LittleEndian.PutUint32(buf[:], u)
				if _, err := dst.Write(buf[:]); err != nil {
					return 0, err
				}
			}
		}
	}
	return outN, nil
}
