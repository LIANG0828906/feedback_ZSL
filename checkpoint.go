package hckernel

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"sort"
)

type CheckpointInstanceState struct {
	Instance      Instance    `json:"instance"`
	Health        HealthState `json:"health"`
	SuccessStreak int         `json:"success_streak"`
	FailureStreak int         `json:"failure_streak"`
	NextPeriodAt  int64       `json:"next_period_at"`
	BackoffUntil  int64       `json:"backoff_until"`
	HalfOpenAt    int64       `json:"half_open_at"`
	RetryAttempts int         `json:"retry_attempts"`
	Outstanding   *Probe      `json:"outstanding,omitempty"`
	DeadlineMs    int64       `json:"deadline_ms,omitempty"`
	LastEventID   string      `json:"last_event_id,omitempty"`
}

// Checkpoint is the durable state. Instance states and queue are serialized
// before CommittedRound; the cursor record is the final commit point.
type Checkpoint struct {
	Format         string                    `json:"format"`
	ProjectID      string                    `json:"project_id"`
	ProjectVersion string                    `json:"project_version"`
	SchemaVersion  string                    `json:"schema_version"`
	Fingerprint    string                    `json:"fingerprint"`
	InstanceStates []CheckpointInstanceState `json:"instance_states"`
	Queue          []DeferredEntry           `json:"queue"`
	QueueSeq       int64                     `json:"queue_seq"`
	EventCursor    int                       `json:"event_cursor"`
	ObsSeq         int                       `json:"obs_seq"`
	CommittedRound int                       `json:"committed_round"`
	LogicalMs      int64                     `json:"logical_ms"`
}

const CheckpointFormat = "hckernel-checkpoint-v1"

type CommitOptions struct {
	// CrashBeforeCursor simulates SIGKILL after state+checksum are durable
	// but before the global round cursor is appended.
	CrashBeforeCursor bool
}

type checkpointEnvelope struct {
	Kind        string          `json:"kind"`
	Round       int             `json:"round"`
	SHA256      string          `json:"sha256,omitempty"`
	Payload     json.RawMessage `json:"payload,omitempty"`
	ProjectID   string          `json:"project_id,omitempty"`
	ProjectVer  string          `json:"project_version,omitempty"`
	SchemaVer   string          `json:"schema_version,omitempty"`
	Fingerprint string          `json:"fingerprint,omitempty"`
	StateSHA    string          `json:"state_sha256,omitempty"`
}

func (e *Engine) Snapshot(round int) Checkpoint {
	states := make([]CheckpointInstanceState, 0, len(e.states))
	for _, id := range e.sortedInstanceIDs() {
		rt := e.states[id]
		cs := CheckpointInstanceState{
			Instance:      rt.Instance,
			Health:        rt.Health,
			SuccessStreak: rt.SuccessStreak,
			FailureStreak: rt.FailureStreak,
			NextPeriodAt:  rt.NextPeriodAt,
			BackoffUntil:  rt.BackoffUntil,
			HalfOpenAt:    rt.HalfOpenAt,
			RetryAttempts: rt.RetryAttempts,
			LastEventID:   rt.LastEventID,
		}
		if rt.Outstanding != nil {
			probe := rt.Outstanding.Probe
			cs.Outstanding = &probe
			cs.DeadlineMs = rt.Outstanding.DeadlineMs
		}
		states = append(states, cs)
	}
	return Checkpoint{
		Format:         CheckpointFormat,
		ProjectID:      e.project.ProjectID,
		ProjectVersion: e.project.ProjectVersion,
		SchemaVersion:  e.project.SourceSchema,
		Fingerprint:    e.fingerprint,
		InstanceStates: states,
		Queue:          append([]DeferredEntry(nil), e.queue...),
		QueueSeq:       e.queueSeq,
		EventCursor:    e.eventCursor,
		ObsSeq:         e.obsSeq,
		CommittedRound: round,
		LogicalMs:      e.nowMs,
	}
}

// Commit appends the state record and then the authoritative cursor record.
func CommitCheckpoint(path string, cp Checkpoint, opts CommitOptions) error {
	if cp.Format == "" {
		cp.Format = CheckpointFormat
	}
	payload, err := json.Marshal(cp)
	if err != nil {
		return newError(ErrCheckpointWrite, "marshal checkpoint failed", withCause(err))
	}
	sum := sha256.Sum256(payload)
	stateSHA := hex.EncodeToString(sum[:])
	stateEnv := checkpointEnvelope{Kind: "state", Round: cp.CommittedRound, SHA256: stateSHA, Payload: payload}
	if err := appendEnvelope(path, stateEnv); err != nil {
		return err
	}
	if opts.CrashBeforeCursor {
		return newError(ErrInjectedCrash, "simulated kill after instance/queue state but before round cursor")
	}
	cursorEnv := checkpointEnvelope{
		Kind:        "cursor",
		Round:       cp.CommittedRound,
		ProjectID:   cp.ProjectID,
		ProjectVer:  cp.ProjectVersion,
		SchemaVer:   cp.SchemaVersion,
		Fingerprint: cp.Fingerprint,
		StateSHA:    stateSHA,
	}
	return appendEnvelope(path, cursorEnv)
}

func appendEnvelope(path string, env checkpointEnvelope) error {
	line, err := json.Marshal(env)
	if err != nil {
		return newError(ErrCheckpointWrite, "marshal envelope failed", withCause(err))
	}
	line = append(line, '\n')
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return newError(ErrCheckpointWrite, "open checkpoint failed", withPath(path), withCause(err))
	}
	defer f.Close()
	if _, err := f.Write(line); err != nil {
		return newError(ErrCheckpointWrite, "append checkpoint failed", withPath(path), withCause(err))
	}
	if err := f.Sync(); err != nil {
		return newError(ErrCheckpointWrite, "fsync checkpoint failed", withPath(path), withCause(err))
	}
	return nil
}

type LoadCheckpointOptions struct {
	ExpectedProjectVersion string
	ExpectedFingerprint    string
}

// LoadCheckpoint returns the last cursor-committed checkpoint. A complete state
// record without a cursor is treated as an aborted/unfinished commit.
func LoadCheckpoint(path string, opts LoadCheckpointOptions) (*Checkpoint, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, newError(ErrCheckpointCorrupt, "read checkpoint failed", withPath(path), withCause(err))
	}
	if len(data) == 0 {
		return nil, nil
	}
	if !bytes.HasSuffix(data, []byte{'\n'}) {
		return nil, newError(ErrCheckpointTruncated, "checkpoint ends without a complete record newline", withPath(path), withOffset(int64(len(data))))
	}

	states := map[string]Checkpoint{}
	var committed *Checkpoint
	lines := bytes.Split(data, []byte{'\n'})
	for lineIndex, line := range lines {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var env checkpointEnvelope
		if err := json.Unmarshal(line, &env); err != nil {
			code := ErrCheckpointCorrupt
			var syntaxErr *json.SyntaxError
			if errors.As(err, &syntaxErr) || errors.Is(err, io.ErrUnexpectedEOF) {
				code = ErrCheckpointTruncated
			}
			return nil, newError(code, "checkpoint record decode failed", withPath(path), withOffset(int64(lineIndex)), withCause(err))
		}
		switch env.Kind {
		case "state":
			sum := sha256.Sum256(env.Payload)
			if hex.EncodeToString(sum[:]) != env.SHA256 {
				return nil, newError(ErrCheckpointTruncated, "state checksum mismatch indicates torn/truncated write", withPath(path), withOffset(int64(lineIndex)))
			}
			var cp Checkpoint
			if err := json.Unmarshal(env.Payload, &cp); err != nil {
				return nil, newError(ErrCheckpointTruncated, "state payload truncated", withPath(path), withOffset(int64(lineIndex)), withCause(err))
			}
			states[env.SHA256] = cp
		case "cursor":
			cp, ok := states[env.StateSHA]
			if !ok {
				return nil, newError(ErrCheckpointCorrupt, "cursor references missing state record", withPath(path), withOffset(int64(lineIndex)))
			}
			if cp.CommittedRound != env.Round {
				return nil, newError(ErrCheckpointCorrupt, "cursor round does not match state round", withPath(path), withOffset(int64(lineIndex)))
			}
			committed = &cp
		default:
			return nil, newError(ErrCheckpointCorrupt, "unknown checkpoint record kind", withPath(path), withOffset(int64(lineIndex)))
		}
	}
	if committed == nil {
		return nil, nil
	}
	if opts.ExpectedFingerprint != "" && committed.Fingerprint != opts.ExpectedFingerprint {
		return nil, newError(ErrCheckpointVersionMatch, "checkpoint canonical fingerprint mismatch", withPath(path))
	}
	if opts.ExpectedProjectVersion != "" && committed.ProjectVersion != opts.ExpectedProjectVersion {
		return nil, newError(ErrCheckpointVersionMatch, "checkpoint project version mismatch", withPath(path))
	}
	return committed, nil
}

// RestoreEngine rebuilds deterministic runtime state from a committed snapshot.
func RestoreEngine(project *Project, events []Event, cp *Checkpoint) (*Engine, error) {
	e, err := NewEngine(project, events)
	if err != nil {
		return nil, err
	}
	if cp == nil {
		return e, nil
	}
	if cp.Format != CheckpointFormat {
		return nil, newError(ErrCheckpointCorrupt, "unsupported checkpoint format")
	}
	if cp.Fingerprint != "" && e.fingerprint != cp.Fingerprint {
		return nil, newError(ErrCheckpointVersionMatch, "snapshot fingerprint does not match project")
	}

	e.states = map[string]*InstanceState{}
	e.queue = append([]DeferredEntry(nil), cp.Queue...)
	e.queueSeq = cp.QueueSeq
	e.eventCursor = cp.EventCursor
	e.obsSeq = cp.ObsSeq
	e.nowMs = cp.LogicalMs
	e.rackActive = map[string]int{}
	e.globalActive = 0

	for _, cs := range cp.InstanceStates {
		rt := &InstanceState{
			Instance:      cs.Instance,
			Health:        cs.Health,
			SuccessStreak: cs.SuccessStreak,
			FailureStreak: cs.FailureStreak,
			NextPeriodAt:  cs.NextPeriodAt,
			BackoffUntil:  cs.BackoffUntil,
			HalfOpenAt:    cs.HalfOpenAt,
			RetryAttempts: cs.RetryAttempts,
			LastEventID:   cs.LastEventID,
		}
		if cs.Outstanding != nil {
			probe := *cs.Outstanding
			rt.Outstanding = &activeProbe{Probe: probe, DeadlineMs: cs.DeadlineMs}
			e.globalActive++
			e.rackActive[probe.RackID]++
		}
		e.states[cs.Instance.ID] = rt
	}
	for _, rack := range project.Racks {
		if _, ok := e.rackActive[rack.ID]; !ok {
			e.rackActive[rack.ID] = 0
		}
	}
	if e.eventCursor < 0 || e.eventCursor > len(e.eventOrder) {
		return nil, newError(ErrCheckpointCorrupt, "event cursor out of range")
	}
	return e, nil
}

// WriteTruncatedCheckpoint is a test/diagnostic helper for the required
// truncated-checkpoint exception injection.
func WriteTruncatedCheckpoint(path string) error {
	valid := `{"kind":"state","round":7,"sha256":"x","payload":{"committed_round":7}}` + "\n"
	partial := `{"kind":"cursor","round":7,"state_sha256":"x"`
	return os.WriteFile(path, []byte(valid+partial), 0o644)
}

func sortedCheckpointStates(states []CheckpointInstanceState) {
	sort.SliceStable(states, func(i, j int) bool { return states[i].Instance.ID < states[j].Instance.ID })
}
