package mirror

import (
	"reflect"
	"testing"
)

func testScreen() Screen {
	return Screen{
		ID: "screen-test", Name: "Test", Domain: "d", OriginX: 64, OriginY: 32,
		Width: 1921, Height: 1081,
		ScaleCapabilities: []Scale{Scale100, Scale125}, DefaultScale: Scale100,
		ChromaCapabilities: []Chroma{Chroma444, Chroma422, Chroma420}, DefaultChroma: Chroma420,
		MaxPacketBytes: 16384,
	}
}

func testWindow(scale Scale, chroma Chroma) Window {
	return Window{
		ID: "w-test", Index: 0, StartTick: 100, EndTick: 130,
		Region: Region{X: 64, Y: 32, W: 1281, H: 721},
		Scale:  scale, Chroma: chroma, MaxPacketBytes: 16384,
	}
}

func TestMapScreenIntegerAlignedAndDeterministic(t *testing.T) {
	screen := testScreen()
	window := testWindow(Scale125, Chroma420)
	first, err := MapScreen(screen, window)
	if err != nil {
		t.Fatal(err)
	}
	if first.Output.Empty() {
		t.Fatal("mapped output is empty")
	}
	if first.Output.X < 0 || first.Output.Y < 0 ||
		first.Output.X2() > screen.Width || first.Output.Y2() > screen.Height {
		t.Fatalf("output %+v escapes screen", first.Output)
	}
	if first.Output.W%2 != 0 || first.Output.H%2 != 0 || first.Output.X%2 != 0 || first.Output.Y%2 != 0 {
		t.Fatalf("chroma 420 output is not aligned: %+v", first.Output)
	}
	if first.Errors.ScaleDenominator != 100 {
		t.Fatalf("scale denominator = %d", first.Errors.ScaleDenominator)
	}
	for i := 0; i < 50; i++ {
		next, err := MapScreen(screen, window)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(first, next) {
			t.Fatalf("mapping changed on iteration %d", i)
		}
	}
}

func TestMapScreenCapacityErrors(t *testing.T) {
	screen := testScreen()
	window := testWindow(Scale150, Chroma420)
	_, err := MapScreen(screen, window)
	if !IsCode(err, CodeCapacity) {
		t.Fatalf("unsupported scale error = %v", err)
	}
	e, _ := AsError(err)
	if e.ScreenID != screen.ID || e.Requested != "150%" || len(e.Supported) == 0 {
		t.Fatalf("capacity context = %+v", e)
	}

	window = testWindow(Scale125, Chroma411)
	_, err = MapScreen(screen, window)
	if !IsCode(err, CodeCapacity) {
		t.Fatalf("unsupported chroma error = %v", err)
	}

	window = testWindow(Scale100, Chroma444)
	window.Region = Region{X: 5000, Y: 5000, W: 10, H: 10}
	_, err = MapScreen(screen, window)
	if !IsCode(err, CodeCapacity) {
		t.Fatalf("empty crop error = %v", err)
	}
}

func TestDistributePacketsRespectsLimitsAndTiming(t *testing.T) {
	screen := testScreen()
	window := testWindow(Scale125, Chroma420)
	mapping, err := MapScreen(screen, window)
	if err != nil {
		t.Fatal(err)
	}
	packets, err := DistributeScreen(screen, window, mapping)
	if err != nil {
		t.Fatal(err)
	}
	if len(packets) == 0 {
		t.Fatal("expected packets")
	}
	limit := min64(screen.MaxPacketBytes, window.MaxPacketBytes)
	lastSeq := 0
	for _, p := range packets {
		if p.Seq != lastSeq+1 {
			t.Fatalf("packet sequence %d after %d", p.Seq, lastSeq)
		}
		lastSeq = p.Seq
		if p.Bytes > limit {
			t.Fatalf("packet %d bytes %d > %d", p.Seq, p.Bytes, limit)
		}
		if p.StartTick < window.StartTick || p.EndTick > window.EndTick || p.StartTick > p.EndTick {
			t.Fatalf("packet %d timing %d-%d outside %d-%d", p.Seq, p.StartTick, p.EndTick, window.StartTick, window.EndTick)
		}
		if p.Region.Empty() || p.Region.X < mapping.Output.X || p.Region.Y < mapping.Output.Y ||
			p.Region.X2() > mapping.Output.X2() || p.Region.Y2() > mapping.Output.Y2() {
			t.Fatalf("packet %d region %+v outside output %+v", p.Seq, p.Region, mapping.Output)
		}
	}
}

func TestDistributePacketLimitError(t *testing.T) {
	screen := testScreen()
	screen.MaxPacketBytes = 1
	window := testWindow(Scale100, Chroma444)
	window.MaxPacketBytes = 1
	mapping, err := MapScreen(screen, window)
	if err != nil {
		t.Fatal(err)
	}
	_, err = DistributeScreen(screen, window, mapping)
	if !IsCode(err, CodePacket) {
		t.Fatalf("error = %v, want E_PACK", err)
	}
	e, _ := AsError(err)
	if e.Limit != 1 || e.Measured <= e.Limit {
		t.Fatalf("packet error context = %+v", e)
	}
}
