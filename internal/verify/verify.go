// Package verify 是独立复核工具：仅凭 SQLite 快照与真值指纹核对，
// 不 import replaykernel/internal/engine，不调用结算主路径。
package verify

import (
	"database/sql"
	"encoding/json"
	"fmt"

	_ "modernc.org/sqlite"

	"replaykernel/internal/model"
)

// Finding 是首个违规项定位。
type Finding struct {
	TurnNo int
	UnitID string
	Field  string
	Expect string
	Got    string
}

// Report 是复核结论。
type Report struct {
	MatchID string
	OK      bool
	First   *Finding
	Checked int
}

type persistedUnit struct {
	hp       int64
	mp       int64
	alive    bool
	cooldown map[string]int
}

// openRO 以只读方式打开检查点库。
func openRO(path string) (*sql.DB, error) {
	dsn := fmt.Sprintf("file:%s?mode=ro&_pragma=query_only(1)", path)
	return sql.Open("sqlite", dsn)
}

// Verify 读取持久化快照，与真值清单逐字段核对生命值、资源值、冷却与胜负。
func Verify(dbPath string, truth *model.MatchTruth) (*Report, error) {
	db, err := openRO(dbPath)
	if err != nil {
		return nil, err
	}
	defer db.Close()

	rep := &Report{MatchID: truth.MatchID, OK: true}
	for _, want := range truth.Turns {
		rows, err := db.Query(
			`SELECT unit_id,hp,mp,alive,cooldown_json,winner
			 FROM snapshots WHERE match_id=? AND turn_no=? ORDER BY rowid`,
			truth.MatchID, want.TurnNo)
		if err != nil {
			return nil, err
		}
		gotUnits := map[string]persistedUnit{}
		var winner int
		for rows.Next() {
			var uid, cdJSON string
			var hp, mp, alive int64
			if err := rows.Scan(&uid, &hp, &mp, &alive, &cdJSON, &winner); err != nil {
				rows.Close()
				return nil, err
			}
			var cd map[string]int
			if err := json.Unmarshal([]byte(cdJSON), &cd); err != nil {
				rows.Close()
				return nil, err
			}
			gotUnits[uid] = persistedUnit{hp: hp, mp: mp, alive: alive == 1, cooldown: cd}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return nil, err
		}

		if f := compareTurn(want, gotUnits, winner); f != nil {
			rep.OK = false
			rep.First = f
			return rep, nil
		}
		rep.Checked++
	}
	return rep, nil
}

func compareTurn(want model.TurnState, got map[string]persistedUnit, winner int) *Finding {
	if winner != want.Winner {
		return &Finding{TurnNo: want.TurnNo, Field: "winner",
			Expect: fmt.Sprint(want.Winner), Got: fmt.Sprint(winner)}
	}
	for _, u := range want.Units {
		g, ok := got[u.UnitID]
		if !ok {
			return &Finding{TurnNo: want.TurnNo, UnitID: u.UnitID, Field: "row_missing",
				Expect: "present", Got: "absent"}
		}
		if g.hp != u.HP {
			return &Finding{TurnNo: want.TurnNo, UnitID: u.UnitID, Field: "hp_milli",
				Expect: fmt.Sprint(u.HP), Got: fmt.Sprint(g.hp)}
		}
		if g.mp != u.MP {
			return &Finding{TurnNo: want.TurnNo, UnitID: u.UnitID, Field: "mp_milli",
				Expect: fmt.Sprint(u.MP), Got: fmt.Sprint(g.mp)}
		}
		if g.alive != u.Alive {
			return &Finding{TurnNo: want.TurnNo, UnitID: u.UnitID, Field: "alive",
				Expect: fmt.Sprint(u.Alive), Got: fmt.Sprint(g.alive)}
		}
		for skill, cd := range u.Cooldowns {
			if g.cooldown[skill] != cd {
				return &Finding{TurnNo: want.TurnNo, UnitID: u.UnitID, Field: "cooldown." + skill,
					Expect: fmt.Sprint(cd), Got: fmt.Sprint(g.cooldown[skill])}
			}
		}
	}
	return nil
}
