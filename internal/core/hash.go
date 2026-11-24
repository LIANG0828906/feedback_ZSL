package core

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
)

type hashPayload struct {
	SchemaVersion int      `json:"schema_version"`
	ID            string   `json:"id"`
	Seed          uint64   `json:"seed"`
	TickUnitMS    int64    `json:"tick_unit_ms"`
	ParamsVersion string   `json:"params_version"`
	FinalTick     int64    `json:"final_tick"`
	Nodes         []Node   `json:"nodes"`
	Intents       []Intent `json:"intents"`
}

// ComputeSceneHash hashes every simulation-affecting field. Truth and the
// stored hash itself are intentionally excluded so recomputing truth does not
// invalidate checkpoints.
func ComputeSceneHash(s *Scenario) (string, error) {
	b, err := json.Marshal(hashPayload{
		SchemaVersion: s.SchemaVersion,
		ID:            s.ID,
		Seed:          s.Seed,
		TickUnitMS:    s.TickUnitMS,
		ParamsVersion: s.ParamsVersion,
		FinalTick:     s.FinalTick,
		Nodes:         s.Nodes,
		Intents:       s.Intents,
	})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

func HashBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
