package balance

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"strconv"
)

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func ProjectContentHash(project *Project) string {
	data, err := json.Marshal(project)
	if err != nil {
		return ""
	}
	return sha256Hex(data)
}

func BudgetHash(budget Budget) string {
	data, err := json.Marshal(budget)
	if err != nil {
		return ""
	}
	return sha256Hex(data)
}

// deriveRoll returns a value in [0, 9999]. The only inputs are the global
// project seed, the scenario instance identifier, and the action turn. The
// scenario instance identifier contains the scenario ID and sample index.
func deriveRoll(seed int64, scenarioInstance string, turn int) int {
	var buf []byte
	buf = strconv.AppendInt(buf, seed, 10)
	buf = append(buf, 0)
	buf = append(buf, scenarioInstance...)
	buf = append(buf, 0)
	buf = strconv.AppendInt(buf, int64(turn), 10)
	sum := sha256.Sum256(buf)
	value := binary.BigEndian.Uint64(sum[:8])
	return int(value % 10000)
}
