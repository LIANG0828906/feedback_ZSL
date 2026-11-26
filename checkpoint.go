package leasekit

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
)

const CodeCheckpointIncomplete = "E_CHECKPOINT_INCOMPLETE"

type HolderCheckpoint struct {
	HolderID    string `json:"holderId"`
	ResourceID  string `json:"resourceId"`
	RenewCount  int    `json:"renewCount"`
	ExpiresAtMS Milli  `json:"expiresAtMs"`
	LastSeq     int    `json:"lastSeq"`
}

type Checkpoint struct {
	Version       string             `json:"version"`
	SchemaVersion string             `json:"schemaVersion"`
	ProjectID     string             `json:"projectId"`
	CommittedSeq  int                `json:"committedSeq"`
	Holders       []HolderCheckpoint `json:"holders"`
	Checksum      string             `json:"checksum,omitempty"`
}

type CheckpointStore interface {
	WriteHolderStates(cp Checkpoint) error
	WriteCommittedSeq(cp Checkpoint) error
	LoadCheckpoint() (*Checkpoint, error)
	Location() string
}

func BuildCheckpoint(p *Project, throughSeq int) (*Checkpoint, error) {
	if throughSeq <= 0 {
		throughSeq = maxSeq(p)
	}
	clone := p.Clone()
	filtered := clone.Operations[:0]
	for _, op := range clone.Operations {
		if op.Seq <= throughSeq {
			filtered = append(filtered, op)
		}
	}
	clone.Operations = filtered
	report, err := Run(clone)
	if err != nil {
		return nil, err
	}
	cp := &Checkpoint{
		Version:       CheckpointVersion,
		SchemaVersion: CanonicalSchemaVersion,
		ProjectID:     p.ProjectID,
		CommittedSeq:  throughSeq,
	}
	for _, st := range report.HolderStates {
		cp.Holders = append(cp.Holders, HolderCheckpoint{
			HolderID: st.HolderID, ResourceID: st.ResourceID, RenewCount: st.RenewCount,
			ExpiresAtMS: st.ExpiresAtMS, LastSeq: st.LastSeq,
		})
	}
	sort.Slice(cp.Holders, func(i, j int) bool { return cp.Holders[i].HolderID < cp.Holders[j].HolderID })
	cp.Checksum = cp.computeChecksum()
	return cp, nil
}

func maxSeq(p *Project) int {
	max := 0
	for _, op := range p.Operations {
		if op.Seq > max {
			max = op.Seq
		}
	}
	return max
}

func (cp Checkpoint) computeChecksum() string {
	checksum := cp.Checksum
	cp.Checksum = ""
	b, _ := json.Marshal(cp)
	sum := sha256.Sum256(b)
	_ = checksum
	return hex.EncodeToString(sum[:])
}

func (cp Checkpoint) verifyChecksum() error {
	want := cp.Checksum
	got := cp.computeChecksum()
	if want == "" || want != got {
		return validationError(CodeCheckpointCorrupt, "checkpoint.checksum", "expected %s, computed %s", want, got)
	}
	return nil
}

func CommitCheckpoint(store CheckpointStore, cp *Checkpoint) error {
	if cp == nil {
		return validationError(CodeCheckpointCorrupt, "checkpoint", "nil checkpoint")
	}
	cp.Version = CheckpointVersion
	cp.SchemaVersion = CanonicalSchemaVersion
	cp.Checksum = cp.computeChecksum()
	// Commit ordering is deliberate: durable holder renewal state hits disk
	// before the global committed sequence moves. A crash between the two is
	// detected as a future/staged holder generation and recomputed.
	if err := store.WriteHolderStates(*cp); err != nil {
		return err
	}
	return store.WriteCommittedSeq(*cp)
}

type FileCheckpointStore struct {
	Dir string
}

func NewFileCheckpointStore(dir string) *FileCheckpointStore {
	return &FileCheckpointStore{Dir: dir}
}

func (s *FileCheckpointStore) Location() string { return s.Dir }

func (s *FileCheckpointStore) holderPath() string {
	return filepath.Join(s.Dir, "holder-states.json")
}

func (s *FileCheckpointStore) commitPath() string {
	return filepath.Join(s.Dir, "commit-seq.json")
}

func (s *FileCheckpointStore) WriteHolderStates(cp Checkpoint) error {
	if err := os.MkdirAll(s.Dir, 0o755); err != nil {
		return err
	}
	body, err := json.MarshalIndent(cp, "", "  ")
	if err != nil {
		return err
	}
	return atomicWriteFile(s.holderPath(), append(body, '\n'))
}

func (s *FileCheckpointStore) WriteCommittedSeq(cp Checkpoint) error {
	if err := os.MkdirAll(s.Dir, 0o755); err != nil {
		return err
	}
	commit := struct {
		Version       string `json:"version"`
		SchemaVersion string `json:"schemaVersion"`
		ProjectID     string `json:"projectId"`
		CommittedSeq  int    `json:"committedSeq"`
		Checksum      string `json:"checksum"`
	}{cp.Version, cp.SchemaVersion, cp.ProjectID, cp.CommittedSeq, cp.Checksum}
	body, err := json.MarshalIndent(commit, "", "  ")
	if err != nil {
		return err
	}
	return atomicWriteFile(s.commitPath(), append(body, '\n'))
}

func (s *FileCheckpointStore) LoadCheckpoint() (*Checkpoint, error) {
	commitBody, err := os.ReadFile(s.commitPath())
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var commit Checkpoint
	if err := json.Unmarshal(commitBody, &commit); err != nil {
		return nil, classifyJSONError(err, s.commitPath())
	}
	if commit.Version != CheckpointVersion || commit.SchemaVersion != CanonicalSchemaVersion {
		return nil, validationError(CodeSchemaVersionMismatch, s.commitPath(), "checkpoint=%s/%s want=%s/%s", commit.Version, commit.SchemaVersion, CheckpointVersion, CanonicalSchemaVersion)
	}

	holderBody, err := os.ReadFile(s.holderPath())
	if errors.Is(err, fs.ErrNotExist) {
		return nil, validationError(CodeCheckpointTruncated, s.holderPath(), "committed sequence exists but holder state is missing")
	}
	if err != nil {
		return nil, err
	}
	var holders Checkpoint
	if err := json.Unmarshal(holderBody, &holders); err != nil {
		return nil, classifyJSONError(err, s.holderPath())
	}
	if holders.CommittedSeq > commit.CommittedSeq {
		return nil, validationError(CodeCheckpointIncomplete, s.holderPath(), "durable holder state seq=%d is ahead of global committed seq=%d; refusing uncommitted renewal", holders.CommittedSeq, commit.CommittedSeq)
	}
	if holders.CommittedSeq != commit.CommittedSeq {
		return nil, validationError(CodeCheckpointTruncated, s.holderPath(), "holder seq=%d does not match commit seq=%d", holders.CommittedSeq, commit.CommittedSeq)
	}
	if err := holders.verifyChecksum(); err != nil {
		return nil, err
	}
	holders.Version = commit.Version
	holders.SchemaVersion = commit.SchemaVersion
	return &holders, nil
}

func classifyJSONError(err error, path string) error {
	var syntax *json.SyntaxError
	var typeErr *json.UnmarshalTypeError
	switch {
	case errors.Is(err, io.ErrUnexpectedEOF):
		return validationError(CodeCheckpointTruncated, path, "truncated JSON: %v", err)
	case errors.As(err, &syntax):
		return validationError(CodeCheckpointTruncated, path, "malformed/truncated JSON at byte %d: %v", syntax.Offset, err)
	case errors.As(err, &typeErr):
		return validationError(CodeCheckpointCorrupt, path, "wrong JSON type: %v", err)
	default:
		return validationError(CodeCheckpointCorrupt, path, "unreadable checkpoint: %v", err)
	}
}

func atomicWriteFile(path string, body []byte) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	if _, err := f.Write(body); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	if dir, err := os.Open(filepath.Dir(path)); err == nil {
		_ = dir.Sync()
		_ = dir.Close()
	}
	return nil
}

type Recovery struct {
	Checkpoint *Checkpoint `json:"checkpoint,omitempty"`
	Report     *Report     `json:"report,omitempty"`
	Recomputed bool        `json:"recomputed"`
	ReasonCode string      `json:"reasonCode,omitempty"`
	Reason     string      `json:"reason,omitempty"`
}

func RecoverOrRecompute(p *Project, store CheckpointStore) (*Recovery, error) {
	cp, err := store.LoadCheckpoint()
	if err == nil && cp != nil {
		return &Recovery{Checkpoint: cp, ReasonCode: CodeOK, Reason: "version-matched checkpoint accepted"}, nil
	}
	code := CodeCheckpointCorrupt
	reason := "checkpoint unavailable; full recompute"
	if biz, ok := AsError(err); ok {
		code = biz.Code
		reason = biz.Error()
	} else if err != nil {
		reason = err.Error()
	}
	report, runErr := Run(p)
	if runErr != nil {
		return nil, runErr
	}
	return &Recovery{Report: report, Recomputed: true, ReasonCode: code, Reason: reason}, nil
}

func (r *Recovery) Repair(p *Project, store CheckpointStore) error {
	if r == nil || !r.Recomputed {
		return nil
	}
	cp, err := BuildCheckpoint(p, maxSeq(p))
	if err != nil {
		return err
	}
	return CommitCheckpoint(store, cp)
}
