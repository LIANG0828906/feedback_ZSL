package grid

import (
	"testing"
)

const truthToleranceUnits int64 = 50_000 // 50 mm, declared prediction tolerance

func TestFramePhysicsMatchesGeneratedTruth(t *testing.T) {
	level := Generate(DefaultSeed)
	if len(level.Truths) != len(level.Requests) {
		t.Fatalf("truth count %d", len(level.Truths))
	}
	for _, truth := range level.Truths {
		req := level.RequestByID(truth.RequestID)
		launch := level.LaunchByID(req.LaunchID)
		result, err := Predict(level, truth.RequestID, *launch)
		if truth.ExpectedErrorCode != "" {
			ge, ok := AsError(err, truth.ExpectedErrorCode)
			if !ok {
				t.Fatalf("%s expected %s, got %v", truth.RequestID, truth.ExpectedErrorCode, err)
			}
			if ge.BounceSeq == 0 || ge.ObjectID == "" {
				t.Fatalf("%s bounce error missing seq/object: %#v", truth.RequestID, ge)
			}
			if truth.ExpectedBounceSeq != 0 && truth.ExpectedBounceSeq != ge.BounceSeq {
				t.Fatalf("%s truth bounce seq %d != error %d", truth.RequestID, truth.ExpectedBounceSeq, ge.BounceSeq)
			}
		} else if err != nil {
			t.Fatalf("%s predict: %v", truth.RequestID, err)
		}
		if result == nil {
			t.Fatalf("%s nil result", truth.RequestID)
		}
		if result.FirstObstacleID != truth.FirstObstacleID || result.Escaped != truth.Escaped {
			t.Fatalf("%s attribution first=%s/%s escaped=%v/%v", truth.RequestID,
				result.FirstObstacleID, truth.FirstObstacleID, result.Escaped, truth.Escaped)
		}
		if len(result.Bounces) != len(truth.Checkpoints) {
			t.Fatalf("%s bounce checkpoints %d != %d", truth.RequestID, len(result.Bounces), len(truth.Checkpoints))
		}
		for i, cp := range truth.Checkpoints {
			got := result.Bounces[i]
			if got.ObstacleID != cp.ObstacleID || got.Seq != cp.BounceSeq {
				t.Fatalf("%s checkpoint %d identity = %s#%d want %s#%d", truth.RequestID, i,
					got.ObstacleID, got.Seq, cp.ObstacleID, cp.BounceSeq)
			}
			if absDiff(got.Point.X, cp.X) > truthToleranceUnits || absDiff(got.Point.Y, cp.Y) > truthToleranceUnits {
				t.Fatalf("%s checkpoint %d point=(%d,%d) truth=(%d,%d)", truth.RequestID, i,
					got.Point.X, got.Point.Y, cp.X, cp.Y)
			}
		}
	}
}

func TestBounceLimitSequenceAndExplicitEscape(t *testing.T) {
	level := Generate(DefaultSeed)
	launch := *level.LaunchByID("LN05")
	result, err := Predict(level, "REQ-05", launch)
	if err != nil {
		t.Fatalf("seed LN05 should stay within its declared limit: %v", err)
	}
	if result.BounceCount != launch.BounceLimit {
		t.Fatalf("LN05 bounces=%d want %d", result.BounceCount, launch.BounceLimit)
	}

	launch.BounceLimit = 1
	_, err = Predict(level, "REQ-05-LIMIT", launch)
	ge, ok := AsError(err, CodeBounce)
	if !ok || ge.BounceSeq != 2 {
		t.Fatalf("limited LN05 second bounce error = %#v", err)
	}
	if ge.RequestID != "REQ-05-LIMIT" || ge.ObjectID == "" {
		t.Fatalf("E_BOUNCE context = %#v", ge)
	}

	ln04 := level.LaunchByID("LN04")
	escapeResult, err := Predict(level, "REQ-04", *ln04)
	if err != nil {
		t.Fatal(err)
	}
	if escapeResult.FirstObstacleID != "" || !escapeResult.Escaped || escapeResult.TerminalReason != "escaped-scene-bounds" {
		t.Fatalf("no-hit attribution = %#v", escapeResult)
	}
}

func TestQuantizationTracesAreRecordedPerFrame(t *testing.T) {
	level := Generate(DefaultSeed)
	launch := *level.LaunchByID("LN01")
	result, err := Predict(level, "REQ-01", launch, PredictOptions{RecordQuantization: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Frames) != launch.FrameLimit {
		t.Fatalf("frame traces = %d", len(result.Frames))
	}
	traceCount := 0
	hitTrace := false
	for _, frame := range result.Frames {
		traceCount += len(frame.Traces)
		for _, tr := range frame.Traces {
			if tr.Kind == "truncation" && tr.Position != "" {
				if contains(tr.Field, ".hit.") {
					hitTrace = true
				}
			}
		}
	}
	if traceCount < launch.FrameLimit*2 {
		t.Fatalf("quantization traces too few: %d", traceCount)
	}
	if !hitTrace {
		t.Fatal("expected at least one hit-coordinate truncation trace")
	}
}

func TestNormalMismatchInjectionLocatesTile(t *testing.T) {
	level := Generate(DefaultSeed)
	cam := DefaultCamera()
	mutated, ge := InjectNormFault(level, cam)
	if ge == nil || ge.Code != CodeNorm || ge.ObjectID != "OB01" || ge.Tile == nil {
		t.Fatalf("norm fault = %#v", ge)
	}
	if err := ValidateNormals(mutated, cam); err == nil {
		t.Fatal("mutated normal unexpectedly valid")
	}
}

func absDiff(a, b int64) int64 {
	if a > b {
		return a - b
	}
	return b - a
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()
}
