package checkpoint

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"

	"credrot/internal/bizerr"
	"credrot/internal/hashutil"
)

const (
	formatVersion = 1
	kindManifest  = "manifest"
	kindState     = "state"
	kindCommit    = "commit"
)

type CommittedSubscription struct {
	SubscriptionID string   `json:"subscription_id"`
	CredentialIDs  []string `json:"credential_ids"`
	Versions       []uint64 `json:"versions"`
	Source         string   `json:"source"`
}

type PendingCredential struct {
	CredentialID    string   `json:"credential_id"`
	SourceVersion   uint64   `json:"source_version"`
	TargetVersion   uint64   `json:"target_version"`
	ReloadBatch     int64    `json:"reload_batch"`
	SubscriptionIDs []string `json:"subscription_ids"`
}

type BatchProgress struct {
	Batch         int64    `json:"batch"`
	Planned       []string `json:"planned"`
	Actual        []string `json:"actual"`
	Failed        []string `json:"failed"`
	Attempts      int      `json:"attempts"`
	Confirmed     bool     `json:"confirmed"`
	CompletedTick int64    `json:"completed_tick"`
}

type State struct {
	ManifestVersion string                  `json:"manifest_version"`
	CommittedTick   int64                   `json:"committed_tick"`
	Subscriptions   []CommittedSubscription `json:"subscriptions"`
	Pending         []PendingCredential     `json:"pending_credentials"`
	Batches         []BatchProgress         `json:"batch_progress"`
}

type RecoveryInfo struct {
	Path                 string `json:"path"`
	RecordsRead          int    `json:"records_read"`
	CommitsSeen          int    `json:"commits_seen"`
	IgnoredDanglingState bool   `json:"ignored_dangling_state"`
	TruncatedTail        bool   `json:"truncated_tail"`
	TruncatedAtOffset    int64  `json:"truncated_at_offset,omitempty"`
	Repaired             bool   `json:"repaired"`
	Rebuilt              bool   `json:"rebuilt,omitempty"`
}

type OpenOptions struct {
	Strict bool
}

type Store struct {
	path     string
	manifest string
	file     *os.File
	nextSeq  uint64
}

type record struct {
	Seq      uint64          `json:"seq"`
	Kind     string          `json:"kind"`
	Payload  json.RawMessage `json:"payload"`
	Checksum string          `json:"checksum"`
}

type manifestPayload struct {
	FormatVersion   int    `json:"format_version"`
	ManifestVersion string `json:"manifest_version"`
}

type statePayload struct {
	ManifestVersion string                  `json:"manifest_version"`
	Subscriptions   []CommittedSubscription `json:"subscriptions"`
	Pending         []PendingCredential     `json:"pending_credentials"`
	Batches         []BatchProgress         `json:"batch_progress"`
}

type commitPayload struct {
	CommittedTick int64  `json:"committed_tick"`
	StateSeq      uint64 `json:"state_seq"`
	StateChecksum string `json:"state_checksum"`
}

func EmptyState(manifestVersion string) *State {
	return &State{
		ManifestVersion: manifestVersion,
		CommittedTick:   -1,
		Subscriptions:   []CommittedSubscription{},
		Pending:         []PendingCredential{},
		Batches:         []BatchProgress{},
	}
}

func Open(path, expectedManifest string, opts OpenOptions) (*Store, *State, RecoveryInfo, error) {
	info := RecoveryInfo{Path: path}
	state, lastSeq, trailing, err := recoverFile(path, expectedManifest, opts.Strict, &info)
	if err != nil {
		return nil, nil, info, err
	}
	if info.TruncatedTail && !opts.Strict && trailing > 0 {
		if err := os.Truncate(path, info.TruncatedAtOffset); err != nil {
			return nil, nil, info, bizerr.Wrap(bizerr.CodeCheckpointCorrupt, "cannot truncate partial checkpoint tail", err, map[string]any{
				"path":   path,
				"offset": info.TruncatedAtOffset,
			})
		}
		info.Repaired = true
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, nil, info, bizerr.Wrap(bizerr.CodeCheckpointCorrupt, "cannot open checkpoint for append", err, map[string]any{"path": path})
	}
	if lastSeq == 0 {
		store := &Store{path: path, manifest: expectedManifest, file: file, nextSeq: 1}
		if err := store.writeRecord(kindManifest, manifestPayload{FormatVersion: formatVersion, ManifestVersion: expectedManifest}); err != nil {
			file.Close()
			return nil, nil, info, err
		}
		return store, EmptyState(expectedManifest), info, nil
	}
	return &Store{path: path, manifest: expectedManifest, file: file, nextSeq: lastSeq + 1}, state, info, nil
}

func OpenOrRebuild(path, expectedManifest string, opts OpenOptions) (*Store, *State, RecoveryInfo, error) {
	store, state, info, err := Open(path, expectedManifest, opts)
	if err == nil {
		return store, state, info, nil
	}
	if !bizerr.IsCode(err, bizerr.CodeManifestMismatch) {
		return nil, nil, info, err
	}
	if store != nil {
		store.Close()
	}
	if removeErr := os.Remove(path); removeErr != nil && !os.IsNotExist(removeErr) {
		return nil, nil, info, bizerr.Wrap(bizerr.CodeCheckpointCorrupt, "cannot remove mismatched checkpoint", removeErr, map[string]any{"path": path})
	}
	store, state, rebuiltInfo, err := Open(path, expectedManifest, opts)
	if err != nil {
		return nil, nil, rebuiltInfo, err
	}
	rebuiltInfo.Rebuilt = true
	return store, state, rebuiltInfo, nil
}

func Verify(path, expectedManifest string) (RecoveryInfo, error) {
	store, _, info, err := Open(path, expectedManifest, OpenOptions{Strict: true})
	if store != nil {
		store.Close()
	}
	return info, err
}

func (s *Store) Close() error {
	if s == nil || s.file == nil {
		return nil
	}
	err := s.file.Close()
	s.file = nil
	return err
}

func (s *Store) Commit(state State) error {
	return s.commit(state, "")
}

type CrashPoint string

const (
	CrashAfterState   CrashPoint = "after_state"
	CrashDuringCommit CrashPoint = "during_commit"
	CrashAfterCommit  CrashPoint = "after_commit"
)

// CommitWithCrash is a deterministic fault-injection seam. The caller closes
// the store after returning, simulating an abrupt process stop.
func (s *Store) CommitWithCrash(state State, point CrashPoint) error {
	return s.commit(state, point)
}

func (s *Store) commit(state State, crash CrashPoint) error {
	if s == nil || s.file == nil {
		return bizerr.New(bizerr.CodeCheckpointCorrupt, "checkpoint store is closed", nil)
	}
	if state.ManifestVersion != s.manifest {
		return bizerr.New(bizerr.CodeManifestMismatch, "state manifest does not match checkpoint manifest", map[string]any{
			"expected": state.ManifestVersion,
			"actual":   s.manifest,
		})
	}
	payload, err := normalizeStatePayload(state)
	if err != nil {
		return err
	}
	stateBytes, err := json.Marshal(payload)
	if err != nil {
		return bizerr.Wrap(bizerr.CodeCheckpointCorrupt, "cannot encode checkpoint state", err, nil)
	}
	stateRec := record{
		Seq:      s.nextSeq,
		Kind:     kindState,
		Payload:  stateBytes,
		Checksum: recordChecksum(s.nextSeq, kindState, stateBytes),
	}
	if err := s.writeRecordObject(stateRec); err != nil {
		return err
	}
	if crash == CrashAfterState {
		return nil
	}
	commitBytes, err := json.Marshal(commitPayload{
		CommittedTick: state.CommittedTick,
		StateSeq:      stateRec.Seq,
		StateChecksum: stateRec.Checksum,
	})
	if err != nil {
		return bizerr.Wrap(bizerr.CodeCheckpointCorrupt, "cannot encode commit record", err, nil)
	}
	commitRec := record{
		Seq:      stateRec.Seq + 1,
		Kind:     kindCommit,
		Payload:  commitBytes,
		Checksum: recordChecksum(stateRec.Seq+1, kindCommit, commitBytes),
	}
	commitLine, err := json.Marshal(commitRec)
	if err != nil {
		return bizerr.Wrap(bizerr.CodeCheckpointCorrupt, "cannot encode commit envelope", err, nil)
	}
	if crash == CrashDuringCommit {
		half := len(commitLine) / 2
		if half < 8 {
			half = len(commitLine)
		}
		if _, err := s.file.Write(commitLine[:half]); err != nil {
			return bizerr.Wrap(bizerr.CodeCheckpointCorrupt, "cannot write partial commit", err, map[string]any{"path": s.path})
		}
		if err := s.file.Sync(); err != nil {
			return bizerr.Wrap(bizerr.CodeCheckpointCorrupt, "cannot sync partial commit", err, map[string]any{"path": s.path})
		}
		s.nextSeq = commitRec.Seq + 1
		return nil
	}
	if err := s.writeRecordObject(commitRec); err != nil {
		return err
	}
	return nil
}

func (s *Store) writeRecord(kind string, payload any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return bizerr.Wrap(bizerr.CodeCheckpointCorrupt, "cannot encode checkpoint payload", err, nil)
	}
	rec := record{
		Seq:      s.nextSeq,
		Kind:     kind,
		Payload:  data,
		Checksum: recordChecksum(s.nextSeq, kind, data),
	}
	return s.writeRecordObject(rec)
}

func (s *Store) writeRecordObject(rec record) error {
	line, err := json.Marshal(rec)
	if err != nil {
		return bizerr.Wrap(bizerr.CodeCheckpointCorrupt, "cannot encode checkpoint envelope", err, nil)
	}
	line = append(line, '\n')
	if _, err := s.file.Write(line); err != nil {
		return bizerr.Wrap(bizerr.CodeCheckpointCorrupt, "cannot append checkpoint record", err, map[string]any{
			"path": s.path,
			"seq":  rec.Seq,
		})
	}
	if err := s.file.Sync(); err != nil {
		return bizerr.Wrap(bizerr.CodeCheckpointCorrupt, "cannot sync checkpoint record", err, map[string]any{
			"path": s.path,
			"seq":  rec.Seq,
		})
	}
	s.nextSeq = rec.Seq + 1
	return nil
}

func recoverFile(path, expectedManifest string, strict bool, info *RecoveryInfo) (*State, uint64, int64, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, 0, 0, bizerr.Wrap(bizerr.CodeCheckpointCorrupt, "cannot create checkpoint directory", err, map[string]any{"path": path})
	}
	file, err := os.Open(path)
	if os.IsNotExist(err) {
		return EmptyState(expectedManifest), 0, 0, nil
	}
	if err != nil {
		return nil, 0, 0, bizerr.Wrap(bizerr.CodeCheckpointCorrupt, "cannot open checkpoint", err, map[string]any{"path": path})
	}
	defer file.Close()

	reader := bufio.NewReader(file)
	var (
		offset         int64
		lastSeq        uint64
		manifestSeen   bool
		actualManifest string
		lastState      *statePayload
		lastStateSeq   uint64
		lastStateSum   string
		committed      = EmptyState(expectedManifest)
		anyCommit      bool
	)
	for {
		lineStart := offset
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			offset += int64(len(line))
		}
		if err != nil {
			if len(line) > 0 && !bytes.HasSuffix(line, []byte("\n")) {
				info.TruncatedTail = true
				info.TruncatedAtOffset = lineStart
				if strict {
					return nil, lastSeq, offset, bizerr.New(bizerr.CodeCheckpointTruncated, "checkpoint has a truncated trailing record", map[string]any{
						"path":   path,
						"offset": lineStart,
					})
				}
				break
			}
			if err != nil && err.Error() != "EOF" {
				return nil, lastSeq, offset, bizerr.Wrap(bizerr.CodeCheckpointCorrupt, "cannot read checkpoint", err, map[string]any{"path": path})
			}
			break
		}
		info.RecordsRead++
		var rec record
		if err := json.Unmarshal(line, &rec); err != nil {
			return nil, lastSeq, offset, bizerr.New(bizerr.CodeCheckpointCorrupt, "checkpoint record is not valid JSON", map[string]any{
				"path":   path,
				"offset": lineStart,
			})
		}
		if rec.Seq <= lastSeq {
			return nil, lastSeq, offset, bizerr.New(bizerr.CodeCheckpointCorrupt, "checkpoint sequence is not increasing", map[string]any{
				"path":     path,
				"seq":      rec.Seq,
				"last_seq": lastSeq,
			})
		}
		if rec.Checksum != recordChecksum(rec.Seq, rec.Kind, rec.Payload) {
			return nil, lastSeq, offset, bizerr.New(bizerr.CodeCheckpointCorrupt, "checkpoint record checksum mismatch", map[string]any{
				"path":   path,
				"seq":    rec.Seq,
				"offset": lineStart,
			})
		}
		lastSeq = rec.Seq
		switch rec.Kind {
		case kindManifest:
			if manifestSeen {
				return nil, lastSeq, offset, bizerr.New(bizerr.CodeCheckpointCorrupt, "duplicate manifest record", map[string]any{"path": path})
			}
			var payload manifestPayload
			if err := json.Unmarshal(rec.Payload, &payload); err != nil {
				return nil, lastSeq, offset, bizerr.Wrap(bizerr.CodeCheckpointCorrupt, "invalid manifest payload", err, map[string]any{"path": path})
			}
			if payload.FormatVersion != formatVersion {
				return nil, lastSeq, offset, bizerr.New(bizerr.CodeCheckpointCorrupt, "unsupported checkpoint format", map[string]any{
					"format_version": payload.FormatVersion,
					"supported":      formatVersion,
				})
			}
			if payload.ManifestVersion != expectedManifest {
				return nil, lastSeq, offset, bizerr.New(bizerr.CodeManifestMismatch, "checkpoint manifest version does not match", map[string]any{
					"path":     path,
					"expected": expectedManifest,
					"actual":   payload.ManifestVersion,
				})
			}
			manifestSeen = true
			actualManifest = payload.ManifestVersion
		case kindState:
			if !manifestSeen {
				return nil, lastSeq, offset, bizerr.New(bizerr.CodeCheckpointCorrupt, "state record precedes manifest", map[string]any{"path": path})
			}
			var payload statePayload
			if err := json.Unmarshal(rec.Payload, &payload); err != nil {
				return nil, lastSeq, offset, bizerr.Wrap(bizerr.CodeCheckpointCorrupt, "invalid state payload", err, map[string]any{"path": path})
			}
			if payload.ManifestVersion != expectedManifest {
				return nil, lastSeq, offset, bizerr.New(bizerr.CodeManifestMismatch, "state manifest version does not match", map[string]any{
					"expected": expectedManifest,
					"actual":   payload.ManifestVersion,
				})
			}
			lastState = &payload
			lastStateSeq = rec.Seq
			lastStateSum = rec.Checksum
		case kindCommit:
			if lastState == nil {
				return nil, lastSeq, offset, bizerr.New(bizerr.CodeCheckpointCorrupt, "commit record has no preceding state", map[string]any{
					"path": path,
					"seq":  rec.Seq,
				})
			}
			var payload commitPayload
			if err := json.Unmarshal(rec.Payload, &payload); err != nil {
				return nil, lastSeq, offset, bizerr.Wrap(bizerr.CodeCheckpointCorrupt, "invalid commit payload", err, map[string]any{"path": path})
			}
			if payload.StateSeq != lastStateSeq || payload.StateChecksum != lastStateSum {
				return nil, lastSeq, offset, bizerr.New(bizerr.CodeCheckpointCorrupt, "commit does not reference the latest state", map[string]any{
					"path":         path,
					"commit_seq":   rec.Seq,
					"state_seq":    payload.StateSeq,
					"latest_state": lastStateSeq,
				})
			}
			committed = &State{
				ManifestVersion: actualManifest,
				CommittedTick:   payload.CommittedTick,
				Subscriptions:   append([]CommittedSubscription(nil), lastState.Subscriptions...),
				Pending:         append([]PendingCredential(nil), lastState.Pending...),
				Batches:         append([]BatchProgress(nil), lastState.Batches...),
			}
			anyCommit = true
			info.CommitsSeen++
			lastState = nil
			lastStateSeq = 0
			lastStateSum = ""
		default:
			return nil, lastSeq, offset, bizerr.New(bizerr.CodeCheckpointCorrupt, "unknown checkpoint record kind", map[string]any{
				"kind": rec.Kind,
				"seq":  rec.Seq,
			})
		}
	}
	if lastState != nil {
		info.IgnoredDanglingState = true
	}
	if !manifestSeen && lastSeq > 0 {
		return nil, lastSeq, offset, bizerr.New(bizerr.CodeCheckpointCorrupt, "checkpoint has no manifest", map[string]any{"path": path})
	}
	if !anyCommit {
		return EmptyState(expectedManifest), lastSeq, offset, nil
	}
	return committed, lastSeq, offset, nil
}

func normalizeStatePayload(state State) (statePayload, error) {
	out := statePayload{
		ManifestVersion: state.ManifestVersion,
		Subscriptions:   append([]CommittedSubscription(nil), state.Subscriptions...),
		Pending:         append([]PendingCredential(nil), state.Pending...),
		Batches:         append([]BatchProgress(nil), state.Batches...),
	}
	for i := range out.Subscriptions {
		if len(out.Subscriptions[i].CredentialIDs) != len(out.Subscriptions[i].Versions) {
			return statePayload{}, bizerr.New(bizerr.CodeCheckpointCorrupt, "credential ids and versions have different lengths", map[string]any{
				"subscription_id": out.Subscriptions[i].SubscriptionID,
				"ids":             len(out.Subscriptions[i].CredentialIDs),
				"versions":        len(out.Subscriptions[i].Versions),
			})
		}
		type idVersion struct {
			id      string
			version uint64
		}
		pairs := make([]idVersion, len(out.Subscriptions[i].CredentialIDs))
		for j := range pairs {
			pairs[j] = idVersion{id: out.Subscriptions[i].CredentialIDs[j], version: out.Subscriptions[i].Versions[j]}
		}
		sort.Slice(pairs, func(a, b int) bool { return pairs[a].id < pairs[b].id })
		ids := make([]string, len(pairs))
		versions := make([]uint64, len(pairs))
		for j, pair := range pairs {
			ids[j] = pair.id
			versions[j] = pair.version
		}
		out.Subscriptions[i].CredentialIDs = ids
		out.Subscriptions[i].Versions = versions
	}
	sort.Slice(out.Subscriptions, func(i, j int) bool {
		return out.Subscriptions[i].SubscriptionID < out.Subscriptions[j].SubscriptionID
	})
	for i := range out.Pending {
		out.Pending[i].SubscriptionIDs = append([]string(nil), out.Pending[i].SubscriptionIDs...)
		sort.Strings(out.Pending[i].SubscriptionIDs)
	}
	sort.Slice(out.Pending, func(i, j int) bool {
		if out.Pending[i].ReloadBatch != out.Pending[j].ReloadBatch {
			return out.Pending[i].ReloadBatch < out.Pending[j].ReloadBatch
		}
		return out.Pending[i].CredentialID < out.Pending[j].CredentialID
	})
	for i := range out.Batches {
		out.Batches[i].Planned = append([]string(nil), out.Batches[i].Planned...)
		out.Batches[i].Actual = append([]string(nil), out.Batches[i].Actual...)
		out.Batches[i].Failed = append([]string(nil), out.Batches[i].Failed...)
		sort.Strings(out.Batches[i].Planned)
		sort.Strings(out.Batches[i].Actual)
		sort.Strings(out.Batches[i].Failed)
	}
	sort.Slice(out.Batches, func(i, j int) bool { return out.Batches[i].Batch < out.Batches[j].Batch })
	return out, nil
}

func recordChecksum(seq uint64, kind string, payload []byte) string {
	h := hashutil.SHA256([]byte(strconv.FormatUint(seq, 10) + "|" + kind + "|" + string(payload)))
	return h
}

func Describe(state *State) string {
	if state == nil {
		return "<nil>"
	}
	return fmt.Sprintf("manifest=%s tick=%d subs=%d pending=%d batches=%d",
		state.ManifestVersion, state.CommittedTick, len(state.Subscriptions), len(state.Pending), len(state.Batches))
}

func ValidateStrict(path, expectedManifest string) error {
	_, err := Verify(path, expectedManifest)
	return err
}
