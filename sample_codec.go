package lightprobe

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
)

const sampleMagic = "LPB1"

type SampleBundle struct {
	ProbeID        string      `json:"probe_id"`
	RoomID         string      `json:"room_id"`
	Radiance       [3]float64  `json:"radiance"`
	RawSampleCount float64     `json:"raw_sample_count"`
	SampleCount    int         `json:"sample_count"`
	SHScale        int64       `json:"sh_scale"`
	SH             [3][9]int64 `json:"sh"`
	ContentHash    string      `json:"content_hash"`
}

func EncodeSampleBundle(b SampleBundle) ([]byte, error) {
	payload, err := json.Marshal(b)
	if err != nil {
		return nil, domainError(EVal, b.ProbeID, "sample_bundle", "encode failed: %v", err)
	}
	sum := sha256.Sum256(payload)
	header := sampleMagic + "\n" + hex.EncodeToString(sum[:]) + "\n"
	return append([]byte(header), payload...), nil
}

func DecodeSampleBundle(data []byte) (*SampleBundle, error) {
	parts := strings.SplitN(string(data), "\n", 3)
	if len(parts) != 3 || parts[0] != sampleMagic {
		return nil, domainError(EDecode, "sample-bundle", "magic", "missing %s envelope", sampleMagic)
	}
	want, err := hex.DecodeString(parts[1])
	if err != nil || len(want) != sha256.Size {
		return nil, domainError(EDecode, "sample-bundle", "checksum", "invalid checksum encoding")
	}
	payload := []byte(parts[2])
	got := sha256.Sum256(payload)
	if !equalBytes(want, got[:]) {
		return nil, domainError(EDecode, "sample-bundle", "checksum", "checksum mismatch")
	}
	var bundle SampleBundle
	if err := json.Unmarshal(payload, &bundle); err != nil {
		return nil, domainError(EDecode, "sample-bundle", "payload", "invalid JSON: %v", err)
	}
	if bundle.ProbeID == "" {
		return nil, domainError(EDecode, "sample-bundle", "probe_id", "missing probe identifier")
	}
	return &bundle, nil
}

func equalBytes(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
