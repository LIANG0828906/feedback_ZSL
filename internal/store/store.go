package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"mocapbench/internal/core"

	_ "modernc.org/sqlite"
)

type Store struct {
	db   *sql.DB
	mu   sync.Mutex
	path string
}

type InitInfo struct {
	Reset         bool
	Reason        string
	ParamsVersion string
	AlgoVersion   string
}

func Open(path string) (*Store, *core.Error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, &core.Error{Code: core.ECodeSchema, Message: "cannot open sqlite: " + err.Error()}
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`PRAGMA busy_timeout=5000; PRAGMA journal_mode=DELETE;`); err != nil {
		db.Close()
		return nil, &core.Error{Code: core.ECodeSchema, Message: "cannot configure sqlite: " + err.Error()}
	}
	s := &Store{db: db, path: path}
	if err := s.createSchema(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Path() string { return s.path }

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) createSchema() *core.Error {
	statements := []string{
		`CREATE TABLE IF NOT EXISTS run_meta (
			id INTEGER PRIMARY KEY CHECK (id = 1),
			clip_id TEXT NOT NULL,
			target_id TEXT NOT NULL,
			params_version TEXT NOT NULL,
			algo_version TEXT NOT NULL,
			updated_at TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS checkpoints (
			clip_id TEXT NOT NULL,
			target_id TEXT NOT NULL,
			frame_index INTEGER NOT NULL,
			params_version TEXT NOT NULL,
			algo_version TEXT NOT NULL,
			state TEXT NOT NULL,
			content_hash TEXT NOT NULL,
			rotations_json TEXT NOT NULL,
			root_json TEXT NOT NULL,
			contact_json TEXT NOT NULL,
			stats_json TEXT NOT NULL,
			diagnostics_json TEXT NOT NULL,
			created_at TEXT NOT NULL,
			PRIMARY KEY (clip_id, target_id, params_version, algo_version, frame_index)
		)`,
	}
	for _, stmt := range statements {
		if _, err := s.db.Exec(stmt); err != nil {
			return &core.Error{Code: core.ECodeSchema, Message: "cannot create checkpoint schema: " + err.Error()}
		}
	}
	return nil
}

func (s *Store) Initialize(clipID, targetID, paramsVersion, algoVersion string) (InitInfo, *core.Error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	info := InitInfo{ParamsVersion: paramsVersion, AlgoVersion: algoVersion}
	var oldClip, oldTarget, oldParams, oldAlgo string
	err := s.db.QueryRow(`SELECT clip_id,target_id,params_version,algo_version FROM run_meta WHERE id=1`).
		Scan(&oldClip, &oldTarget, &oldParams, &oldAlgo)
	now := time.Now().UTC().Format(time.RFC3339Nano)
	switch {
	case err == sql.ErrNoRows:
		_, err = s.db.Exec(`INSERT INTO run_meta(id,clip_id,target_id,params_version,algo_version,updated_at) VALUES(1,?,?,?,?,?)`,
			clipID, targetID, paramsVersion, algoVersion, now)
		if err != nil {
			return info, &core.Error{Code: core.ECodeSchema, Message: "cannot insert run meta: " + err.Error()}
		}
	case err != nil:
		return info, &core.Error{Code: core.ECodeSchema, Message: "cannot read run meta: " + err.Error()}
	default:
		if oldParams != paramsVersion || oldAlgo != algoVersion || oldClip != clipID || oldTarget != targetID {
			tx, terr := s.db.Begin()
			if terr != nil {
				return info, &core.Error{Code: core.ECodeSchema, Message: terr.Error()}
			}
			if _, err := tx.Exec(`DELETE FROM checkpoints`); err != nil {
				tx.Rollback()
				return info, &core.Error{Code: core.ECodeSchema, Message: "cannot invalidate old checkpoints: " + err.Error()}
			}
			if _, err := tx.Exec(`UPDATE run_meta SET clip_id=?,target_id=?,params_version=?,algo_version=?,updated_at=? WHERE id=1`,
				clipID, targetID, paramsVersion, algoVersion, now); err != nil {
				tx.Rollback()
				return info, &core.Error{Code: core.ECodeSchema, Message: "cannot update run meta: " + err.Error()}
			}
			if err := tx.Commit(); err != nil {
				return info, &core.Error{Code: core.ECodeSchema, Message: "cannot commit version invalidation: " + err.Error()}
			}
			info.Reset = true
			info.Reason = fmt.Sprintf("version/identity mismatch: old=%s/%s/%s/%s new=%s/%s/%s/%s",
				oldClip, oldTarget, oldParams, oldAlgo, clipID, targetID, paramsVersion, algoVersion)
		}
	}
	return info, nil
}

func (s *Store) SaveCommittedFrame(result core.FrameResult) *core.Error {
	s.mu.Lock()
	defer s.mu.Unlock()
	rotations, err := json.Marshal(result.Rotations)
	if err != nil {
		return &core.Error{Code: core.ECodeSchema, Frame: result.Frame, Message: err.Error()}
	}
	root, err := json.Marshal(result.Root)
	if err != nil {
		return &core.Error{Code: core.ECodeSchema, Frame: result.Frame, Message: err.Error()}
	}
	contact, err := json.Marshal(result.Contacts)
	if err != nil {
		return &core.Error{Code: core.ECodeSchema, Frame: result.Frame, Message: err.Error()}
	}
	stats, err := json.Marshal(result.Stats)
	if err != nil {
		return &core.Error{Code: core.ECodeSchema, Frame: result.Frame, Message: err.Error()}
	}
	diag, err := json.Marshal(result.Diagnostics)
	if err != nil {
		return &core.Error{Code: core.ECodeSchema, Frame: result.Frame, Message: err.Error()}
	}
	tx, err := s.db.Begin()
	if err != nil {
		return &core.Error{Code: core.ECodeSchema, Frame: result.Frame, Message: err.Error()}
	}
	defer tx.Rollback()
	_, err = tx.Exec(`INSERT OR REPLACE INTO checkpoints
		(clip_id,target_id,frame_index,params_version,algo_version,state,content_hash,rotations_json,root_json,contact_json,stats_json,diagnostics_json,created_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		result.ClipID, result.TargetID, result.Frame, result.ParamsVersion, result.AlgoVersion,
		core.StateCommitted, result.ContentHash, string(rotations), string(root), string(contact), string(stats), string(diag),
		time.Now().UTC().Format(time.RFC3339Nano))
	if err != nil {
		return &core.Error{Code: core.ECodeSchema, Frame: result.Frame, Message: "cannot write checkpoint: " + err.Error()}
	}
	if err := tx.Commit(); err != nil {
		return &core.Error{Code: core.ECodeSchema, Frame: result.Frame, Message: "cannot commit checkpoint: " + err.Error()}
	}
	return nil
}

func (s *Store) LoadCommittedFrames(clipID, targetID, paramsVersion, algoVersion string) ([]core.FrameResult, *core.Error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.Query(`SELECT frame_index,state,content_hash,rotations_json,root_json,contact_json,stats_json,diagnostics_json,params_version,algo_version
		FROM checkpoints WHERE clip_id=? AND target_id=? AND params_version=? AND algo_version=? AND state=? ORDER BY frame_index ASC`,
		clipID, targetID, paramsVersion, algoVersion, core.StateCommitted)
	if err != nil {
		return nil, &core.Error{Code: core.ECodeSchema, Message: err.Error()}
	}
	defer rows.Close()
	var out []core.FrameResult
	expected := 0
	for rows.Next() {
		var frame int
		var state, hash, rotationsJSON, rootJSON, contactJSON, statsJSON, diagJSON, pv, av string
		if err := rows.Scan(&frame, &state, &hash, &rotationsJSON, &rootJSON, &contactJSON, &statsJSON, &diagJSON, &pv, &av); err != nil {
			return nil, &core.Error{Code: core.ECodeSchema, Message: err.Error()}
		}
		if frame != expected {
			break
		}
		if pv != paramsVersion || av != algoVersion {
			return nil, &core.Error{Code: "E_VERSION", Frame: frame, Message: "persisted frame version mismatch"}
		}
		r := core.FrameResult{
			ClipID: clipID, TargetID: targetID, Frame: frame, State: state, ContentHash: hash,
			ParamsVersion: pv, AlgoVersion: av,
		}
		if err := json.Unmarshal([]byte(rotationsJSON), &r.Rotations); err != nil {
			return nil, &core.Error{Code: core.ECodeSchema, Frame: frame, Field: "rotations_json", Message: err.Error()}
		}
		if err := json.Unmarshal([]byte(rootJSON), &r.Root); err != nil {
			return nil, &core.Error{Code: core.ECodeSchema, Frame: frame, Field: "root_json", Message: err.Error()}
		}
		if err := json.Unmarshal([]byte(contactJSON), &r.Contacts); err != nil {
			return nil, &core.Error{Code: core.ECodeSchema, Frame: frame, Field: "contact_json", Message: err.Error()}
		}
		if err := json.Unmarshal([]byte(statsJSON), &r.Stats); err != nil {
			return nil, &core.Error{Code: core.ECodeSchema, Frame: frame, Field: "stats_json", Message: err.Error()}
		}
		if err := json.Unmarshal([]byte(diagJSON), &r.Diagnostics); err != nil {
			return nil, &core.Error{Code: core.ECodeSchema, Frame: frame, Field: "diagnostics_json", Message: err.Error()}
		}
		if got := r.ComputeHash(); got != hash {
			return nil, &core.Error{Code: core.ECodeSchema, Frame: frame, Field: "content_hash", Message: "checkpoint hash verification failed", Details: map[string]string{"expected": hash, "actual": got}}
		}
		out = append(out, r)
		expected++
	}
	if err := rows.Err(); err != nil {
		return nil, &core.Error{Code: core.ECodeSchema, Message: err.Error()}
	}
	return out, nil
}

func (s *Store) Count() (int, *core.Error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM checkpoints`).Scan(&count); err != nil {
		return 0, &core.Error{Code: core.ECodeSchema, Message: err.Error()}
	}
	return count, nil
}
