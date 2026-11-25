package review

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"sort"

	_ "modernc.org/sqlite"

	"battlekernel/internal/canonical"
	"battlekernel/internal/model"
)

type Violation struct {
	Round    int64  `json:"round,omitempty"`
	Field    string `json:"field"`
	Expected string `json:"expected"`
	Got      string `json:"got"`
	Source   string `json:"source"`
}

type Report struct {
	OK                  bool       `json:"ok"`
	DBPath              string     `json:"db_path"`
	ProjectID           string     `json:"project_id"`
	RoundsChecked       int        `json:"rounds_checked"`
	SnapshotHashesOK    bool       `json:"snapshot_hashes_ok"`
	TruthFingerprintsOK bool       `json:"truth_fingerprints_ok"`
	Violation           *Violation `json:"violation,omitempty"`
}

type fingerprintFile struct {
	Projects []FingerprintProject `json:"projects"`
}

type FingerprintProject struct {
	ProjectID string   `json:"project_id"`
	Truth     string   `json:"truth_fingerprint"`
	Rounds    []string `json:"round_fingerprints"`
}

func ReviewDatabase(ctx context.Context, dbPath, fingerprintPath, projectID string) (*Report, error) {
	fpData, err := os.ReadFile(fingerprintPath)
	if err != nil {
		return nil, fmt.Errorf("read truth fingerprints: %w", err)
	}
	var fingerprints fingerprintFile
	if err := json.Unmarshal(fpData, &fingerprints); err != nil {
		return nil, fmt.Errorf("decode truth fingerprints: %w", err)
	}

	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	if _, err := db.ExecContext(ctx, `PRAGMA query_only=ON`); err != nil {
		return nil, err
	}

	if projectID == "" {
		if err := db.QueryRowContext(ctx, `SELECT project_id FROM meta WHERE key='project_json' LIMIT 1`).Scan(&projectID); err != nil {
			return nil, fmt.Errorf("select project: %w", err)
		}
	}
	report := &Report{DBPath: dbPath, ProjectID: projectID, OK: true, SnapshotHashesOK: true, TruthFingerprintsOK: true}

	var projectJSON string
	if err := db.QueryRowContext(ctx, `SELECT value FROM meta WHERE project_id=? AND key='project_json'`, projectID).Scan(&projectJSON); err != nil {
		return nil, fmt.Errorf("load persisted project envelope: %w", err)
	}
	var project model.Project
	if err := json.Unmarshal([]byte(projectJSON), &project); err != nil {
		return nil, fmt.Errorf("decode persisted project envelope: %w", err)
	}
	project.Truth = nil

	rows, err := db.QueryContext(ctx, `SELECT round,result_json,content_hash FROM rounds WHERE project_id=? AND status='committed' ORDER BY round ASC`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var snapshots []model.RoundResult
	var storedHashes []string
	for rows.Next() {
		var round int64
		var data, hash string
		if err := rows.Scan(&round, &data, &hash); err != nil {
			return nil, err
		}
		var snapshot model.RoundResult
		if err := json.Unmarshal([]byte(data), &snapshot); err != nil {
			return fail(report, &Violation{Round: round, Field: "snapshot.result_json", Expected: "valid JSON", Got: err.Error(), Source: "sqlite"}), nil
		}
		calculatedHash, err := hashResult(snapshot)
		if err != nil {
			return nil, err
		}
		if hash != calculatedHash || snapshot.ContentHash != calculatedHash {
			report.SnapshotHashesOK = false
			return fail(report, &Violation{Round: round, Field: "snapshot.content_hash", Expected: calculatedHash, Got: hash, Source: "sqlite"}), nil
		}
		snapshots = append(snapshots, snapshot)
		storedHashes = append(storedHashes, hash)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	var expectedFP *FingerprintProject
	for i := range fingerprints.Projects {
		if fingerprints.Projects[i].ProjectID == projectID {
			fp := fingerprints.Projects[i]
			expectedFP = &fp
			break
		}
	}
	if expectedFP == nil {
		return nil, fmt.Errorf("no truth fingerprint for project %s", projectID)
	}

	states := initialStates(&project)
	if len(snapshots) != len(project.Rounds) {
		return fail(report, &Violation{Field: "rounds.length", Expected: fmt.Sprint(len(project.Rounds)), Got: fmt.Sprint(len(snapshots)), Source: "sqlite"}), nil
	}

	var truthRoundHashes []string
	damage := 0
	for i, spec := range project.Rounds {
		calcResult, truthRound, nextStates, roundDamage, err := independentSettle(&project, spec, states)
		if err != nil {
			return fail(report, &Violation{Round: spec.Round, Field: "independent_calculation", Expected: "success", Got: err.Error(), Source: "review-calculator"}), nil
		}
		states = nextStates
		damage += roundDamage
		report.RoundsChecked++

		snapshot := snapshots[i]
		if violation := compareSnapshot(spec.Round, snapshot, calcResult); violation != nil {
			return fail(report, violation), nil
		}
		truthHash, err := canonical.JSONHash(truthRound)
		if err != nil {
			return nil, err
		}
		truthRoundHashes = append(truthRoundHashes, truthHash)
		if i >= len(expectedFP.Rounds) || expectedFP.Rounds[i] != truthHash {
			report.TruthFingerprintsOK = false
			expected := ""
			if i < len(expectedFP.Rounds) {
				expected = expectedFP.Rounds[i]
			}
			return fail(report, &Violation{Round: spec.Round, Field: fmt.Sprintf("truth_fingerprint.rounds[%d]", i), Expected: expected, Got: truthHash, Source: "truth-fingerprint"}), nil
		}
	}

	winner := "defender"
	if damage >= project.EnemyHP {
		winner = "attacker"
	}
	topHash, err := canonical.JSONHash(struct {
		Version string   `json:"version"`
		ID      string   `json:"id"`
		Rounds  []string `json:"rounds"`
		Winner  string   `json:"winner"`
	}{model.TruthFingerprintVer, project.ID, truthRoundHashes, winner})
	if err != nil {
		return nil, err
	}
	if topHash != expectedFP.Truth {
		report.TruthFingerprintsOK = false
		return fail(report, &Violation{Field: "truth_fingerprint", Expected: expectedFP.Truth, Got: topHash, Source: "truth-fingerprint"}), nil
	}
	_ = storedHashes
	return report, nil
}

func fail(report *Report, violation *Violation) *Report {
	report.OK = false
	report.Violation = violation
	return report
}

func hashResult(result model.RoundResult) (string, error) {
	result.ContentHash = ""
	return canonical.JSONHash(result)
}

func initialStates(p *model.Project) map[string]model.UnitState {
	out := map[string]model.UnitState{}
	for _, state := range p.InitialStates {
		state.Alive = true
		out[state.UnitID] = state
	}
	return out
}

func independentSettle(p *model.Project, spec model.RoundSpec, incoming map[string]model.UnitState) (model.RoundResult, model.RoundTruth, map[string]model.UnitState, int, error) {
	states := map[string]model.UnitState{}
	for k, v := range incoming {
		states[k] = v
	}
	result := model.RoundResult{Round: spec.Round, TimestampMs: spec.TimestampMs, Status: "committed"}
	truth := model.RoundTruth{Round: spec.Round, TimestampMs: spec.TimestampMs}
	frozen := map[string]bool{}

	for _, action := range spec.Actions {
		skill, ok := p.Skill(action.SkillID)
		if !ok {
			return result, truth, nil, 0, fmt.Errorf("unknown skill %s", action.SkillID)
		}
		state := states[action.UnitID]
		if skill.Conditional && (action.Flag == nil || !*action.Flag) {
			entry := model.UnqueuedEntry{Round: spec.Round, UnitID: action.UnitID, SkillID: action.SkillID, Reason: "flag_false", Detail: "normalized condition flag is false"}
			result.Unqueued = append(result.Unqueued, entry)
			truth.Unqueued = append(truth.Unqueued, entry)
			frozen[action.UnitID] = true
			continue
		}
		if state.CooldownRemaining > 0 {
			detail := fmt.Sprintf("cooldown_remaining=%d", state.CooldownRemaining)
			entry := model.UnqueuedEntry{Round: spec.Round, UnitID: action.UnitID, SkillID: action.SkillID, Reason: "cooldown_wait", Detail: detail}
			result.Unqueued = append(result.Unqueued, entry)
			truth.Unqueued = append(truth.Unqueued, entry)
			continue
		}
		weight := activeWeight(p, action.UnitID, spec.Round)
		tie := activeTie(p, action.UnitID, spec.Round)
		entry := model.QueueEntry{Round: spec.Round, UnitID: action.UnitID, SkillID: action.SkillID, Weight: weight, TieOrdinal: tie,
			Basis: fmt.Sprintf("weight=%d:desc;tie_ordinal=%d:asc;unit_id=%s:asc;skill_id=%s:asc", weight, tie, action.UnitID, action.SkillID)}
		result.Queue = append(result.Queue, entry)
		truth.Order = append(truth.Order, entry)
	}
	sortQueue(result.Queue)
	sortQueue(truth.Order)
	for i := range result.Queue {
		result.Queue[i].Position = i + 1
		truth.Order[i].Position = i + 1
	}
	sortUnqueued(result.Unqueued)
	sortUnqueued(truth.Unqueued)

	cast := map[string]bool{}
	castPosition := 0
	for _, entry := range result.Queue {
		skill, _ := p.Skill(entry.SkillID)
		state := states[entry.UnitID]
		record := model.ActionRecord{QueuePosition: entry.Position, Round: spec.Round, UnitID: entry.UnitID, SkillID: entry.SkillID,
			Weight: entry.Weight, TieOrdinal: entry.TieOrdinal, ResourceBefore: state.Resource, CooldownBefore: state.CooldownRemaining, Power: skill.Power}
		if state.Resource < skill.Cost {
			record.Position = len(result.Settlements) + 1
			record.Decision = "skipped_resource"
			record.Reason = fmt.Sprintf("resource %d < cost %d; no resource consumed", state.Resource, skill.Cost)
			record.ResourceAfter = state.Resource
			record.CooldownAfter = state.CooldownRemaining
		} else {
			state.Resource -= skill.Cost
			state.CooldownRemaining = skill.Cooldown
			record.Position = len(result.Settlements) + 1
			record.Decision = "cast"
			record.Damage = skill.Power
			record.ResourceAfter = state.Resource
			record.CooldownAfter = state.CooldownRemaining
			result.TotalDamage += skill.Power
			truth.TotalDamage += skill.Power
			cast[entry.UnitID] = true
			castPosition++
			truthRecord := record
			truthRecord.Position = castPosition
			truth.Casts = append(truth.Casts, truthRecord)
		}
		result.Settlements = append(result.Settlements, record)
		if record.Decision == "cast" {
			actual := record
			actual.Position = castPosition
			result.ActionOrder = append(result.ActionOrder, actual)
		}
		states[entry.UnitID] = state
	}

	for _, unit := range p.Units {
		state := states[unit.ID]
		state.Resource += unit.Regen
		if state.Resource > unit.MaxResource {
			state.Resource = unit.MaxResource
		}
		if !frozen[unit.ID] && !cast[unit.ID] && state.CooldownRemaining > 0 {
			state.CooldownRemaining--
		}
		states[unit.ID] = state
	}

	result.States = sortedStates(states)
	truth.States = sortedStates(states)
	for i := range truth.Casts {
		state := states[truth.Casts[i].UnitID]
		truth.Casts[i].ResourceAfter = state.Resource
		truth.Casts[i].CooldownAfter = state.CooldownRemaining
	}
	return result, truth, states, result.TotalDamage, nil
}

func activeWeight(p *model.Project, unitID string, round int64) int {
	unit, _ := p.Unit(unitID)
	value := unit.BaseWeight
	for _, edit := range p.WeightEdits {
		if edit.UnitID == unitID && round >= edit.FromRound {
			value = edit.Weight
		}
	}
	return value
}

func activeTie(p *model.Project, unitID string, round int64) int {
	unit, _ := p.Unit(unitID)
	value := unit.TieOrdinal
	activeOffset := -1
	for _, command := range p.Reorders {
		if command.UnitID == unitID && round > command.AfterRound && command.Offset > activeOffset {
			value = command.NewTieOrdinal
			activeOffset = command.Offset
		}
	}
	return value
}

func sortQueue(entries []model.QueueEntry) {
	sort.SliceStable(entries, func(i, j int) bool {
		a, b := entries[i], entries[j]
		if a.Weight != b.Weight {
			return a.Weight > b.Weight
		}
		if a.TieOrdinal != b.TieOrdinal {
			return a.TieOrdinal < b.TieOrdinal
		}
		if a.UnitID != b.UnitID {
			return a.UnitID < b.UnitID
		}
		return a.SkillID < b.SkillID
	})
}

func sortUnqueued(entries []model.UnqueuedEntry) {
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].UnitID != entries[j].UnitID {
			return entries[i].UnitID < entries[j].UnitID
		}
		return entries[i].SkillID < entries[j].SkillID
	})
}

func sortedStates(states map[string]model.UnitState) []model.UnitState {
	out := make([]model.UnitState, 0, len(states))
	for _, state := range states {
		out = append(out, state)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UnitID < out[j].UnitID })
	return out
}

func compareSnapshot(round int64, snapshot, calculated model.RoundResult) *Violation {
	if len(snapshot.Queue) != len(calculated.Queue) {
		return &Violation{Round: round, Field: "queue.length", Expected: fmt.Sprint(len(calculated.Queue)), Got: fmt.Sprint(len(snapshot.Queue)), Source: "snapshot"}
	}
	for i := range calculated.Queue {
		a, b := snapshot.Queue[i], calculated.Queue[i]
		if a.Position != b.Position || a.UnitID != b.UnitID || a.SkillID != b.SkillID {
			return &Violation{Round: round, Field: fmt.Sprintf("queue[%d]", i), Expected: fmt.Sprintf("%d:%s:%s", b.Position, b.UnitID, b.SkillID), Got: fmt.Sprintf("%d:%s:%s", a.Position, a.UnitID, a.SkillID), Source: "snapshot"}
		}
	}
	if len(snapshot.ActionOrder) != len(calculated.ActionOrder) {
		return &Violation{Round: round, Field: "action_order.length", Expected: fmt.Sprint(len(calculated.ActionOrder)), Got: fmt.Sprint(len(snapshot.ActionOrder)), Source: "snapshot"}
	}
	for i := range calculated.ActionOrder {
		a, b := snapshot.ActionOrder[i], calculated.ActionOrder[i]
		if a.Position != b.Position || a.UnitID != b.UnitID || a.SkillID != b.SkillID {
			return &Violation{Round: round, Field: fmt.Sprintf("action_order[%d]", i), Expected: fmt.Sprintf("%d:%s:%s", b.Position, b.UnitID, b.SkillID), Got: fmt.Sprintf("%d:%s:%s", a.Position, a.UnitID, a.SkillID), Source: "snapshot"}
		}
	}
	stateMap := model.StateMap(calculated.States)
	for _, snapshotState := range snapshot.States {
		calculatedState := stateMap[snapshotState.UnitID]
		if snapshotState.CooldownRemaining != calculatedState.CooldownRemaining {
			return &Violation{Round: round, Field: fmt.Sprintf("states[%s].cooldown_remaining", snapshotState.UnitID), Expected: fmt.Sprint(calculatedState.CooldownRemaining), Got: fmt.Sprint(snapshotState.CooldownRemaining), Source: "snapshot"}
		}
		if snapshotState.Resource != calculatedState.Resource {
			return &Violation{Round: round, Field: fmt.Sprintf("states[%s].resource", snapshotState.UnitID), Expected: fmt.Sprint(calculatedState.Resource), Got: fmt.Sprint(snapshotState.Resource), Source: "snapshot"}
		}
	}
	return nil
}
