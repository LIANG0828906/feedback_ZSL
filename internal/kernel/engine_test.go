package kernel

import "testing"

func TestTruthSamplesBouncesAndAttribution(t *testing.T) {
	sample, err := GenerateSampleSet(DefaultSeed)
	if err != nil {
		t.Fatal(err)
	}
	truthByRequest := map[string]map[int]TruthSample{}
	for _, truthSample := range sample.Truth.Samples {
		if truthByRequest[truthSample.RequestID] == nil {
			truthByRequest[truthSample.RequestID] = map[int]TruthSample{}
		}
		truthByRequest[truthSample.RequestID][truthSample.LocalFrame] = truthSample
	}

	for _, traj := range sample.Report.Trajectories {
		for _, actual := range traj.Samples {
			expected := truthByRequest[traj.RequestID][actual.LocalFrame]
			if !WithinTruthTolerance(actual.Position, Vec2{X: expected.X, Y: expected.Y}, PosScale) {
				t.Fatalf("sample outside tolerance req=%s frame=%d actual=%+v truth=%+v",
					traj.RequestID, actual.LocalFrame, actual.Position, expected)
			}
			if actual.Quant.MoveDenominator != 1000 {
				t.Fatalf("missing quantization record at %s/%d", traj.RequestID, actual.LocalFrame)
			}
		}
	}

	if got := sample.Report.Trajectories[0].Attribution; got.BounceCount < 2 {
		t.Fatalf("vertical bounce count=%d want >=2 (%+v)", got.BounceCount, got)
	}
	escape := sample.Report.Trajectories[3].Attribution
	if escape.FirstHitSegmentID != "" || !escape.Escaped || escape.Outcome != "escaped_no_hit" {
		t.Fatalf("no-hit attribution not explicit escape: %+v", escape)
	}

	bounceTruth := map[string]TruthBounce{}
	for _, bounce := range sample.Truth.Bounces {
		key := bounce.RequestID + "#" + itoa(bounce.Index)
		bounceTruth[key] = bounce
	}
	for _, traj := range sample.Report.Trajectories {
		for _, bounce := range traj.Bounces {
			expected := bounceTruth[traj.RequestID+"#"+itoa(bounce.Index)]
			if expected.SegmentID != bounce.SegmentID {
				t.Fatalf("bounce segment mismatch: %+v vs %+v", bounce, expected)
			}
			if !WithinTruthTolerance(bounce.Position, Vec2{X: expected.X, Y: expected.Y}, PosScale) {
				t.Fatalf("bounce outside tolerance: %+v vs %+v", bounce.Position, expected)
			}
		}
	}
}
