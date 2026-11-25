package review

import (
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"

	_ "modernc.org/sqlite"

	"audiobus/errs"
)

type TruthPacket struct {
	Window      int    `json:"window"`
	Channel     int    `json:"channel"`
	Seq         int    `json:"seq"`
	Start       int    `json:"start"`
	Samples     int    `json:"samples"`
	Disposition string `json:"disposition"`
	Mark        int64  `json:"mark"`
	SHA256      string `json:"sha256"`
}

type Truth struct {
	Frames              map[string][]int64 `json:"frames"`
	Packets             []TruthPacket      `json:"packets"`
	ChannelFingerprints map[string]string  `json:"channelFingerprints"`
}

type Result struct {
	PacketsChecked  int      `json:"packetsChecked"`
	ChannelsChecked int      `json:"channelsChecked"`
	CheckedPackets  []string `json:"checkedPackets,omitempty"`
}

func LoadTruth(path string) (*Truth, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var t Truth
	if err := json.Unmarshal(b, &t); err != nil {
		return nil, &errs.Error{Code: errs.EDecode, Path: path, Reason: "invalid truth JSON", Err: err}
	}
	return &t, nil
}

func ReviewSQLite(dbPath, truthPath string) (*Result, error) {
	truth, err := LoadTruth(truthPath)
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", dbPath+"?mode=ro")
	if err != nil {
		return nil, err
	}
	defer db.Close()
	rows, err := db.Query(`SELECT window, channel, seq, start, samples, frames FROM packets ORDER BY window, channel, seq`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	reconstructed := map[int][]int64{}
	result := &Result{}
	for rows.Next() {
		var window, ch, seq, start, samples int
		var blob []byte
		if err := rows.Scan(&window, &ch, &seq, &start, &samples, &blob); err != nil {
			return nil, err
		}
		decoded := make([]int64, len(blob)/8)
		for i := range decoded {
			decoded[i] = int64(binary.LittleEndian.Uint64(blob[i*8 : (i+1)*8]))
		}
		truthFrames := truth.Frames[fmt.Sprint(ch)]
		if start+samples > len(truthFrames) {
			return nil, reviewMismatch(ch, int64(start), seq, "persisted packet exceeds truth")
		}
		if samples > len(decoded) {
			return nil, reviewMismatch(ch, int64(start), seq, "persisted blob shorter than sample count")
		}
		for i := 0; i < samples; i++ {
			if decoded[i] != truthFrames[start+i] {
				return nil, reviewMismatch(ch, int64(start+i), seq, fmt.Sprintf("value %d != truth %d", decoded[i], truthFrames[start+i]))
			}
		}
		tp := findTruthPacket(truth, window, ch, seq)
		if tp == nil {
			return nil, reviewMismatch(ch, int64(start), seq, "packet not present in truth")
		}
		if tp.Start != start || tp.Samples != samples {
			return nil, reviewMismatch(ch, int64(start), seq, fmt.Sprintf("boundary/count start=%d samples=%d truth start=%d samples=%d", start, samples, tp.Start, tp.Samples))
		}
		sum := sha256.Sum256(blob)
		if hex.EncodeToString(sum[:]) != tp.SHA256 {
			return nil, reviewMismatch(ch, int64(start), seq, "persisted padded packet bytes do not match truth fingerprint")
		}
		if reconstructed[ch] == nil {
			reconstructed[ch] = make([]int64, len(truthFrames))
		}
		copy(reconstructed[ch][start:start+samples], decoded[:samples])
		result.PacketsChecked++
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for chS, expected := range truth.ChannelFingerprints {
		var ch int
		fmt.Sscanf(chS, "%d", &ch)
		frames := reconstructed[ch]
		if frames == nil {
			return nil, reviewMismatch(ch, 0, 0, "channel missing from persistent packets")
		}
		var b []byte
		for _, v := range frames {
			var buf [8]byte
			binary.LittleEndian.PutUint64(buf[:], uint64(v))
			b = append(b, buf[:]...)
		}
		sum := sha256.Sum256(b)
		if hex.EncodeToString(sum[:]) != expected {
			return nil, reviewMismatch(ch, 0, 0, "channel fingerprint mismatch")
		}
		result.ChannelsChecked++
	}
	return result, nil
}

func findTruthPacket(t *Truth, window, ch, seq int) *TruthPacket {
	for i := range t.Packets {
		p := &t.Packets[i]
		if p.Window == window && p.Channel == ch && p.Seq == seq {
			return p
		}
	}
	return nil
}

func reviewMismatch(ch int, offset int64, seq int, reason string) error {
	return &errs.Error{Code: errs.EDiff, Channel: ch, First: offset, FirstPack: seq, Reason: "independent review: " + reason}
}
