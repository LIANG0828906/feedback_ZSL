package maskqc

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

type RecoveryMeta struct {
	ProjectHash       string `json:"project_hash"`
	OrderVersion      string `json:"order_version"`
	ConversionVersion string `json:"conversion_version"`
	AlgorithmVersion  string `json:"algorithm_version"`
	TopologyVersion   string `json:"topology_version"`
}

type CommitRecord struct {
	ID            int64  `json:"id"`
	EventKey      string `json:"event_key"`
	IconID        string `json:"icon_id"`
	StrokeID      string `json:"stroke_id"`
	StrokeVersion string `json:"stroke_version"`
	OrderIndex    int    `json:"order_index"`
	CreatedAt     string `json:"created_at"`
}

type ComponentRecord struct {
	TileID            string `json:"tile_id"`
	StrokeID          string `json:"stroke_id"`
	CacheKey          string `json:"cache_key"`
	ComponentHash     string `json:"component_hash"`
	InputHash         string `json:"input_hash"`
	ResidualDigest    string `json:"residual_digest"`
	OrderVersion      string `json:"order_version"`
	ConversionVersion string `json:"conversion_version"`
	AlgorithmVersion  string `json:"algorithm_version"`
	Width             int    `json:"width"`
	Height            int    `json:"height"`
	Alpha             []byte `json:"-"`
}

type StrokeCommit struct {
	EventKey      string
	IconID        string
	StrokeID      string
	StrokeVersion string
	OrderIndex    int
	Components    []ComponentRecord
}

type TileResultRecord struct {
	CacheKey string
	IconID   string
	TileID   string
	Hash     string
	Mask     *Mask
	EventKey string
}

type Store struct {
	db *sql.DB
	mu sync.Mutex
}

func OpenStore(path string) (*Store, error) {
	dsn := path
	if path != ":memory:" && path != "" {
		dsn = path + "?_journal_mode=WAL&_busy_timeout=5000&_foreign_keys=on"
	}
	db, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, err
	}
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) migrate() error {
	_, err := s.db.Exec(`
CREATE TABLE IF NOT EXISTS meta (
  key TEXT PRIMARY KEY,
  value TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS commits (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  event_key TEXT NOT NULL UNIQUE,
  icon_id TEXT NOT NULL,
  stroke_id TEXT NOT NULL,
  stroke_version TEXT NOT NULL,
  order_index INTEGER NOT NULL,
  created_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS components (
  cache_key TEXT PRIMARY KEY,
  commit_id INTEGER NOT NULL REFERENCES commits(id),
  tile_id TEXT NOT NULL,
  stroke_id TEXT NOT NULL,
  component_hash TEXT NOT NULL,
  input_hash TEXT NOT NULL,
  residual_digest TEXT NOT NULL,
  order_version TEXT NOT NULL,
  conversion_version TEXT NOT NULL,
  algorithm_version TEXT NOT NULL,
  width INTEGER NOT NULL,
  height INTEGER NOT NULL,
  alpha BLOB NOT NULL,
  UNIQUE(commit_id,tile_id)
);
CREATE TABLE IF NOT EXISTS tile_results (
  cache_key TEXT PRIMARY KEY,
  icon_id TEXT NOT NULL,
  tile_id TEXT NOT NULL,
  result_hash TEXT NOT NULL,
  width INTEGER NOT NULL,
  height INTEGER NOT NULL,
  mask BLOB NOT NULL,
  event_key TEXT NOT NULL
);
`)
	return err
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) InitializeMeta(meta RecoveryMeta) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.setMetaTx(context.Background(), nil, meta)
}

func (s *Store) setMetaTx(ctx context.Context, tx *sql.Tx, meta RecoveryMeta) error {
	rows := [][2]string{
		{"project_hash", meta.ProjectHash},
		{"order_version", meta.OrderVersion},
		{"conversion_version", meta.ConversionVersion},
		{"algorithm_version", meta.AlgorithmVersion},
		{"topology_version", meta.TopologyVersion},
	}
	for _, kv := range rows {
		var err error
		if tx != nil {
			_, err = tx.ExecContext(ctx, `INSERT INTO meta(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, kv[0], kv[1])
		} else {
			_, err = s.db.ExecContext(ctx, `INSERT INTO meta(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, kv[0], kv[1])
		}
		if err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) ValidateRecovery(want RecoveryMeta) error {
	got, err := s.Meta()
	if err != nil {
		return err
	}
	if got.ProjectHash == "" {
		return s.InitializeMeta(want)
	}
	mismatch := map[string]any{}
	if got.ProjectHash != want.ProjectHash {
		mismatch["project_hash"] = map[string]string{"expected": want.ProjectHash, "actual": got.ProjectHash}
	}
	if got.OrderVersion != want.OrderVersion {
		mismatch["order_version"] = map[string]string{"expected": want.OrderVersion, "actual": got.OrderVersion}
	}
	if got.ConversionVersion != want.ConversionVersion {
		mismatch["conversion_version"] = map[string]string{"expected": want.ConversionVersion, "actual": got.ConversionVersion}
	}
	if got.AlgorithmVersion != want.AlgorithmVersion {
		mismatch["algorithm_version"] = map[string]string{"expected": want.AlgorithmVersion, "actual": got.AlgorithmVersion}
	}
	if len(mismatch) > 0 {
		return &Error{Code: ECorrupt, FieldPath: "checkpoint.meta", Message: "checkpoint identity/version mismatch; reject and rebuild", Details: map[string]any{"mismatch": mismatch, "rebuild_required": true}}
	}

	// Verify every persisted component hash so bit rot or forced-write corruption
	// is detected before reuse.
	rows, err := s.db.Query(`SELECT cache_key, component_hash, alpha FROM components`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var key, hash string
		var alpha []byte
		if err := rows.Scan(&key, &hash, &alpha); err != nil {
			return err
		}
		if HashBytes(alpha) != hash {
			return &Error{Code: ECorrupt, FieldPath: "components." + key + ".alpha", Message: "component content hash mismatch; reject and rebuild", Details: map[string]any{"cache_key": key, "rebuild_required": true}}
		}
	}
	return rows.Err()
}

func (s *Store) Meta() (RecoveryMeta, error) {
	var m RecoveryMeta
	keys := map[string]*string{
		"project_hash": &m.ProjectHash, "order_version": &m.OrderVersion,
		"conversion_version": &m.ConversionVersion, "algorithm_version": &m.AlgorithmVersion,
		"topology_version": &m.TopologyVersion,
	}
	rows, err := s.db.Query(`SELECT key,value FROM meta`)
	if err != nil {
		return m, err
	}
	defer rows.Close()
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return m, err
		}
		if ptr, ok := keys[k]; ok {
			*ptr = v
		}
	}
	return m, rows.Err()
}

// CommitStroke writes one logical stroke commit and all of its tile components
// in a single immediate transaction. The mutex plus MaxOpenConns(1) makes
// commit ordering serial and stable.
func (s *Store) CommitStroke(ctx context.Context, c StrokeCommit) (*CommitRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sort.Slice(c.Components, func(i, j int) bool { return c.Components[i].TileID < c.Components[j].TileID })
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{})
	if err != nil {
		return nil, err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	res, err := tx.ExecContext(ctx, `INSERT INTO commits(event_key,icon_id,stroke_id,stroke_version,order_index,created_at) VALUES(?,?,?,?,?,?)`,
		c.EventKey, c.IconID, c.StrokeID, c.StrokeVersion, c.OrderIndex, now)
	if err != nil {
		return nil, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}
	for _, comp := range c.Components {
		_, err = tx.ExecContext(ctx, `INSERT OR REPLACE INTO components
(cache_key,commit_id,tile_id,stroke_id,component_hash,input_hash,residual_digest,order_version,conversion_version,algorithm_version,width,height,alpha)
VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			comp.CacheKey, id, comp.TileID, comp.StrokeID, comp.ComponentHash, comp.InputHash, comp.ResidualDigest,
			comp.OrderVersion, comp.ConversionVersion, comp.AlgorithmVersion, comp.Width, comp.Height, comp.Alpha)
		if err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	committed = true
	return &CommitRecord{ID: id, EventKey: c.EventKey, IconID: c.IconID, StrokeID: c.StrokeID, StrokeVersion: c.StrokeVersion, OrderIndex: c.OrderIndex, CreatedAt: now}, nil
}

func (s *Store) LastCommit(iconID string) (*CommitRecord, error) {
	row := s.db.QueryRow(`SELECT id,event_key,icon_id,stroke_id,stroke_version,order_index,created_at FROM commits WHERE icon_id=? ORDER BY order_index DESC,id DESC LIMIT 1`, iconID)
	var c CommitRecord
	err := row.Scan(&c.ID, &c.EventKey, &c.IconID, &c.StrokeID, &c.StrokeVersion, &c.OrderIndex, &c.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return &c, err
}

func (s *Store) ListCommits(iconID string) ([]CommitRecord, error) {
	q := `SELECT id,event_key,icon_id,stroke_id,stroke_version,order_index,created_at FROM commits`
	var args []any
	if iconID != "" {
		q += ` WHERE icon_id=?`
		args = append(args, iconID)
	}
	q += ` ORDER BY order_index ASC,id ASC`
	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CommitRecord
	for rows.Next() {
		var c CommitRecord
		if err := rows.Scan(&c.ID, &c.EventKey, &c.IconID, &c.StrokeID, &c.StrokeVersion, &c.OrderIndex, &c.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (s *Store) PutTileResult(r TileResultRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec(`INSERT OR REPLACE INTO tile_results(cache_key,icon_id,tile_id,result_hash,width,height,mask,event_key) VALUES(?,?,?,?,?,?,?,?)`,
		r.CacheKey, r.IconID, r.TileID, r.Hash, r.Mask.Width, r.Mask.Height, r.Mask.Alpha, r.EventKey)
	return err
}

func (s *Store) GetTileResult(cacheKey string) (*TileResultRecord, error) {
	var r TileResultRecord
	var w, h int
	var alpha []byte
	err := s.db.QueryRow(`SELECT cache_key,icon_id,tile_id,result_hash,width,height,mask,event_key FROM tile_results WHERE cache_key=?`, cacheKey).
		Scan(&r.CacheKey, &r.IconID, &r.TileID, &r.Hash, &w, &h, &alpha, &r.EventKey)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	r.Mask = &Mask{Width: w, Height: h, Alpha: alpha}
	return &r, nil
}

func (s *Store) Rebuild(meta RecoveryMeta) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, err := s.db.Exec(`DELETE FROM components; DELETE FROM commits; DELETE FROM tile_results; DELETE FROM meta;`); err != nil {
		return err
	}
	return s.setMetaTx(context.Background(), nil, meta)
}

// CorruptMeta is a test/diagnostic hook for damaged persistence.
func (s *Store) CorruptMeta(key, value string) error {
	_, err := s.db.Exec(`UPDATE meta SET value=? WHERE key=?`, value+"-corrupt", key)
	return err
}

// CorruptComponent flips one persisted alpha byte without updating its hash.
func (s *Store) CorruptComponent() error {
	res, err := s.db.Exec(`UPDATE components SET alpha=X'00' WHERE rowid=(SELECT rowid FROM components LIMIT 1)`)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func ProjectIdentityHash(p *Project) string {
	h := sha256.New()
	fmt.Fprintf(h, "%d|%s|%s\n", p.Seed, p.Name, p.Schema)
	for _, ic := range p.Icons {
		fmt.Fprintf(h, "icon=%s %dx%d tile=%dx%d candidate=%s truth=%s\n",
			ic.ID, ic.Width, ic.Height, ic.TileWidth, ic.TileHeight, ic.Candidate.Hash(), ic.Truth.Hash())
		for _, t := range ic.TruthTiles {
			fmt.Fprintf(h, "truth-tile=%s %v %s\n", t.ID, t.Rect, t.Hash)
		}
		for _, s := range ic.Strokes {
			fmt.Fprintf(h, "stroke=%s hash=%s\n", s.ID, s.ContentHash())
		}
	}
	return hex.EncodeToString(h.Sum(nil))
}

func ResidualDigest(traces []FieldTrace) string {
	h := sha256.New()
	for _, tr := range traces {
		fmt.Fprintf(h, "%s|%s|%s|%d|%d\n", tr.FieldPath, tr.Operation, tr.NewValue, tr.Residual, tr.OperandA)
	}
	return hex.EncodeToString(h.Sum(nil)[:16])
}
