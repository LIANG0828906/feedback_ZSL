package checkpoint

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"sync"

	"proxydiag/bizerr"
	"proxydiag/domain"
)

type Decision struct {
	Seq          int    `json:"seq"`
	ListenerID   string `json:"listener_id"`
	RouteID      string `json:"route_id"`
	UpstreamID   string `json:"upstream_id"`
	RequestLine  string `json:"request_line"`
	Terminal     string `json:"terminal"`
	ResultDigest string `json:"result_digest"`
}

type HealthEntry struct {
	ID      string `json:"id"`
	Healthy bool   `json:"healthy"`
}

type State struct {
	Version          string                      `json:"version"`
	LastCommittedSeq int                         `json:"last_committed_seq"`
	Decisions        map[int]Decision            `json:"decisions"`
	ListenerCaches   map[string]map[int]Decision `json:"listener_caches"`
	UpstreamHealth   map[string]bool             `json:"upstream_health"`
}

func emptyState(version string) *State {
	return &State{
		Version: version, Decisions: map[int]Decision{},
		ListenerCaches: map[string]map[int]Decision{}, UpstreamHealth: map[string]bool{},
	}
}

type prepareRecord struct {
	Type           string        `json:"type"`
	TxID           string        `json:"txid"`
	Version        string        `json:"version"`
	Seq            int           `json:"seq"`
	ListenerID     string        `json:"listener_id"`
	Decision       Decision      `json:"decision"`
	UpstreamHealth []HealthEntry `json:"upstream_health"`
	Checksum       string        `json:"checksum"`
}

type commitRecord struct {
	Type            string `json:"type"`
	TxID            string `json:"txid"`
	Version         string `json:"version"`
	Seq             int    `json:"seq"`
	PrepareChecksum string `json:"prepare_checksum"`
	Checksum        string `json:"checksum"`
}

func payloadChecksum(v any) (string, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

func (p prepareRecord) checksum() (string, error) {
	p.Checksum = ""
	return payloadChecksum(p)
}

func (c commitRecord) checksum() (string, error) {
	c.Checksum = ""
	return payloadChecksum(c)
}

func NewStore(path, version string, lastCommittedSeq int) (*Store, error) {
	if version == "" {
		return nil, bizerr.New(bizerr.CheckpointVersion, "checkpoint store requires a project version")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, bizerr.Wrap(bizerr.IO, "create checkpoint directory", err).With("path", path)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, bizerr.Wrap(bizerr.IO, "open checkpoint journal", err).With("path", path)
	}
	return &Store{path: path, version: version, lastSeq: lastCommittedSeq, file: f}, nil
}

type Store struct {
	mu      sync.Mutex
	path    string
	version string
	lastSeq int
	file    *os.File
}

func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.file == nil {
		return nil
	}
	err := s.file.Close()
	s.file = nil
	return err
}

func (s *Store) LastCommittedSeq() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastSeq
}

func (s *Store) Commit(seq int, decision Decision, upstreams []domain.Upstream) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if seq <= s.lastSeq {
		return bizerr.New(bizerr.CheckpointSequence, "checkpoint sequence must advance").
			WithInt("seq", seq).WithInt("last_committed_seq", s.lastSeq)
	}
	if decision.Seq != seq {
		return bizerr.New(bizerr.CheckpointSequence, "decision sequence does not match commit sequence").
			WithInt("seq", seq).WithInt("decision_seq", decision.Seq)
	}
	prep := prepareRecord{
		Type: "prepare", Version: s.version, Seq: seq,
		ListenerID: decision.ListenerID, Decision: decision,
		UpstreamHealth: healthSnapshot(upstreams),
	}
	prep.TxID = transactionID(s.version, seq, decision)
	sum, err := prep.checksum()
	if err != nil {
		return bizerr.Wrap(bizerr.Internal, "checksum checkpoint prepare", err)
	}
	prep.Checksum = sum
	if err := s.append(prep); err != nil {
		return err
	}
	com := commitRecord{
		Type: "commit", TxID: prep.TxID, Version: s.version, Seq: seq,
		PrepareChecksum: prep.Checksum,
	}
	csum, err := com.checksum()
	if err != nil {
		return bizerr.Wrap(bizerr.Internal, "checksum checkpoint commit", err)
	}
	com.Checksum = csum
	if err := s.append(com); err != nil {
		return err
	}
	s.lastSeq = seq
	return nil
}

// commitPrepareOnly is used by kill-point tests and diagnostics. It persists
// listener/upstream state but deliberately never advances global sequence.
func (s *Store) commitPrepareOnly(seq int, decision Decision, upstreams []domain.Upstream) error {
	prep := prepareRecord{
		Type: "prepare", Version: s.version, Seq: seq, ListenerID: decision.ListenerID,
		Decision: decision, UpstreamHealth: healthSnapshot(upstreams),
	}
	prep.TxID = transactionID(s.version, seq, decision)
	sum, err := prep.checksum()
	if err != nil {
		return err
	}
	prep.Checksum = sum
	return s.append(prep)
}

// StageWithoutCommit writes only listener/upstream state. It is intended for
// crash diagnostics and never advances the global sequence.
func (s *Store) StageWithoutCommit(seq int, decision Decision, upstreams []domain.Upstream) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.commitPrepareOnly(seq, decision, upstreams)
}

func (s *Store) append(v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return bizerr.Wrap(bizerr.Internal, "marshal checkpoint record", err)
	}
	b = append(b, '\n')
	n, err := s.file.Write(b)
	if err != nil {
		return bizerr.Wrap(bizerr.IO, "append checkpoint record", err).With("path", s.path)
	}
	if n != len(b) {
		return bizerr.New(bizerr.IO, "short checkpoint write").With("path", s.path)
	}
	if err := s.file.Sync(); err != nil {
		return bizerr.Wrap(bizerr.IO, "fsync checkpoint record", err).With("path", s.path)
	}
	return nil
}

func healthSnapshot(upstreams []domain.Upstream) []HealthEntry {
	out := make([]HealthEntry, 0, len(upstreams))
	for _, u := range upstreams {
		out = append(out, HealthEntry{ID: u.ID, Healthy: u.Healthy})
	}
	return out
}

func transactionID(version string, seq int, d Decision) string {
	h := sha256.Sum256([]byte(fmt.Sprintf("%s|%d|%s|%s|%s|%s", version, seq,
		d.ListenerID, d.RouteID, d.UpstreamID, d.ResultDigest)))
	return hex.EncodeToString(h[:16])
}

// Recover accepts only a complete, checksum-valid, version-matching journal.
// On a truncation/checksum/order error, the returned state contains all
// decisions committed before the damaged record so prior data remains usable.
func Recover(path, expectedVersion string) (*State, error) {
	if expectedVersion == "" {
		return emptyState(""), bizerr.New(bizerr.CheckpointVersion,
			"checkpoint recovery requires an expected project version")
	}
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return emptyState(expectedVersion), nil
		}
		return emptyState(expectedVersion), bizerr.Wrap(bizerr.IO, "open checkpoint journal", err).With("path", path)
	}
	defer f.Close()
	state := emptyState(expectedVersion)
	reader := bufio.NewReader(f)
	pending := map[string]prepareRecord{}
	lineNo := 0
	foundVersion := ""
	for {
		line, readErr := reader.ReadBytes('\n')
		if len(line) > 0 {
			lineNo++
			if readErr == io.EOF {
				return state, bizerr.New(bizerr.CheckpointTruncated, "checkpoint tail is not newline-terminated").
					With("path", path).WithInt("line", lineNo)
			}
			if readErr != nil {
				return state, bizerr.Wrap(bizerr.IO, "read checkpoint journal", readErr).
					With("path", path).WithInt("line", lineNo)
			}
			line = line[:len(line)-1]
			if len(line) == 0 {
				return state, bizerr.New(bizerr.CheckpointTruncated, "empty checkpoint record").
					With("path", path).WithInt("line", lineNo)
			}
			var envelope struct {
				Type     string `json:"type"`
				Version  string `json:"version"`
				Checksum string `json:"checksum"`
			}
			if err := json.Unmarshal(line, &envelope); err != nil {
				return state, bizerr.Wrap(bizerr.CheckpointTruncated, "malformed checkpoint record", err).
					With("path", path).WithInt("line", lineNo)
			}
			if envelope.Version == "" {
				return state, bizerr.New(bizerr.CheckpointOrder, "checkpoint record has no version").
					With("path", path).WithInt("line", lineNo)
			}
			if foundVersion == "" {
				foundVersion = envelope.Version
			}
			if envelope.Version != foundVersion {
				return emptyState(foundVersion), bizerr.New(bizerr.CheckpointOrder, "checkpoint mixes project versions").
					With("path", path).WithInt("line", lineNo).
					With("expected", expectedVersion).With("found", envelope.Version)
			}
			if expectedVersion != "" && foundVersion != expectedVersion {
				return emptyState(foundVersion), bizerr.New(bizerr.CheckpointVersion,
					"checkpoint version does not match active snapshot").
					With("path", path).With("expected", expectedVersion).With("found", foundVersion)
			}
			switch envelope.Type {
			case "prepare":
				var prep prepareRecord
				if err := json.Unmarshal(line, &prep); err != nil {
					return state, bizerr.Wrap(bizerr.CheckpointTruncated, "malformed prepare record", err).
						With("path", path).WithInt("line", lineNo)
				}
				want, err := prep.checksum()
				if err != nil {
					return state, bizerr.Wrap(bizerr.Internal, "compute prepare checksum", err)
				}
				if prep.Checksum != want {
					return state, bizerr.New(bizerr.CheckpointChecksum, "prepare checksum mismatch").
						With("path", path).WithInt("line", lineNo).With("txid", prep.TxID)
				}
				pending[prep.TxID] = prep
			case "commit":
				var com commitRecord
				if err := json.Unmarshal(line, &com); err != nil {
					return state, bizerr.Wrap(bizerr.CheckpointTruncated, "malformed commit record", err).
						With("path", path).WithInt("line", lineNo)
				}
				want, err := com.checksum()
				if err != nil {
					return state, bizerr.Wrap(bizerr.Internal, "compute commit checksum", err)
				}
				if com.Checksum != want {
					return state, bizerr.New(bizerr.CheckpointChecksum, "commit checksum mismatch").
						With("path", path).WithInt("line", lineNo).With("txid", com.TxID)
				}
				prep, ok := pending[com.TxID]
				if !ok {
					return state, bizerr.New(bizerr.CheckpointOrder, "commit has no preceding prepare").
						With("path", path).WithInt("line", lineNo).With("txid", com.TxID)
				}
				if prep.Checksum != com.PrepareChecksum || prep.Seq != com.Seq || prep.Version != com.Version {
					return state, bizerr.New(bizerr.CheckpointOrder, "commit does not match prepare").
						With("path", path).WithInt("line", lineNo).With("txid", com.TxID)
				}
				if com.Seq <= state.LastCommittedSeq {
					return state, bizerr.New(bizerr.CheckpointSequence, "checkpoint sequence did not advance").
						With("path", path).WithInt("line", lineNo).
						WithInt("seq", com.Seq).WithInt("last_committed_seq", state.LastCommittedSeq)
				}
				applyPrepared(state, prep)
				delete(pending, com.TxID)
			default:
				return state, bizerr.New(bizerr.CheckpointOrder, "unknown checkpoint record type").
					With("path", path).WithInt("line", lineNo).With("type", envelope.Type)
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return state, bizerr.Wrap(bizerr.IO, "read checkpoint journal", readErr).With("path", path)
		}
	}
	// Complete prepare records without a commit are ignored: global sequence is
	// authoritative and must be advanced only by the following commit record.
	return state, nil
}

func applyPrepared(state *State, prep prepareRecord) {
	state.Version = prep.Version
	state.LastCommittedSeq = prep.Seq
	state.Decisions[prep.Seq] = prep.Decision
	if state.ListenerCaches[prep.ListenerID] == nil {
		state.ListenerCaches[prep.ListenerID] = map[int]Decision{}
	}
	state.ListenerCaches[prep.ListenerID][prep.Seq] = prep.Decision
	for _, h := range prep.UpstreamHealth {
		state.UpstreamHealth[h.ID] = h.Healthy
	}
}

func (s *State) Clone() *State {
	out := emptyState(s.Version)
	out.LastCommittedSeq = s.LastCommittedSeq
	for k, v := range s.Decisions {
		out.Decisions[k] = v
	}
	for listener, cache := range s.ListenerCaches {
		out.ListenerCaches[listener] = map[int]Decision{}
		for seq, d := range cache {
			out.ListenerCaches[listener][seq] = d
		}
	}
	for id, healthy := range s.UpstreamHealth {
		out.UpstreamHealth[id] = healthy
	}
	return out
}

func (s *State) SortedSeqs() []int {
	out := make([]int, 0, len(s.Decisions))
	for seq := range s.Decisions {
		out = append(out, seq)
	}
	sort.Ints(out)
	return out
}

func DigestDecision(d Decision) string {
	b, _ := json.Marshal(d)
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
