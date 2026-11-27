package grid

import (
	"fmt"
	"math/rand"
)

func mm(v int64) int64 { return v * PositionScale }

func mmVec(x, y int64) Vec2 { return Vec2{X: mm(x), Y: mm(y)} }

// Generate returns the fixed-seed deterministic level. The scene table is an
// explicit seed catalog so samples, hashes and diagnostics remain stable
// without consulting an external level service.
func Generate(seed int64) *Level {
	if seed == 0 {
		seed = DefaultSeed
	}
	raw := []struct {
		id     string
		seq    int
		a      Vec2
		restit int64 // basis points
		mat    int
		pen    bool
	}{
		{"OB01", 1, mmVec(1000, 12000), 8500, 2, false},
		{"OB02", 2, mmVec(1000, 4000), 7500, 4, false},
		{"OB03", 3, mmVec(5000, 3000), 6000, 2, false},
		{"OB04", 4, mmVec(17000, 3000), 7500, 4, false},
		{"OB05", 5, mmVec(7000, 9500), 6000, 2, false},
		{"OB06", 6, mmVec(13000, 8000), 7500, 4, false},
		{"OB07", 7, mmVec(3000, 6000), 8500, 2, false},
	}
	ends := []Vec2{
		mmVec(20000, 12000),
		mmVec(1000, 12000),
		mmVec(13000, 3000),
		mmVec(17000, 9000),
		mmVec(11000, 5500),
		mmVec(16000, 8000),
		mmVec(5000, 6000),
	}
	obstacles := make([]Obstacle, 0, len(raw))
	for i, r := range raw {
		b := ends[i]
		n := RightNormal(r.a, b)
		obstacles = append(obstacles, Obstacle{
			ID: r.id, Seq: r.seq, A: r.a, B: b, Normal: n,
			Restitution: r.restit, MaterialVer: r.mat, Penetrable: r.pen,
			EndpointVer: 1, NormalVer: 1, RestitutionVer: 1,
		})
	}

	launches := []Launch{
		{ID: "LN01", Origin: mmVec(2000, 10000), Velocity: mmVec(3500, -2500), BounceLimit: 16, FrameLimit: 90},
		{ID: "LN02", Origin: mmVec(8000, 11000), Velocity: mmVec(1198, -7000), BounceLimit: 16, FrameLimit: 90},
		{ID: "LN03", Origin: mmVec(12000, 10000), Velocity: mmVec(-2800, -1800), BounceLimit: 16, FrameLimit: 90},
		{ID: "LN04", Origin: mmVec(21000, 5000), Velocity: mmVec(4000, -6000), BounceLimit: 16, FrameLimit: 90},
		{ID: "LN05", Origin: mmVec(9000, 6000), Velocity: mmVec(500, 198), BounceLimit: 2, FrameLimit: 90},
	}

	rng := rand.New(rand.NewSource(seed))
	requests := make([]FrameRequest, len(launches))
	frame := 0
	for i := range launches {
		if i > 0 {
			frame += 3 + int(rng.Int63n(3))
		}
		requests[i] = FrameRequest{
			RequestID: fmt.Sprintf("REQ-%02d", i+1), Frame: frame, LaunchID: launches[i].ID,
		}
	}

	level := &Level{
		SchemaVersion: 3,
		Seed:          seed,
		Code:          fmt.Sprintf("LVL-SEED-%d", seed),
		Gravity:       mmVec(0, 9798),
		StepMS:        20,
		Bounds:        [2]Point{mmVec(0, 0), mmVec(24000, 16000)},
		Obstacles:     obstacles,
		Launches:      launches,
		Requests:      requests,
	}
	level.Truths = BuildTruths(level)
	return level
}

func (l *Level) LaunchByID(id string) *Launch {
	for i := range l.Launches {
		if l.Launches[i].ID == id {
			return &l.Launches[i]
		}
	}
	return nil
}

func (l *Level) ObstacleByID(id string) *Obstacle {
	for i := range l.Obstacles {
		if l.Obstacles[i].ID == id {
			return &l.Obstacles[i]
		}
	}
	return nil
}

func (l *Level) RequestByID(id string) *FrameRequest {
	for i := range l.Requests {
		if l.Requests[i].RequestID == id {
			return &l.Requests[i]
		}
	}
	return nil
}

// BuildTruths runs the deterministic reference core and turns its bounce
// stream into the repository-carried truth manifest.
func BuildTruths(level *Level) []Truth {
	out := make([]Truth, 0, len(level.Requests))
	for _, req := range level.Requests {
		launch := level.LaunchByID(req.LaunchID)
		if launch == nil {
			continue
		}
		result, err := Predict(level, req.RequestID, *launch, PredictOptions{RecordQuantization: false})
		truth := Truth{RequestID: req.RequestID, LaunchID: launch.ID}
		if result != nil {
			truth.FirstObstacleID = result.FirstObstacleID
			truth.BounceCount = result.BounceCount
			truth.Escaped = result.Escaped
			truth.TerminalReason = result.TerminalReason
			truth.Checkpoints = make([]TruthCheckpoint, 0, len(result.Bounces))
			for _, b := range result.Bounces {
				truth.Checkpoints = append(truth.Checkpoints, TruthCheckpoint{
					Frame: b.Frame, ObstacleID: b.ObstacleID,
					X: b.Point.X, Y: b.Point.Y, BounceSeq: b.Seq,
				})
			}
		}
		if err != nil {
			if ge, ok := AsError(err, CodeBounce); ok {
				truth.ExpectedErrorCode = CodeBounce
				truth.ExpectedBounceSeq = ge.BounceSeq
				if truth.TerminalReason == "" {
					truth.TerminalReason = "bounce_limit"
				}
			} else if ge, ok := AsError(err, ""); ok {
				truth.TerminalReason = "error:" + string(ge.Code)
			}
		}
		out = append(out, truth)
	}
	return out
}
