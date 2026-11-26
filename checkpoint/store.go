// Package checkpoint provides the SQLite-equivalent durable semantics required
// by this repository. sqlite3 is not available from the Go standard library,
// so a single file ledger is replaced atomically under flock.
package checkpoint

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"syscall"

	"dialogueaudio/audioerr"
	"dialogueaudio/engine"
	"dialogueaudio/model"
)

const (
	ledgerFormat        = "dialogue-file-checkpoint"
	ledgerFormatVersion = 1
	ledgerFilename      = "checkpoint.json"
	lockFilename        = ".checkpoint.lock"
)

// Identity binds a checkpoint to one exact project revision and algorithm.
type Identity struct {
	ProjectID          string            `json:"project_id"`
	SourceVersion      int               `json:"source_version"`
	ProjectFingerprint string            `json:"project_fingerprint"`
	QuantVersion       string            `json:"quant_version"`
	ConversionVersion  string            `json:"conversion_version"`
	AlgorithmVersion   string            `json:"algorithm_version"`
	ClipIDs            []string          `json:"clip_ids"`
	ClipHashes         map[string]string `json:"clip_hashes"`
}

// IdentityFor creates the validation identity for a run.
func IdentityFor(project model.Project, quantVersion, conversionVersion, algorithmVersion string) Identity {
	clips := append([]model.Clip(nil), project.Clips...)
	sort.Slice(clips, func(i, j int) bool { return clips[i].ID < clips[j].ID })
	ids := make([]string, 0, len(clips))
	hashes := make(map[string]string, len(clips))
	for _, clip := range clips {
		ids = append(ids, clip.ID)
		hashes[clip.ID] = clip.ContentHash
	}
	return Identity{
		ProjectID: project.ProjectID, SourceVersion: project.SourceVersion,
		ProjectFingerprint: projectFingerprint(project),
		QuantVersion:       quantVersion, ConversionVersion: conversionVersion,
		AlgorithmVersion: algorithmVersion, ClipIDs: ids, ClipHashes: hashes,
	}
}

func projectFingerprint(project model.Project) string {
	clips := append([]model.Clip(nil), project.Clips...)
	sort.Slice(clips, func(i, j int) bool { return clips[i].ID < clips[j].ID })
	for i := range clips {
		clips[i].PCM = nil
	}
	payload := struct {
		ProjectID          string       `json:"project_id"`
		SampleRate         int          `json:"sample_rate"`
		FrameLen           int64        `json:"frame_len"`
		Hop                int64        `json:"hop"`
		Target             int64        `json:"target"`
		SessionGain        int64        `json:"session_gain"`
		MaxAdjustment      int64        `json:"max_adjustment"`
		CalibrationEnabled bool         `json:"calibration_enabled"`
		Clips              []model.Clip `json:"clips"`
	}{
		ProjectID: project.ProjectID, SampleRate: project.SampleRate,
		FrameLen: project.FrameLenSamples, Hop: project.HopSamples,
		Target: project.TargetLoudnessQ16, SessionGain: project.SessionGainQ16,
		MaxAdjustment: project.MaxAdjustmentDBQ16, CalibrationEnabled: project.CalibrationEnabled,
		Clips: clips,
	}
	data, _ := json.Marshal(payload)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// Record is one atomic clip commit.
type Record struct {
	Seq                 int                    `json:"seq"`
	ClipID              string                 `json:"clip_id"`
	EnvelopeDigest      string                 `json:"envelope_digest"`
	EnvelopeKey         string                 `json:"envelope_key"`
	CalibrationKey      string                 `json:"calibration_key"`
	ContentHash         string                 `json:"content_hash"`
	SourceVersion       int                    `json:"source_version"`
	ConversionVersion   string                 `json:"conversion_version"`
	QuantVersion        string                 `json:"quant_version"`
	AlgorithmVersion    string                 `json:"algorithm_version"`
	GainAdjustmentDBQ16 int64                  `json:"gain_adjustment_db_q16"`
	GainAdjustmentQ16   int64                  `json:"gain_adjustment_q16"`
	Envelope            engine.ClipEnvelope    `json:"envelope"`
	Calibration         engine.ClipCalibration `json:"calibration"`
	Checksum            string                 `json:"checksum"`
}

// CommitInput is the minimal data supplied by the coordinator.
type CommitInput struct {
	ClipID         string
	EnvelopeKey    string
	CalibrationKey string
	Envelope       engine.ClipEnvelope
	Calibration    engine.ClipCalibration
}

type ledger struct {
	Format        string   `json:"format"`
	FormatVersion int      `json:"format_version"`
	Identity      Identity `json:"identity"`
	Records       []Record `json:"records"`
	LedgerHash    string   `json:"ledger_hash"`
}

// RebuildRequiredError is returned for any stale or mismatched checkpoint.
type RebuildRequiredError struct {
	Reason string
}

func (e *RebuildRequiredError) Error() string {
	return "checkpoint rebuild required: " + e.Reason
}

// IsRebuildRequired reports the mandatory full-rebuild condition.
func IsRebuildRequired(err error) bool {
	var target *RebuildRequiredError
	return errors.As(err, &target)
}

// Store serializes commits to one ledger file.
type Store struct {
	dir      string
	identity Identity
	mu       sync.Mutex
}

// New creates a checkpoint handle. It does not write anything until Commit or
// Reset is called.
func New(dir string, identity Identity) (*Store, error) {
	if dir == "" {
		return nil, audioerr.New(audioerr.CodeCheckpoint, "checkpoint.dir", "directory is required")
	}
	if len(identity.ClipIDs) == 0 {
		return nil, audioerr.New(audioerr.CodeCheckpoint, "checkpoint.identity", "clip order is required")
	}
	return &Store{dir: dir, identity: cloneIdentity(identity)}, nil
}

func cloneIdentity(in Identity) Identity {
	out := in
	out.ClipIDs = append([]string(nil), in.ClipIDs...)
	out.ClipHashes = make(map[string]string, len(in.ClipHashes))
	for key, value := range in.ClipHashes {
		out.ClipHashes[key] = value
	}
	return out
}

// Dir returns the store directory.
func (s *Store) Dir() string { return s.dir }

// Path returns the ledger file path.
func (s *Store) Path() string { return filepath.Join(s.dir, ledgerFilename) }

// Load validates all committed records. A missing ledger is a valid empty
// checkpoint; any mismatch requires a whole-project rebuild.
func (s *Store) Load() ([]Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	unlock, err := s.lockFile()
	if err != nil {
		return nil, err
	}
	defer unlock()
	l, err := s.readLedger()
	if err != nil {
		return nil, err
	}
	if l == nil {
		return []Record{}, nil
	}
	if err := validateLedger(l, s.identity); err != nil {
		return nil, err
	}
	return append([]Record(nil), l.Records...), nil
}

// Commit appends one clip in one atomic file transaction. It refuses
// interleaving, out-of-order clips, and stale metadata.
func (s *Store) Commit(input CommitInput) (Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	unlock, err := s.lockFile()
	if err != nil {
		return Record{}, err
	}
	defer unlock()
	l, err := s.readLedger()
	if err != nil {
		return Record{}, err
	}
	if l == nil {
		l = s.emptyLedger()
		l.LedgerHash = ledgerChecksum(l)
	}
	if err := validateLedger(l, s.identity); err != nil {
		return Record{}, err
	}
	seq := len(l.Records) + 1
	if seq > len(s.identity.ClipIDs) {
		return Record{}, audioerr.New(audioerr.CodeCheckpoint, "checkpoint.seq", "all clips are already committed")
	}
	expected := s.identity.ClipIDs[seq-1]
	if input.ClipID != expected {
		return Record{}, audioerr.New(audioerr.CodeCheckpoint, "checkpoint.commit_order",
			fmt.Sprintf("expected clip %s at seq %d, got %s", expected, seq, input.ClipID))
	}
	if input.Envelope.ClipID != input.ClipID || input.Calibration.ClipID != input.ClipID {
		return Record{}, audioerr.Schema("checkpoint.clip_id", "envelope/calibration clip mismatch")
	}
	expectedHash := s.identity.ClipHashes[input.ClipID]
	if input.Envelope.ContentHash != expectedHash {
		return Record{}, &RebuildRequiredError{Reason: "content hash changed for " + input.ClipID}
	}
	record := Record{
		Seq: seq, ClipID: input.ClipID,
		EnvelopeDigest: digestEnvelope(input.Envelope),
		EnvelopeKey:    input.EnvelopeKey, CalibrationKey: input.CalibrationKey,
		ContentHash: expectedHash, SourceVersion: s.identity.SourceVersion,
		ConversionVersion: s.identity.ConversionVersion,
		QuantVersion:      s.identity.QuantVersion, AlgorithmVersion: s.identity.AlgorithmVersion,
		GainAdjustmentDBQ16: input.Calibration.AppliedAdjustmentDBQ16,
		GainAdjustmentQ16:   input.Calibration.AdjustmentGainQ16,
		Envelope:            input.Envelope, Calibration: input.Calibration,
	}
	record.Checksum = recordChecksum(record)
	l.Records = append(l.Records, record)
	l.LedgerHash = ledgerChecksum(l)
	if err := s.writeLedger(l); err != nil {
		return Record{}, err
	}
	return record, nil
}

// Reset atomically discards all records and writes the current identity.
func (s *Store) Reset() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	unlock, err := s.lockFile()
	if err != nil {
		return err
	}
	defer unlock()
	l := s.emptyLedger()
	l.LedgerHash = ledgerChecksum(l)
	return s.writeLedger(l)
}

func (s *Store) emptyLedger() *ledger {
	return &ledger{
		Format: ledgerFormat, FormatVersion: ledgerFormatVersion,
		Identity: cloneIdentity(s.identity), Records: []Record{},
	}
}

func (s *Store) readLedger() (*ledger, error) {
	data, err := os.ReadFile(s.Path())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, audioerr.New(audioerr.CodeIO, s.Path(), err.Error())
	}
	var l ledger
	if err := json.Unmarshal(data, &l); err != nil {
		return nil, &RebuildRequiredError{Reason: "unreadable ledger: " + err.Error()}
	}
	if l.Format != ledgerFormat || l.FormatVersion != ledgerFormatVersion {
		return nil, &RebuildRequiredError{Reason: "unsupported ledger format"}
	}
	return &l, nil
}

func (s *Store) writeLedger(l *ledger) error {
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return audioerr.New(audioerr.CodeIO, s.dir, err.Error())
	}
	data, err := json.MarshalIndent(l, "", "  ")
	if err != nil {
		return audioerr.New(audioerr.CodeCheckpoint, "checkpoint.encode", err.Error())
	}
	data = append(data, '\n')
	file, err := os.CreateTemp(s.dir, ledgerFilename+".tmp-*")
	if err != nil {
		return audioerr.New(audioerr.CodeIO, s.dir, err.Error())
	}
	temp := file.Name()
	cleanup := func() {
		_ = file.Close()
		_ = os.Remove(temp)
	}
	if _, err := file.Write(data); err != nil {
		cleanup()
		return audioerr.New(audioerr.CodeIO, temp, err.Error())
	}
	if err := file.Sync(); err != nil {
		cleanup()
		return audioerr.New(audioerr.CodeIO, temp, err.Error())
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(temp)
		return audioerr.New(audioerr.CodeIO, temp, err.Error())
	}
	if err := os.Rename(temp, s.Path()); err != nil {
		_ = os.Remove(temp)
		return audioerr.New(audioerr.CodeIO, s.Path(), err.Error())
	}
	if dir, err := os.Open(s.dir); err == nil {
		_ = dir.Sync()
		_ = dir.Close()
	}
	return nil
}

func (s *Store) lockFile() (func(), error) {
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return nil, audioerr.New(audioerr.CodeIO, s.dir, err.Error())
	}
	file, err := os.OpenFile(filepath.Join(s.dir, lockFilename), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, audioerr.New(audioerr.CodeIO, "checkpoint.lock", err.Error())
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX); err != nil {
		_ = file.Close()
		return nil, audioerr.New(audioerr.CodeIO, "checkpoint.lock", err.Error())
	}
	return func() {
		_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
		_ = file.Close()
	}, nil
}

func validateLedger(l *ledger, identity Identity) error {
	if l.LedgerHash != ledgerChecksum(l) {
		return &RebuildRequiredError{Reason: "ledger checksum mismatch"}
	}
	if err := validateIdentity(l.Identity, identity); err != nil {
		return err
	}
	for i := range l.Records {
		record := &l.Records[i]
		if record.Seq != i+1 {
			return &RebuildRequiredError{Reason: fmt.Sprintf("commit sequence gap at record %d", i+1)}
		}
		if record.ClipID != identity.ClipIDs[i] {
			return &RebuildRequiredError{Reason: fmt.Sprintf("commit order mismatch at seq %d", i+1)}
		}
		if record.Checksum != recordChecksum(*record) {
			return &RebuildRequiredError{Reason: "record checksum mismatch for " + record.ClipID}
		}
		if record.ContentHash != identity.ClipHashes[record.ClipID] {
			return &RebuildRequiredError{Reason: "content hash mismatch for " + record.ClipID}
		}
		if record.SourceVersion != identity.SourceVersion ||
			record.ConversionVersion != identity.ConversionVersion ||
			record.QuantVersion != identity.QuantVersion ||
			record.AlgorithmVersion != identity.AlgorithmVersion {
			return &RebuildRequiredError{Reason: "version mismatch for " + record.ClipID}
		}
		if record.EnvelopeDigest != digestEnvelope(record.Envelope) {
			return &RebuildRequiredError{Reason: "envelope digest mismatch for " + record.ClipID}
		}
	}
	return nil
}

func validateIdentity(stored, current Identity) error {
	if stored.ProjectID != current.ProjectID ||
		stored.SourceVersion != current.SourceVersion ||
		stored.ProjectFingerprint != current.ProjectFingerprint ||
		stored.QuantVersion != current.QuantVersion ||
		stored.ConversionVersion != current.ConversionVersion ||
		stored.AlgorithmVersion != current.AlgorithmVersion {
		return &RebuildRequiredError{Reason: "project/algorithm identity mismatch"}
	}
	if len(stored.ClipIDs) != len(current.ClipIDs) {
		return &RebuildRequiredError{Reason: "clip count mismatch"}
	}
	for i := range stored.ClipIDs {
		if stored.ClipIDs[i] != current.ClipIDs[i] {
			return &RebuildRequiredError{Reason: "clip order mismatch"}
		}
		if stored.ClipHashes[stored.ClipIDs[i]] != current.ClipHashes[current.ClipIDs[i]] {
			return &RebuildRequiredError{Reason: "clip content hash mismatch for " + stored.ClipIDs[i]}
		}
	}
	return nil
}

func digestEnvelope(env engine.ClipEnvelope) string {
	// Only deterministic measurement fields participate; diagnostic traces are
	// included because they are also deterministic integer records.
	data, _ := json.Marshal(env)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func recordChecksum(record Record) string {
	record.Checksum = ""
	data, _ := json.Marshal(record)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func ledgerChecksum(l *ledger) string {
	copy := *l
	copy.LedgerHash = ""
	data, _ := json.Marshal(copy)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}
