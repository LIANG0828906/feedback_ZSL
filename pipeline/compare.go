package pipeline

import (
	"context"
	"fmt"
	"math"
)

type Mismatch struct {
	Found    bool    `json:"found"`
	Window   int     `json:"window"`
	Frame    int     `json:"frame"`
	X        int     `json:"x"`
	Y        int     `json:"y"`
	Channel  int     `json:"channel"`
	Expected float64 `json:"expected"`
	Actual   float64 `json:"actual"`
	Delta    float64 `json:"delta"`
	Message  string  `json:"message,omitempty"`
}

func (m Mismatch) Error() string {
	if !m.Found {
		return "frames match"
	}
	return fmt.Sprintf("first mismatch at window %d frame %d pixel (%d,%d) channel %d: %.9g != %.9g",
		m.Window, m.Frame, m.X, m.Y, m.Channel, m.Expected, m.Actual)
}

type CompareReport struct {
	Match     bool             `json:"match"`
	Windows   int              `json:"windows"`
	Frames    int              `json:"frames"`
	Pixels    int              `json:"pixels"`
	First     Mismatch         `json:"first_mismatch"`
	PerWindow []WindowMismatch `json:"per_window"`
}

type WindowMismatch struct {
	Window int      `json:"window"`
	Match  bool     `json:"match"`
	First  Mismatch `json:"first_mismatch"`
}

func FirstFrameMismatch(window int, expected, actual *Frame) Mismatch {
	if expected == nil || actual == nil {
		return Mismatch{Found: true, Window: window, Message: "nil frame"}
	}
	if expected.Frame != actual.Frame || expected.Width != actual.Width || expected.Height != actual.Height || expected.Channels != actual.Channels {
		return Mismatch{
			Found: true, Window: window, Frame: expected.Frame,
			Message: fmt.Sprintf("frame shape mismatch: %d/%d/%d vs %d/%d/%d", expected.Frame, expected.Width, expected.Height, actual.Frame, actual.Width, actual.Height),
		}
	}
	if expected.Timestamp != actual.Timestamp || expected.TimeUnit != actual.TimeUnit {
		return Mismatch{
			Found: true, Window: window, Frame: expected.Frame,
			Message: fmt.Sprintf("time base mismatch: %v %s vs %v %s", expected.Timestamp, expected.TimeUnit, actual.Timestamp, actual.TimeUnit),
		}
	}
	for p := 0; p < expected.Width*expected.Height; p++ {
		for c := 0; c < expected.Channels; c++ {
			idx := p*expected.Channels + c
			if expected.Pixels[idx] != actual.Pixels[idx] {
				return Mismatch{
					Found: true, Window: window, Frame: expected.Frame,
					X: p % expected.Width, Y: p / expected.Width, Channel: c,
					Expected: float64(expected.Pixels[idx]), Actual: float64(actual.Pixels[idx]),
					Delta: math.Abs(float64(expected.Pixels[idx]) - float64(actual.Pixels[idx])),
				}
			}
		}
	}
	return Mismatch{Window: window, Frame: expected.Frame}
}

func CompareWindowResults(expected, actual *WindowResult) WindowMismatch {
	out := WindowMismatch{Window: expected.Window, Match: true}
	if expected.Window != actual.Window || expected.StartFrame != actual.StartFrame || expected.EndFrame != actual.EndFrame || len(expected.Frames) != len(actual.Frames) {
		out.Match = false
		out.First = Mismatch{Found: true, Window: expected.Window, Message: "window shape mismatch"}
		return out
	}
	for i := range expected.Frames {
		mismatch := FirstFrameMismatch(expected.Window, expected.Frames[i], actual.Frames[i])
		if mismatch.Found {
			out.Match = false
			out.First = mismatch
			return out
		}
	}
	return out
}

// CompareCachedWindows reads committed window packets from two frame stores and
// reports the first window/frame/pixel difference.
func CompareCachedWindows(ctx context.Context, expectedDir, actualDir string, windowCount int) (CompareReport, error) {
	expectedStore := FrameStore{Dir: expectedDir}
	actualStore := FrameStore{Dir: actualDir}
	report := CompareReport{Match: true, Windows: windowCount}
	for i := 0; i < windowCount; i++ {
		if err := ctx.Err(); err != nil {
			return report, err
		}
		expected, err := expectedStore.ReadWindow(i)
		if err != nil {
			return report, err
		}
		actual, err := actualStore.ReadWindow(i)
		if err != nil {
			return report, err
		}
		result := CompareWindowResults(expected, actual)
		report.PerWindow = append(report.PerWindow, result)
		report.Frames += len(expected.Frames)
		for _, frame := range expected.Frames {
			report.Pixels += frame.Width * frame.Height
		}
		if !result.Match {
			report.Match = false
			report.First = result.First
			return report, nil
		}
	}
	return report, nil
}

// DiffHeatmap returns a row-major grid of normalized maximum-channel deltas.
// It is used by the desktop view and is deterministic for the same frames.
func DiffHeatmap(expected, actual *Frame, cellsX, cellsY int) ([][]float64, error) {
	if cellsX <= 0 || cellsY <= 0 {
		return nil, paraError(-1, "heatmap dimensions must be positive")
	}
	if expected == nil || actual == nil || expected.Width != actual.Width || expected.Height != actual.Height || expected.Channels != actual.Channels {
		return nil, paraError(-1, "heatmap frames are not compatible")
	}
	grid := make([][]float64, cellsY)
	for y := range grid {
		grid[y] = make([]float64, cellsX)
	}
	for y := 0; y < expected.Height; y++ {
		cy := y * cellsY / expected.Height
		for x := 0; x < expected.Width; x++ {
			cx := x * cellsX / expected.Width
			idx := (y*expected.Width + x) * expected.Channels
			delta := 0.0
			for c := 0; c < expected.Channels; c++ {
				d := math.Abs(float64(expected.Pixels[idx+c] - actual.Pixels[idx+c]))
				if d > delta {
					delta = d
				}
			}
			if delta > grid[cy][cx] {
				grid[cy][cx] = delta
			}
		}
	}
	return grid, nil
}
