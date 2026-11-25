package kernel

import (
	"context"
	"testing"

	"example.com/multiscreen-cast/protocol"
)

func testDisplay(id string, mode protocol.CropMode) protocol.Endpoint {
	return protocol.Endpoint{
		ID: id,
		Topology: protocol.Topology{
			X: 0, Y: 0, Width: 100, Height: 100,
			Resolution: protocol.Resolution{Width: 100, Height: 100},
			Scale:      protocol.NewRatio(1, 1),
		},
		Capability: protocol.Capability{
			Resolution: protocol.Resolution{Width: 100, Height: 100},
			Scale:      protocol.NewRatio(1, 1),
			CropMode:   mode,
		},
	}
}

func TestMappingIntegerCropAndReadings(t *testing.T) {
	display := testDisplay("screen-a", protocol.CropFloor)
	display.Capability.Scale = protocol.NewRatio(3, 2)
	display.Topology.Scale = protocol.NewRatio(3, 2)
	display.Topology.Width = 100
	display.Topology.Height = 100
	display.Capability.Resolution = protocol.Resolution{Width: 150, Height: 150}
	display.Topology.Resolution = display.Capability.Resolution
	region := protocol.Rect{X: 1, Y: 2, Width: 40, Height: 30}
	results, queues, err := mapWindow(context.Background(), []protocol.Endpoint{display}, region, 1, 2,
		protocol.CurrentAlgorithmVersion, DefaultMappingConfig())
	if err != nil {
		t.Fatal(err)
	}
	if len(queues) != 0 {
		t.Fatalf("unexpected queue records: %+v", queues)
	}
	result := results[0]
	want := protocol.Rect{X: 1, Y: 3, Width: 60, Height: 45}
	if result.Pixel != want {
		t.Fatalf("pixel=%+v want=%+v", result.Pixel, want)
	}
	if result.World != region {
		t.Fatalf("world=%+v want=%+v", result.World, region)
	}
	if result.ScreenWorld != region {
		t.Fatalf("screen world=%+v want=%+v", result.ScreenWorld, region)
	}
}

func TestCropDirectionChangesEdges(t *testing.T) {
	region := protocol.Rect{X: 0, Y: 0, Width: 3, Height: 3}
	floorDisplay := testDisplay("screen-floor", protocol.CropFloor)
	floorDisplay.Capability.Scale = protocol.NewRatio(1, 2)
	floorDisplay.Topology.Scale = protocol.NewRatio(1, 2)
	floorDisplay.Capability.Resolution = protocol.Resolution{Width: 50, Height: 50}
	floorDisplay.Topology.Resolution = floorDisplay.Capability.Resolution
	floorDisplay.Topology.Width = 100
	floorDisplay.Topology.Height = 100

	ceilDisplay := floorDisplay
	ceilDisplay.ID = "screen-ceil"
	ceilDisplay.Capability.CropMode = protocol.CropCeil
	results, _, err := mapWindow(context.Background(), []protocol.Endpoint{floorDisplay, ceilDisplay}, region, 1, 1,
		protocol.CurrentAlgorithmVersion, DefaultMappingConfig())
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]ScreenResult{}
	for _, result := range results {
		byID[result.DisplayID] = result
	}
	if byID["screen-floor"].Pixel != (protocol.Rect{Width: 1, Height: 1}) {
		t.Fatalf("floor pixel=%+v", byID["screen-floor"].Pixel)
	}
	if byID["screen-ceil"].Pixel != (protocol.Rect{Width: 2, Height: 2}) {
		t.Fatalf("ceil pixel=%+v", byID["screen-ceil"].Pixel)
	}
}

func TestMappingParameterErrorHasDisplayAndField(t *testing.T) {
	display := testDisplay("screen-a", protocol.CropFloor)
	display.Capability.Resolution.Width = 20000
	_, _, err := mapWindow(context.Background(), []protocol.Endpoint{display}, protocol.Rect{Width: 10, Height: 10},
		1, 1, protocol.CurrentAlgorithmVersion, DefaultMappingConfig())
	if err == nil || err.Code != protocol.CodePara {
		t.Fatalf("expected E_PARA, got %v", err)
	}
	if err.DisplayID != "screen-a" || err.Field == "" {
		t.Fatalf("error is not located: %+v", err)
	}
}

func TestWorkerQueueAndBufferBound(t *testing.T) {
	cfg := DefaultMappingConfig()
	cfg.MaxWorkers = 1
	cfg.PerWorkerPixelBytes = 128
	displays := []protocol.Endpoint{
		testDisplay("screen-a", protocol.CropFloor),
		testDisplay("screen-b", protocol.CropFloor),
		testDisplay("screen-c", protocol.CropFloor),
	}
	results, queues, err := mapWindow(context.Background(), displays, protocol.Rect{Width: 100, Height: 100},
		1, 1, protocol.CurrentAlgorithmVersion, cfg)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 3 {
		t.Fatalf("dropped screens: %d results", len(results))
	}
	if len(queues) != 2 {
		t.Fatalf("expected deterministic queue records, got %+v", queues)
	}
	for _, result := range results {
		if result.PixelBufferBytes > cfg.PerWorkerPixelBytes {
			t.Fatalf("buffer %d exceeds per-worker limit %d", result.PixelBufferBytes, cfg.PerWorkerPixelBytes)
		}
		if result.BufferChunks < 2 {
			t.Fatalf("expected chunked pixel staging, got %d chunks", result.BufferChunks)
		}
	}
}
