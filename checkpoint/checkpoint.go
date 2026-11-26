package checkpoint

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"sync/atomic"

	"serviceregistry/internal/sqlitefile"
	"serviceregistry/registry"
)

type Identity struct {
	EngineVersion  string
	ProjectVersion string
	ModelHash      string
}

type Checkpoint struct {
	Snapshot registry.Snapshot
	Steps    []registry.StepResult
	Meta     Meta
}

type Meta struct {
	EngineVersion  string `json:"engine_version"`
	ProjectVersion string `json:"project_version"`
	ModelHash      string `json:"model_hash"`
	SchemaVersion  string `json:"schema_version"`
	CommitSeq      int64  `json:"commit_seq"`
	LastAtMS       int64  `json:"last_at_ms"`
	NextTick       int64  `json:"next_tick"`
	NextQueueSeq   int64  `json:"next_queue_seq"`
	Checksum       string `json:"checksum"`
}

type ResumeMode string

const (
	ResumeModeFromCheckpoint ResumeMode = "resume"
	ResumeModeFullRecompute  ResumeMode = "full_recompute"
)

type ResumeStatus struct {
	Mode   ResumeMode `json:"mode"`
	Code   string     `json:"code,omitempty"`
	Reason string     `json:"reason,omitempty"`
}

type FaultPoint string

const (
	FaultBeforeTempWrite FaultPoint = "before_temp_write"
	FaultAfterTempWrite  FaultPoint = "after_temp_write"
	FaultBeforeRename    FaultPoint = "before_rename"
	FaultAfterRename     FaultPoint = "after_rename"
)

type FaultHook func(point FaultPoint) error

type Store struct {
	Path  string
	Fault FaultHook
}

var tempCounter atomic.Uint64

func (s *Store) Save(checkpointValue *Checkpoint) error {
	if checkpointValue == nil {
		return registry.NewError(registry.CodeCheckpointIO, "checkpoint is nil", registry.ErrorContext{Checkpoint: s.Path})
	}
	data, err := encodeCheckpoint(checkpointValue)
	if err != nil {
		return err
	}
	if _, err := sqlitefile.Decode(data); err != nil {
		return registry.WrapError(registry.CodeInternal, "generated checkpoint is not a readable SQLite file", registry.ErrorContext{Checkpoint: s.Path}, err)
	}
	if dir := filepath.Dir(s.Path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return registry.WrapError(registry.CodeCheckpointIO, "cannot create checkpoint directory", registry.ErrorContext{Checkpoint: s.Path}, err)
		}
	}
	if s.Fault != nil {
		if err := s.Fault(FaultBeforeTempWrite); err != nil {
			return checkpointFaultError(s.Path, FaultBeforeTempWrite, err)
		}
	}
	tempPath := filepath.Join(filepath.Dir(s.Path), fmt.Sprintf(".%s.tmp-%d-%d", filepath.Base(s.Path), os.Getpid(), tempCounter.Add(1)))
	file, err := os.OpenFile(tempPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return registry.WrapError(registry.CodeCheckpointIO, "cannot create checkpoint temp file", registry.ErrorContext{Checkpoint: s.Path}, err)
	}
	cleanup := true
	defer func() {
		_ = file.Close()
		if cleanup {
			_ = os.Remove(tempPath)
		}
	}()
	if _, err := file.Write(data); err != nil {
		return registry.WrapError(registry.CodeCheckpointIO, "cannot write checkpoint temp file", registry.ErrorContext{Checkpoint: s.Path}, err)
	}
	if err := file.Sync(); err != nil {
		return registry.WrapError(registry.CodeCheckpointIO, "cannot fsync checkpoint temp file", registry.ErrorContext{Checkpoint: s.Path}, err)
	}
	if err := file.Close(); err != nil {
		return registry.WrapError(registry.CodeCheckpointIO, "cannot close checkpoint temp file", registry.ErrorContext{Checkpoint: s.Path}, err)
	}
	if s.Fault != nil {
		if err := s.Fault(FaultAfterTempWrite); err != nil {
			return checkpointFaultError(s.Path, FaultAfterTempWrite, err)
		}
		if err := s.Fault(FaultBeforeRename); err != nil {
			return checkpointFaultError(s.Path, FaultBeforeRename, err)
		}
	}
	if err := os.Rename(tempPath, s.Path); err != nil {
		return registry.WrapError(registry.CodeCheckpointIO, "cannot atomically replace checkpoint", registry.ErrorContext{Checkpoint: s.Path}, err)
	}
	cleanup = false
	if err := syncDirectory(filepath.Dir(s.Path)); err != nil {
		return registry.WrapError(registry.CodeCheckpointIO, "cannot fsync checkpoint directory", registry.ErrorContext{Checkpoint: s.Path}, err)
	}
	if s.Fault != nil {
		if err := s.Fault(FaultAfterRename); err != nil {
			return checkpointFaultError(s.Path, FaultAfterRename, err)
		}
	}
	return nil
}

func checkpointFaultError(path string, point FaultPoint, cause error) error {
	return registry.WrapError(registry.CodeCheckpointIO, "injected checkpoint fault at "+string(point), registry.ErrorContext{Checkpoint: path}, cause)
}

func syncDirectory(dir string) error {
	if dir == "" {
		dir = "."
	}
	handle, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer handle.Close()
	return handle.Sync()
}

func (s *Store) Load() (*Checkpoint, error) {
	data, err := os.ReadFile(s.Path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, registry.WrapError(registry.CodeCheckpointIO, "checkpoint does not exist", registry.ErrorContext{Checkpoint: s.Path}, err)
		}
		return nil, registry.WrapError(registry.CodeCheckpointIO, "cannot read checkpoint", registry.ErrorContext{Checkpoint: s.Path}, err)
	}
	database, err := sqlitefile.Decode(data)
	if err != nil {
		code := registry.CodeCheckpointCorrupt
		if errors.Is(err, sqlitefile.ErrTruncated) {
			code = registry.CodeCheckpointTruncated
		}
		return nil, registry.WrapError(code, "cannot decode SQLite checkpoint", registry.ErrorContext{Checkpoint: s.Path}, err)
	}
	value, err := decodeCheckpoint(database)
	if err != nil {
		if biz, ok := registry.AsError(err); ok {
			biz.Context.Checkpoint = s.Path
			return nil, biz
		}
		return nil, registry.WrapError(registry.CodeCheckpointCorrupt, "invalid checkpoint content", registry.ErrorContext{Checkpoint: s.Path}, err)
	}
	return value, nil
}

func (s *Store) LoadCompatible(identity Identity) (*Checkpoint, ResumeStatus, error) {
	value, err := s.Load()
	if err != nil {
		if biz, ok := registry.AsError(err); ok && biz.Code == registry.CodeCheckpointIO && errors.Is(err, fs.ErrNotExist) {
			return nil, ResumeStatus{Mode: ResumeModeFullRecompute, Reason: "no checkpoint file"}, nil
		}
		return nil, ResumeStatus{}, err
	}
	reasons := make([]string, 0, 3)
	if value.Meta.EngineVersion != identity.EngineVersion {
		reasons = append(reasons, "engine version differs")
	}
	if value.Meta.ProjectVersion != identity.ProjectVersion {
		reasons = append(reasons, "project version differs")
	}
	if value.Meta.ModelHash != identity.ModelHash {
		reasons = append(reasons, "normalized project hash differs")
	}
	if len(reasons) > 0 {
		return value, ResumeStatus{
			Mode:   ResumeModeFullRecompute,
			Code:   registry.CodeCheckpointVersion,
			Reason: stringsJoin(reasons, "; "),
		}, nil
	}
	return value, ResumeStatus{Mode: ResumeModeFromCheckpoint, Reason: "checkpoint is complete and version-matched"}, nil
}

func stringsJoin(values []string, separator string) string {
	result := ""
	for index, value := range values {
		if index > 0 {
			result += separator
		}
		result += value
	}
	return result
}

type checksumDocument struct {
	EngineVersion  string   `json:"engine_version"`
	ProjectVersion string   `json:"project_version"`
	ModelHash      string   `json:"model_hash"`
	SchemaVersion  string   `json:"schema_version"`
	CommitSeq      int64    `json:"commit_seq"`
	LastAtMS       int64    `json:"last_at_ms"`
	NextTick       int64    `json:"next_tick"`
	NextQueueSeq   int64    `json:"next_queue_seq"`
	Instances      []string `json:"instances"`
	Queue          []string `json:"queue"`
	InFlight       []string `json:"in_flight"`
	Steps          []string `json:"steps"`
}

func encodeCheckpoint(value *Checkpoint) ([]byte, error) {
	snapshot := value.Snapshot
	if snapshot.EngineVersion == "" {
		snapshot.EngineVersion = registry.EngineVersion
	}
	if snapshot.CommitSeq != len(value.Steps)-1 {
		return nil, registry.NewError(registry.CodeCheckpointCorrupt, "snapshot commit sequence does not match committed steps", registry.ErrorContext{})
	}
	if snapshot.NextTick != int64(snapshot.CommitSeq+1) {
		return nil, registry.NewError(registry.CodeCheckpointCorrupt, "snapshot next tick is not commit+1", registry.ErrorContext{})
	}
	if len(value.Steps) > 0 {
		last := value.Steps[len(value.Steps)-1]
		if int64(last.Seq) != int64(snapshot.CommitSeq) || last.AtMS != snapshot.LastAtMS {
			return nil, registry.NewError(registry.CodeCheckpointCorrupt, "last committed step does not match snapshot", registry.ErrorContext{})
		}
	}
	instanceRows := make([][]any, 0, len(snapshot.Instances))
	instanceJSON := make([]string, 0, len(snapshot.Instances))
	for ordinal, instance := range snapshot.Instances {
		data, err := json.Marshal(instance)
		if err != nil {
			return nil, registry.WrapError(registry.CodeInternal, "cannot encode instance state", registry.ErrorContext{InstanceID: instance.InstanceID}, err)
		}
		text := string(data)
		instanceJSON = append(instanceJSON, text)
		instanceRows = append(instanceRows, []any{int64(ordinal), instance.InstanceID, text})
	}
	queue := append([]registry.ProbeTaskSnapshot(nil), snapshot.Queue...)
	sort.SliceStable(queue, func(i, j int) bool { return queue[i].QueueSeq < queue[j].QueueSeq })
	queueRows := make([][]any, 0, len(queue))
	queueJSON := make([]string, 0, len(queue))
	for _, task := range queue {
		data, err := json.Marshal(task)
		if err != nil {
			return nil, registry.WrapError(registry.CodeInternal, "cannot encode deferred probe", registry.ErrorContext{InstanceID: task.InstanceID}, err)
		}
		text := string(data)
		queueJSON = append(queueJSON, text)
		queueRows = append(queueRows, []any{task.QueueSeq, task.ProbeID, text})
	}
	inFlightRows := make([][]any, 0, len(snapshot.InFlight))
	inFlightJSON := make([]string, 0, len(snapshot.InFlight))
	for _, task := range snapshot.InFlight {
		data, err := json.Marshal(task)
		if err != nil {
			return nil, registry.WrapError(registry.CodeInternal, "cannot encode in-flight probe", registry.ErrorContext{InstanceID: task.InstanceID}, err)
		}
		text := string(data)
		inFlightJSON = append(inFlightJSON, text)
		inFlightRows = append(inFlightRows, []any{task.InstanceID, task.ProbeID, text})
	}
	steps := append([]registry.StepResult(nil), value.Steps...)
	sort.SliceStable(steps, func(i, j int) bool { return steps[i].Seq < steps[j].Seq })
	stepRows := make([][]any, 0, len(steps))
	stepJSON := make([]string, 0, len(steps))
	for _, step := range steps {
		data, err := json.Marshal(step)
		if err != nil {
			return nil, registry.WrapError(registry.CodeInternal, "cannot encode committed step", registry.ErrorContext{Tick: int64Ptr(int64(step.Tick))}, err)
		}
		text := string(data)
		stepJSON = append(stepJSON, text)
		stepRows = append(stepRows, []any{int64(step.Seq), step.Tick, step.AtMS, text})
	}
	digestDocument := checksumDocument{
		EngineVersion:  snapshot.EngineVersion,
		ProjectVersion: snapshot.ProjectVersion,
		ModelHash:      snapshot.ModelHash,
		SchemaVersion:  string(snapshot.SchemaVersion),
		CommitSeq:      int64(snapshot.CommitSeq),
		LastAtMS:       snapshot.LastAtMS,
		NextTick:       snapshot.NextTick,
		NextQueueSeq:   snapshot.NextQueueSeq,
		Instances:      instanceJSON,
		Queue:          queueJSON,
		InFlight:       inFlightJSON,
		Steps:          stepJSON,
	}
	digestData, err := json.Marshal(digestDocument)
	if err != nil {
		return nil, registry.WrapError(registry.CodeInternal, "cannot encode checkpoint digest", registry.ErrorContext{}, err)
	}
	sum := sha256.Sum256(digestData)
	checksum := hex.EncodeToString(sum[:])
	metaRow := []any{
		int64(1),
		snapshot.EngineVersion,
		snapshot.ProjectVersion,
		snapshot.ModelHash,
		string(snapshot.SchemaVersion),
		int64(snapshot.CommitSeq),
		snapshot.LastAtMS,
		snapshot.NextTick,
		snapshot.NextQueueSeq,
		checksum,
		int64(1),
	}
	tables := []sqlitefile.Table{
		{
			Name: "instance_state",
			Columns: []sqlitefile.Column{
				{Name: "ordinal", Type: "INTEGER"},
				{Name: "instance_id", Type: "TEXT"},
				{Name: "state_json", Type: "TEXT"},
			},
			Rows: instanceRows,
		},
		{
			Name: "deferred_queue",
			Columns: []sqlitefile.Column{
				{Name: "queue_seq", Type: "INTEGER"},
				{Name: "probe_id", Type: "TEXT"},
				{Name: "task_json", Type: "TEXT"},
			},
			Rows: queueRows,
		},
		{
			Name: "inflight_probe",
			Columns: []sqlitefile.Column{
				{Name: "instance_id", Type: "TEXT"},
				{Name: "probe_id", Type: "TEXT"},
				{Name: "task_json", Type: "TEXT"},
			},
			Rows: inFlightRows,
		},
		{
			Name: "committed_steps",
			Columns: []sqlitefile.Column{
				{Name: "seq", Type: "INTEGER"},
				{Name: "tick", Type: "INTEGER"},
				{Name: "at_ms", Type: "INTEGER"},
				{Name: "result_json", Type: "TEXT"},
			},
			Rows: stepRows,
		},
		{
			Name: "checkpoint_meta",
			Columns: []sqlitefile.Column{
				{Name: "id", Type: "INTEGER"},
				{Name: "engine_version", Type: "TEXT"},
				{Name: "project_version", Type: "TEXT"},
				{Name: "model_hash", Type: "TEXT"},
				{Name: "schema_version", Type: "TEXT"},
				{Name: "commit_seq", Type: "INTEGER"},
				{Name: "last_at_ms", Type: "INTEGER"},
				{Name: "next_tick", Type: "INTEGER"},
				{Name: "next_queue_seq", Type: "INTEGER"},
				{Name: "checksum", Type: "TEXT"},
				{Name: "complete", Type: "INTEGER"},
			},
			Rows: [][]any{metaRow},
		},
	}
	return sqlitefile.Encode(tables)
}

func decodeCheckpoint(database *sqlitefile.Database) (*Checkpoint, error) {
	metaTable, err := requiredTable(database, "checkpoint_meta")
	if err != nil {
		return nil, err
	}
	metaRow, err := singleRow(metaTable)
	if err != nil {
		return nil, err
	}
	complete, ok := metaRow.Int64(metaTable.Column("complete"))
	if !ok || complete != 1 {
		return nil, registry.NewError(registry.CodeCheckpointCorrupt, "checkpoint commit marker is incomplete", registry.ErrorContext{})
	}
	meta := Meta{
		EngineVersion:  stringCell(metaTable, metaRow, "engine_version"),
		ProjectVersion: stringCell(metaTable, metaRow, "project_version"),
		ModelHash:      stringCell(metaTable, metaRow, "model_hash"),
		SchemaVersion:  stringCell(metaTable, metaRow, "schema_version"),
		CommitSeq:      int64Cell(metaTable, metaRow, "commit_seq"),
		LastAtMS:       int64Cell(metaTable, metaRow, "last_at_ms"),
		NextTick:       int64Cell(metaTable, metaRow, "next_tick"),
		NextQueueSeq:   int64Cell(metaTable, metaRow, "next_queue_seq"),
		Checksum:       stringCell(metaTable, metaRow, "checksum"),
	}
	instanceTable, err := requiredTable(database, "instance_state")
	if err != nil {
		return nil, err
	}
	queueTable, err := requiredTable(database, "deferred_queue")
	if err != nil {
		return nil, err
	}
	inFlightTable, err := requiredTable(database, "inflight_probe")
	if err != nil {
		return nil, err
	}
	stepsTable, err := requiredTable(database, "committed_steps")
	if err != nil {
		return nil, err
	}
	snapshot := registry.Snapshot{
		EngineVersion:  meta.EngineVersion,
		ProjectVersion: meta.ProjectVersion,
		ModelHash:      meta.ModelHash,
		SchemaVersion:  registry.SchemaVersion(meta.SchemaVersion),
		NextTick:       meta.NextTick,
		CommitSeq:      int(meta.CommitSeq),
		LastAtMS:       meta.LastAtMS,
		NextQueueSeq:   meta.NextQueueSeq,
		Instances:      []registry.RuntimeSnapshot{},
		Queue:          []registry.ProbeTaskSnapshot{},
		InFlight:       []registry.ProbeTaskSnapshot{},
	}
	instanceJSON := make([]string, 0, len(instanceTable.Rows))
	instances := append([]sqlitefile.Row(nil), instanceTable.Rows...)
	sort.SliceStable(instances, func(i, j int) bool {
		left, _ := instances[i].Int64(instanceTable.Column("ordinal"))
		right, _ := instances[j].Int64(instanceTable.Column("ordinal"))
		return left < right
	})
	for _, row := range instances {
		text, ok := row.String(instanceTable.Column("state_json"))
		if !ok {
			return nil, registry.NewError(registry.CodeCheckpointCorrupt, "instance state is not text", registry.ErrorContext{})
		}
		var instance registry.RuntimeSnapshot
		if err := json.Unmarshal([]byte(text), &instance); err != nil {
			return nil, registry.WrapError(registry.CodeCheckpointCorrupt, "cannot decode instance state", registry.ErrorContext{InstanceID: stringCell(instanceTable, row, "instance_id")}, err)
		}
		snapshot.Instances = append(snapshot.Instances, instance)
		instanceJSON = append(instanceJSON, text)
	}
	queueJSON := make([]string, 0, len(queueTable.Rows))
	queue := append([]sqlitefile.Row(nil), queueTable.Rows...)
	sort.SliceStable(queue, func(i, j int) bool {
		left, _ := queue[i].Int64(queueTable.Column("queue_seq"))
		right, _ := queue[j].Int64(queueTable.Column("queue_seq"))
		return left < right
	})
	for _, row := range queue {
		text, ok := row.String(queueTable.Column("task_json"))
		if !ok {
			return nil, registry.NewError(registry.CodeCheckpointCorrupt, "queued task is not text", registry.ErrorContext{})
		}
		var task registry.ProbeTaskSnapshot
		if err := json.Unmarshal([]byte(text), &task); err != nil {
			return nil, registry.WrapError(registry.CodeCheckpointCorrupt, "cannot decode queued task", registry.ErrorContext{InstanceID: stringCell(queueTable, row, "instance_id")}, err)
		}
		snapshot.Queue = append(snapshot.Queue, task)
		queueJSON = append(queueJSON, text)
	}
	inFlightJSON := make([]string, 0, len(inFlightTable.Rows))
	for _, row := range inFlightTable.Rows {
		text, ok := row.String(inFlightTable.Column("task_json"))
		if !ok {
			return nil, registry.NewError(registry.CodeCheckpointCorrupt, "in-flight task is not text", registry.ErrorContext{})
		}
		var task registry.ProbeTaskSnapshot
		if err := json.Unmarshal([]byte(text), &task); err != nil {
			return nil, registry.WrapError(registry.CodeCheckpointCorrupt, "cannot decode in-flight task", registry.ErrorContext{InstanceID: stringCell(inFlightTable, row, "instance_id")}, err)
		}
		snapshot.InFlight = append(snapshot.InFlight, task)
		inFlightJSON = append(inFlightJSON, text)
	}
	steps := append([]sqlitefile.Row(nil), stepsTable.Rows...)
	sort.SliceStable(steps, func(i, j int) bool {
		left, _ := steps[i].Int64(stepsTable.Column("seq"))
		right, _ := steps[j].Int64(stepsTable.Column("seq"))
		return left < right
	})
	stepJSON := make([]string, 0, len(steps))
	committed := make([]registry.StepResult, 0, len(steps))
	for _, row := range steps {
		text, ok := row.String(stepsTable.Column("result_json"))
		if !ok {
			return nil, registry.NewError(registry.CodeCheckpointCorrupt, "committed step is not text", registry.ErrorContext{})
		}
		var step registry.StepResult
		if err := json.Unmarshal([]byte(text), &step); err != nil {
			return nil, registry.WrapError(registry.CodeCheckpointCorrupt, "cannot decode committed step", registry.ErrorContext{}, err)
		}
		committed = append(committed, step)
		stepJSON = append(stepJSON, text)
	}
	digestDocument := checksumDocument{
		EngineVersion:  meta.EngineVersion,
		ProjectVersion: meta.ProjectVersion,
		ModelHash:      meta.ModelHash,
		SchemaVersion:  meta.SchemaVersion,
		CommitSeq:      meta.CommitSeq,
		LastAtMS:       meta.LastAtMS,
		NextTick:       meta.NextTick,
		NextQueueSeq:   meta.NextQueueSeq,
		Instances:      instanceJSON,
		Queue:          queueJSON,
		InFlight:       inFlightJSON,
		Steps:          stepJSON,
	}
	digestData, err := json.Marshal(digestDocument)
	if err != nil {
		return nil, registry.WrapError(registry.CodeInternal, "cannot encode checkpoint digest", registry.ErrorContext{}, err)
	}
	sum := sha256.Sum256(digestData)
	if hex.EncodeToString(sum[:]) != meta.Checksum {
		return nil, registry.NewError(registry.CodeCheckpointCorrupt, "checkpoint checksum mismatch", registry.ErrorContext{})
	}
	if meta.CommitSeq != int64(len(committed)-1) {
		return nil, registry.NewError(registry.CodeCheckpointCorrupt, "committed step count does not match commit sequence", registry.ErrorContext{})
	}
	if meta.NextTick != meta.CommitSeq+1 {
		return nil, registry.NewError(registry.CodeCheckpointCorrupt, "next tick does not follow commit sequence", registry.ErrorContext{})
	}
	if len(committed) > 0 {
		last := committed[len(committed)-1]
		if int64(last.Seq) != meta.CommitSeq || last.AtMS != meta.LastAtMS {
			return nil, registry.NewError(registry.CodeCheckpointCorrupt, "last step metadata mismatch", registry.ErrorContext{})
		}
	}
	return &Checkpoint{Snapshot: snapshot, Steps: committed, Meta: meta}, nil
}

func requiredTable(database *sqlitefile.Database, name string) (*sqlitefile.TableData, error) {
	table := database.Tables[name]
	if table == nil {
		return nil, registry.NewError(registry.CodeCheckpointCorrupt, "checkpoint table is missing", registry.ErrorContext{FieldName: name})
	}
	return table, nil
}

func singleRow(table *sqlitefile.TableData) (sqlitefile.Row, error) {
	if len(table.Rows) != 1 {
		return sqlitefile.Row{}, registry.NewError(registry.CodeCheckpointCorrupt, "checkpoint metadata must contain exactly one row", registry.ErrorContext{})
	}
	return table.Rows[0], nil
}

func stringCell(table *sqlitefile.TableData, row sqlitefile.Row, column string) string {
	value, _ := row.String(table.Column(column))
	return value
}

func int64Cell(table *sqlitefile.TableData, row sqlitefile.Row, column string) int64 {
	value, _ := row.Int64(table.Column(column))
	return value
}

func int64Ptr(value int64) *int64 {
	return &value
}
