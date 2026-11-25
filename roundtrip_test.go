package macrokit

import "testing"

func TestHighLowHighFormatRoundTripPreservesFramesAndTriggers(t *testing.T) {
	set := GenerateSamples(FixedSampleSeed)
	for _, sample := range set.Projects {
		cycles := []struct {
			name       string
			highFormat int
			lowFormat  int
		}{
			{"v3-v1-v3", FormatTreeFrameV3, FormatFlatFrameV1},
			{"v2-v1-v2", FormatNestedMSV2, FormatFlatFrameV1},
			{"v3-v2-v3", FormatTreeFrameV3, FormatNestedMSV2},
		}
		for _, cycle := range cycles {
			t.Run(sample.Slug+"/"+cycle.name, func(t *testing.T) {
				high, _, err := ConvertProject(sample.Project, cycle.highFormat)
				if err != nil {
					t.Fatalf("high export: %v", err)
				}
				low, _, err := ConvertProject(high, cycle.lowFormat)
				if err != nil {
					t.Fatalf("low export: %v", err)
				}
				highAgain, _, err := ConvertProject(low, cycle.highFormat)
				if err != nil {
					t.Fatalf("high re-export: %v", err)
				}

				for _, original := range high.Branches {
					roundTripped := highAgain.BranchByID[original.ID]
					if roundTripped == nil {
						t.Fatalf("branch %s disappeared", original.ID)
					}
					if roundTripped.DelayFrames != original.DelayFrames {
						t.Fatalf("branch %s delay=%d want %d", original.ID, roundTripped.DelayFrames, original.DelayFrames)
					}
					if roundTripped.TriggerWindowFrames != original.TriggerWindowFrames {
						t.Fatalf("branch %s window=%d want %d", original.ID, roundTripped.TriggerWindowFrames, original.TriggerWindowFrames)
					}
					for i, step := range original.Steps {
						if roundTripped.Steps[i].GapFrames != step.GapFrames {
							t.Fatalf("step %s gap changed", step.ID)
						}
					}
				}

				cfg := ExecutorConfig{FrameCount: sample.Truth.FrameCount}
				before, err := RunProject(high, sample.Events, cfg)
				if err != nil {
					t.Fatal(err)
				}
				after, err := RunProject(highAgain, sample.Events, cfg)
				if err != nil {
					t.Fatal(err)
				}
				if err := CompareResults(before, after); err != nil {
					t.Fatalf("trigger/frame output changed after round trip: %v", err)
				}
			})
		}
	}
}
