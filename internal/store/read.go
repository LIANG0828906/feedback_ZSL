package store

import (
	"database/sql"
	"encoding/json"

	"replaykernel/internal/model"
)

// WindowRow 是 windows 表的一行。
type WindowRow struct {
	WindowNo     int
	FromTurn     int
	ToTurn       int
	Status       string
	ActionOffset int
	RngDraws     int
	RngSeed      int64
	ContentHash  string
	ChainHash    string
	ParamVersion string
	AlgoVersion  string
}

// LoadSnapshots 从持久化层读取一局回放的全部逐回合快照（按回合、单位顺序）。
func (s *Store) LoadSnapshots(matchID string) ([]model.TurnState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	rows, err := s.db.Query(
		`SELECT turn_no,unit_id,hp,mp,alive,cooldown_json,winner
		 FROM snapshots WHERE match_id=? ORDER BY turn_no, rowid`, matchID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var turns []model.TurnState
	idxByTurn := map[int]int{}
	for rows.Next() {
		var turnNo, hp, mp, alive, winner int64
		var unitID, cdJSON string
		if err := rows.Scan(&turnNo, &unitID, &hp, &mp, &alive, &cdJSON, &winner); err != nil {
			return nil, err
		}
		var cd map[string]int
		if err := json.Unmarshal([]byte(cdJSON), &cd); err != nil {
			return nil, err
		}
		idx, ok := idxByTurn[int(turnNo)]
		if !ok {
			idx = len(turns)
			idxByTurn[int(turnNo)] = idx
			turns = append(turns, model.TurnState{TurnNo: int(turnNo), Winner: int(winner)})
		}
		turns[idx].Units = append(turns[idx].Units, model.UnitState{
			UnitID:    unitID,
			HP:        hp,
			MP:        mp,
			Alive:     alive == 1,
			Cooldowns: cd,
		})
	}
	return turns, rows.Err()
}

// LoadRulings 读取全部裁决明细，按写入顺序（seq）还原，保证可逐条重放。
func (s *Store) LoadRulings(matchID string) ([]model.Ruling, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	rows, err := s.db.Query(
		`SELECT turn_no,unit_id,skill_id,violations_json,reason,allowed
		 FROM rulings WHERE match_id=? ORDER BY window_no, seq`, matchID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []model.Ruling
	for rows.Next() {
		var r model.Ruling
		var vJSON, reason string
		var allowed int
		if err := rows.Scan(&r.TurnNo, &r.UnitID, &r.SkillID, &vJSON, &reason, &allowed); err != nil {
			return nil, err
		}
		if err := json.Unmarshal([]byte(vJSON), &r.Violations); err != nil {
			return nil, err
		}
		r.Reason = model.Violation(reason)
		r.Allowed = allowed == 1
		out = append(out, r)
	}
	return out, rows.Err()
}

// LoadWindows 读取窗口记录并校验哈希链与版本指纹。
func (s *Store) LoadWindows(matchID string) ([]WindowRow, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	rows, err := s.db.Query(
		`SELECT window_no,from_turn,to_turn,status,action_offset,rng_draws,rng_seed,
		        content_hash,chain_hash,param_version,algo_version
		 FROM windows WHERE match_id=? ORDER BY window_no`, matchID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []WindowRow
	prev := RootChain
	for rows.Next() {
		var w WindowRow
		var status string
		if err := rows.Scan(&w.WindowNo, &w.FromTurn, &w.ToTurn, &status,
			&w.ActionOffset, &w.RngDraws, &w.RngSeed, &w.ContentHash,
			&w.ChainHash, &w.ParamVersion, &w.AlgoVersion); err != nil {
			return nil, err
		}
		w.Status = status
		if status == string(model.WindowCommitted) {
			want := ChainHash(prev, w.ContentHash)
			if want != w.ChainHash {
				return nil, model.NewError(model.EState,
					"checkpoint hash chain broken at window %d", w.WindowNo).WithWindow(w.WindowNo)
			}
			prev = w.ChainHash
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// LoadDecision 读取自适应决策记录。
func (s *Store) LoadDecision(matchID string) (model.Decision, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	row := s.db.QueryRow(
		`SELECT match_id,algorithm,window_size,turns,units,est_mem,mem_budget,est_time,time_budget,basis
		 FROM decisions WHERE match_id=?`, matchID)
	var d model.Decision
	var algo string
	err := row.Scan(&d.MatchID, &algo, &d.WindowSize, &d.Turns, &d.Units,
		&d.EstMemBytes, &d.MemBudget, &d.EstTimeMicros, &d.TimeBudget, &d.Basis)
	if err == sql.ErrNoRows {
		return d, model.NewError(model.EState, "decision record missing for %s", matchID)
	}
	d.Algorithm = model.Algorithm(algo)
	return d, err
}
