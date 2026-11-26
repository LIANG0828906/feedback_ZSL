package slikernel

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"time"

	_ "modernc.org/sqlite"
)

const (
	checkpointFormatVersion = 1
)

type CheckpointMeta struct {
	FormatVersion  int    `json:"format_version"`
	ProjectID      string `json:"project_id"`
	RuleVersion    string `json:"rule_version"`
	CommittedSeq   int64  `json:"committed_seq"`
	ObservationSeq int64  `json:"observation_seq"`
	BaseCount      int    `json:"base_count"`
	WindowCount    int    `json:"window_count"`
	UpdatedAt      string `json:"updated_at"`
}

type CheckpointSnapshot struct {
	Meta        CheckpointMeta `json:"meta"`
	Report      *Report        `json:"report"`
	BaseWindows []*BaseAgg     `json:"base_windows"`
}

type RecoveryMode string

const (
	RecoveryResume        RecoveryMode = "resume"
	RecoveryFullRecompute RecoveryMode = "full_recompute"
)

type RecoveryInfo struct {
	Mode          RecoveryMode    `json:"mode"`
	Reason        Code            `json:"reason,omitempty"`
	Checkpoint    *CheckpointMeta `json:"checkpoint,omitempty"`
	FullRecompute bool            `json:"full_recompute"`
}

type CheckpointStore struct {
	db   *sql.DB
	path string
}

func OpenCheckpoint(path string) (*CheckpointStore, error) {
	if path == "" {
		return nil, biz(CodeInvalidInput, "open_checkpoint", nil, ctx("field", "path"))
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, checkpointDBError("open_checkpoint", err)
	}
	db.SetMaxOpenConns(1)
	store := &CheckpointStore{db: db, path: path}
	if err := store.initialize(context.Background()); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

func (store *CheckpointStore) Close() error {
	if store == nil || store.db == nil {
		return nil
	}
	return store.db.Close()
}

func (store *CheckpointStore) initialize(ctx context.Context) error {
	statements := []string{
		`PRAGMA busy_timeout = 5000`,
		`PRAGMA journal_mode = WAL`,
		`PRAGMA synchronous = FULL`,
		`CREATE TABLE IF NOT EXISTS checkpoint_meta (
			id INTEGER PRIMARY KEY CHECK (id = 1),
			format_version INTEGER NOT NULL,
			project_id TEXT NOT NULL,
			rule_version TEXT NOT NULL,
			committed_seq INTEGER NOT NULL,
			observation_seq INTEGER NOT NULL,
			base_count INTEGER NOT NULL,
			window_count INTEGER NOT NULL,
			updated_at TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS base_window_checkpoints (
			rule_version TEXT NOT NULL,
			commit_seq INTEGER NOT NULL,
			service TEXT NOT NULL,
			sli TEXT NOT NULL,
			aggregation_revision TEXT NOT NULL,
			base_index INTEGER NOT NULL,
			state_json TEXT NOT NULL,
			PRIMARY KEY (rule_version, service, sli, aggregation_revision, base_index)
		)`,
		`CREATE TABLE IF NOT EXISTS evaluation_window_checkpoints (
			rule_version TEXT NOT NULL,
			commit_seq INTEGER NOT NULL,
			service TEXT NOT NULL,
			sli TEXT NOT NULL,
			aggregation_revision TEXT NOT NULL,
			eval_index INTEGER NOT NULL,
			result_ordinal INTEGER NOT NULL,
			state_json TEXT NOT NULL,
			PRIMARY KEY (rule_version, service, sli, aggregation_revision, eval_index)
		)`,
	}
	for _, statement := range statements {
		if _, err := store.db.ExecContext(ctx, statement); err != nil {
			return checkpointDBError("initialize_checkpoint", err)
		}
	}
	return nil
}

// Save writes base states first, evaluation states second, and only then
// advances checkpoint_meta.committed_seq in the same SQLite transaction. A
// process death before COMMIT leaves the previous committed checkpoint intact.
func (store *CheckpointStore) Save(execCtx context.Context, report *Report, baseWindows []*BaseAgg) (CheckpointMeta, error) {
	if report == nil {
		return CheckpointMeta{}, biz(CodeInvalidInput, "save_checkpoint", nil, ctx("field", "report"))
	}
	if report.RuleVersion == "" {
		return CheckpointMeta{}, biz(CodeInvalidInput, "save_checkpoint", nil, ctx("field", "rule_version"))
	}
	if len(baseWindows) == 0 {
		return CheckpointMeta{}, biz(CodeCheckpointCorrupt, "save_checkpoint", nil,
			ctx("base_windows", len(baseWindows)))
	}
	nextSeq, err := store.nextCommitSeq(execCtx)
	if err != nil {
		return CheckpointMeta{}, err
	}
	tx, err := store.db.BeginTx(execCtx, &sql.TxOptions{Isolation: sql.LevelSerializable})
	if err != nil {
		return CheckpointMeta{}, checkpointDBError("save_checkpoint.begin", err)
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(execCtx, `DELETE FROM base_window_checkpoints WHERE rule_version = ?`, report.RuleVersion); err != nil {
		return CheckpointMeta{}, checkpointDBError("save_checkpoint.clear_base", err)
	}
	if _, err := tx.ExecContext(execCtx, `DELETE FROM evaluation_window_checkpoints WHERE rule_version = ?`, report.RuleVersion); err != nil {
		return CheckpointMeta{}, checkpointDBError("save_checkpoint.clear_eval", err)
	}
	baseInsert, err := tx.PrepareContext(execCtx, `INSERT INTO base_window_checkpoints
		(rule_version, commit_seq, service, sli, aggregation_revision, base_index, state_json)
		VALUES (?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return CheckpointMeta{}, checkpointDBError("save_checkpoint.prepare_base", err)
	}
	defer baseInsert.Close()
	sortedBase := append([]*BaseAgg(nil), baseWindows...)
	sort.Slice(sortedBase, func(i, j int) bool {
		if sortedBase[i].Service != sortedBase[j].Service {
			return sortedBase[i].Service < sortedBase[j].Service
		}
		if sortedBase[i].SLI != sortedBase[j].SLI {
			return sortedBase[i].SLI < sortedBase[j].SLI
		}
		return sortedBase[i].WindowIndex < sortedBase[j].WindowIndex
	})
	for _, base := range sortedBase {
		if base == nil {
			return CheckpointMeta{}, biz(CodeCheckpointCorrupt, "save_checkpoint.base", nil, ctx("field", "nil"))
		}
		encoded, err := json.Marshal(base)
		if err != nil {
			return CheckpointMeta{}, biz(CodeCheckpointCorrupt, "save_checkpoint.base_json", err, nil)
		}
		if _, err := baseInsert.ExecContext(execCtx, report.RuleVersion, nextSeq, base.Service, base.SLI,
			base.AggregationRevision, base.WindowIndex, string(encoded)); err != nil {
			return CheckpointMeta{}, checkpointDBError("save_checkpoint.insert_base", err)
		}
	}
	resultInsert, err := tx.PrepareContext(execCtx, `INSERT INTO evaluation_window_checkpoints
		(rule_version, commit_seq, service, sli, aggregation_revision, eval_index, result_ordinal, state_json)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return CheckpointMeta{}, checkpointDBError("save_checkpoint.prepare_eval", err)
	}
	defer resultInsert.Close()
	for ordinal, result := range report.Results {
		encoded, err := json.Marshal(result)
		if err != nil {
			return CheckpointMeta{}, biz(CodeCheckpointCorrupt, "save_checkpoint.result_json", err, nil)
		}
		if _, err := resultInsert.ExecContext(execCtx, report.RuleVersion, nextSeq, result.Service, result.SLI,
			result.AggregationRevision, result.EvalIndex, ordinal, string(encoded)); err != nil {
			return CheckpointMeta{}, checkpointDBError("save_checkpoint.insert_eval", err)
		}
	}
	meta := CheckpointMeta{
		FormatVersion: checkpointFormatVersion, ProjectID: report.ProjectID,
		RuleVersion: report.RuleVersion, CommittedSeq: nextSeq,
		ObservationSeq: report.CommittedObservationSeq,
		BaseCount:      len(sortedBase), WindowCount: len(report.Results),
		UpdatedAt: time.Now().UTC().Format(time.RFC3339Nano),
	}
	if _, err := tx.ExecContext(execCtx, `INSERT INTO checkpoint_meta
		(id, format_version, project_id, rule_version, committed_seq, observation_seq, base_count, window_count, updated_at)
		VALUES (1, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET
			format_version=excluded.format_version,
			project_id=excluded.project_id,
			rule_version=excluded.rule_version,
			committed_seq=excluded.committed_seq,
			observation_seq=excluded.observation_seq,
			base_count=excluded.base_count,
			window_count=excluded.window_count,
			updated_at=excluded.updated_at`,
		meta.FormatVersion, meta.ProjectID, meta.RuleVersion, meta.CommittedSeq,
		meta.ObservationSeq, meta.BaseCount, meta.WindowCount, meta.UpdatedAt); err != nil {
		return CheckpointMeta{}, checkpointDBError("save_checkpoint.advance_commit", err)
	}
	if err := tx.Commit(); err != nil {
		return CheckpointMeta{}, checkpointDBError("save_checkpoint.commit", err)
	}
	return meta, nil
}

func (store *CheckpointStore) nextCommitSeq(ctx context.Context) (int64, error) {
	var current int64
	err := store.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(committed_seq), 0) FROM checkpoint_meta`).Scan(&current)
	if err != nil {
		return 0, checkpointDBError("save_checkpoint.next_seq", err)
	}
	return current + 1, nil
}

// Load performs strict validation. Any damaged state, orphan commit or
// version mismatch is an error; callers that want recovery semantics should
// use Recover.
func (store *CheckpointStore) Load(execCtx context.Context, expectedRuleVersion string) (*CheckpointSnapshot, error) {
	var integrity string
	if err := store.db.QueryRowContext(execCtx, `PRAGMA quick_check`).Scan(&integrity); err != nil {
		return nil, checkpointDBError("load_checkpoint.integrity", err)
	}
	if integrity != "ok" {
		return nil, biz(CodeCheckpointCorrupt, "load_checkpoint.integrity", nil, ctx("result", integrity))
	}
	var tableCount int
	if err := store.db.QueryRowContext(execCtx,
		`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='checkpoint_meta'`).Scan(&tableCount); err != nil {
		return nil, checkpointDBError("load_checkpoint.table", err)
	}
	if tableCount == 0 {
		return nil, biz(CodeNoCheckpoint, "load_checkpoint", nil, ctx("path", store.path))
	}
	var meta CheckpointMeta
	err := store.db.QueryRowContext(execCtx, `SELECT format_version, project_id, rule_version, committed_seq,
		observation_seq, base_count, window_count, updated_at FROM checkpoint_meta WHERE id=1`).
		Scan(&meta.FormatVersion, &meta.ProjectID, &meta.RuleVersion, &meta.CommittedSeq,
			&meta.ObservationSeq, &meta.BaseCount, &meta.WindowCount, &meta.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, biz(CodeCheckpointCorrupt, "load_checkpoint.meta", nil, ctx("field", "checkpoint_meta"))
	}
	if err != nil {
		return nil, checkpointDBError("load_checkpoint.meta", err)
	}
	if meta.FormatVersion != checkpointFormatVersion {
		return nil, biz(CodeCheckpointVersion, "load_checkpoint", nil,
			ctx2("stored", meta.FormatVersion, "expected", checkpointFormatVersion))
	}
	if expectedRuleVersion != "" && meta.RuleVersion != expectedRuleVersion {
		return nil, biz(CodeCheckpointVersion, "load_checkpoint", nil,
			ctx2("stored", meta.RuleVersion, "expected", expectedRuleVersion))
	}
	var orphaned int
	if err := store.db.QueryRowContext(execCtx, `SELECT
		(SELECT COUNT(*) FROM base_window_checkpoints WHERE rule_version=? AND commit_seq > ?) +
		(SELECT COUNT(*) FROM evaluation_window_checkpoints WHERE rule_version=? AND commit_seq > ?)`,
		meta.RuleVersion, meta.CommittedSeq, meta.RuleVersion, meta.CommittedSeq).Scan(&orphaned); err != nil {
		return nil, checkpointDBError("load_checkpoint.orphan", err)
	}
	if orphaned != 0 {
		return nil, biz(CodeCheckpointCorrupt, "load_checkpoint.orphan", nil, ctx("rows", orphaned))
	}
	baseRows, err := store.loadBaseRows(execCtx, meta)
	if err != nil {
		return nil, err
	}
	resultRows, err := store.loadResultRows(execCtx, meta)
	if err != nil {
		return nil, err
	}
	if len(baseRows) != meta.BaseCount || len(resultRows) != meta.WindowCount {
		return nil, biz(CodeCheckpointCorrupt, "load_checkpoint.counts", nil, map[string]any{
			"base_rows": len(baseRows), "base_expected": meta.BaseCount,
			"window_rows": len(resultRows), "window_expected": meta.WindowCount,
		})
	}
	report := &Report{
		ProjectID: meta.ProjectID, RuleVersion: meta.RuleVersion,
		CommittedObservationSeq: meta.ObservationSeq, Results: make([]WindowResult, 0, len(resultRows)),
	}
	for _, result := range resultRows {
		report.Results = append(report.Results, result)
	}
	return &CheckpointSnapshot{Meta: meta, Report: report, BaseWindows: baseRows}, nil
}

func (store *CheckpointStore) loadBaseRows(execCtx context.Context, meta CheckpointMeta) ([]*BaseAgg, error) {
	rows, err := store.db.QueryContext(execCtx, `SELECT state_json FROM base_window_checkpoints
		WHERE rule_version=? AND commit_seq <= ?
		ORDER BY service, sli, base_index`, meta.RuleVersion, meta.CommittedSeq)
	if err != nil {
		return nil, checkpointDBError("load_checkpoint.base_query", err)
	}
	defer rows.Close()
	var output []*BaseAgg
	for rows.Next() {
		var encoded string
		if err := rows.Scan(&encoded); err != nil {
			return nil, checkpointDBError("load_checkpoint.base_scan", err)
		}
		var base BaseAgg
		if err := json.Unmarshal([]byte(encoded), &base); err != nil {
			return nil, biz(CodeCheckpointCorrupt, "load_checkpoint.base_json", err, ctx("base_index", base.WindowIndex))
		}
		copied := base
		output = append(output, &copied)
	}
	if err := rows.Err(); err != nil {
		return nil, checkpointDBError("load_checkpoint.base_rows", err)
	}
	return output, nil
}

func (store *CheckpointStore) loadResultRows(execCtx context.Context, meta CheckpointMeta) ([]WindowResult, error) {
	rows, err := store.db.QueryContext(execCtx, `SELECT state_json FROM evaluation_window_checkpoints
		WHERE rule_version=? AND commit_seq <= ?
		ORDER BY result_ordinal`, meta.RuleVersion, meta.CommittedSeq)
	if err != nil {
		return nil, checkpointDBError("load_checkpoint.result_query", err)
	}
	defer rows.Close()
	var output []WindowResult
	for rows.Next() {
		var encoded string
		if err := rows.Scan(&encoded); err != nil {
			return nil, checkpointDBError("load_checkpoint.result_scan", err)
		}
		var result WindowResult
		if err := json.Unmarshal([]byte(encoded), &result); err != nil {
			return nil, biz(CodeCheckpointCorrupt, "load_checkpoint.result_json", err,
				ctx("eval_index", result.EvalIndex))
		}
		output = append(output, result)
	}
	if err := rows.Err(); err != nil {
		return nil, checkpointDBError("load_checkpoint.result_rows", err)
	}
	return output, nil
}

// Recover distinguishes a version mismatch (full recompute, no partial reuse)
// from physical checkpoint corruption (hard error).
func (store *CheckpointStore) Recover(ctx context.Context, expectedRuleVersion string) (RecoveryInfo, *CheckpointSnapshot, error) {
	snapshot, err := store.Load(ctx, expectedRuleVersion)
	if err == nil {
		return RecoveryInfo{
			Mode: RecoveryResume, Checkpoint: &snapshot.Meta, FullRecompute: false,
		}, snapshot, nil
	}
	switch ErrorCode(err) {
	case CodeCheckpointVersion, CodeNoCheckpoint:
		return RecoveryInfo{
			Mode: RecoveryFullRecompute, Reason: ErrorCode(err), FullRecompute: true,
		}, nil, nil
	default:
		return RecoveryInfo{}, nil, err
	}
}

func (engine *Engine) SaveCheckpoint(ctx context.Context, store *CheckpointStore) (CheckpointMeta, error) {
	engine.mu.Lock()
	defer engine.mu.Unlock()
	report := engine.buildReport(engine.results)
	bases := make([]*BaseAgg, 0, len(engine.baseCache))
	for _, base := range engine.baseCache {
		bases = append(bases, base)
	}
	return store.Save(ctx, report, bases)
}

// RestoreCheckpoint resumes only when the rule version and committed
// observation sequence both match and the checkpoint covers the exact current
// window set. Otherwise the engine performs a complete recomputation.
func (engine *Engine) RestoreCheckpoint(ctx context.Context, store *CheckpointStore) (RecoveryInfo, error) {
	engine.mu.Lock()
	defer engine.mu.Unlock()
	info, snapshot, err := store.Recover(ctx, engine.ruleVersion)
	if err != nil {
		return RecoveryInfo{}, err
	}
	if snapshot == nil {
		if _, err := engine.computeAllLocked(ctx); err != nil {
			return RecoveryInfo{}, err
		}
		return info, nil
	}
	if snapshot.Meta.ObservationSeq != engine.project.LastObservationSeq() {
		info = RecoveryInfo{
			Mode: RecoveryFullRecompute, Reason: CodeObservationSeq,
			FullRecompute: true, Checkpoint: &snapshot.Meta,
		}
		if _, err := engine.computeAllLocked(ctx); err != nil {
			return RecoveryInfo{}, err
		}
		return info, nil
	}
	baseCache := make(map[baseCacheKey]*BaseAgg, len(snapshot.BaseWindows))
	for _, base := range snapshot.BaseWindows {
		key := baseCacheKey{
			Service: base.Service, SLI: base.SLI, Revision: base.AggregationRevision, Index: base.WindowIndex,
		}
		baseCache[key] = base
	}
	results := make(map[resultCacheKey]*WindowResult, len(snapshot.Report.Results))
	for index := range snapshot.Report.Results {
		result := snapshot.Report.Results[index]
		key := resultCacheKey{
			Service: result.Service, SLI: result.SLI, Revision: result.AggregationRevision, Index: result.EvalIndex,
		}
		results[key] = &result
	}
	if err := engine.verifyCheckpointCoverage(baseCache, results); err != nil {
		return RecoveryInfo{}, err
	}
	engine.baseCache = baseCache
	engine.results = results
	return info, nil
}

func (engine *Engine) verifyCheckpointCoverage(baseCache map[baseCacheKey]*BaseAgg, results map[resultCacheKey]*WindowResult) error {
	baseTasks, err := allBaseTasks(engine.project)
	if err != nil {
		return err
	}
	evalTasks, err := allEvalTasks(engine.project)
	if err != nil {
		return err
	}
	if len(baseCache) != len(baseTasks) || len(results) != len(evalTasks) {
		return biz(CodeCheckpointCorrupt, "restore_checkpoint.coverage", nil, map[string]any{
			"base_cached": len(baseCache), "base_expected": len(baseTasks),
			"eval_cached": len(results), "eval_expected": len(evalTasks),
		})
	}
	for _, task := range baseTasks {
		if baseCache[task.key()] == nil {
			return biz(CodeCheckpointCorrupt, "restore_checkpoint.base_key", nil,
				ctx3("service", task.service, "sli", task.sli.ID, "index", task.index))
		}
	}
	for _, task := range evalTasks {
		if results[task.cacheKey()] == nil {
			return biz(CodeCheckpointCorrupt, "restore_checkpoint.eval_key", nil,
				ctx3("service", task.service, "sli", task.sli.ID, "index", task.evalIndex))
		}
	}
	return nil
}

func checkpointDBError(op string, err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return biz(CodeGracefulInterrupted, op, err, nil)
	}
	message := err.Error()
	if errors.Is(err, sql.ErrConnDone) || errors.Is(err, sql.ErrTxDone) {
		return biz(CodeCheckpointCorrupt, op, err, nil)
	}
	return biz(CodeCheckpointCorrupt, op, err, ctx("detail", boundedDetail(message)))
}

func boundedDetail(value string) string {
	if len(value) <= 240 {
		return value
	}
	return value[:240]
}

func RemoveCheckpointFiles(path string) error {
	for _, candidate := range []string{path, path + "-wal", path + "-shm"} {
		if err := os.Remove(candidate); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

func DescribeCheckpoint(snapshot *CheckpointSnapshot) string {
	if snapshot == nil {
		return "<nil>"
	}
	return fmt.Sprintf("rule=%s seq=%d bases=%d windows=%d",
		snapshot.Meta.RuleVersion, snapshot.Meta.CommittedSeq,
		len(snapshot.BaseWindows), len(snapshot.Report.Results))
}
