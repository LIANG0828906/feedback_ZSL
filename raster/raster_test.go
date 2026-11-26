package raster_test

import (
	"bytes"
	"sync"
	"testing"

	"maskqc/model"
	"maskqc/raster"
)

func TestCoverageTileAndConcurrencyInvariance(t *testing.T) {
	stroke := model.Stroke{
		ID: "s01", Seq: 1, X0: 18, Y0: 17, X1: 49, Y1: 41,
		Radius: 7, Hardness: 63, Weight: 7310, Priority: 1,
	}
	width, height := 64, 64
	full, err := raster.Coverage(stroke, width, height)
	if err != nil {
		t.Fatal(err)
	}
	again, err := raster.Coverage(stroke, width, height)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(full, again) {
		t.Fatal("same stroke produced different full coverage")
	}
	tiled := make([]byte, width*height)
	for y := 0; y < height; y += 16 {
		for x := 0; x < width; x += 16 {
			rect := model.Rect{MinX: x, MinY: y, MaxX: min(width-1, x+15), MaxY: min(height-1, y+15)}
			tile, err := raster.CoverageRect(stroke, rect, width, height)
			if err != nil {
				t.Fatal(err)
			}
			for row := 0; row < rect.Height(); row++ {
				copy(tiled[(rect.MinY+row)*width+rect.MinX:(rect.MinY+row)*width+rect.MaxX+1],
					tile[row*rect.Width():(row+1)*rect.Width()])
			}
		}
	}
	if !bytes.Equal(full, tiled) {
		t.Fatal("tiled coverage differs from full coverage")
	}
	var wait sync.WaitGroup
	hashes := make(chan string, 24)
	for index := 0; index < 24; index++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			coverage, err := raster.Coverage(stroke, width, height)
			if err != nil {
				hashes <- err.Error()
				return
			}
			hashes <- model.HashBytes(coverage)
		}()
	}
	wait.Wait()
	close(hashes)
	expected := model.HashBytes(full)
	for hash := range hashes {
		if hash != expected {
			t.Fatalf("concurrent coverage hash differs: %s", hash)
		}
	}
}

func TestCompositionUsesUniqueDeterministicOrder(t *testing.T) {
	width, height := 48, 48
	candidate := make([]byte, width*height)
	for index := range candidate {
		candidate[index] = byte(index % 17)
	}
	strokes := []model.Stroke{
		{ID: "s1", Seq: 1, X0: 8, Y0: 16, X1: 38, Y1: 30, Radius: 6, Hardness: 72, Weight: 3100, Priority: 20, CompositeOrder: 1},
		{ID: "s2", Seq: 2, X0: 12, Y0: 30, X1: 34, Y1: 12, Radius: 5, Hardness: 88, Weight: 8100, Priority: 20, CompositeOrder: 1},
		{ID: "s3", Seq: 3, X0: 14, Y0: 20, X1: 32, Y1: 20, Radius: 4, Hardness: 55, Weight: 5600, Priority: 20, CompositeOrder: 1},
	}
	first, err := raster.Compose(candidate, width, height, strokes)
	if err != nil {
		t.Fatal(err)
	}
	shuffled := []model.Stroke{strokes[2], strokes[0], strokes[1]}
	second, err := raster.Compose(candidate, width, height, shuffled)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first.Accumulated, second.Accumulated) {
		t.Fatal("input slice declaration order changed deterministic composite output")
	}
	if first.OrderVersion != second.OrderVersion {
		t.Fatal("order version changed after equivalent shuffle")
	}
	changedSeq := append([]model.Stroke(nil), strokes...)
	changedSeq[0].Seq = 4
	changedSeq[1].Seq = 1
	changedSeq[2].Seq = 3
	third, err := raster.Compose(candidate, width, height, changedSeq)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(first.Accumulated, third.Accumulated) {
		t.Fatal("changing stroke sequence did not affect an overlapping composite")
	}
}

func TestParameterAndAlphaRules(t *testing.T) {
	base := model.Stroke{ID: "bad", Seq: 1, Radius: 4, Hardness: 70, Weight: 8000, X0: 1, Y0: 1, X1: 4, Y1: 4}
	tests := []struct {
		name   string
		change func(*model.Stroke)
		field  string
	}{
		{name: "hardness", change: func(s *model.Stroke) { s.Hardness = 101 }, field: "hardness"},
		{name: "weight", change: func(s *model.Stroke) { s.Weight = 10001 }, field: "weight"},
		{name: "radius", change: func(s *model.Stroke) { s.Radius = 0 }, field: "radius"},
	}
	for _, test := range tests {
		stroke := base
		test.change(&stroke)
		err := raster.ValidateStroke(stroke)
		if err == nil || !model.HasCode(err, model.CodeParam) {
			t.Fatalf("%s did not return E_PARA: %v", test.name, err)
		}
		if domainErr, ok := err.(*model.Error); !ok || domainErr.Field != test.field || domainErr.Allowed == "" {
			t.Fatalf("%s error lacks field or allowed range: %#v", test.name, err)
		}
	}
	if raster.AlphaOver(0, 128) != 128 {
		t.Fatal("alpha-over over empty base is wrong")
	}
	if raster.AlphaOver(255, 0) != 255 {
		t.Fatal("transparent source changed opaque base")
	}
	if raster.AlphaOver(0, 255) != 255 {
		t.Fatal("opaque source did not saturate")
	}
}
