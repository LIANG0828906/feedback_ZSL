package layercache

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"

	"layercache/internal/sqlitelite"
)

const CheckpointFormatVersion = "layercache-checkpoint-v1"

type PersistedObject struct {
	ID            string    `json:"id"`
	LayerID       string    `json:"layerId"`
	Heat          int64     `json:"heat"`
	HeatLevel     HeatLevel `json:"heatLevel"`
	LastAccessMS  int64     `json:"lastAccessMs"`
	PendingWrites int64     `json:"pendingWrites"`
}

type PersistedLayer struct {
	ID        string `json:"id"`
	Occupancy int64  `json:"occupancyBytes"`
}

type MigrationProgress struct {
	NextIndex          int               `json:"nextIndex"`
	NextTimeMS         int64             `json:"nextTimeMs"`
	LastCommittedTick  int64             `json:"lastCommittedTick"`
	LastCommittedBatch string            `json:"lastCommittedBatch"`
	PendingBatchID     string            `json:"pendingBatchId"`
	PendingMigrations  []Migration       `json:"pendingMigrations"`
	CurrentLayer       map[string]string `json:"currentLayer"`
}

type GateProgress struct {
	PendingWrites map[string]int64     `json:"pendingWrites"`
	Pending       map[string]Migration `json:"pending"`
}

type CheckpointMeta struct {
	FormatVersion string `json:"formatVersion"`
	ProjectKey    string `json:"projectKey"`
	SchemaVersion string `json:"schemaVersion"`
	CommittedSeq  int64  `json:"committedSeq"`
	NextIndex     int    `json:"nextIndex"`
	NextTimeMS    int64  `json:"nextTimeMs"`
	ObjectCount   int    `json:"objectCount"`
	LayerCount    int    `json:"layerCount"`
	StateHash     string `json:"stateHash"`
}

type CheckpointPayload struct {
	ProjectKey    string            `json:"projectKey"`
	SchemaVersion string            `json:"schemaVersion"`
	CommittedSeq  int64             `json:"committedSeq"`
	NextIndex     int               `json:"nextIndex"`
	NextTimeMS    int64             `json:"nextTimeMs"`
	Runtime       runtimeState      `json:"runtime"`
	Objects       []PersistedObject `json:"objects"`
	Layers        []PersistedLayer  `json:"layers"`
}

type CheckpointStore struct {
	Path string
}

func NewCheckpointStore(path string) *CheckpointStore {
	return &CheckpointStore{Path: path}
}

func (s *CheckpointStore) Save(payload *CheckpointPayload, expectedVersion string) error {
	if expectedVersion == "" {
		expectedVersion = CheckpointFormatVersion
	}
	if payload == nil {
		return bizError(CodeCheckpointIncomplete, "", "checkpoint", "nil checkpoint payload")
	}
	rows := make([]sqlitelite.Row, 0, len(payload.Objects)+len(payload.Layers)+4)
	var stateDigest []byte
	rowID := int64(1)
	add := func(kind, name, version string, value any, checksum bool) error {
		raw, err := json.Marshal(value)
		if err != nil {
			return err
		}
		var sum string
		if checksum {
			h := sha256.Sum256(append([]byte(kind+"\x00"+name+"\x00"+string(raw)+"\x00"), stateDigest...))
			stateDigest = h[:]
			sum = hex.EncodeToString(h[:])
		}
		if len(raw) > 30000 {
			return bizError(CodeCheckpointTooLarge, payload.SchemaVersion, "checkpoint."+kind+"."+name, "row exceeds SQLite leaf capacity")
		}
		rows = append(rows, sqlitelite.Row{RowID: rowID, Values: []any{kind, name, version, string(raw), sum}})
		rowID++
		return nil
	}
	for _, object := range payload.Objects {
		if err := add("object", object.ID, expectedVersion, object, true); err != nil {
			return err
		}
	}
	for _, layer := range payload.Layers {
		if err := add("layer", layer.ID, expectedVersion, layer, true); err != nil {
			return err
		}
	}
	progress := MigrationProgress{
		NextIndex: payload.NextIndex, NextTimeMS: payload.NextTimeMS,
		LastCommittedTick: payload.CommittedSeq, LastCommittedBatch: payload.Runtime.LastCommittedBatch,
		PendingBatchID: payload.Runtime.PendingBatchID, PendingMigrations: payload.Runtime.PendingMigrations,
		CurrentLayer: payload.Runtime.CurrentLayer,
	}
	if err := add("migration", "progress", expectedVersion, progress, true); err != nil {
		return err
	}
	gate := GateProgress{PendingWrites: payload.Runtime.PendingWrites, Pending: payload.Runtime.Pending}
	if err := add("gate", "progress", expectedVersion, gate, true); err != nil {
		return err
	}
	meta := CheckpointMeta{
		FormatVersion: expectedVersion, ProjectKey: payload.ProjectKey, SchemaVersion: payload.SchemaVersion,
		CommittedSeq: payload.CommittedSeq, NextIndex: payload.NextIndex, NextTimeMS: payload.NextTimeMS,
		ObjectCount: len(payload.Objects), LayerCount: len(payload.Layers),
		StateHash: hex.EncodeToString(stateDigest),
	}
	metaRaw, err := json.Marshal(meta)
	if err != nil {
		return err
	}
	// The meta row is deliberately last: object/layer/gate state is durable
	// before the global sequence and commit marker become visible.
	rows = append(rows, sqlitelite.Row{RowID: rowID, Values: []any{"meta", "commit", expectedVersion, string(metaRaw), meta.StateHash}})
	if err := sqlitelite.WriteCheckpoint(s.Path, rows); err != nil {
		return mapSQLiteError(payload.SchemaVersion, "checkpoint", err)
	}
	return nil
}

func (s *CheckpointStore) Load(expectedVersion string) (*CheckpointPayload, error) {
	if expectedVersion == "" {
		expectedVersion = CheckpointFormatVersion
	}
	rows, err := sqlitelite.ReadCheckpoint(s.Path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, bizError(CodeCheckpointIncomplete, "", "checkpoint", "checkpoint file does not exist")
		}
		return nil, mapSQLiteError("", "checkpoint", err)
	}
	if len(rows) < 5 {
		return nil, bizError(CodeCheckpointIncomplete, "", "checkpoint", "checkpoint has no commit marker")
	}
	parse := func(row sqlitelite.Row) (kind, name, version, payload, checksum string, err error) {
		if len(row.Values) != 5 {
			err = errors.New("checkpoint row does not have five columns")
			return
		}
		var ok bool
		kind, ok = row.Values[0].(string)
		if !ok {
			err = errors.New("kind is not text")
			return
		}
		name, ok = row.Values[1].(string)
		if !ok {
			err = errors.New("name is not text")
			return
		}
		version, ok = row.Values[2].(string)
		if !ok {
			err = errors.New("version is not text")
			return
		}
		payload, ok = row.Values[3].(string)
		if !ok {
			err = errors.New("payload is not text")
			return
		}
		checksum, ok = row.Values[4].(string)
		if !ok {
			err = errors.New("checksum is not text")
			return
		}
		return
	}
	metaRow := rows[len(rows)-1]
	kind, name, version, metaRaw, _, err := parse(metaRow)
	if err != nil {
		return nil, bizError(CodeCheckpointCorrupt, "", "checkpoint.meta", "%v", err)
	}
	if kind != "meta" || name != "commit" {
		return nil, bizError(CodeCheckpointIncomplete, version, "checkpoint", "missing final commit marker")
	}
	if version != expectedVersion {
		return nil, bizError(CodeCheckpointVersionMismatch, version, "checkpoint.meta.version", "expected %q, found %q", expectedVersion, version)
	}
	var meta CheckpointMeta
	if err := json.Unmarshal([]byte(metaRaw), &meta); err != nil {
		return nil, bizError(CodeCheckpointCorrupt, version, "checkpoint.meta", "invalid meta JSON: %v", err)
	}
	if meta.FormatVersion != expectedVersion {
		return nil, bizError(CodeCheckpointVersionMismatch, meta.FormatVersion, "checkpoint.meta.formatVersion", "checkpoint format mismatch")
	}
	payload := &CheckpointPayload{
		ProjectKey: meta.ProjectKey, SchemaVersion: meta.SchemaVersion,
		CommittedSeq: meta.CommittedSeq, NextIndex: meta.NextIndex, NextTimeMS: meta.NextTimeMS,
	}
	var digest []byte
	var objectCount, layerCount int
	for _, row := range rows[:len(rows)-1] {
		kind, name, version, raw, checksum, err := parse(row)
		if err != nil {
			return nil, bizError(CodeCheckpointCorrupt, version, "checkpoint", "%v", err)
		}
		if version != expectedVersion {
			return nil, bizError(CodeCheckpointVersionMismatch, version, fmt.Sprintf("checkpoint.%s.%s", kind, name), "row version mismatch")
		}
		h := sha256.Sum256(append([]byte(kind+"\x00"+name+"\x00"+raw+"\x00"), digest...))
		digest = h[:]
		if checksum != hex.EncodeToString(h[:]) {
			return nil, bizError(CodeCheckpointCorrupt, version, fmt.Sprintf("checkpoint.%s.%s", kind, name), "checksum mismatch")
		}
		switch kind {
		case "object":
			var object PersistedObject
			if err := json.Unmarshal([]byte(raw), &object); err != nil {
				return nil, bizError(CodeCheckpointCorrupt, version, "checkpoint.object."+name, "invalid JSON")
			}
			payload.Objects = append(payload.Objects, object)
			objectCount++
		case "layer":
			var layer PersistedLayer
			if err := json.Unmarshal([]byte(raw), &layer); err != nil {
				return nil, bizError(CodeCheckpointCorrupt, version, "checkpoint.layer."+name, "invalid JSON")
			}
			payload.Layers = append(payload.Layers, layer)
			layerCount++
		case "migration":
			var progress MigrationProgress
			if err := json.Unmarshal([]byte(raw), &progress); err != nil {
				return nil, bizError(CodeCheckpointCorrupt, version, "checkpoint.migration."+name, "invalid JSON")
			}
			payload.Runtime.NextIndex = progress.NextIndex
			payload.Runtime.NextTimeMS = progress.NextTimeMS
			payload.Runtime.LastCommittedTick = progress.LastCommittedTick
			payload.Runtime.LastCommittedBatch = progress.LastCommittedBatch
			payload.Runtime.PendingBatchID = progress.PendingBatchID
			payload.Runtime.PendingMigrations = progress.PendingMigrations
			payload.Runtime.CurrentLayer = progress.CurrentLayer
		case "gate":
			var gate GateProgress
			if err := json.Unmarshal([]byte(raw), &gate); err != nil {
				return nil, bizError(CodeCheckpointCorrupt, version, "checkpoint.gate."+name, "invalid JSON")
			}
			payload.Runtime.PendingWrites = gate.PendingWrites
			payload.Runtime.Pending = gate.Pending
		default:
			return nil, bizError(CodeCheckpointCorrupt, version, "checkpoint."+kind, "unknown row kind")
		}
	}
	if objectCount != meta.ObjectCount || layerCount != meta.LayerCount {
		return nil, bizError(CodeCheckpointIncomplete, version, "checkpoint.meta", "object/layer count mismatch")
	}
	if hex.EncodeToString(digest) != meta.StateHash {
		return nil, bizError(CodeCheckpointCorrupt, version, "checkpoint.meta.stateHash", "state hash mismatch")
	}
	if err := validateCheckpointPayload(payload); err != nil {
		return nil, err
	}
	return payload, nil
}

func validateCheckpointPayload(payload *CheckpointPayload) error {
	if payload.Runtime.CurrentLayer == nil {
		return bizError(CodeCheckpointIncomplete, payload.SchemaVersion, "checkpoint.migration.currentLayer", "missing object layer map")
	}
	objectIDs := map[string]PersistedObject{}
	for _, object := range payload.Objects {
		if _, exists := objectIDs[object.ID]; exists {
			return bizError(CodeCheckpointCorrupt, payload.SchemaVersion, "checkpoint.object."+object.ID, "duplicate object")
		}
		objectIDs[object.ID] = object
		if payload.Runtime.CurrentLayer[object.ID] != object.LayerID {
			return bizError(CodeCheckpointCorrupt, payload.SchemaVersion, "checkpoint.object."+object.ID, "object layer map mismatch")
		}
	}
	for id, move := range payload.Runtime.Pending {
		if payload.Runtime.CurrentLayer[id] != move.SourceLayer {
			return bizError(CodeCheckpointCorrupt, payload.SchemaVersion, "checkpoint.gate.pending."+id, "pending object is not owned by source layer")
		}
	}
	if payload.Runtime.PendingBatchID == "" && (len(payload.Runtime.Pending) != 0 || len(payload.Runtime.PendingMigrations) != 0) {
		return bizError(CodeCheckpointCorrupt, payload.SchemaVersion, "checkpoint.migration", "pending state without batch id")
	}
	// Layer rows are checked for non-negativity and uniqueness. Occupancy
	// consistency is validated by callers that know object sizes.
	layerIDs := map[string]bool{}
	for _, layer := range payload.Layers {
		if layerIDs[layer.ID] {
			return bizError(CodeCheckpointCorrupt, payload.SchemaVersion, "checkpoint.layer."+layer.ID, "duplicate layer")
		}
		layerIDs[layer.ID] = true
		if layer.Occupancy < 0 {
			return bizError(CodeCheckpointCorrupt, payload.SchemaVersion, "checkpoint.layer."+layer.ID, "negative occupancy")
		}
	}
	return nil
}

func (s *CheckpointStore) Discard() error {
	if err := os.Remove(s.Path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func mapSQLiteError(version, location string, err error) error {
	switch {
	case errors.Is(err, sqlitelite.ErrTruncated):
		return bizError(CodeCheckpointTruncated, version, location, "%v", err)
	case errors.Is(err, sqlitelite.ErrTooLarge):
		return bizError(CodeCheckpointTooLarge, version, location, "%v", err)
	case errors.Is(err, sqlitelite.ErrCorrupt):
		return bizError(CodeCheckpointCorrupt, version, location, "%v", err)
	default:
		return bizError(CodeCheckpointCorrupt, version, location, "%v", err)
	}
}

// RecoverCheckpoint loads a checkpoint and, on version mismatch, discards it
// and invokes rebuild. The mismatched E_xxx code is surfaced in DiscardedCode.
func RecoverCheckpoint(store *CheckpointStore, expectedVersion string, rebuild func() (*CheckpointPayload, error)) (*RecoveryResult, error) {
	state, err := store.Load(expectedVersion)
	if err == nil {
		return &RecoveryResult{State: state, Rebuilt: false}, nil
	}
	var biz *BizError
	if !errors.As(err, &biz) {
		return nil, err
	}
	if biz.Code != CodeCheckpointVersionMismatch {
		return nil, err
	}
	if err := store.Discard(); err != nil {
		return nil, err
	}
	rebuilt, err := rebuild()
	if err != nil {
		return nil, err
	}
	return &RecoveryResult{State: rebuilt, Rebuilt: true, DiscardedCode: biz.Code}, nil
}

type RecoveryResult struct {
	State         *CheckpointPayload `json:"state"`
	Rebuilt       bool               `json:"rebuilt"`
	DiscardedCode string             `json:"discardedCode,omitempty"`
}

func validateCheckpointAgainstProject(payload *CheckpointPayload, project *Project) error {
	if payload.ProjectKey != project.SemanticKey() {
		return bizError(CodeCheckpointVersionMismatch, payload.SchemaVersion, "checkpoint.projectKey", "checkpoint is for a different project")
	}
	objectSizes := map[string]int64{}
	for _, object := range project.Objects {
		objectSizes[object.ID] = object.SizeBytes
	}
	if len(payload.Objects) != len(objectSizes) {
		return bizError(CodeCheckpointIncomplete, payload.SchemaVersion, "checkpoint.objects", "object count mismatch")
	}
	occupancy := map[string]int64{}
	for _, object := range payload.Objects {
		size, ok := objectSizes[object.ID]
		if !ok {
			return bizError(CodeCheckpointCorrupt, payload.SchemaVersion, "checkpoint.object."+object.ID, "unknown object")
		}
		layerID, ok := payload.Runtime.CurrentLayer[object.ID]
		if !ok {
			return bizError(CodeCheckpointIncomplete, payload.SchemaVersion, "checkpoint.migration.currentLayer", "missing object")
		}
		knownLayer := false
		for _, layer := range project.Layers {
			if layer.ID == layerID {
				knownLayer = true
				break
			}
		}
		if !knownLayer {
			return bizError(CodeCheckpointCorrupt, payload.SchemaVersion, "checkpoint.object."+object.ID, "unknown layer")
		}
		occupancy[layerID] += size
	}
	persistedLayers := map[string]int64{}
	for _, layer := range payload.Layers {
		persistedLayers[layer.ID] = layer.Occupancy
	}
	for _, layer := range project.Layers {
		if persistedLayers[layer.ID] != occupancy[layer.ID] {
			return bizError(CodeCheckpointCorrupt, payload.SchemaVersion, "checkpoint.layer."+layer.ID, "occupancy mismatch")
		}
		if occupancy[layer.ID] > layer.CapacityBytes {
			return bizError(CodeCapacityExceeded, payload.SchemaVersion, "checkpoint.layer."+layer.ID, "occupancy exceeds capacity")
		}
	}
	return nil
}

// ResumeFromCheckpoint validates and constructs an engine positioned after the
// persisted logical instant. Only committed object ownership is used for
// capacity accounting.
func ResumeFromCheckpoint(project *Project, store *CheckpointStore, expectedVersion string) (*Engine, *CheckpointPayload, error) {
	payload, err := store.Load(expectedVersion)
	if err != nil {
		return nil, nil, err
	}
	if payload.ProjectKey != project.SemanticKey() {
		return nil, nil, bizError(CodeCheckpointVersionMismatch, payload.SchemaVersion, "checkpoint.projectKey", "checkpoint project fingerprint mismatch")
	}
	if err := validateCheckpointAgainstProject(payload, project); err != nil {
		return nil, nil, err
	}
	engine, err := NewEngine(project)
	if err != nil {
		return nil, nil, err
	}
	engine.lastRun = &RunResult{ProjectKey: project.SemanticKey(), LastCommittedTick: payload.CommittedSeq, LastCommittedBatch: payload.Runtime.LastCommittedBatch}
	return engine, payload, nil
}

func sortedPersistedObjects(objects []PersistedObject) []PersistedObject {
	out := append([]PersistedObject(nil), objects...)
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}
