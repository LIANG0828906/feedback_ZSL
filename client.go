package syncgame

import (
	"fmt"
	"sort"
)

type FieldDelta struct {
	PlayerID string `json:"player_id"`
	Field    string `json:"field"`
	Before   any    `json:"before"`
	After    any    `json:"after"`
	Delta    int    `json:"delta"`
}

type RollbackEvent struct {
	ClientID    string       `json:"client_id"`
	PlayerID    string       `json:"player_id"`
	Tick        int          `json:"tick"`
	Frame       int          `json:"frame"`
	Depth       int          `json:"depth"`
	Consecutive int          `json:"consecutive"`
	Correction  int          `json:"correction"`
	Fields      []FieldDelta `json:"fields"`
}

type CorrectionPoint struct {
	Frame      int    `json:"frame"`
	Tick       int    `json:"tick"`
	Mode       string `json:"mode"`
	Correction int    `json:"correction"`
	Rollback   bool   `json:"rollback"`
}

type InputLatency struct {
	Tick        int  `json:"tick"`
	Frame       int  `json:"frame"`
	VisibleAt   int  `json:"visible_at"`
	Delay       int  `json:"delay_frames"`
	Limit       int  `json:"limit_frames"`
	WithinBound bool `json:"within_bound"`
}

type ClientRun struct {
	ClientID         string            `json:"client_id"`
	PlayerID         string            `json:"player_id"`
	TierID           string            `json:"tier_id"`
	Frames           []VisibleFrame    `json:"frames"`
	RollbackEvents   []RollbackEvent   `json:"rollback_events"`
	CorrectionCurve  []CorrectionPoint `json:"correction_curve"`
	InputLatency     []InputLatency    `json:"input_latency"`
	RollbackCount    int               `json:"rollback_count"`
	MaxRollbackDepth int               `json:"max_rollback_depth"`
	MaxConsecutive   int               `json:"max_consecutive_rollbacks"`
	Algorithm        string            `json:"algorithm_version"`
	TraceHash        string            `json:"trace_hash,omitempty"`
}

type ClientOptions struct {
	// PredictionHorizon optionally limits speculative local input. Zero uses
	// the tier's rollback depth as the horizon.
	PredictionHorizon int
}

func SimulateClient(sample *Sample, auth *AuthorityRun, clientID, tierID string, opts ClientOptions) (*ClientRun, error) {
	engine, err := NewEngine(sample)
	if err != nil {
		return nil, err
	}
	client, ok := sample.Client(clientID)
	if !ok {
		return nil, tickErr(0, "*", "client_id", fmt.Sprintf("unknown client %q", clientID))
	}
	tier, ok := sample.Tier(tierID)
	if !ok {
		return nil, tickErr(0, "*", "latency_tier", fmt.Sprintf("unknown latency tier %q", tierID))
	}
	if len(auth.States) == 0 {
		return nil, tickErr(0, client.PlayerID, "auth_states", "authoritative trace is empty")
	}
	finalTick := len(auth.States) - 1
	delayTicks := divCeil(tier.ObservationDelayFrames*sample.TickRate, sample.RenderFPS)
	correctionFrames := tier.MaxCorrectionFrames
	if correctionFrames >= tier.MaxInputLatencyFrames {
		correctionFrames = tier.MaxInputLatencyFrames - 1
	}
	if correctionFrames < 1 {
		correctionFrames = 1
	}
	horizon := opts.PredictionHorizon
	if horizon <= 0 {
		horizon = tier.MaxRollbackDepth
	}
	if horizon <= 0 {
		horizon = 1
	}

	localInputs := indexLocalInputs(sample, client.PlayerID)
	preds := make(map[int]WorldState)
	preds[0] = engine.InitialState()
	predictAt := func(tick int) (WorldState, error) {
		if tick < 0 {
			tick = 0
		}
		for n := maxTickKey(preds); n < tick; n++ {
			prev, ok := preds[n]
			if !ok {
				return WorldState{}, tickErr(n, client.PlayerID, "prediction",
					fmt.Sprintf("prediction chain broke at tick %d", n))
			}
			intent := TickIntent{Tick: n, Inputs: localInputs[n]}
			next, stepErr := engine.Step(prev, intent)
			if stepErr != nil {
				return WorldState{}, stepErr
			}
			preds[n+1] = next
		}
		return preds[tick], nil
	}

	run := &ClientRun{
		ClientID:  clientID,
		PlayerID:  client.PlayerID,
		TierID:    tierID,
		Algorithm: sample.AlgorithmVersion,
	}
	latestAuth := -1
	consecutive := 0
	correctionLeft := 0
	maxFrames := divCeil(finalTick*sample.RenderFPS, sample.TickRate) + delayTicks + correctionFrames + 4
	for frame := 0; frame <= maxFrames; frame++ {
		targetTick := frame * sample.TickRate / sample.RenderFPS
		if targetTick > finalTick {
			targetTick = finalTick
		}
		freshAuth := targetTick - delayTicks
		if freshAuth > finalTick {
			freshAuth = finalTick
		}

		var frameEvent *RollbackEvent
		for latestAuth < freshAuth {
			nextAuth := latestAuth + 1
			predicted, predErr := predictAt(nextAuth)
			if predErr != nil {
				return nil, predErr
			}
			deltas := compareOwnedState(client.PlayerID, predicted, auth.States[nextAuth])
			depth := targetTick - nextAuth
			if len(deltas) > 0 {
				consecutive++
				correction := 0
				for _, d := range deltas {
					correction += absInt(d.Delta)
				}
				if depth > tier.MaxRollbackDepth {
					return nil, &GameError{
						Code:     CodeRollback,
						Message:  "rollback depth exceeds tier bound",
						Tick:     nextAuth,
						PlayerID: client.PlayerID,
						Frame:    frame,
						Field:    firstDeltaField(deltas),
						Measured: depth,
						Limit:    tier.MaxRollbackDepth,
					}
				}
				if consecutive > tier.MaxConsecutiveRollbacks {
					return nil, &GameError{
						Code:     CodeRollback,
						Message:  "consecutive rollbacks exceed tier bound",
						Tick:     nextAuth,
						PlayerID: client.PlayerID,
						Frame:    frame,
						Field:    firstDeltaField(deltas),
						Measured: consecutive,
						Limit:    tier.MaxConsecutiveRollbacks,
					}
				}
				event := RollbackEvent{
					ClientID:    clientID,
					PlayerID:    client.PlayerID,
					Tick:        nextAuth,
					Frame:       frame,
					Depth:       depth,
					Consecutive: consecutive,
					Correction:  correction,
					Fields:      deltas,
				}
				run.RollbackEvents = append(run.RollbackEvents, event)
				run.RollbackCount++
				if depth > run.MaxRollbackDepth {
					run.MaxRollbackDepth = depth
				}
				if consecutive > run.MaxConsecutive {
					run.MaxConsecutive = consecutive
				}
				frameEvent = &event
				correctionLeft = correctionFrames
			} else {
				consecutive = 0
			}

			// Roll back to the authoritative tick and discard every speculative
			// state after it. Pending inputs are replayed lazily by predictAt.
			for tick := range preds {
				if tick > nextAuth {
					delete(preds, tick)
				}
			}
			preds[nextAuth] = CloneWorld(auth.States[nextAuth])
			latestAuth = nextAuth
		}

		predicted, predErr := predictAt(targetTick)
		if predErr != nil {
			return nil, predErr
		}
		mode := FrameModePredicted
		display := predicted
		basis := "authority-base + unconfirmed-local-inputs"
		if latestAuth < 0 {
			basis = "local-input-extrapolation"
		}
		if correctionLeft > 0 {
			display = CloneWorld(auth.States[latestAuth])
			mode = FrameModeAuthority
			basis = fmt.Sprintf("rollback-to-authority; consistency-first; correction-window=%d; input-latency-guard=%d",
				correctionFrames, tier.MaxInputLatencyFrames)
			correctionLeft--
		} else if latestAuth >= targetTick {
			display = CloneWorld(auth.States[targetTick])
			mode = FrameModeAuthority
			basis = "authority-frame"
		}
		if targetTick-latestAuth > horizon && mode == FrameModePredicted {
			return nil, &GameError{
				Code:     CodeRollback,
				Message:  "prediction horizon exceeded",
				Tick:     targetTick,
				PlayerID: client.PlayerID,
				Frame:    frame,
				Field:    "prediction_horizon",
				Measured: targetTick - latestAuth,
				Limit:    horizon,
			}
		}

		correction := 0
		if frameEvent != nil {
			correction = frameEvent.Correction
		}
		run.Frames = append(run.Frames, VisibleFrame{
			Frame:         frame,
			Tick:          display.Tick,
			Mode:          mode,
			Basis:         basis,
			AuthorityTick: latestAuth,
			Correction:    correction,
			Rollback:      frameEvent != nil,
			State:         CloneWorld(display),
		})
		run.CorrectionCurve = append(run.CorrectionCurve, CorrectionPoint{
			Frame:      frame,
			Tick:       display.Tick,
			Mode:       mode,
			Correction: correction,
			Rollback:   frameEvent != nil,
		})
		if targetTick >= finalTick && latestAuth >= finalTick && correctionLeft == 0 && frame > 0 {
			break
		}
	}

	if err := measureInputLatency(sample, run, tier, localInputs, finalTick); err != nil {
		return nil, err
	}
	return run, nil
}

func indexLocalInputs(sample *Sample, playerID string) map[int][]Intent {
	byTick := make(map[int][]Intent, sample.MaxTicks)
	for _, tickInput := range sample.Intents {
		for _, in := range tickInput.Inputs {
			if in.PlayerID == playerID {
				byTick[tickInput.Tick] = append(byTick[tickInput.Tick], in)
			}
		}
	}
	return byTick
}

func compareOwnedState(playerID string, predicted, authoritative WorldState) []FieldDelta {
	var deltas []FieldDelta
	pred, ok := findUnit(predicted, playerID)
	if !ok {
		return []FieldDelta{{PlayerID: playerID, Field: "unit", Before: "missing", After: "present"}}
	}
	auth, ok := findUnit(authoritative, playerID)
	if !ok {
		return []FieldDelta{{PlayerID: playerID, Field: "unit", Before: "present", After: "missing"}}
	}
	add := func(field string, before, after int) {
		if before != after {
			deltas = append(deltas, FieldDelta{
				PlayerID: playerID,
				Field:    field,
				Before:   before,
				After:    after,
				Delta:    after - before,
			})
		}
	}
	add("x", pred.X, auth.X)
	add("y", pred.Y, auth.Y)
	add("direction", pred.Direction, auth.Direction)
	add("health", pred.Health, auth.Health)
	add("max_health", pred.MaxHealth, auth.MaxHealth)
	add("resource", pred.Resource, auth.Resource)
	add("respawn_at", pred.RespawnAt, auth.RespawnAt)
	add("kills", pred.Kills, auth.Kills)
	if pred.Alive != auth.Alive {
		deltas = append(deltas, FieldDelta{
			PlayerID: playerID,
			Field:    "alive",
			Before:   pred.Alive,
			After:    auth.Alive,
		})
	}
	keys := make(map[string]bool, len(pred.SkillCooldown)+len(auth.SkillCooldown))
	for key := range pred.SkillCooldown {
		keys[key] = true
	}
	for key := range auth.SkillCooldown {
		keys[key] = true
	}
	sortedKeys := make([]string, 0, len(keys))
	for key := range keys {
		sortedKeys = append(sortedKeys, key)
	}
	sort.Strings(sortedKeys)
	for _, key := range sortedKeys {
		add("skill_cooldown["+key+"]", pred.SkillCooldown[key], auth.SkillCooldown[key])
	}
	if predicted.Finished != authoritative.Finished {
		deltas = append(deltas, FieldDelta{
			PlayerID: playerID,
			Field:    "finished",
			Before:   predicted.Finished,
			After:    authoritative.Finished,
		})
	}
	if predicted.Winner != authoritative.Winner {
		deltas = append(deltas, FieldDelta{
			PlayerID: playerID,
			Field:    "winner",
			Before:   predicted.Winner,
			After:    authoritative.Winner,
		})
	}
	return deltas
}

func measureInputLatency(sample *Sample, run *ClientRun, tier LatencyTier, localInputs map[int][]Intent, finalTick int) error {
	ticks := make([]int, 0, len(localInputs))
	for tick := range localInputs {
		if tick < finalTick {
			ticks = append(ticks, tick)
		}
	}
	sort.Ints(ticks)
	for _, tick := range ticks {
		inputFrame := divCeil(tick*sample.RenderFPS, sample.TickRate)
		visibleAt := -1
		for _, frame := range run.Frames {
			if frame.Frame < inputFrame {
				continue
			}
			if frame.Mode == FrameModePredicted && frame.State.Tick >= tick+1 {
				visibleAt = frame.Frame
				break
			}
		}
		latency := InputLatency{Tick: tick, Frame: inputFrame, VisibleAt: visibleAt, Limit: tier.MaxInputLatencyFrames}
		if visibleAt >= 0 {
			latency.Delay = visibleAt - inputFrame
		} else {
			latency.Delay = tier.MaxInputLatencyFrames + 1
		}
		latency.WithinBound = latency.Delay <= tier.MaxInputLatencyFrames
		run.InputLatency = append(run.InputLatency, latency)
		if !latency.WithinBound {
			return &GameError{
				Code:     CodeLatency,
				Message:  "predicted input response exceeds tier bound",
				Tick:     tick,
				PlayerID: run.PlayerID,
				Frame:    visibleAt,
				Field:    "input_response",
				Measured: latency.Delay,
				Limit:    tier.MaxInputLatencyFrames,
			}
		}
	}
	return nil
}

func findUnit(state WorldState, playerID string) (UnitState, bool) {
	for _, u := range state.Units {
		if u.PlayerID == playerID {
			return u, true
		}
	}
	return UnitState{}, false
}

func firstDeltaField(deltas []FieldDelta) string {
	if len(deltas) == 0 {
		return ""
	}
	return deltas[0].Field
}

func maxTickKey(m map[int]WorldState) int {
	max := -1
	for key := range m {
		if key > max {
			max = key
		}
	}
	return max
}

func divCeil(a, b int) int {
	if b <= 0 {
		return 0
	}
	if a <= 0 {
		return 0
	}
	return (a + b - 1) / b
}

func absInt(v int) int {
	if v < 0 {
		return -v
	}
	return v
}
