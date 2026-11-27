package grid

import (
	"fmt"
	"math/big"
)

const timeFractionScale int64 = 1024
const surfaceNudgeUnits int64 = 10

type Bounce struct {
	Seq        int    `json:"seq"`
	Frame      int    `json:"frame"`
	ObstacleID string `json:"obstacleId"`
	Point      Vec2   `json:"point"`
	Incoming   Vec2   `json:"incoming"`
	Outgoing   Vec2   `json:"outgoing"`
}

type FrameReport struct {
	Frame     int          `json:"frame"`
	Start     Vec2         `json:"start"`
	End       Vec2         `json:"end"`
	Velocity  Vec2         `json:"velocity"`
	Bounces   []Bounce     `json:"bounces,omitempty"`
	Waypoints []Vec2       `json:"waypoints,omitempty"`
	Traces    []FieldTrace `json:"traces,omitempty"`
}

type PredictResult struct {
	RequestID       string         `json:"requestId"`
	LaunchID        string         `json:"launchId"`
	Path            []Vec2         `json:"path"`
	Bounces         []Bounce       `json:"bounces"`
	Frames          []FrameReport  `json:"frames,omitempty"`
	FirstObstacleID string         `json:"firstObstacleId,omitempty"`
	BounceCount     int            `json:"bounceCount"`
	Escaped         bool           `json:"escaped"`
	TerminalReason  string         `json:"terminalReason"`
	HitStats        map[string]int `json:"hitStats"`
}

type PredictOptions struct {
	RecordQuantization bool
}

type sweepHit struct {
	obstacle *Obstacle
	num      *big.Int
	den      *big.Int
}

// Predict runs the deterministic fixed-point ballistic core for one request.
func Predict(level *Level, requestID string, launch Launch, opts ...PredictOptions) (*PredictResult, error) {
	opt := PredictOptions{RecordQuantization: true}
	if len(opts) > 0 {
		opt = opts[0]
	}
	result := &PredictResult{
		RequestID: requestID,
		LaunchID:  launch.ID,
		Path:      []Vec2{launch.Origin},
		HitStats:  map[string]int{},
	}
	pos := launch.Origin
	vel := launch.Velocity
	stepMS := level.StepMS
	if stepMS <= 0 {
		stepMS = 20
	}

	for frame := 1; frame <= launch.FrameLimit; frame++ {
		report := FrameReport{Frame: frame, Start: pos}
		traces := []FieldTrace{}

		gx, gtrunc := scaledImpulse(level.Gravity.X, stepMS, 1000, fmt.Sprintf("request.%s.frame[%d].gravity.x", requestID, frame))
		gy, gytrunc := scaledImpulse(level.Gravity.Y, stepMS, 1000, fmt.Sprintf("request.%s.frame[%d].gravity.y", requestID, frame))
		if opt.RecordQuantization {
			traces = append(traces, gtrunc, gytrunc)
		}
		var err error
		vel.X, err = addChecked("velocity.x", vel.X, gx)
		if err != nil {
			return result, withRequest(err, requestID)
		}
		vel.Y, err = addChecked("velocity.y", vel.Y, gy)
		if err != nil {
			return result, withRequest(err, requestID)
		}

		intervalMS := stepMS
		waypoints := []Vec2{pos}
		frameBounces := []Bounce{}

		for substep := 0; substep < 8 && intervalMS > 0; substep++ {
			dx, tx := scaledImpulse(vel.X, intervalMS, 1000, fmt.Sprintf("request.%s.frame[%d].displacement.x", requestID, frame))
			dy, ty := scaledImpulse(vel.Y, intervalMS, 1000, fmt.Sprintf("request.%s.frame[%d].displacement.y", requestID, frame))
			if opt.RecordQuantization {
				traces = append(traces, tx, ty)
			}
			end := Vec2{X: pos.X + dx, Y: pos.Y + dy}
			hit := findSweepHit(pos, end, level.Obstacles)
			if hit == nil {
				pos = end
				waypoints = append(waypoints, pos)
				break
			}

			hx, hy, qtraces := quantizeHit(pos, dx, dy, hit, requestID, frame)
			if opt.RecordQuantization {
				traces = append(traces, qtraces...)
			}
			hitPoint := Vec2{X: hx, Y: hy}
			if result.BounceCount >= launch.BounceLimit {
				ge := &Error{
					Code: CodeBounce, RequestID: requestID, ObjectID: hit.obstacle.ID, Field: "bounceLimit",
					Message:   fmt.Sprintf("bounce limit %d exceeded", launch.BounceLimit),
					BounceSeq: result.BounceCount + 1,
				}
				return result, ge
			}

			incoming := vel
			out, rtraces := reflectVelocity(vel, hit.obstacle.Normal, hit.obstacle.Restitution,
				fmt.Sprintf("request.%s.frame[%d].%s.reflection", requestID, frame, hit.obstacle.ID))
			if opt.RecordQuantization {
				traces = append(traces, rtraces...)
			}
			seq := result.BounceCount + 1
			bounce := Bounce{
				Seq: seq, Frame: frame, ObstacleID: hit.obstacle.ID, Point: hitPoint,
				Incoming: incoming, Outgoing: out,
			}
			result.BounceCount++
			result.Bounces = append(result.Bounces, bounce)
			frameBounces = append(frameBounces, bounce)
			result.HitStats[hit.obstacle.ID]++
			if result.FirstObstacleID == "" {
				result.FirstObstacleID = hit.obstacle.ID
			}
			result.Path = append(result.Path, hitPoint)
			waypoints = append(waypoints, hitPoint)

			dot := vel.X*hit.obstacle.Normal.X + vel.Y*hit.obstacle.Normal.Y
			pos = nudgeFromSurface(hitPoint, hit.obstacle.Normal, dot)
			vel = out

			remainingMS, remTrace := remainingInterval(intervalMS, hit.num, hit.den, requestID, frame, hit.obstacle.ID)
			if opt.RecordQuantization && remTrace != nil {
				traces = append(traces, *remTrace)
			}
			intervalMS = remainingMS
			if intervalMS == 0 {
				break
			}
			if substep == 7 {
				pos = end
				waypoints = append(waypoints, pos)
			}
		}

		report.End = pos
		report.Velocity = vel
		report.Bounces = frameBounces
		report.Waypoints = waypoints
		if opt.RecordQuantization {
			report.Traces = traces
			result.Frames = append(result.Frames, report)
		}
		result.Path = append(result.Path, pos)

		if pos.X < level.Bounds[0].X || pos.X > level.Bounds[1].X ||
			pos.Y < level.Bounds[0].Y || pos.Y > level.Bounds[1].Y {
			result.Escaped = true
			result.TerminalReason = "escaped-scene-bounds"
			return result, nil
		}
	}

	result.TerminalReason = "frame-limit"
	return result, nil
}

func withRequest(err error, requestID string) error {
	if ge, ok := err.(*Error); ok && ge.ObjectID == "" {
		ge.ObjectID = requestID
	}
	return err
}

func scaledImpulse(value, ms, divisor int64, field string) (int64, FieldTrace) {
	num := value * ms
	q := num / divisor
	rem := num % divisor
	trace := FieldTrace{
		Kind: "truncation", Field: field, Position: "fixed-divide",
		OldValue: fmt.Sprintf("%d*%d", value, ms), NewValue: fmt.Sprint(q),
		Rounding: declaredRounding, Numerator: num, Denominator: divisor, Remainder: rem,
	}
	return q, trace
}

func findSweepHit(from, to Vec2, obstacles []Obstacle) *sweepHit {
	d := to.Sub(from)
	var best *sweepHit
	for i := range obstacles {
		o := &obstacles[i]
		e := o.B.Sub(o.A)
		q := from.Sub(o.A)

		ex := big.NewInt(e.X)
		ey := big.NewInt(e.Y)
		dx := big.NewInt(d.X)
		dy := big.NewInt(d.Y)
		qx := big.NewInt(q.X)
		qy := big.NewInt(q.Y)

		den := crossBig(ex, ey, dx, dy)
		numT := new(big.Int).Neg(crossBig(ex, ey, qx, qy))
		uNum := new(big.Int).Neg(crossBig(dx, dy, qx, qy))
		if den.Sign() == 0 {
			continue
		}
		if den.Sign() < 0 {
			den.Neg(den)
			numT.Neg(numT)
			uNum.Neg(uNum)
		}
		if numT.Sign() < 0 || numT.Cmp(den) > 0 {
			continue
		}

		if uNum.Sign() < 0 || uNum.Cmp(den) > 0 {
			continue
		}

		candidate := &sweepHit{obstacle: o, num: new(big.Int).Set(numT), den: new(big.Int).Set(den)}
		if best == nil || fractionLess(candidate.num, candidate.den, best.num, best.den) {
			best = candidate
		}
	}
	return best
}

func crossBig(ax, ay, bx, by *big.Int) *big.Int {
	left := new(big.Int).Mul(ax, by)
	right := new(big.Int).Mul(ay, bx)
	return left.Sub(left, right)
}

func fractionLess(aNum, aDen, bNum, bDen *big.Int) bool {
	left := new(big.Int).Mul(aNum, bDen)
	right := new(big.Int).Mul(bNum, aDen)
	return left.Cmp(right) < 0
}

func quantizeHit(from Vec2, dx, dy int64, hit *sweepHit, requestID string, frame int) (int64, int64, []FieldTrace) {
	traces := make([]FieldTrace, 0, 2)
	quantizeOne := func(base, delta int64, axis string) int64 {
		baseBig := big.NewInt(base)
		total := new(big.Int).Mul(baseBig, hit.den)
		total.Add(total, new(big.Int).Mul(big.NewInt(delta), hit.num))
		qBig, remBig := new(big.Int).QuoRem(total, hit.den, new(big.Int))
		q := qBig.Int64()
		rem := remBig.Int64()
		field := fmt.Sprintf("request.%s.frame[%d].hit.%s", requestID, frame, axis)
		traces = append(traces, FieldTrace{
			Kind: "truncation", ObjectID: hit.obstacle.ID, Field: field,
			Position: "sweep-rational->micrometre", OldValue: total.String() + "/" + hit.den.String(),
			NewValue: fmt.Sprint(q), Rounding: declaredRounding,
			Numerator: safeInt64Or(total, 0), Denominator: safeInt64Or(hit.den, 1), Remainder: rem,
		})
		return q
	}
	return quantizeOne(from.X, dx, "x"), quantizeOne(from.Y, dy, "y"), traces
}

func safeInt64Or(v *big.Int, fallback int64) int64 {
	if v.IsInt64() {
		return v.Int64()
	}
	return fallback
}

func reflectVelocity(in Vec2, n Vec2, restitutionBPS int64, field string) (Vec2, []FieldTrace) {
	nx := big.NewInt(n.X)
	ny := big.NewInt(n.Y)
	vx := big.NewInt(in.X)
	vy := big.NewInt(in.Y)
	dot := new(big.Int).Add(new(big.Int).Mul(vx, nx), new(big.Int).Mul(vy, ny))
	normSq := new(big.Int).Add(new(big.Int).Mul(nx, nx), new(big.Int).Mul(ny, ny))
	factor := big.NewInt(10000 + restitutionBPS)
	den := new(big.Int).Mul(big.NewInt(10000), normSq)
	traces := []FieldTrace{}

	reflectAxis := func(velocity *big.Int, normal *big.Int, axis string) int64 {
		num := new(big.Int).Mul(factor, dot)
		num.Mul(num, normal)
		deltaBig, remBig := new(big.Int).QuoRem(num, den, new(big.Int))
		outBig := new(big.Int).Sub(velocity, deltaBig)
		traces = append(traces, FieldTrace{
			Kind: "truncation", Field: field + "." + axis, Position: "reflection-fixed-q",
			OldValue: num.String() + "/" + den.String(), NewValue: outBig.String(),
			Rounding: declaredRounding, Numerator: safeInt64Or(num, 0),
			Denominator: safeInt64Or(den, 1), Remainder: safeInt64Or(remBig, 0),
		})
		return outBig.Int64()
	}

	out := Vec2{
		X: reflectAxis(vx, nx, "x"),
		Y: reflectAxis(vy, ny, "y"),
	}
	return out, traces
}

func nudgeFromSurface(p Vec2, n Vec2, velocityDotNormal int64) Vec2 {
	sign := int64(1)
	if velocityDotNormal > 0 {
		sign = -1
	}
	x := p.X + sign*n.X*surfaceNudgeUnits/NormalScale
	y := p.Y + sign*n.Y*surfaceNudgeUnits/NormalScale
	if n.X != 0 && x == p.X {
		x = p.X + sign
	}
	if n.Y != 0 && y == p.Y {
		y = p.Y + sign
	}
	return Vec2{X: x, Y: y}
}

func remainingInterval(intervalMS int64, numT, denT *big.Int, requestID string, frame int, obstacleID string) (int64, *FieldTrace) {
	// Quantize the hit time to timeFractionScale fixed fractions.
	tQBig := new(big.Int).Mul(numT, big.NewInt(timeFractionScale))
	tQBig.Quo(tQBig, denT)
	tQ := tQBig.Int64()
	if tQ < 0 {
		tQ = 0
	}
	if tQ > timeFractionScale {
		tQ = timeFractionScale
	}
	remaining := intervalMS * (timeFractionScale - tQ) / timeFractionScale
	trace := &FieldTrace{
		Kind: "truncation", ObjectID: obstacleID,
		Field:    fmt.Sprintf("request.%s.frame[%d].remainingMs", requestID, frame),
		Position: "time-fraction->ms", OldValue: numT.String() + "/" + denT.String(),
		NewValue: fmt.Sprint(remaining), Rounding: declaredRounding,
		Numerator: intervalMS * (timeFractionScale - tQ), Denominator: timeFractionScale,
	}
	return remaining, trace
}
