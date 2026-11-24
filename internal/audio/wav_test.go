package audio

import (
	"bytes"
	"encoding/binary"
	"testing"
)

func TestWriteFloat32WAVHeaderAndLength(t *testing.T) {
	channels := [][]float64{{0.1, -0.2, 0.3}, {0.4, -0.5, 0.6}}
	var buffer bytes.Buffer
	if err := WriteFloat32WAV(&buffer, 48000, channels); err != nil {
		t.Fatal(err)
	}
	data := buffer.Bytes()
	if len(data) != 44+3*2*4 {
		t.Fatalf("WAV length %d", len(data))
	}
	if string(data[0:4]) != "RIFF" || string(data[8:12]) != "WAVE" || string(data[36:40]) != "data" {
		t.Fatalf("bad WAV header")
	}
	if got := binary.LittleEndian.Uint32(data[24:28]); got != 48000 {
		t.Fatalf("sample rate %d", got)
	}
	if got := binary.LittleEndian.Uint16(data[22:24]); got != 2 {
		t.Fatalf("channels %d", got)
	}
	if got := binary.LittleEndian.Uint32(data[40:44]); got != 24 {
		t.Fatalf("data size %d", got)
	}
}
