package checkpoint

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"

	"featurekernel/internal/kernel"
)

const SnapshotMagic = "FEATURE_KERNEL_CHECKPOINT_V1"

type FileEnvelope struct {
	Magic         string          `json:"magic"`
	FormatVersion string          `json:"format_version"`
	Payload       json.RawMessage `json:"payload"`
	SHA256        string          `json:"sha256"`
}

func WriteSnapshotFile(path string, snapshot *Snapshot) error {
	if snapshot == nil {
		return kernel.NewError(kernel.CodeInvalidArgument, "snapshot is required", nil)
	}
	if snapshot.CheckpointVersion == "" {
		snapshot.CheckpointVersion = CheckpointVersionV1
	}
	payload, err := json.Marshal(snapshot)
	if err != nil {
		return kernel.WrapError(kernel.CodeCheckpointTx, err, map[string]any{"stage": "marshal_snapshot"})
	}
	canonicalPayload, err := canonicalJSON(payload)
	if err != nil {
		return kernel.WrapError(kernel.CodeCheckpointTx, err, map[string]any{"stage": "canonical_snapshot"})
	}
	sum := sha256.Sum256(canonicalPayload)
	envelope := FileEnvelope{
		Magic: SnapshotMagic, FormatVersion: CheckpointVersionV1,
		Payload: payload, SHA256: hex.EncodeToString(sum[:]),
	}
	data, err := json.MarshalIndent(envelope, "", "  ")
	if err != nil {
		return kernel.WrapError(kernel.CodeCheckpointTx, err, map[string]any{"stage": "marshal_envelope"})
	}
	data = append(data, '\n')
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return kernel.WrapError(kernel.CodeCheckpointOpen, err, map[string]any{"path": path})
	}
	return nil
}

func ReadSnapshotFile(path string) (*Snapshot, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, kernel.WrapError(kernel.CodeCheckpointOpen, err, map[string]any{"path": path})
	}
	var envelope FileEnvelope
	if err := json.Unmarshal(data, &envelope); err != nil {
		code := kernel.CodeCheckpointCorrupt
		if isTruncatedJSONError(err) {
			code = kernel.CodeCheckpointTruncated
		}
		return nil, kernel.WrapError(code, err, map[string]any{"path": path, "stage": "snapshot_envelope"})
	}
	if envelope.Magic != SnapshotMagic {
		return nil, kernel.NewError(kernel.CodeCheckpointCorrupt, "checkpoint magic does not match", map[string]any{"path": path, "magic": envelope.Magic})
	}
	canonical, err := canonicalJSON(envelope.Payload)
	if err != nil {
		return nil, kernel.WrapError(kernel.CodeCheckpointCorrupt, err, map[string]any{"path": path, "stage": "canonical_payload"})
	}
	sum := sha256.Sum256(canonical)
	if hex.EncodeToString(sum[:]) != envelope.SHA256 {
		return nil, kernel.NewError(kernel.CodeCheckpointTruncated, "checkpoint checksum does not match; file may be truncated", map[string]any{"path": path})
	}
	if envelope.FormatVersion != CheckpointVersionV1 {
		return nil, kernel.NewError(kernel.CodeCheckpointVersionMismatch, "snapshot format version does not match", map[string]any{
			"stored": envelope.FormatVersion, "current": CheckpointVersionV1,
		})
	}
	var snapshot Snapshot
	if err := json.Unmarshal(envelope.Payload, &snapshot); err != nil {
		code := kernel.CodeCheckpointCorrupt
		if isTruncatedJSONError(err) {
			code = kernel.CodeCheckpointTruncated
		}
		return nil, kernel.WrapError(code, err, map[string]any{"path": path, "stage": "snapshot_payload"})
	}
	if snapshot.CheckpointVersion != CheckpointVersionV1 {
		return nil, kernel.NewError(kernel.CodeCheckpointVersionMismatch, "snapshot checkpoint version does not match", map[string]any{
			"stored": snapshot.CheckpointVersion, "current": CheckpointVersionV1,
		})
	}
	if snapshot.CanonicalVersion != kernel.CanonicalVersion {
		return nil, kernel.NewError(kernel.CodeCheckpointVersionMismatch, "snapshot canonical version does not match; full recomputation required", map[string]any{
			"stored": snapshot.CanonicalVersion, "current": kernel.CanonicalVersion,
		})
	}
	return &snapshot, nil
}

func canonicalJSON(raw json.RawMessage) ([]byte, error) {
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, err
	}
	return json.Marshal(value)
}

func isTruncatedJSONError(err error) bool {
	return errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, io.EOF) ||
		strings.Contains(err.Error(), "unexpected end of JSON input")
}
