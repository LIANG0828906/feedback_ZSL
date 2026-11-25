package kernel

import "fmt"

func PrepareScene(scene *Scene) error {
	if !scene.Prepared {
		if scene.Width == 0 {
			scene.Width = DefaultWidthUnits * PosScale
		} else if scene.Width < 10_000_000 {
			scene.Width *= PosScale
		}
		if scene.Height == 0 {
			scene.Height = DefaultHeightUnits * PosScale
		} else if scene.Height < 10_000_000 {
			scene.Height *= PosScale
		}
		if scene.MSPerTick == 0 {
			scene.MSPerTick = DefaultTickMS
		}
		if scene.StepTicks == 0 {
			scene.StepTicks = 1
		}
		if scene.StepMS == 0 {
			stepMS, ok := TicksToMS(scene.StepTicks, scene.MSPerTick)
			if !ok {
				return overflowError("$.timing.step_ms", scene.LevelID, scene.SchemaVersion, scene.StepTicks, scene.MSPerTick)
			}
			scene.StepMS = stepMS
		}
	}
	for i := range scene.Segments {
		seg := &scene.Segments[i]
		if seg.NormalSide == "" {
			seg.NormalSide = "left"
		}
		seg.Normal = NormalizeFixed(RawNormal(seg.A, seg.B, seg.NormalSide))
		if seg.MaterialID == "" {
			seg.MaterialID = "default"
			seg.MaterialVersion = 1
		}
	}
	scene.Prepared = true
	if err := ValidateScene(scene); err != nil {
		return err
	}
	return nil
}

func ValidateScene(scene *Scene) error {
	version := scene.SchemaVersion
	if scene.LevelID == "" {
		return schemaError("$.level_id", version, "level id is required")
	}
	if scene.StepMS <= 0 {
		return parameterError("$.timing.step_ms", scene.LevelID, version, "step must be positive")
	}
	if len(scene.Segments) == 0 {
		return schemaError("$.segments", version, "at least one segment is required")
	}
	seenSegments := map[string]bool{}
	for i, seg := range scene.Segments {
		path := fmt.Sprintf("$.segments[%d]", i)
		if seg.ID == "" {
			return schemaError(path+".id", version, "segment id is required")
		}
		if seenSegments[seg.ID] {
			return schemaError(path+".id", version, "duplicate segment id %q", seg.ID)
		}
		seenSegments[seg.ID] = true
		if seg.A == seg.B {
			return parameterError(path+".endpoints", seg.ID, version, "segment endpoints must differ")
		}
		if seg.Restitution < 0 || seg.Restitution > CoeffScale {
			return parameterError(path+".restitution", seg.ID, version,
				"restitution %d outside [0,%d]", seg.Restitution, CoeffScale)
		}
		if seg.PenetrationDepth < 0 {
			return parameterError(path+".penetration.depth", seg.ID, version, "penetration depth must be non-negative")
		}
	}

	seenLaunchers := map[string]bool{}
	for i, launcher := range scene.Launchers {
		path := fmt.Sprintf("$.launchers[%d]", i)
		if launcher.ID == "" {
			return schemaError(path+".id", version, "launcher id is required")
		}
		if seenLaunchers[launcher.ID] {
			return schemaError(path+".id", version, "duplicate launcher id %q", launcher.ID)
		}
		seenLaunchers[launcher.ID] = true
	}

	var previousFrame *int64
	for i, request := range scene.Requests {
		path := fmt.Sprintf("$.requests[%d].logical_frame", i)
		if request.ID == "" {
			return schemaError(fmt.Sprintf("$.requests[%d].id", i), version, "request id is required")
		}
		if !seenLaunchers[request.LauncherID] {
			return schemaError(fmt.Sprintf("$.requests[%d].launcher_id", i), version,
				"unknown launcher %q", request.LauncherID)
		}
		if previousFrame != nil && request.LogicalFrame < *previousFrame {
			return orderError(path, request.ID, version,
				"logical frame %d precedes previous frame %d", request.LogicalFrame, *previousFrame)
		}
		previousFrame = &request.LogicalFrame
	}
	return nil
}

func ValidateTruthTimeline(truth *Truth) error {
	var previous struct {
		request string
		frame   int64
		set     bool
	}
	for i, sample := range truth.Samples {
		if previous.set && sample.RequestID == previous.request && sample.LogicalFrame < previous.frame {
			return orderError(fmt.Sprintf("$.truth.samples[%d].logical_frame", i), sample.RequestID, 0,
				"truth endpoint/time inversion: frame %d after %d", sample.LogicalFrame, previous.frame)
		}
		previous.request = sample.RequestID
		previous.frame = sample.LogicalFrame
		previous.set = true
	}
	return nil
}
