package sample

import (
	"fmt"
	"sort"

	"lightshow"
)

// VerifySchedule compares sampled truth, consumption order and arbitration
// records for one source version.
func VerifySchedule(version lightshow.Version, schedule *lightshow.ScheduleResult, truth Truth) error {
	frames := make(map[int]lightshow.FrameOutput, len(schedule.Frames))
	for _, frame := range schedule.Frames {
		frames[frame.Frame] = frame
	}
	colorTolerance := lightshow.ColorTolerance(version)
	for _, sample := range truth.Samples {
		frame, ok := frames[sample.Frame]
		if !ok {
			return fmt.Errorf("%s truth: missing frame %d", version, sample.Frame)
		}
		if diff64(frame.AtUS, sample.AtUS) > truth.Tolerance.TimeUS {
			return fmt.Errorf("%s truth: frame %d time %d outside tolerance %d", version, sample.Frame, frame.AtUS, truth.Tolerance.TimeUS)
		}
		if len(frame.Lights) != len(sample.Lights) {
			return fmt.Errorf("%s truth: frame %d has %d lights, want %d", version, sample.Frame, len(frame.Lights), len(sample.Lights))
		}
		for i, want := range sample.Lights {
			got := frame.Lights[i]
			if got.FixtureID != want.FixtureID {
				return fmt.Errorf("%s truth: frame %d fixture order %q != %q", version, sample.Frame, got.FixtureID, want.FixtureID)
			}
			if absInt(int(got.Color.R)-int(want.Color.R)) > colorTolerance ||
				absInt(int(got.Color.G)-int(want.Color.G)) > colorTolerance ||
				absInt(int(got.Color.B)-int(want.Color.B)) > colorTolerance {
				return fmt.Errorf("%s truth: frame %d fixture %s color %s != %s (tolerance %d)",
					version, sample.Frame, got.FixtureID, got.Color, want.Color, colorTolerance)
			}
			if got.Intensity != want.Intensity || got.Position != want.Position || got.Focus != want.Focus {
				return fmt.Errorf("%s truth: frame %d fixture %s non-color output mismatch", version, sample.Frame, got.FixtureID)
			}
		}
	}
	if version != lightshow.Version1 {
		if len(schedule.Consumption) != len(truth.ChangeConsumptionOrder) {
			return fmt.Errorf("%s truth: consumed %d changes, want %d", version, len(schedule.Consumption), len(truth.ChangeConsumptionOrder))
		}
		for i, want := range truth.ChangeConsumptionOrder {
			if schedule.Consumption[i].ChangeID != want {
				return fmt.Errorf("%s truth: consumption[%d] %q != %q", version, i, schedule.Consumption[i].ChangeID, want)
			}
		}
	} else {
		return nil
	}
	decisions := make(map[int][]lightshow.Decision)
	for _, decision := range schedule.Decisions {
		decisions[decision.Frame] = append(decisions[decision.Frame], decision)
	}
	var gotResolutions []GroupResolution
	if version != lightshow.Version1 {
		for _, sample := range truth.Samples {
			for _, d := range decisions[sample.Frame] {
				resolution := GroupResolution{
					Frame: d.Frame, AtUS: d.AtUS, FixtureID: d.FixtureID, Parameter: d.Parameter,
					Winner: d.Winner.Source, Basis: d.Basis,
				}
				for _, overridden := range d.Overridden {
					resolution.Overridden = append(resolution.Overridden, overridden.Source)
				}
				gotResolutions = append(gotResolutions, resolution)
			}
		}
	}
	sort.SliceStable(gotResolutions, func(i, j int) bool {
		a, b := gotResolutions[i], gotResolutions[j]
		if a.Frame != b.Frame {
			return a.Frame < b.Frame
		}
		if a.FixtureID != b.FixtureID {
			return a.FixtureID < b.FixtureID
		}
		return a.Parameter < b.Parameter
	})
	if len(gotResolutions) != len(truth.GroupResolutions) {
		return fmt.Errorf("%s truth: arbitration count %d != %d", version, len(gotResolutions), len(truth.GroupResolutions))
	}
	for i := range gotResolutions {
		if !resolutionsEqual(gotResolutions[i], truth.GroupResolutions[i]) {
			return fmt.Errorf("%s truth: arbitration[%d] mismatch at frame %d fixture %s parameter %s",
				version, i, gotResolutions[i].Frame, gotResolutions[i].FixtureID, gotResolutions[i].Parameter)
		}
	}
	return nil
}

func resolutionsEqual(a, b GroupResolution) bool {
	if a.Frame != b.Frame || a.AtUS != b.AtUS || a.FixtureID != b.FixtureID ||
		a.Parameter != b.Parameter || a.Winner != b.Winner || a.Basis != b.Basis ||
		len(a.Overridden) != len(b.Overridden) {
		return false
	}
	for i := range a.Overridden {
		if a.Overridden[i] != b.Overridden[i] {
			return false
		}
	}
	return true
}

func absInt(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

func diff64(a, b int64) int64 {
	if a > b {
		return a - b
	}
	return b - a
}
