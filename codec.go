package fog

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"strings"
)

type MediumEnvelope struct {
	ID      string `json:"id"`
	Codec   string `json:"codec"`
	SHA256  string `json:"sha256"`
	DataB64 string `json:"data_b64"`
}

func EncodeMedium(medium *Medium) ([]byte, error) {
	payload, err := json.Marshal(medium)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(payload)
	envelope := MediumEnvelope{
		ID:      medium.ID,
		Codec:   "application/json+base64",
		SHA256:  hex.EncodeToString(sum[:]),
		DataB64: base64.StdEncoding.EncodeToString(payload),
	}
	return json.MarshalIndent(envelope, "", "  ")
}

func DecodeMedium(document []byte) (*Medium, error) {
	var envelope MediumEnvelope
	if err := json.Unmarshal(document, &envelope); err != nil {
		e := newError(CodeDecode, "medium envelope JSON is corrupt: %v", err)
		e.Field = "document"
		e.Value = truncateForError(string(document))
		if id := extractMediumID(document); id != "" {
			e.MediumID = id
		}
		return nil, e
	}
	if envelope.ID == "" {
		e := newError(CodeDecode, "medium envelope id is missing")
		e.Field = "id"
		e.Value = envelope.ID
		return nil, e
	}
	payload, err := base64.StdEncoding.DecodeString(envelope.DataB64)
	if err != nil {
		return nil, decodeMediumError(envelope.ID, "data_b64", envelope.DataB64, "base64 decode failed: %v", err)
	}
	sum := sha256.Sum256(payload)
	if envelope.SHA256 != hex.EncodeToString(sum[:]) {
		return nil, decodeMediumError(envelope.ID, "sha256", envelope.SHA256, "checksum mismatch; expected %s", hex.EncodeToString(sum[:]))
	}
	var medium Medium
	if err := json.Unmarshal(payload, &medium); err != nil {
		return nil, decodeMediumError(envelope.ID, "data", truncateForError(string(payload)), "medium payload decode failed: %v", err)
	}
	if medium.ID != envelope.ID {
		return nil, decodeMediumError(envelope.ID, "id", medium.ID, "envelope id does not match payload id")
	}
	return &medium, nil
}

func CorruptEncodedMedium(document []byte) ([]byte, string, error) {
	var envelope map[string]any
	if err := json.Unmarshal(document, &envelope); err != nil {
		return nil, "", err
	}
	data, _ := envelope["data_b64"].(string)
	if data == "" {
		envelope["data_b64"] = "%%%not-base64"
	} else {
		pos := len(data) / 2
		ch := data[pos]
		if ch == 'A' {
			ch = 'B'
		} else {
			ch = 'A'
		}
		data = data[:pos] + string(ch) + data[pos+1:]
		envelope["data_b64"] = data
	}
	out, err := json.Marshal(envelope)
	return out, envelope["id"].(string), err
}

func decodeMediumError(id, field string, value any, format string, args ...any) *Error {
	e := newError(CodeDecode, format, args...)
	e.MediumID = id
	e.Field = field
	e.Value = value
	return e
}

func extractMediumID(document []byte) string {
	var partial struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(document, &partial); err == nil {
		return partial.ID
	}
	text := string(document)
	marker := `"id":"`
	idx := strings.Index(text, marker)
	if idx < 0 {
		marker = `"id": "`
		idx = strings.Index(text, marker)
	}
	if idx < 0 {
		return ""
	}
	start := idx + len(marker)
	end := strings.IndexByte(text[start:], '"')
	if end < 0 {
		return ""
	}
	return text[start : start+end]
}

func truncateForError(value string) string {
	if len(value) > 160 {
		return value[:160] + "..."
	}
	return value
}
