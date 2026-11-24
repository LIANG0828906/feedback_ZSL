package syncgame

import (
	"fmt"
	"sort"
)

func FinalizeTruth(sample *Sample) error {
	auth, err := SimulateAuthoritative(sample)
	if err != nil {
		return err
	}
	finalTick := len(auth.States) - 1
	keySet := map[int]bool{0: true, 1: true, finalTick: true}
	for _, divisor := range []int{4, 2} {
		keySet[finalTick/divisor] = true
	}
	keyTicks := make([]int, 0, len(keySet))
	for tick := range keySet {
		if tick >= 0 && tick <= finalTick {
			keyTicks = append(keyTicks, tick)
		}
	}
	sort.Ints(keyTicks)
	truth := Truth{KeyTicks: keyTicks}
	for _, tick := range keyTicks {
		truth.Authoritative = append(truth.Authoritative, TruthTick{
			Tick:  tick,
			State: CloneWorld(auth.States[tick]),
		})
	}
	for _, client := range sample.Clients {
		run, clientErr := SimulateClient(sample, auth, client.ID, client.LatencyTier, ClientOptions{})
		if clientErr != nil {
			return fmt.Errorf("client %s truth: %w", client.ID, clientErr)
		}
		truth.Clients = append(truth.Clients, ClientTruth{
			ClientID: client.ID,
			TierID:   client.LatencyTier,
			Frames:   run.Frames,
		})
	}
	settlement := auth.Settlement
	truth.Settlement = &settlement
	truth.ContentHash = hashCanonical(truth)
	sample.Truth = truth
	return nil
}

func ValidateTruth(sample *Sample) error {
	if sample.Truth.Settlement == nil {
		return tickErr(0, "*", "truth.settlement", "truth settlement is missing")
	}
	auth, err := SimulateAuthoritative(sample)
	if err != nil {
		return err
	}
	for _, expected := range sample.Truth.Authoritative {
		if expected.Tick < 0 || expected.Tick >= len(auth.States) {
			return tickErr(expected.Tick, "*", "truth.authoritative", "truth tick is outside the simulation")
		}
		if mismatch := compareWorldStateChanges(expected.Tick, auth.States[expected.Tick], expected.State); mismatch != nil {
			return mismatch
		}
	}
	for _, clientTruth := range sample.Truth.Clients {
		client, ok := sample.Client(clientTruth.ClientID)
		if !ok {
			return tickErr(0, "*", "truth.clients.client_id", "truth references an unknown client")
		}
		run, runErr := SimulateClient(sample, auth, client.ID, clientTruth.TierID, ClientOptions{})
		if runErr != nil {
			return runErr
		}
		if err := compareVisibleFrames(clientTruth.Frames, run.Frames); err != nil {
			return err
		}
	}
	expectedSettlement := sample.Truth.Settlement
	actual := auth.Settlement
	for i := range expectedSettlement.Players {
		if i >= len(actual.Players) {
			return tickErr(actual.LockedTick, "*", "truth.settlement.players", "settlement player count differs")
		}
		exp, got := expectedSettlement.Players[i], actual.Players[i]
		if exp.PlayerID != got.PlayerID || exp.Health != got.Health || exp.Resource != got.Resource ||
			exp.Alive != got.Alive || exp.Kills != got.Kills {
			return &GameError{
				Code:     CodeDiverge,
				Message:  "truth settlement differs from authoritative simulation",
				Tick:     actual.LockedTick,
				PlayerID: exp.PlayerID,
				Field:    "settlement",
				Left:     exp,
				Right:    got,
			}
		}
	}
	return nil
}

func compareVisibleFrames(expected, actual []VisibleFrame) error {
	n := len(expected)
	if len(actual) < n {
		n = len(actual)
	}
	for i := 0; i < n; i++ {
		if expected[i].Frame != actual[i].Frame || expected[i].Tick != actual[i].Tick ||
			expected[i].Mode != actual[i].Mode {
			return &GameError{
				Code:    CodeDiverge,
				Message: "client visible frame differs from generated truth",
				Tick:    actual[i].Tick,
				Frame:   actual[i].Frame,
				Field:   "visible_frame",
				Left:    expected[i],
				Right:   actual[i],
			}
		}
		if mismatch := compareWorldStateChanges(actual[i].Tick, expected[i].State, actual[i].State); mismatch != nil {
			mismatch.Frame = actual[i].Frame
			return mismatch
		}
	}
	if len(expected) != len(actual) {
		return &GameError{
			Code:    CodeDiverge,
			Message: "client visible frame count differs",
			Field:   "frames",
			Left:    len(expected),
			Right:   len(actual),
		}
	}
	return nil
}
