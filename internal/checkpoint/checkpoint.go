package checkpoint

import (
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"strings"

	_ "modernc.org/sqlite"

	"grayrelease/internal/engine"
	"grayrelease/internal/model"
)

const (
	CodeTruncated       = "E_CKPT_TRUNCATED"
	CodeVersionMismatch = "E_VERSION_MISMATCH"
	CodeCorrupt         = "E_CKPT_CORRUPT"
	CodeWrite           = "E_CKPT_WRITE"

	metaCheckpointSchema = "checkpoint_schema"
	metaEngineering      = "engineering_version"
	metaCommittedTick    = "committed_tick"
)

type Store struct {
	db                 *sql.DB
	path               string
	EngineeringVersion string
}

func OpenStore(path, expectedEngineeringVersion string) (*Store, error) {
	if info, err := os.Stat(path); err == nil && !info.IsDir() {
		if info.Size() == 0 {
			return nil, model.NewError(CodeTruncated, "checkpoint file is zero bytes").With("path", path).With("size", 0)
		}
		if err := preflightHeader(path); err != nil {
			return nil, err
		}
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, model.NewError("E_CKPT_STAT", "cannot stat checkpoint").With("path", path).With("cause", err.Error())
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, model.NewError(CodeCorrupt, "cannot open sqlite checkpoint").With("path", path).With("cause", err.Error())
	}
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, model.NewError(CodeTruncated, "checkpoint database ping failed").With("path", path).With("cause", err.Error())
	}
	if err := integrityCheck(db, path); err != nil {
		_ = db.Close()
		return nil, err
	}
	store := &Store{db: db, path: path}
	if err := store.init(expectedEngineeringVersion); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

func preflightHeader(path string) error {
	header := make([]byte, 16)
	file, err := os.Open(path)
	if err != nil {
		return model.NewError(CodeCorrupt, "cannot read checkpoint header").With("path", path).With("cause", err.Error())
	}
	defer file.Close()
	n, _ := file.Read(header)
	if n < 16 || string(header) != "SQLite format 3\x00" {
		return model.NewError(CodeTruncated, "checkpoint is not a complete SQLite database").
			With("path", path).With("header_bytes", n)
	}
	return nil
}

func integrityCheck(db *sql.DB, path string) error {
	rows, err := db.Query(`PRAGMA integrity_check;`)
	if err != nil {
		return model.NewError(CodeTruncated, "checkpoint integrity check failed").With("path", path).With("cause", err.Error())
	}
	defer rows.Close()
	for rows.Next() {
		var result string
		if err := rows.Scan(&result); err != nil {
			return model.NewError(CodeTruncated, "checkpoint integrity scan failed").With("path", path).With("cause", err.Error())
		}
		if !strings.EqualFold(result, "ok") {
			return model.NewError(CodeTruncated, "checkpoint integrity check reported corruption").
				With("path", path).With("integrity", result)
		}
	}
	if err := rows.Err(); err != nil {
		return model.NewError(CodeTruncated, "checkpoint integrity cursor failed").With("path", path).With("cause", err.Error())
	}
	return nil
}

func (s *Store) init(expectedEngineeringVersion string) error {
	_, err := s.db.Exec(`
CREATE TABLE IF NOT EXISTS meta (
	key TEXT PRIMARY KEY,
	value TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS route_mapping (
	tick INTEGER NOT NULL,
	request_id TEXT NOT NULL,
	service TEXT NOT NULL,
	stable_key TEXT NOT NULL,
	rule_id TEXT NOT NULL,
	side TEXT NOT NULL,
	bucket INTEGER NOT NULL,
	payload TEXT NOT NULL,
	PRIMARY KEY (tick, request_id)
);
CREATE INDEX IF NOT EXISTS idx_route_mapping_service_tick ON route_mapping(service, tick);
CREATE TABLE IF NOT EXISTS mirror_queue_snapshot (
	tick INTEGER NOT NULL,
	service TEXT NOT NULL,
	next_seq INTEGER NOT NULL,
	state_json TEXT NOT NULL,
	PRIMARY KEY (tick, service)
);
`)
	if err != nil {
		return model.NewError(CodeWrite, "initialize checkpoint schema failed").With("cause", err.Error())
	}

	var engineeringVersion string
	err = s.db.QueryRow(`SELECT value FROM meta WHERE key=?`, metaEngineering).Scan(&engineeringVersion)
	if errors.Is(err, sql.ErrNoRows) {
		if expectedEngineeringVersion == "" {
			return model.NewError(CodeVersionMismatch, "new checkpoint requires engineering version")
		}
		tx, err := s.db.Begin()
		if err != nil {
			return model.NewError(CodeWrite, "begin checkpoint meta failed").With("cause", err.Error())
		}
		_, err = tx.Exec(`INSERT INTO meta(key,value) VALUES(?,?)`, metaCheckpointSchema, "1")
		if err == nil {
			_, err = tx.Exec(`INSERT INTO meta(key,value) VALUES(?,?)`, metaEngineering, expectedEngineeringVersion)
		}
		if err == nil {
			_, err = tx.Exec(`INSERT INTO meta(key,value) VALUES(?,?)`, metaCommittedTick, "-1")
		}
		if err != nil {
			_ = tx.Rollback()
			return model.NewError(CodeWrite, "insert checkpoint meta failed").With("cause", err.Error())
		}
		if err := tx.Commit(); err != nil {
			return model.NewError(CodeWrite, "commit checkpoint meta failed").With("cause", err.Error())
		}
		s.EngineeringVersion = expectedEngineeringVersion
		return nil
	}
	if err != nil {
		return model.NewError(CodeCorrupt, "read checkpoint engineering version failed").With("cause", err.Error())
	}
	if expectedEngineeringVersion != "" && engineeringVersion != expectedEngineeringVersion {
		return model.NewError(CodeVersionMismatch, "checkpoint engineering version does not match current snapshot").
			With("checkpoint_version", engineeringVersion).With("snapshot_version", expectedEngineeringVersion)
	}
	s.EngineeringVersion = engineeringVersion
	return nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

func (s *Store) CommittedTick() (int, error) {
	var value string
	if err := s.db.QueryRow(`SELECT value FROM meta WHERE key=?`, metaCommittedTick).Scan(&value); err != nil {
		return -1, model.NewError(CodeCorrupt, "read committed tick failed").With("cause", err.Error())
	}
	var tick int
	if _, err := parseInt(value, &tick); err != nil {
		return -1, model.NewError(CodeCorrupt, "committed tick is not an integer").With("value", value)
	}
	return tick, nil
}

func parseInt(value string, target *int) (int, error) {
	n := 0
	negative := false
	for i, r := range value {
		if i == 0 && r == '-' {
			negative = true
			continue
		}
		if r < '0' || r > '9' {
			return 0, errors.New("invalid integer")
		}
		n = n*10 + int(r-'0')
	}
	if negative {
		n = -n
	}
	*target = n
	return n, nil
}

func (s *Store) WriteRouteMappings(tick int, decisions []*engine.Decision) error {
	tx, err := s.db.Begin()
	if err != nil {
		return model.NewError(CodeWrite, "begin route mapping write failed").With("tick", tick).With("cause", err.Error())
	}
	if _, err := tx.Exec(`DELETE FROM route_mapping WHERE tick=?`, tick); err != nil {
		_ = tx.Rollback()
		return model.NewError(CodeWrite, "replace route mapping failed").With("tick", tick).With("cause", err.Error())
	}
	stmt, err := tx.Prepare(`INSERT INTO route_mapping
		(tick,request_id,service,stable_key,rule_id,side,bucket,payload)
		VALUES(?,?,?,?,?,?,?,?)`)
	if err != nil {
		_ = tx.Rollback()
		return model.NewError(CodeWrite, "prepare route mapping failed").With("cause", err.Error())
	}
	defer stmt.Close()
	for _, decision := range decisions {
		payload, err := json.Marshal(decision)
		if err != nil {
			_ = tx.Rollback()
			return model.NewError(CodeWrite, "marshal route decision failed").With("request_id", decision.RequestID).With("cause", err.Error())
		}
		if _, err := stmt.Exec(tick, decision.RequestID, decision.Service, decision.Key,
			decision.SelectedRuleID, decision.Side, decision.Bucket, string(payload)); err != nil {
			_ = tx.Rollback()
			return model.NewError(CodeWrite, "insert route mapping failed").With("request_id", decision.RequestID).With("cause", err.Error())
		}
	}
	if err := tx.Commit(); err != nil {
		return model.NewError(CodeWrite, "commit route mappings failed").With("tick", tick).With("cause", err.Error())
	}
	return nil
}

func (s *Store) WriteMirrorQueues(tick int, states map[string]engine.BrokerState) error {
	tx, err := s.db.Begin()
	if err != nil {
		return model.NewError(CodeWrite, "begin mirror queue write failed").With("tick", tick).With("cause", err.Error())
	}
	if _, err := tx.Exec(`DELETE FROM mirror_queue_snapshot WHERE tick=?`, tick); err != nil {
		_ = tx.Rollback()
		return model.NewError(CodeWrite, "replace mirror queue failed").With("tick", tick).With("cause", err.Error())
	}
	stmt, err := tx.Prepare(`INSERT INTO mirror_queue_snapshot(tick,service,next_seq,state_json) VALUES(?,?,?,?)`)
	if err != nil {
		_ = tx.Rollback()
		return model.NewError(CodeWrite, "prepare mirror queue failed").With("cause", err.Error())
	}
	defer stmt.Close()
	for service, state := range states {
		payload, err := json.Marshal(state)
		if err != nil {
			_ = tx.Rollback()
			return model.NewError(CodeWrite, "marshal mirror queue failed").With("service", service).With("cause", err.Error())
		}
		if _, err := stmt.Exec(tick, service, state.NextSeq, string(payload)); err != nil {
			_ = tx.Rollback()
			return model.NewError(CodeWrite, "insert mirror queue failed").With("service", service).With("cause", err.Error())
		}
	}
	if err := tx.Commit(); err != nil {
		return model.NewError(CodeWrite, "commit mirror queues failed").With("tick", tick).With("cause", err.Error())
	}
	return nil
}

func (s *Store) CommitTick(tick int) error {
	result, err := s.db.Exec(`INSERT INTO meta(key,value) VALUES(?,?)
		ON CONFLICT(key) DO UPDATE SET value=excluded.value
		WHERE CAST(value AS INTEGER) < ?`, metaCommittedTick, itoa(tick), tick)
	if err != nil {
		return model.NewError(CodeWrite, "update committed tick failed").With("tick", tick).With("cause", err.Error())
	}
	rows, _ := result.RowsAffected()
	if rows == 0 {
		current, _ := s.CommittedTick()
		if current >= tick {
			return nil
		}
		return model.NewError(CodeWrite, "refusing to move committed tick backwards").With("tick", tick).With("current", current)
	}
	return nil
}

func itoa(value int) string {
	if value == 0 {
		return "0"
	}
	negative := value < 0
	if negative {
		value = -value
	}
	var buf [20]byte
	pos := len(buf)
	for value > 0 {
		pos--
		buf[pos] = byte('0' + value%10)
		value /= 10
	}
	if negative {
		pos--
		buf[pos] = '-'
	}
	return string(buf[pos:])
}

type RecoveryData struct {
	CommittedTick       int                           `json:"committed_tick"`
	EngineeringVersion  string                        `json:"engineering_version"`
	QueueStates         map[string]engine.BrokerState `json:"queue_states,omitempty"`
	DeletedFutureRoutes int64                         `json:"deleted_future_routes"`
	DeletedFutureQueues int64                         `json:"deleted_future_queues"`
}

func (s *Store) Recover() (*RecoveryData, error) {
	committed, err := s.CommittedTick()
	if err != nil {
		return nil, err
	}
	routeResult, err := s.db.Exec(`DELETE FROM route_mapping WHERE tick > ?`, committed)
	if err != nil {
		return nil, model.NewError(CodeCorrupt, "discard future route state failed").With("cause", err.Error())
	}
	queueResult, err := s.db.Exec(`DELETE FROM mirror_queue_snapshot WHERE tick > ?`, committed)
	if err != nil {
		return nil, model.NewError(CodeCorrupt, "discard future mirror state failed").With("cause", err.Error())
	}
	routesDeleted, _ := routeResult.RowsAffected()
	queuesDeleted, _ := queueResult.RowsAffected()

	data := &RecoveryData{
		CommittedTick: committed, EngineeringVersion: s.EngineeringVersion,
		QueueStates:         map[string]engine.BrokerState{},
		DeletedFutureRoutes: routesDeleted, DeletedFutureQueues: queuesDeleted,
	}
	if committed < 0 {
		return data, nil
	}
	rows, err := s.db.Query(`SELECT service,state_json FROM mirror_queue_snapshot WHERE tick=?`, committed)
	if err != nil {
		return nil, model.NewError(CodeCorrupt, "load committed mirror queue failed").With("tick", committed).With("cause", err.Error())
	}
	defer rows.Close()
	for rows.Next() {
		var service, payload string
		if err := rows.Scan(&service, &payload); err != nil {
			return nil, model.NewError(CodeCorrupt, "scan mirror queue failed").With("cause", err.Error())
		}
		var state engine.BrokerState
		if err := json.Unmarshal([]byte(payload), &state); err != nil {
			return nil, model.NewError(CodeCorrupt, "unmarshal mirror queue failed").With("service", service).With("cause", err.Error())
		}
		data.QueueStates[service] = state
	}
	return data, nil
}

func (s *Store) CountRouteMappings(tick int) (int, error) {
	var count int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM route_mapping WHERE tick=?`, tick).Scan(&count)
	return count, err
}

func (s *Store) Path() string {
	return s.path
}
