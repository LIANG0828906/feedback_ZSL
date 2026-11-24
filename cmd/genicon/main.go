package main

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/png"
	"log"
	"os"
	"path/filepath"
)

func main() {
	img := render(512)
	var p bytes.Buffer
	if err := png.Encode(&p, img); err != nil {
		log.Fatal(err)
	}
	if err := os.MkdirAll("build/windows", 0o755); err != nil {
		log.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join("build", "appicon.png"), p.Bytes(), 0o644); err != nil {
		log.Fatal(err)
	}
	small := render(256)
	var sp bytes.Buffer
	if err := png.Encode(&sp, small); err != nil {
		log.Fatal(err)
	}
	// ICO container with an embedded 256x256 PNG image.
	var ico bytes.Buffer
	binary.Write(&ico, binary.LittleEndian, uint16(0))
	binary.Write(&ico, binary.LittleEndian, uint16(1))
	binary.Write(&ico, binary.LittleEndian, uint16(1))
	ico.WriteByte(0)
	ico.WriteByte(0)
	ico.WriteByte(0)
	ico.WriteByte(0)
	binary.Write(&ico, binary.LittleEndian, uint16(1))
	binary.Write(&ico, binary.LittleEndian, uint16(32))
	binary.Write(&ico, binary.LittleEndian, uint32(sp.Len()))
	binary.Write(&ico, binary.LittleEndian, uint32(22))
	ico.Write(sp.Bytes())
	if err := os.WriteFile(filepath.Join("build", "windows", "icon.ico"), ico.Bytes(), 0o644); err != nil {
		log.Fatal(err)
	}
}

func render(size int) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, size, size))
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			t := float64(y) / float64(size-1)
			bg := color.NRGBA{uint8(8 + 8*t), uint8(18 + 12*t), uint8(25 + 17*t), 255}
			img.SetNRGBA(x, y, bg)
		}
	}
	for i, h := range []int{size * 150 / 512, size * 250 / 512, size * 185 / 512} {
		x0 := size*150/512 + i*size*72/512
		for y := size*370/512 - h; y < size*370/512; y++ {
			for x := x0; x < x0+size*44/512; x++ {
				img.SetNRGBA(x, y, color.NRGBA{54, 214, 195, 255})
			}
		}
	}
	return img
}
