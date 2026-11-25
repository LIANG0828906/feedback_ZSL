package schema

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"

	"obsproj/internal/model"
)

const (
	Version1 = "v1"
	Version2 = "v2"
	Version3 = "v3"
)

type ExportResult struct {
	Data   []byte       `json:"data"`
	Losses []model.Loss `json:"losses,omitempty"`
}

type object map[string]json.RawMessage

func schemaError(version, field, nativeField, message string) *model.KernelError {
	return &model.KernelError{
		Code:        model.CodeSchema,
		Message:     message,
		Version:     version,
		Field:       field,
		NativeField: nativeField,
	}
}

func decodeError(message, cause string) *model.KernelError {
	return &model.KernelError{
		Code:    model.CodeDecode,
		Message: message,
		Cause:   cause,
	}
}

func asObject(raw json.RawMessage, version, field, nativeField string) (object, *model.KernelError) {
	var out object
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, schemaError(version, field, nativeField, "expected object")
	}
	if out == nil {
		return nil, schemaError(version, field, nativeField, "expected non-null object")
	}
	return out, nil
}

func asArray(raw json.RawMessage, version, field, nativeField string) ([]json.RawMessage, *model.KernelError) {
	var out []json.RawMessage
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, schemaError(version, field, nativeField, "expected array")
	}
	return out, nil
}

func required(obj object, key, version, field, nativeKey string) (json.RawMessage, *model.KernelError) {
	raw, ok := obj[key]
	if !ok || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, schemaError(version, field, nativeKey, "required field is missing")
	}
	return raw, nil
}

func stringField(obj object, key, version, field, nativeKey string) (string, *model.KernelError) {
	raw, err := required(obj, key, version, field, nativeKey)
	if err != nil {
		return "", err
	}
	var out string
	if jsonErr := json.Unmarshal(raw, &out); jsonErr != nil {
		return "", schemaError(version, field, nativeKey, "expected string")
	}
	return out, nil
}

func intField(obj object, key, version, field, nativeKey string) (int64, *model.KernelError) {
	raw, err := required(obj, key, version, field, nativeKey)
	if err != nil {
		return 0, err
	}
	var out int64
	if jsonErr := json.Unmarshal(raw, &out); jsonErr != nil {
		return 0, schemaError(version, field, nativeKey, "expected integer")
	}
	return out, nil
}

func objectField(obj object, key, version, field, nativeKey string) (object, *model.KernelError) {
	raw, err := required(obj, key, version, field, nativeKey)
	if err != nil {
		return nil, err
	}
	return asObject(raw, version, field, nativeKey)
}

func optionalObject(obj object, key, version, field, nativeKey string) (object, bool, *model.KernelError) {
	raw, ok := obj[key]
	if !ok || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, false, nil
	}
	out, err := asObject(raw, version, field, nativeKey)
	return out, true, err
}

func arrayField(obj object, key, version, field, nativeKey string) ([]json.RawMessage, *model.KernelError) {
	raw, err := required(obj, key, version, field, nativeKey)
	if err != nil {
		return nil, err
	}
	return asArray(raw, version, field, nativeKey)
}

func pathf(format string, args ...any) string {
	return fmt.Sprintf(format, args...)
}

func parseInt(raw json.RawMessage, version, field, nativeField string) (int64, *model.KernelError) {
	var out int64
	if err := json.Unmarshal(raw, &out); err != nil {
		return 0, schemaError(version, field, nativeField, "expected integer")
	}
	return out, nil
}

func Decode(data []byte) (*model.Session, error) {
	var root object
	if err := json.Unmarshal(data, &root); err != nil {
		return nil, decodeError("invalid JSON session document", err.Error())
	}
	rawVersion, ok := root["schema_version"]
	if !ok {
		return nil, schemaError("", "schema_version", "schema_version", "required field is missing")
	}
	var version string
	if err := json.Unmarshal(rawVersion, &version); err != nil {
		return nil, schemaError("", "schema_version", "schema_version", "expected string")
	}
	var (
		session *model.Session
		err     *model.KernelError
	)
	switch version {
	case Version1:
		session, err = decodeV1(root)
	case Version2:
		session, err = decodeV2(root)
	case Version3:
		session, err = decodeV3(root)
	default:
		return nil, &model.KernelError{
			Code:        model.CodeSchema,
			Message:     "unknown schema version",
			Version:     version,
			Field:       "schema_version",
			NativeField: "schema_version",
		}
	}
	if err != nil {
		return nil, err
	}
	session.SourceVersion = version
	session.Normalize()
	if err := validateSessionShape(session); err != nil {
		return nil, err
	}
	return session, nil
}

func validateSessionShape(s *model.Session) *model.KernelError {
	if s.ID == "" {
		return schemaError(s.SourceVersion, "session_id", "session_id", "session id must not be empty")
	}
	if s.TickRate <= 0 {
		return schemaError(s.SourceVersion, "tick_rate", "tick_rate", "tick rate must be positive")
	}
	if len(s.Screens) == 0 {
		return schemaError(s.SourceVersion, "screens", "screens", "at least one screen is required")
	}
	if len(s.Commands) == 0 {
		return schemaError(s.SourceVersion, "frames", "frames", "at least one frame command is required")
	}
	seenScreens := map[string]bool{}
	for i, screen := range s.Screens {
		if screen.ID == "" {
			return schemaError(s.SourceVersion, pathf("screens[%d].id", i), "id", "screen id is required")
		}
		if seenScreens[screen.ID] {
			return schemaError(s.SourceVersion, pathf("screens[%d].id", i), "id", "duplicate screen id")
		}
		seenScreens[screen.ID] = true
		if screen.Resolution.W <= 0 || screen.Resolution.H <= 0 {
			return schemaError(s.SourceVersion, pathf("screens[%d].resolution", i), "width/height", "resolution must be positive")
		}
		if len(screen.Capability.Scales) == 0 || len(screen.Capability.Chromas) == 0 {
			return schemaError(s.SourceVersion, pathf("screens[%d].capability", i), "capability", "capability lists must not be empty")
		}
		if screen.Capability.MaxPacketSize <= 0 {
			return schemaError(s.SourceVersion, pathf("screens[%d].capability.max_packet_size", i), "max_packet_size", "max packet size must be positive")
		}
	}
	for i, command := range s.Commands {
		if command.Window <= 0 {
			return schemaError(s.SourceVersion, pathf("frames[%d].window_no", i), "window_no", "window number must be positive")
		}
		if !seenScreens[command.ScreenID] {
			return schemaError(s.SourceVersion, pathf("frames[%d].screen_id", i), "screen_id", "unknown screen")
		}
		if command.Region.W <= 0 || command.Region.H <= 0 {
			return schemaError(s.SourceVersion, pathf("frames[%d].region", i), "region", "region width and height must be positive")
		}
	}
	seenEvents := map[string]bool{}
	for i, event := range s.Events {
		if event.ID == "" {
			return schemaError(s.SourceVersion, pathf("events[%d].event_id", i), "event_id", "event id is required")
		}
		if seenEvents[event.ID] {
			return &model.KernelError{
				Code:     model.CodeConflict,
				Message:  "duplicate event id",
				Version:  s.SourceVersion,
				Field:    pathf("events[%d].event_id", i),
				Window:   event.Window,
				ScreenID: event.ScreenID,
			}
		}
		seenEvents[event.ID] = true
		if !seenScreens[event.ScreenID] {
			return schemaError(s.SourceVersion, pathf("events[%d].screen_id", i), "screen_id", "unknown screen")
		}
		switch event.Kind {
		case model.EventJoin, model.EventExit:
		case model.EventTopology:
			if event.Topology == nil {
				return schemaError(s.SourceVersion, pathf("events[%d].topology", i), "topology", "topology patch is required")
			}
		default:
			return schemaError(s.SourceVersion, pathf("events[%d].kind", i), "kind", "unknown event kind")
		}
	}
	return nil
}

func Export(session *model.Session, version string) (ExportResult, error) {
	if session == nil {
		return ExportResult{}, model.NewError(model.CodeSchema, "nil session")
	}
	work := session.Clone()
	work.Normalize()
	var (
		data   []byte
		losses []model.Loss
		err    error
	)
	switch version {
	case Version1:
		data, losses, err = encodeV1(work)
	case Version2:
		data, losses, err = encodeV2(work)
	case Version3:
		data, losses, err = encodeV3(work)
	default:
		return ExportResult{}, &model.KernelError{
			Code:    model.CodeSchema,
			Message: "unknown schema version",
			Version: version,
			Field:   "schema_version",
		}
	}
	if err != nil {
		return ExportResult{}, err
	}
	result := ExportResult{Data: data, Losses: losses}
	if len(losses) > 0 {
		sort.Slice(losses, func(i, j int) bool {
			if losses[i].Path != losses[j].Path {
				return losses[i].Path < losses[j].Path
			}
			return losses[i].OldValue < losses[j].OldValue
		})
		result.Losses = losses
		return result, &model.KernelError{
			Code:    model.CodeLossy,
			Message: "target schema cannot represent every source value",
			Version: version,
			Losses:  losses,
		}
	}
	return result, nil
}

func Encode(session *model.Session, version string) ([]byte, error) {
	result, err := Export(session, version)
	if result.Data == nil {
		return nil, err
	}
	return result.Data, err
}

func marshal(v any) ([]byte, error) {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return nil, model.NewError(model.CodeDecode, "failed to encode session: "+err.Error())
	}
	return append(data, '\n'), nil
}
