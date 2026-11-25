package checkpoint

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	_ "modernc.org/sqlite"

	"github.com/liang/hapticfeedback/internal/haptic/errs"
	"github.com/liang/hapticfeedback/internal/haptic/model"
)

type Meta struct {
	RuleVersion      string `json:"rule_version"`
	AlgorithmVersion string `json:"algorithm_version"`
	Fingerprint      string `json:"semantic_fingerprint"`
	StreamSeed       int64  `json:"stream_seed"`
	CommittedPackets int    `json:"committed_packets"`
}

type Store struct {
	db      *sql.DB
	path    string
	writeMu sync.Mutex
}

func Open(path string) (*Store, error) {
	if dir := filepath.Dir(path); dir != "." && dir != "" {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	if _, err := db.Exec(`PRAGMA busy_timeout=5000; PRAGMA journal_mode=DELETE; PRAGMA synchronous=FULL;`); err != nil {
		db.Close()
		return nil, err
	}
	s := &Store{db: db, path: path}
	if err := s.init(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) init() error {
	_, err := s.db.Exec(`
CREATE TABLE IF NOT EXISTS meta (
  key TEXT PRIMARY KEY,
  value TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS packets (
  global_seq INTEGER PRIMARY KEY,
  channel TEXT NOT NULL,
  packet_index INTEGER NOT NULL,
  start_sample INTEGER NOT NULL,
  end_sample INTEGER NOT NULL,
  sample_count INTEGER NOT NULL,
  timestamp_ms INTEGER NOT NULL,
  window_start_ms INTEGER NOT NULL,
  window_end_ms INTEGER NOT NULL,
  frames_json TEXT NOT NULL,
  frame_digest TEXT NOT NULL,
  boundary_json TEXT NOT NULL,
  consumed_event_offset INTEGER NOT NULL,
  event_count INTEGER NOT NULL,
  effective_count INTEGER NOT NULL,
  stream_seed INTEGER NOT NULL,
  rng_progress INTEGER NOT NULL,
  fingerprint_delta TEXT NOT NULL,
  semantic_fingerprint TEXT NOT NULL,
  content_hash TEXT NOT NULL,
  rule_version TEXT NOT NULL,
  algorithm_version TEXT NOT NULL,
  branch_sources_json TEXT NOT NULL,
  source_branch INTEGER NOT NULL,
  rule_snapshot_json TEXT NOT NULL,
  decisions_json TEXT NOT NULL,
  events_json TEXT NOT NULL,
  UNIQUE(channel, packet_index)
);
`)
	return err
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) Reset() error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM packets`); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM meta`); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) Path() string { return s.path }

func (s *Store) CommitPacket(pkt model.Packet, meta Meta) error {
	framesJSON, err := json.Marshal(pkt.Frames)
	if err != nil {
		return err
	}
	boundaryJSON, _ := json.Marshal(map[string]any{
		"channel": pkt.Channel, "packet_index": pkt.PacketIndex,
		"start_sample": pkt.StartSample, "end_sample": pkt.EndSample, "sample_count": pkt.SampleCount,
		"timestamp_ms": pkt.TimestampMS, "window_start_ms": pkt.WindowStartMS, "window_end_ms": pkt.WindowEndMS,
	})
	branchesJSON, _ := json.Marshal(pkt.BranchSources)
	rulesJSON, _ := json.Marshal(pkt.RuleSnapshot)
	decisionsJSON, _ := json.Marshal(pkt.Decisions)
	eventsJSON, _ := json.Marshal(pkt.Events)

	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	var nextSeq int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM packets`).Scan(&nextSeq); err != nil {
		return err
	}
	if pkt.GlobalSeq != nextSeq {
		return errs.New(errs.EState, "packet commit sequence is not contiguous").
			WithPath("packets.global_seq")
	}
	_, err = tx.Exec(`INSERT INTO packets (
global_seq,channel,packet_index,start_sample,end_sample,sample_count,timestamp_ms,window_start_ms,window_end_ms,
frames_json,frame_digest,boundary_json,consumed_event_offset,event_count,effective_count,stream_seed,rng_progress,
fingerprint_delta,semantic_fingerprint,content_hash,rule_version,algorithm_version,branch_sources_json,source_branch,
rule_snapshot_json,decisions_json,events_json)
VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		pkt.GlobalSeq, pkt.Channel, pkt.PacketIndex, pkt.StartSample, pkt.EndSample, pkt.SampleCount, pkt.TimestampMS,
		pkt.WindowStartMS, pkt.WindowEndMS, string(framesJSON), pkt.FrameDigest, string(boundaryJSON),
		pkt.ConsumedEventOffset, pkt.EventCount, pkt.EffectiveCount, pkt.StreamSeed, pkt.RNGProgress,
		pkt.FingerprintDelta, pkt.SemanticFingerprint, pkt.ContentHash, pkt.RuleVersion, pkt.AlgorithmVersion,
		string(branchesJSON), pkt.SourceBranch, string(rulesJSON), string(decisionsJSON), string(eventsJSON),
	)
	if err != nil {
		return err
	}

	meta.CommittedPackets = nextSeq + 1
	values := map[string]string{
		"rule_version": model.RuleVersion, "algorithm_version": model.AlgorithmVersion,
		"semantic_fingerprint": meta.Fingerprint, "stream_seed": fmt.Sprint(meta.StreamSeed),
		"committed_packets": fmt.Sprint(meta.CommittedPackets),
	}
	for key, value := range values {
		if _, err := tx.Exec(`INSERT INTO meta(key,value) VALUES(?,?)
ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) LoadMeta() (Meta, error) {
	rows, err := s.db.Query(`SELECT key,value FROM meta`)
	if err != nil {
		return Meta{}, err
	}
	defer rows.Close()
	values := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return Meta{}, err
		}
		values[k] = v
	}
	var m Meta
	m.RuleVersion = values["rule_version"]
	m.AlgorithmVersion = values["algorithm_version"]
	m.Fingerprint = values["semantic_fingerprint"]
	fmt.Sscan(values["stream_seed"], &m.StreamSeed)
	fmt.Sscan(values["committed_packets"], &m.CommittedPackets)
	return m, nil
}

func (s *Store) LoadPackets() ([]model.Packet, error) {
	rows, err := s.db.Query(`SELECT global_seq,channel,packet_index,start_sample,end_sample,sample_count,timestamp_ms,
window_start_ms,window_end_ms,frames_json,frame_digest,consumed_event_offset,event_count,effective_count,stream_seed,
rng_progress,fingerprint_delta,semantic_fingerprint,content_hash,rule_version,algorithm_version,branch_sources_json,
source_branch,rule_snapshot_json,decisions_json,events_json FROM packets ORDER BY global_seq`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Packet
	for rows.Next() {
		var pkt model.Packet
		var framesJSON, branchesJSON, rulesJSON, decisionsJSON, eventsJSON string
		if err := rows.Scan(&pkt.GlobalSeq, &pkt.Channel, &pkt.PacketIndex, &pkt.StartSample, &pkt.EndSample,
			&pkt.SampleCount, &pkt.TimestampMS, &pkt.WindowStartMS, &pkt.WindowEndMS, &framesJSON, &pkt.FrameDigest,
			&pkt.ConsumedEventOffset, &pkt.EventCount, &pkt.EffectiveCount, &pkt.StreamSeed, &pkt.RNGProgress,
			&pkt.FingerprintDelta, &pkt.SemanticFingerprint, &pkt.ContentHash, &pkt.RuleVersion, &pkt.AlgorithmVersion,
			&branchesJSON, &pkt.SourceBranch, &rulesJSON, &decisionsJSON, &eventsJSON); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(framesJSON), &pkt.Frames); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(branchesJSON), &pkt.BranchSources); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(rulesJSON), &pkt.RuleSnapshot); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(decisionsJSON), &pkt.Decisions); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(eventsJSON), &pkt.Events); err != nil {
			return nil, err
		}
		out = append(out, pkt)
	}
	return out, rows.Err()
}

func Validate(packets []model.Packet, expectedFingerprint string) error {
	for i, pkt := range packets {
		if pkt.GlobalSeq != i {
			return errs.New(errs.EHash, "checkpoint packet sequence gap").
				WithPath(fmt.Sprintf("packets.global_seq expected=%d actual=%d", i, pkt.GlobalSeq))
		}
		if got := model.DigestFrames(pkt.Frames); got != pkt.FrameDigest {
			return hashError(pkt.GlobalSeq, "frame_digest", pkt.FrameDigest, got)
		}
		if got := model.PacketContentHash(pkt); got != pkt.ContentHash {
			return hashError(pkt.GlobalSeq, "content_hash", pkt.ContentHash, got)
		}
		if pkt.RuleVersion != model.RuleVersion || pkt.AlgorithmVersion != model.AlgorithmVersion {
			return hashError(pkt.GlobalSeq, "version", model.RuleVersion+"/"+model.AlgorithmVersion,
				pkt.RuleVersion+"/"+pkt.AlgorithmVersion)
		}
		if expectedFingerprint != "" && pkt.SemanticFingerprint != expectedFingerprint {
			return hashError(pkt.GlobalSeq, "semantic_fingerprint", expectedFingerprint, pkt.SemanticFingerprint)
		}
	}
	return nil
}

func hashError(seq int, item string, expected, actual any) error {
	e := errs.New(errs.EHash, "checkpoint validation mismatch; all packets must be recomputed")
	e.Seq = seq
	e.Field = item
	e.Details = map[string]any{"item": item, "expected": expected, "actual": actual, "action": "discard_entire_checkpoint"}
	return e
}

func RemoveArtifacts(path string) error {
	for _, suffix := range []string{"", "-wal", "-shm"} {
		if err := os.Remove(path + suffix); err != nil && !os.IsNotExist(err) {
			return err
		}
	}
	return nil
}
