package syncgame

import (
	"fmt"
	"sort"
)

func compareWorldStateChanges(tick int, expected, actual WorldState) *GameError {
	return compareWorldStates(tick, expected, actual)
}

func compareWorldStates(tick int, left, right WorldState) *GameError {
	diff := func(playerID, field string, leftValue, rightValue any) *GameError {
		return &GameError{
			Code:     CodeDiverge,
			Message:  "state divergence detected",
			Tick:     tick,
			PlayerID: playerID,
			Field:    field,
			Left:     leftValue,
			Right:    rightValue,
		}
	}
	if left.Tick != right.Tick {
		return diff("*", "tick", left.Tick, right.Tick)
	}
	if left.Finished != right.Finished {
		return diff("*", "finished", left.Finished, right.Finished)
	}
	if left.Winner != right.Winner {
		return diff("*", "winner", left.Winner, right.Winner)
	}
	if left.LockedTick != right.LockedTick {
		return diff("*", "locked_tick", left.LockedTick, right.LockedTick)
	}
	leftUnits := unitMap(left)
	rightUnits := unitMap(right)
	ids := make([]string, 0, len(leftUnits)+len(rightUnits))
	for id := range leftUnits {
		ids = append(ids, id)
	}
	for id := range rightUnits {
		if _, exists := leftUnits[id]; !exists {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	for _, id := range ids {
		a, aok := leftUnits[id]
		b, bok := rightUnits[id]
		if !aok || !bok {
			return diff(id, "unit", aok, bok)
		}
		if a.X != b.X {
			return diff(id, "x", a.X, b.X)
		}
		if a.Y != b.Y {
			return diff(id, "y", a.Y, b.Y)
		}
		if a.Direction != b.Direction {
			return diff(id, "direction", a.Direction, b.Direction)
		}
		if a.Health != b.Health {
			return diff(id, "health", a.Health, b.Health)
		}
		if a.MaxHealth != b.MaxHealth {
			return diff(id, "max_health", a.MaxHealth, b.MaxHealth)
		}
		if a.Resource != b.Resource {
			return diff(id, "resource", a.Resource, b.Resource)
		}
		if a.Alive != b.Alive {
			return diff(id, "alive", a.Alive, b.Alive)
		}
		if a.RespawnAt != b.RespawnAt {
			return diff(id, "respawn_at", a.RespawnAt, b.RespawnAt)
		}
		if a.Kills != b.Kills {
			return diff(id, "kills", a.Kills, b.Kills)
		}
		keys := make(map[string]bool, len(a.SkillCooldown)+len(b.SkillCooldown))
		for key := range a.SkillCooldown {
			keys[key] = true
		}
		for key := range b.SkillCooldown {
			keys[key] = true
		}
		sortedKeys := make([]string, 0, len(keys))
		for key := range keys {
			sortedKeys = append(sortedKeys, key)
		}
		sort.Strings(sortedKeys)
		for _, key := range sortedKeys {
			if a.SkillCooldown[key] != b.SkillCooldown[key] {
				return diff(id, "skill_cooldown["+key+"]", a.SkillCooldown[key], b.SkillCooldown[key])
			}
		}
	}
	return nil
}

func unitMap(state WorldState) map[string]UnitState {
	m := make(map[string]UnitState, len(state.Units))
	for _, u := range state.Units {
		m[u.PlayerID] = u
	}
	return m
}

func CompareAuthority(left, right *AuthorityRun) *GameError {
	if left == nil || right == nil {
		return &GameError{Code: CodeDiverge, Message: "authority run is nil", Field: "authority"}
	}
	n := len(left.States)
	if len(right.States) < n {
		n = len(right.States)
	}
	for i := 0; i < n; i++ {
		if mismatch := compareWorldStates(left.States[i].Tick, left.States[i], right.States[i]); mismatch != nil {
			mismatch.Message = fmt.Sprintf("authority traces diverge at trace index %d", i)
			return mismatch
		}
	}
	if len(left.States) != len(right.States) {
		tick := n
		if tick < len(left.States) {
			tick = left.States[tick].Tick
		}
		return &GameError{
			Code:    CodeDiverge,
			Message: "authority trace lengths differ",
			Tick:    tick,
			Field:   "trace_length",
			Left:    len(left.States),
			Right:   len(right.States),
		}
	}
	if left.Settlement.Winner != right.Settlement.Winner ||
		left.Settlement.LockedTick != right.Settlement.LockedTick ||
		len(left.Settlement.Players) != len(right.Settlement.Players) {
		return &GameError{
			Code:    CodeDiverge,
			Message: "settlement headers differ",
			Tick:    left.Settlement.LockedTick,
			Field:   "settlement",
			Left:    left.Settlement,
			Right:   right.Settlement,
		}
	}
	for i := range left.Settlement.Players {
		a, b := left.Settlement.Players[i], right.Settlement.Players[i]
		if a != b {
			return &GameError{
				Code:     CodeDiverge,
				Message:  "settlement player differs",
				Tick:     left.Settlement.LockedTick,
				PlayerID: a.PlayerID,
				Field:    "settlement",
				Left:     a,
				Right:    b,
			}
		}
	}
	return nil
}

func CompareClientRuns(left, right *ClientRun) *GameError {
	if left == nil || right == nil {
		return &GameError{Code: CodeDiverge, Message: "client run is nil", Field: "client"}
	}
	n := len(left.Frames)
	if len(right.Frames) < n {
		n = len(right.Frames)
	}
	for i := 0; i < n; i++ {
		a, b := left.Frames[i], right.Frames[i]
		if a.Frame != b.Frame || a.Tick != b.Tick || a.Mode != b.Mode {
			return &GameError{
				Code:     CodeDiverge,
				Message:  "client frame metadata differs",
				Tick:     a.Tick,
				PlayerID: left.PlayerID,
				Frame:    a.Frame,
				Field:    "visible_frame",
				Left:     a,
				Right:    b,
			}
		}
		if mismatch := compareWorldStates(a.Tick, a.State, b.State); mismatch != nil {
			mismatch.PlayerID = left.PlayerID
			mismatch.Frame = a.Frame
			mismatch.Message = "client visible state differs"
			return mismatch
		}
	}
	if len(left.Frames) != len(right.Frames) {
		return &GameError{
			Code:     CodeDiverge,
			Message:  "client frame count differs",
			PlayerID: left.PlayerID,
			Field:    "frames",
			Left:     len(left.Frames),
			Right:    len(right.Frames),
		}
	}
	if left.RollbackCount != right.RollbackCount {
		return &GameError{
			Code:     CodeDiverge,
			Message:  "rollback count differs",
			PlayerID: left.PlayerID,
			Field:    "rollback_count",
			Left:     left.RollbackCount,
			Right:    right.RollbackCount,
		}
	}
	return nil
}
