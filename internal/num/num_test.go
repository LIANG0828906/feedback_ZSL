package num_test

import (
	"math"
	"testing"

	"audioforge/internal/errs"
	"audioforge/internal/model"
	"audioforge/internal/num"
)

func TestTimeAndDimensionConversionsAreIntegerOnlyAndRoundTrip(t *testing.T) {
	samples, ok := num.TimeToSamples(3, 48000, 48000, model.TimeTick, model.RoundFloor)
	if !ok || samples != 3 {
		t.Fatalf("tick conversion = %d, %v", samples, ok)
	}
	samples, ok = num.TimeToSamples(7, 48000, 48000, model.TimeMillisecond, model.RoundFloor)
	if !ok || samples != 336 {
		t.Fatalf("ms conversion = %d, %v", samples, ok)
	}
	back, ok := num.SamplesToTime(336, 48000, 48000, model.TimeMillisecond, model.RoundFloor)
	if !ok || back != 7 {
		t.Fatalf("ms round trip = %d, %v", back, ok)
	}
	duration, err := num.IntensityToDuration(10, 16, model.RoundFloor)
	if err != nil || duration != 160 {
		t.Fatalf("intensity to duration = %d, %v", duration, err)
	}
	intensity, err := num.DurationToIntensity(duration, 16, model.RoundFloor)
	if err != nil || intensity != 10 {
		t.Fatalf("duration to intensity = %d, %v", intensity, err)
	}
	rounded, ok := num.ScaleDiv(5, 1, 2, model.RoundCeil)
	if !ok || rounded != 3 {
		t.Fatalf("ceil rounding = %d, %v", rounded, ok)
	}
}

func TestConversionOverflowHasOverflowCode(t *testing.T) {
	_, err := num.IntensityToDuration(math.MaxInt64, 16, model.RoundFloor)
	if !errs.IsCode(err, errs.EOver) {
		t.Fatalf("expected E_OVER, got %v", err)
	}
	if _, ok := num.TimeToSamples(math.MaxInt64, generatorRate, generatorRate, model.TimeMillisecond, model.RoundFloor); ok {
		t.Fatal("expected overflow")
	}
}

const generatorRate int64 = 48000
