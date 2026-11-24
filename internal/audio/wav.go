package audio

import (
	"encoding/binary"
	"fmt"
	"io"
	"math"
)

func WriteFloat32WAV(w io.Writer, sampleRate int, channels [][]float64) error {
	if sampleRate <= 0 {
		return fmt.Errorf("invalid sample rate %d", sampleRate)
	}
	if len(channels) == 0 || len(channels) > math.MaxUint16 {
		return fmt.Errorf("invalid channel count %d", len(channels))
	}
	length := len(channels[0])
	for _, channel := range channels {
		if len(channel) != length {
			return fmt.Errorf("ragged channel lengths")
		}
	}
	dataBytes := uint32(length * len(channels) * 4)
	byteRate := uint32(sampleRate * len(channels) * 4)
	blockAlign := uint16(len(channels) * 4)
	header := make([]byte, 44)
	copy(header[0:4], "RIFF")
	binary.LittleEndian.PutUint32(header[4:8], 36+dataBytes)
	copy(header[8:12], "WAVE")
	copy(header[12:16], "fmt ")
	binary.LittleEndian.PutUint32(header[16:20], 16)
	binary.LittleEndian.PutUint16(header[20:22], 3) // IEEE float
	binary.LittleEndian.PutUint16(header[22:24], uint16(len(channels)))
	binary.LittleEndian.PutUint32(header[24:28], uint32(sampleRate))
	binary.LittleEndian.PutUint32(header[28:32], byteRate)
	binary.LittleEndian.PutUint16(header[32:34], blockAlign)
	binary.LittleEndian.PutUint16(header[34:36], 32)
	copy(header[36:40], "data")
	binary.LittleEndian.PutUint32(header[40:44], dataBytes)
	if _, err := w.Write(header); err != nil {
		return err
	}
	buf := make([]byte, 4)
	for n := 0; n < length; n++ {
		for channel := range channels {
			v := channels[channel][n]
			if math.IsNaN(v) {
				v = 0
			}
			v = math.Max(-1, math.Min(1, v))
			binary.LittleEndian.PutUint32(buf, math.Float32bits(float32(v)))
			if _, err := w.Write(buf); err != nil {
				return err
			}
		}
	}
	return nil
}
