package kernel

import (
	"math/big"
)

type sweepHit struct {
	segmentIndex int
	tNum         *big.Int
	tDen         *big.Int
	uNum         *big.Int
	pointXNum    *big.Int
	pointYNum    *big.Int
}

func Predict(scene *Scene) (*PredictionReport, error) {
	if err := PrepareScene(scene); err != nil {
		return nil, err
	}
	report := &PredictionReport{Trajectories: make([]Trajectory, 0, len(scene.Requests))}

	for _, request := range scene.Requests {
		launcher, err := findLauncher(scene, request.LauncherID)
		if err != nil {
			return nil, err
		}
		maxFrames := request.MaxFrames
		if maxFrames <= 0 {
			maxFrames = DefaultMaxFrames
		}

		traj := Trajectory{
			RequestID:  request.ID,
			LauncherID: launcher.ID,
			Samples: []Sample{{
				RequestID:    request.ID,
				LauncherID:   launcher.ID,
				LogicalFrame: request.LogicalFrame,
				LocalFrame:   0,
				Position:     launcher.Position,
				Quant: Quantization{
					Frame:             0,
					TruncatedPosition: launcher.Position,
					MoveDenominator:   1000,
					HitDenominator:    1,
				},
			}},
		}
		hitIDs := []string{}
		firstHit := ""
		bounceCount := 0
		pos := launcher.Position
		velocity := launcher.Velocity
		escaped := false
		terminalFrame := 0

		for frame := 1; frame <= maxFrames; frame++ {
			quant := Quantization{
				Frame:             frame,
				TruncatedPosition: pos,
				MoveDenominator:   1000,
				HitDenominator:    1,
			}

			dvx, gremX, err := MulDiv(scene.Gravity.X, scene.StepMS, 1000, RoundingTruncate)
			if err != nil {
				return nil, err
			}
			dvy, gremY, err := MulDiv(scene.Gravity.Y, scene.StepMS, 1000, RoundingTruncate)
			if err != nil {
				return nil, err
			}
			quant.GravityRemainder = Vec2{X: gremX, Y: gremY}
			velocity.X += dvx
			velocity.Y += dvy
			beforeMoveVelocity := velocity

			moveX, mremX, err := MulDiv(velocity.X, scene.StepMS, 1000, RoundingTruncate)
			if err != nil {
				return nil, err
			}
			moveY, mremY, err := MulDiv(velocity.Y, scene.StepMS, 1000, RoundingTruncate)
			if err != nil {
				return nil, err
			}
			quant.MoveRemainder = Vec2{X: mremX, Y: mremY}
			target := Vec2{X: pos.X + moveX, Y: pos.Y + moveY}

			hit := sweepFirst(scene.Segments, pos, Vec2{X: moveX, Y: moveY}, velocity)
			if hit != nil {
				seg := scene.Segments[hit.segmentIndex]
				hitPos, hitRem, den, err := rationalPoint(pos, Vec2{X: moveX, Y: moveY}, hit)
				if err != nil {
					return nil, err
				}
				quant.HitRemainder = hitRem
				quant.HitDenominator = den
				quant.TruncatedPosition = hitPos

				if firstHit == "" {
					firstHit = seg.ID
				}
				hitIDs = append(hitIDs, seg.ID)

				bounce := Bounce{
					RequestID:      request.ID,
					Index:          len(traj.Bounces),
					Frame:          frame,
					SegmentID:      seg.ID,
					Position:       hitPos,
					Normal:         seg.Normal,
					VelocityBefore: beforeMoveVelocity,
					Restitution:    seg.Restitution,
					Penetrated:     seg.Penetrable,
				}

				if seg.Penetrable {
					bounce.VelocityAfter = velocity
					traj.Bounces = append(traj.Bounces, bounce)
					pos = target
				} else {
					after, rem, err := reflectVelocity(velocity, seg, scene.SchemaVersion)
					if err != nil {
						return nil, err
					}
					bounce.VelocityAfter = after
					quant.ReflectionRemainder = rem
					traj.Bounces = append(traj.Bounces, bounce)
					bounceCount++
					velocity = after
					pos = hitPos
				}
			} else {
				pos = target
				quant.TruncatedPosition = pos
			}

			traj.Samples = append(traj.Samples, Sample{
				RequestID:    request.ID,
				LauncherID:   launcher.ID,
				LogicalFrame: request.LogicalFrame + int64(frame),
				LocalFrame:   frame,
				Position:     pos,
				Quant:        quant,
			})

			if pos.X < 0 || pos.Y < 0 || pos.X > scene.Width || pos.Y > scene.Height {
				escaped = true
				terminalFrame = frame
				break
			}
			terminalFrame = frame
		}

		outcome := "inside_frame_limit"
		if escaped {
			if bounceCount == 0 {
				outcome = "escaped_no_hit"
			} else {
				outcome = "escaped_after_bounce"
			}
		} else if bounceCount > 0 {
			outcome = "bounced_inside_limit"
		}

		traj.Attribution = Attribution{
			RequestID:         request.ID,
			LauncherID:        launcher.ID,
			FirstHitSegmentID: firstHit,
			BounceCount:       bounceCount,
			HitSegmentIDs:     hitIDs,
			Escaped:           escaped,
			TerminalFrame:     terminalFrame,
			End:               pos,
			Outcome:           outcome,
		}
		report.Trajectories = append(report.Trajectories, traj)
	}
	return report, nil
}

func findLauncher(scene *Scene, id string) (*Launcher, error) {
	for i := range scene.Launchers {
		if scene.Launchers[i].ID == id {
			return &scene.Launchers[i], nil
		}
	}
	return nil, schemaError("$.launchers", scene.SchemaVersion, "unknown launcher %q", id)
}

func sweepFirst(segments []Segment, origin, move Vec2, velocity Vec2) *sweepHit {
	if move.X == 0 && move.Y == 0 {
		return nil
	}
	var best *sweepHit

	for i := range segments {
		seg := &segments[i]
		sx := bigSub(seg.B.X, seg.A.X)
		sy := bigSub(seg.B.Y, seg.A.Y)
		rx := big.NewInt(move.X)
		ry := big.NewInt(move.Y)

		rxs := new(big.Int).Sub(
			new(big.Int).Mul(rx, sy),
			new(big.Int).Mul(ry, sx),
		)
		if rxs.Sign() == 0 {
			continue
		}

		qx := bigSub(seg.A.X, origin.X)
		qy := bigSub(seg.A.Y, origin.Y)
		qxs := new(big.Int).Sub(
			new(big.Int).Mul(qx, sy),
			new(big.Int).Mul(qy, sx),
		)
		qxr := new(big.Int).Sub(
			new(big.Int).Mul(qx, ry),
			new(big.Int).Mul(qy, rx),
		)

		tNum := new(big.Int).Set(qxs)
		uNum := new(big.Int).Set(qxr)
		tDen := new(big.Int).Set(rxs)
		if tDen.Sign() < 0 {
			tDen.Neg(tDen)
			tNum.Neg(tNum)
			uNum.Neg(uNum)
		}
		if tNum.Sign() < 0 || tNum.Cmp(tDen) > 0 {
			continue
		}
		if uNum.Sign() < 0 || uNum.Cmp(tDen) > 0 {
			continue
		}

		normal := RawNormal(seg.A, seg.B, seg.NormalSide)
		if bigDot(velocity, normal).Sign() >= 0 {
			continue
		}

		hit := &sweepHit{segmentIndex: i, tNum: tNum, tDen: tDen, uNum: uNum}
		if best == nil || new(big.Int).Mul(hit.tNum, best.tDen).Cmp(new(big.Int).Mul(best.tNum, hit.tDen)) < 0 {
			best = hit
		}
	}
	return best
}

func rationalPoint(origin, move Vec2, hit *sweepHit) (Vec2, Vec2, int64, error) {
	den := hit.tDen
	xNum := new(big.Int).Add(
		new(big.Int).Mul(big.NewInt(origin.X), den),
		new(big.Int).Mul(big.NewInt(move.X), hit.tNum),
	)
	yNum := new(big.Int).Add(
		new(big.Int).Mul(big.NewInt(origin.Y), den),
		new(big.Int).Mul(big.NewInt(move.Y), hit.tNum),
	)
	qx, rx := new(big.Int), new(big.Int)
	qy, ry := new(big.Int), new(big.Int)
	qx.QuoRem(xNum, den, rx)
	qy.QuoRem(yNum, den, ry)
	if !qx.IsInt64() || !qy.IsInt64() || !den.IsInt64() {
		return Vec2{}, Vec2{}, 0, overflowError("$.sweep.point", "", 0, xNum.String(), den.String())
	}
	return Vec2{X: qx.Int64(), Y: qy.Int64()},
		Vec2{X: rx.Int64(), Y: ry.Int64()},
		den.Int64(),
		nil
}

func reflectVelocity(v Vec2, seg Segment, sourceVersion int) (Vec2, Vec2, error) {
	normal := RawNormal(seg.A, seg.B, seg.NormalSide)
	dot := bigDot(v, normal)
	n2 := new(big.Int).Add(
		new(big.Int).Mul(big.NewInt(normal.X), big.NewInt(normal.X)),
		new(big.Int).Mul(big.NewInt(normal.Y), big.NewInt(normal.Y)),
	)
	if n2.Sign() == 0 {
		return v, Vec2{}, nil
	}
	kNum := big.NewInt(CoeffScale + seg.Restitution)
	den := new(big.Int).Mul(big.NewInt(CoeffScale), n2)

	reflectComponent := func(component int64, normalComponent int64) (int64, int64, error) {
		num := new(big.Int).Mul(kNum, dot)
		num.Mul(num, big.NewInt(normalComponent))
		q := new(big.Int)
		r := new(big.Int)
		q.QuoRem(num, den, r)
		if !q.IsInt64() {
			return 0, 0, overflowError("$.reflection", seg.ID, sourceVersion, v, normal)
		}
		rem := int64(0)
		if r.IsInt64() {
			rem = r.Int64()
		}
		delta := q.Int64()
		out, ok := Sub64(component, delta)
		if !ok {
			return 0, 0, overflowError("$.reflection.velocity", seg.ID, sourceVersion, component, delta)
		}
		return out, rem, nil
	}

	rx, remX, err := reflectComponent(v.X, normal.X)
	if err != nil {
		return Vec2{}, Vec2{}, err
	}
	ry, remY, err := reflectComponent(v.Y, normal.Y)
	if err != nil {
		return Vec2{}, Vec2{}, err
	}
	return Vec2{X: rx, Y: ry}, Vec2{X: remX, Y: remY}, nil
}
