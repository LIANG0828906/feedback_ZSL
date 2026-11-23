package pipeline

import (
	"encoding/json"
	"errors"
)

// errSimulatedCrash represents a forced termination after durable commits. Only
// blocks already synced to the checkpoint log can be recovered afterwards.
var errSimulatedCrash = errors.New("simulated process termination")

// IsSimulatedCrash reports whether err is the crash-injection sentinel.
func IsSimulatedCrash(err error) bool { return errors.Is(err, errSimulatedCrash) }

func mustMarshal(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}
