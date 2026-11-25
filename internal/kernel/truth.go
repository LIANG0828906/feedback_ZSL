package kernel

import "encoding/json"

func BuildTruth(scene *Scene, report *PredictionReport) *Truth {
	truth := &Truth{
		LevelID:       scene.LevelID,
		Seed:          scene.Seed,
		PositionScale: PosScale,
		CoeffScale:    CoeffScale,
		Samples:       []TruthSample{},
		Bounces:       []TruthBounce{},
		Attributions:  []TruthAttribution{},
	}
	for _, traj := range report.Trajectories {
		for _, sample := range traj.Samples {
			truth.Samples = append(truth.Samples, TruthSample{
				RequestID:    sample.RequestID,
				LauncherID:   sample.LauncherID,
				LogicalFrame: sample.LogicalFrame,
				LocalFrame:   sample.LocalFrame,
				X:            sample.Position.X,
				Y:            sample.Position.Y,
			})
		}
		for _, bounce := range traj.Bounces {
			truth.Bounces = append(truth.Bounces, TruthBounce{
				RequestID: bounce.RequestID,
				Index:     bounce.Index,
				Frame:     bounce.Frame,
				SegmentID: bounce.SegmentID,
				X:         bounce.Position.X,
				Y:         bounce.Position.Y,
			})
		}
		truth.Attributions = append(truth.Attributions, TruthAttribution{
			RequestID:            traj.Attribution.RequestID,
			LauncherID:           traj.Attribution.LauncherID,
			FirstHitSegmentID:    traj.Attribution.FirstHitSegmentID,
			BounceCount:          traj.Attribution.BounceCount,
			HitSegmentIDs:        append([]string{}, traj.Attribution.HitSegmentIDs...),
			Escaped:              traj.Attribution.Escaped,
			TerminalLogicalFrame: traj.Samples[len(traj.Samples)-1].LogicalFrame,
			EndX:                 traj.Attribution.End.X,
			EndY:                 traj.Attribution.End.Y,
		})
	}
	return truth
}

func MarshalTruth(truth *Truth) ([]byte, error) {
	return json.MarshalIndent(truth, "", "  ")
}

func LoadTruth(data []byte) (*Truth, error) {
	var truth Truth
	if err := json.Unmarshal(data, &truth); err != nil {
		return nil, asError(Issue{
			Code:    CodeDecode,
			Field:   "$.truth",
			Message: "truth document is not decodable JSON: " + err.Error(),
		})
	}
	if truth.LevelID == "" {
		return nil, schemaError("$.level_id", 0, "truth level id is missing")
	}
	if err := ValidateTruthTimeline(&truth); err != nil {
		return nil, err
	}
	return &truth, nil
}

func WithinTruthTolerance(actual, expected Vec2, tolerance int64) bool {
	dx := actual.X - expected.X
	dy := actual.Y - expected.Y
	if dx < 0 {
		dx = -dx
	}
	if dy < 0 {
		dy = -dy
	}
	return dx <= tolerance && dy <= tolerance
}
