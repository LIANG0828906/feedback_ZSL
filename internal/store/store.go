package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	_ "modernc.org/sqlite"

	"audiobus/domain"
	aerr "audiobus/errs"
	"audiobus/internal/packet"
)

type Store struct {
	db   *sql.DB
	path string
	mu   sync.Mutex
}

func Open(path string) (*Store, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	s := &Store{db: db, path: path}
	if _, err := db.Exec(`PRAGMA journal_mode=WAL; PRAGMA foreign_keys=ON; PRAGMA busy_timeout=5000;`); err != nil {
		return nil, err
	}
	if err := s.initSchema(context.Background()); err != nil {
		return nil, err
	}
	return s, nil
}

func OpenReadOnly(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path+"?mode=ro")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	return &Store{db: db, path: path}, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) DB() *sql.DB { return s.db }

func (s *Store) initSchema(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS meta (k TEXT PRIMARY KEY, v TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS packets (
  window INT NOT NULL,
  channel INT NOT NULL,
  seq INT NOT NULL,
  start INT NOT NULL,
  samples INT NOT NULL,
  capacity INT NOT NULL DEFAULT 0,
  disp TEXT NOT NULL,
  mark INT NOT NULL,
  prev_offset INT NOT NULL DEFAULT 0,
  frames BLOB NOT NULL,
  hash TEXT NOT NULL,
  event_ids TEXT NOT NULL DEFAULT '[]',
  template_hash TEXT NOT NULL DEFAULT '',
  quant_unit INT NOT NULL DEFAULT 0,
  consumed_offset INT NOT NULL DEFAULT 0,
  branch_version TEXT NOT NULL DEFAULT '',
  algo_version TEXT NOT NULL DEFAULT '',
  state TEXT NOT NULL DEFAULT 'committed',
  PRIMARY KEY (window, channel, seq)
);
CREATE TABLE IF NOT EXISTS windows (
  window INT NOT NULL,
  channel INT NOT NULL,
  state TEXT NOT NULL,
  consumed INT NOT NULL,
  hash TEXT NOT NULL,
  PRIMARY KEY (window, channel)
);
CREATE TABLE IF NOT EXISTS window_batches (
  window INT PRIMARY KEY,
  state TEXT NOT NULL,
  hash TEXT NOT NULL,
  committed_at TEXT NOT NULL
);`)
	if err != nil {
		return err
	}
	return s.migrateColumns(ctx)
}

func (s *Store) migrateColumns(ctx context.Context) error {
	cols := map[string]string{}
	rows, err := s.db.QueryContext(ctx, `PRAGMA table_info(packets)`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			rows.Close()
			return err
		}
		cols[name] = ctype
	}
	rows.Close()
	adds := map[string]string{
		"capacity":        "INT NOT NULL DEFAULT 0",
		"prev_offset":     "INT NOT NULL DEFAULT 0",
		"event_ids":       "TEXT NOT NULL DEFAULT '[]'",
		"template_hash":   "TEXT NOT NULL DEFAULT ''",
		"quant_unit":      "INT NOT NULL DEFAULT 0",
		"consumed_offset": "INT NOT NULL DEFAULT 0",
		"branch_version":  "TEXT NOT NULL DEFAULT ''",
		"algo_version":    "TEXT NOT NULL DEFAULT ''",
		"state":           "TEXT NOT NULL DEFAULT 'committed'",
	}
	for name, ddl := range adds {
		if _, ok := cols[name]; !ok {
			if _, err := s.db.ExecContext(ctx, `ALTER TABLE packets ADD COLUMN `+name+` `+ddl); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Store) SetMeta(ctx context.Context, k, v string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.ExecContext(ctx, `INSERT INTO meta(k,v) VALUES(?,?) ON CONFLICT(k) DO UPDATE SET v=excluded.v`, k, v)
	return err
}

func (s *Store) Meta(ctx context.Context, k string) (string, error) {
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT v FROM meta WHERE k=?`, k).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return v, err
}

type WindowCommit struct {
	Window  int
	Packets []domain.Packet
}

func (s *Store) CommitWindow(ctx context.Context, branchVersion, algoVersion string, wc WindowCommit) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `INSERT INTO meta(k,v) VALUES('branchVersion',?) ON CONFLICT(k) DO UPDATE SET v=excluded.v`, branchVersion); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO meta(k,v) VALUES('algoVersion',?) ON CONFLICT(k) DO UPDATE SET v=excluded.v`, algoVersion); err != nil {
		return err
	}
	batchHash := sha256.New()
	channelConsumed := map[int]int64{}
	channelHash := map[int]string{}
	for _, p := range wc.Packets {
		encoded, hash, err := packet.EncodedBytes(p)
		if err != nil {
			return err
		}
		if hash != p.ContentHash && p.Disposition != domain.TruncDisposition {
			// ContentHash is always the padded-frame hash for pad/full packets.
			if packet.HashFrames(p.PaddedFrames) != p.ContentHash {
				return &aerr.Error{Code: aerr.EHash, Channel: p.Channel, Window: p.Window, Packet: p.Seq, Reason: "packet content hash mismatch before commit"}
			}
		}
		ids, _ := json.Marshal(p.EventIDs)
		if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO packets
(window,channel,seq,start,samples,capacity,disp,mark,prev_offset,frames,hash,event_ids,template_hash,quant_unit,consumed_offset,branch_version,algo_version,state)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			p.Window, p.Channel, p.Seq, p.Start, p.Samples, p.CapacitySamples, p.Disposition, p.Mark, p.PrevOffset,
			encoded, p.ContentHash, string(ids), p.TemplateHash, p.QuantUnit, p.ConsumedOffset, p.BranchVersion, p.AlgoVersion, "committed"); err != nil {
			return err
		}
		fmt.Fprintf(batchHash, "%d:%d:%s;", p.Channel, p.Seq, p.ContentHash)
		channelConsumed[p.Channel] = p.ConsumedOffset
		h := sha256.Sum256([]byte(channelHash[p.Channel] + p.ContentHash))
		channelHash[p.Channel] = hex.EncodeToString(h[:])
	}
	for ch, hash := range channelHash {
		if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO windows(window,channel,state,consumed,hash) VALUES(?,?, 'Committed', ?,?)`, wc.Window, ch, channelConsumed[ch], hash); err != nil {
			return err
		}
	}
	batchSum := hex.EncodeToString(batchHash.Sum(nil))
	if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO window_batches(window,state,hash,committed_at) VALUES(?, 'committed', ?,?)`, wc.Window, batchSum, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) LoadPackets(ctx context.Context) ([]domain.Packet, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT window,channel,seq,start,samples,COALESCE(capacity,0),disp,mark,COALESCE(prev_offset,0),frames,hash,
COALESCE(event_ids,'[]'),COALESCE(template_hash,''),COALESCE(quant_unit,0),COALESCE(consumed_offset,0),COALESCE(branch_version,''),COALESCE(algo_version,'')
FROM packets ORDER BY window,channel,seq`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []domain.Packet{}
	for rows.Next() {
		var p domain.Packet
		var blob []byte
		var hash, ids string
		if err := rows.Scan(&p.Window, &p.Channel, &p.Seq, &p.Start, &p.Samples, &p.CapacitySamples, &p.Disposition, &p.Mark, &p.PrevOffset,
			&blob, &hash, &ids, &p.TemplateHash, &p.QuantUnit, &p.ConsumedOffset, &p.BranchVersion, &p.AlgoVersion); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(ids), &p.EventIDs); err != nil {
			return nil, err
		}
		frameCount := len(blob) / 8
		decoded := make([]int64, frameCount)
		for i := range decoded {
			decoded[i] = int64(binary.LittleEndian.Uint64(blob[i*8 : (i+1)*8]))
		}
		p.PaddedFrames = decoded
		if p.CapacitySamples == 0 {
			p.CapacitySamples = frameCount
		}
		if p.Samples <= frameCount {
			p.Frames = append([]int64(nil), decoded[:p.Samples]...)
		} else {
			p.Frames = decoded
		}
		p.ContentHash = hash
		out = append(out, p)
	}
	return out, rows.Err()
}

func (s *Store) ValidateSnapshot(ctx context.Context, branchVersion, algoVersion string) error {
	bv, err := s.Meta(ctx, "branchVersion")
	if err != nil {
		return err
	}
	av, err := s.Meta(ctx, "algoVersion")
	if err != nil {
		return err
	}
	if bv != branchVersion || av != algoVersion {
		return &aerr.Error{Code: aerr.EHash, Reason: fmt.Sprintf("version mismatch branch=%s/%s algo=%s/%s", bv, branchVersion, av, algoVersion)}
	}
	packets, err := s.LoadPackets(ctx)
	if err != nil {
		return err
	}
	for _, p := range packets {
		if got := packet.HashFrames(p.PaddedFrames); got != p.ContentHash {
			return &aerr.Error{Code: aerr.EHash, Window: p.Window, Channel: p.Channel, Packet: p.Seq, Reason: "persisted frame hash mismatch"}
		}
	}
	return nil
}

func (s *Store) Reset(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, table := range []string{"packets", "windows", "window_batches", "meta"} {
		if _, err := tx.ExecContext(ctx, "DELETE FROM "+table); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func OpenValidated(ctx context.Context, path, branchVersion, algoVersion string) (*Store, bool, error) {
	s, err := Open(path)
	if err != nil {
		return nil, false, err
	}
	if err := s.ValidateSnapshot(ctx, branchVersion, algoVersion); err != nil {
		var ae *aerr.Error
		if errors.As(err, &ae) && ae.Code == aerr.EHash {
			if err := s.Reset(ctx); err != nil {
				return nil, false, err
			}
			return s, true, nil
		}
		s.Close()
		return nil, false, err
	}
	return s, false, nil
}
