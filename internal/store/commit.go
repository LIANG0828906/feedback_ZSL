package store

import (
	"database/sql"
	"encoding/json"
	"fmt"

	"replaykernel/internal/model"
)

// MetaRecord 是一局回放的检查点元信息。
type MetaRecord struct {
	MatchID      string
	ReplayHash   string
	TruthHash    string
	ParamVersion string
	AlgoVersion  string
	Seed         int64
	WindowSize   int
	Algorithm    model.Algorithm
}

// InitMatch 写入对局元信息与自适应决策；若已存在则校验回放哈希与版本指纹。
func (s *Store) InitMatch(m MetaRecord, d model.Decision) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	existing, err := readMeta(tx, m.MatchID)
	if err != nil && err != sql.ErrNoRows {
		return err
	}
	if err == nil {
		if existing.ReplayHash != m.ReplayHash ||
			existing.TruthHash != m.TruthHash ||
			existing.ParamVersion != m.ParamVersion ||
			existing.AlgoVersion != m.AlgoVersion {
			return model.NewError(model.EState,
				"stale checkpoint: hash/version mismatch, full invalidation required")
		}
		return nil
	}

	if _, err := tx.Exec(
		`INSERT INTO meta(match_id,replay_hash,truth_hash,param_version,algo_version,seed,window_size,algorithm)
		 VALUES(?,?,?,?,?,?,?,?)`,
		m.MatchID, m.ReplayHash, m.TruthHash, m.ParamVersion, m.AlgoVersion,
		m.Seed, m.WindowSize, string(m.Algorithm)); err != nil {
		return err
	}
	if _, err := tx.Exec(
		`INSERT INTO decisions(match_id,algorithm,window_size,turns,units,est_mem,mem_budget,est_time,time_budget,basis)
		 VALUES(?,?,?,?,?,?,?,?,?,?)`,
		d.MatchID, string(d.Algorithm), d.WindowSize, d.Turns, d.Units,
		d.EstMemBytes, d.MemBudget, d.EstTimeMicros, d.TimeBudget, d.Basis); err != nil {
		return err
	}
	return tx.Commit()
}

func readMeta(tx *sql.Tx, matchID string) (MetaRecord, error) {
	return queryMeta(tx, matchID)
}

type rowQuerier interface {
	QueryRow(query string, args ...any) *sql.Row
}

func queryMeta(q rowQuerier, matchID string) (MetaRecord, error) {
	row := q.QueryRow(
		`SELECT match_id,replay_hash,truth_hash,param_version,algo_version,seed,window_size,algorithm
		 FROM meta WHERE match_id=?`, matchID)
	var m MetaRecord
	var algo string
	err := row.Scan(&m.MatchID, &m.ReplayHash, &m.TruthHash, &m.ParamVersion,
		&m.AlgoVersion, &m.Seed, &m.WindowSize, &algo)
	m.Algorithm = model.Algorithm(algo)
	return m, err
}

// ReadMeta 供恢复路径读取元信息。
func (s *Store) ReadMeta(matchID string) (MetaRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return queryMeta(s.db, matchID)
}

// CommitWindow 在单个事务内原子写入一个窗口的全部快照、裁决与窗口记录。
// prevChain 为上一窗口的链哈希（首窗口传 RootChain）。
func (s *Store) CommitWindow(m MetaRecord, report *model.WindowReport, prevChain string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.Begin()
	if err != nil {
		return "", err
	}
	defer func() { _ = tx.Rollback() }()

	// 顺序稳定：窗口必须按编号连续提交，禁止重叠/乱序。
	var cnt int
	if err := tx.QueryRow(
		`SELECT COUNT(*) FROM windows WHERE match_id=?`, m.MatchID).Scan(&cnt); err != nil {
		return "", err
	}
	if cnt != report.WindowNo {
		return "", model.NewError(model.EState,
			"out-of-order window commit: expected window %d got %d",
			cnt, report.WindowNo).WithWindow(report.WindowNo)
	}

	chain := ChainHash(prevChain, report.ContentHash)
	for _, snap := range report.Snapshots {
		for _, u := range snap.Units {
			cdJSON, err := json.Marshal(u.Cooldowns)
			if err != nil {
				return "", err
			}
			alive := 0
			if u.Alive {
				alive = 1
			}
			if _, err := tx.Exec(
				`INSERT INTO snapshots(match_id,turn_no,unit_id,hp,mp,alive,cooldown_json,winner)
				 VALUES(?,?,?,?,?,?,?,?)`,
				m.MatchID, snap.TurnNo, u.UnitID, u.HP, u.MP, alive, string(cdJSON), snap.Winner); err != nil {
				return "", err
			}
		}
	}

	for seq, r := range report.Rulings {
		vJSON, err := json.Marshal(r.Violations)
		if err != nil {
			return "", err
		}
		if _, err := tx.Exec(
			`INSERT INTO rulings(match_id,window_no,turn_no,unit_id,skill_id,violations_json,reason,allowed,seq)
			 VALUES(?,?,?,?,?,?,?,?,?)`,
			m.MatchID, report.WindowNo, r.TurnNo, r.UnitID, r.SkillID,
			string(vJSON), string(r.Reason), boolInt(r.Allowed), seq); err != nil {
			return "", err
		}
	}

	if _, err := tx.Exec(
		`INSERT INTO windows(match_id,window_no,from_turn,to_turn,status,action_offset,rng_draws,rng_seed,content_hash,chain_hash,param_version,algo_version)
		 VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`,
		m.MatchID, report.WindowNo, report.FromTurn, report.ToTurn, string(model.WindowCommitted),
		report.ActionOffset, report.RngDraws, report.RngSeed, report.ContentHash, chain,
		m.ParamVersion, m.AlgoVersion); err != nil {
		return "", err
	}

	if _, err := tx.Exec(
		`INSERT INTO transitions(match_id,window_no,from_status,to_status,seq)
		 VALUES(?,?,?,?,?)`,
		m.MatchID, report.WindowNo, string(model.WindowRunning), string(model.WindowCommitted), report.WindowNo); err != nil {
		return "", err
	}

	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("commit window %d: %w", report.WindowNo, err)
	}
	return chain, nil
}

// MarkFailed 记录失败窗口迁移（running -> failed），不写入任何快照。
func (s *Store) MarkFailed(matchID string, windowNo int, code string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(
		`INSERT INTO transitions(match_id,window_no,from_status,to_status,seq)
		 VALUES(?,?,?,?,?)`,
		matchID, windowNo, string(model.WindowRunning), string(model.WindowFailed), windowNo); err != nil {
		return err
	}
	if _, err := tx.Exec(
		`INSERT OR REPLACE INTO windows(match_id,window_no,from_turn,to_turn,status,action_offset,rng_draws,rng_seed,content_hash,chain_hash,param_version,algo_version)
		 VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`,
		matchID, windowNo, 0, 0, string(model.WindowFailed), 0, 0, 0, code, "",
		model.ParamVersion, model.AlgoVersion); err != nil {
		return err
	}
	return tx.Commit()
}

// CommittedWindows 返回已提交窗口编号列表（升序）。
func (s *Store) CommittedWindows(matchID string) ([]int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, err := s.db.Query(
		`SELECT window_no FROM windows WHERE match_id=? AND status=? ORDER BY window_no`,
		matchID, string(model.WindowCommitted))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int
	for rows.Next() {
		var n int
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// Invalidate 整体作废某对局全部检查点（哈希/版本不匹配时调用，禁止部分混用）。
func (s *Store) Invalidate(matchID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	for _, table := range []string{"snapshots", "rulings", "windows", "transitions", "decisions", "meta"} {
		if _, err := tx.Exec(fmt.Sprintf("DELETE FROM %s WHERE match_id=?", table), matchID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// AppendRuling 独立写入一条裁决明细（用于 E_CONFLICT 链路在窗口失败前留痕）。
func (s *Store) AppendRuling(matchID string, windowNo int, r model.Ruling) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	var seq int
	if err := s.db.QueryRow(
		`SELECT COALESCE(MAX(seq)+1,0) FROM rulings WHERE match_id=? AND window_no=?`,
		matchID, windowNo).Scan(&seq); err != nil {
		return err
	}
	vJSON, err := json.Marshal(r.Violations)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(
		`INSERT INTO rulings(match_id,window_no,turn_no,unit_id,skill_id,violations_json,reason,allowed,seq)
		 VALUES(?,?,?,?,?,?,?,?,?)`,
		matchID, windowNo, r.TurnNo, r.UnitID, r.SkillID,
		string(vJSON), string(r.Reason), boolInt(r.Allowed), seq)
	return err
}

// PurgeFailedWindow 删除某失败窗口的残留行，使窗口可按原编号干净重试。
// 仅允许清理非 committed 窗口；已提交窗口受冻结保护，拒绝删除。
func (s *Store) PurgeFailedWindow(matchID string, windowNo int) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	var status string
	err = tx.QueryRow(
		`SELECT status FROM windows WHERE match_id=? AND window_no=?`,
		matchID, windowNo).Scan(&status)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		return err
	}
	if status == string(model.WindowCommitted) {
		return model.NewError(model.EState,
			"cannot purge committed window %d: frozen", windowNo).WithWindow(windowNo)
	}
	for _, table := range []string{"windows", "transitions"} {
		if _, err := tx.Exec(
			fmt.Sprintf("DELETE FROM %s WHERE match_id=? AND window_no=?", table),
			matchID, windowNo); err != nil {
			return err
		}
	}
	return tx.Commit()
}
