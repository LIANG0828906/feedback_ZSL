package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"driftconverge/internal/model"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

var ErrNotFound = errors.New("round status not found")
var ErrRoundExists = errors.New("round id already committed")

type Store struct {
	pool *pgxpool.Pool
}

func Open(ctx context.Context, databaseURL string) (*Store, error) {
	config, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		return nil, fmt.Errorf("parse database URL: %w", err)
	}
	config.MaxConns = 4
	config.MinConns = 1
	config.MaxConnLifetime = 30 * time.Minute
	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}
	return &Store{pool: pool}, nil
}

func (s *Store) Close() { s.pool.Close() }

func (s *Store) Migrate(ctx context.Context) error {
	if _, err := s.pool.Exec(ctx, schema); err != nil {
		return fmt.Errorf("apply schema: %w", err)
	}
	return nil
}

func (s *Store) CommitRound(ctx context.Context, report model.RoundReport) error {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return fmt.Errorf("begin round transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()

	if _, err := tx.Exec(
		ctx,
		`INSERT INTO drift_rounds (round_id, started_at, committed_at, duration_ms, status)
		 VALUES ($1, $2, $3, $4, 'committed')`,
		report.RoundID,
		report.StartedAt,
		report.CommittedAt,
		report.DurationMS,
	); err != nil {
		if isUniqueViolation(err) {
			return ErrRoundExists
		}
		return fmt.Errorf("insert round: %w", err)
	}

	for _, cluster := range report.Clusters {
		model.SortDifferences(cluster.Differences)
		model.SortPolicyBlocks(cluster.PolicyBlocks)
		if cluster.Differences == nil {
			cluster.Differences = []model.Difference{}
		}
		if cluster.PolicyBlocks == nil {
			cluster.PolicyBlocks = []model.PolicyBlock{}
		}
		if cluster.ApplyResults == nil {
			cluster.ApplyResults = []model.ApplyResult{}
		}
		if _, err := tx.Exec(
			ctx,
			`INSERT INTO drift_round_clusters (
			   round_id, cluster, branch, git_commit, observable, comparison_success, expected_count,
			   diff_count, policy_block_count, blocked_count, converged_resources,
			   convergence, error_code, error_message, error_resource_key, error_field,
			   error_source_branch, last_observed_round_id
			 ) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17,$18)`,
			report.RoundID,
			cluster.Cluster,
			cluster.Branch,
			cluster.GitCommit,
			cluster.Observable,
			cluster.ComparisonSuccess,
			cluster.ExpectedCount,
			cluster.DiffCount,
			cluster.PolicyBlockCount,
			cluster.BlockedCount,
			cluster.ConvergedResources,
			cluster.Convergence,
			cluster.ErrorCode,
			cluster.ErrorMessage,
			cluster.ErrorResourceKey,
			cluster.ErrorField,
			cluster.ErrorSourceBranch,
			cluster.LastObservedRoundID,
		); err != nil {
			return fmt.Errorf("insert cluster %s: %w", cluster.Cluster, err)
		}
		for _, item := range cluster.Differences {
			expectedJSON, marshalErr := marshalJSON(item.Expected)
			if marshalErr != nil {
				return fmt.Errorf("marshal difference expected value: %w", marshalErr)
			}
			actualJSON, marshalErr := marshalJSON(item.Actual)
			if marshalErr != nil {
				return fmt.Errorf("marshal difference actual value: %w", marshalErr)
			}
			if _, err := tx.Exec(
				ctx,
				`INSERT INTO drift_differences (
				   round_id, cluster, resource_key, field, component, env_key,
				   expected, actual, resolution, reason
				 ) VALUES ($1,$2,$3,$4,$5,$6,$7::jsonb,$8::jsonb,$9,$10)`,
				report.RoundID,
				cluster.Cluster,
				item.ResourceKey,
				item.Field,
				item.Component,
				item.EnvKey,
				string(expectedJSON),
				string(actualJSON),
				item.Resolution,
				item.Reason,
			); err != nil {
				return fmt.Errorf("insert difference for %s: %w", item.ResourceKey, err)
			}
		}
		for _, item := range cluster.PolicyBlocks {
			valueJSON, marshalErr := marshalJSON(item.Value)
			if marshalErr != nil {
				return fmt.Errorf("marshal policy value: %w", marshalErr)
			}
			if _, err := tx.Exec(
				ctx,
				`INSERT INTO drift_policy_blocks (
				   round_id, cluster, resource_key, field, value, source_branch,
				   declared_in_branch, reason
				 ) VALUES ($1,$2,$3,$4,$5::jsonb,$6,$7,$8)`,
				report.RoundID,
				cluster.Cluster,
				item.ResourceKey,
				item.Field,
				string(valueJSON),
				item.SourceBranch,
				item.DeclaredInBranch,
				item.Reason,
			); err != nil {
				return fmt.Errorf("insert policy block for %s: %w", item.ResourceKey, err)
			}
		}
		for _, item := range cluster.ApplyResults {
			if _, err := tx.Exec(
				ctx,
				`INSERT INTO drift_apply_results (
				   round_id, cluster, resource_key, action, status, error_code, message, applied_at
				 ) VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
				report.RoundID,
				cluster.Cluster,
				item.ResourceKey,
				item.Action,
				item.Status,
				item.ErrorCode,
				item.Message,
				item.AppliedAt,
			); err != nil {
				return fmt.Errorf("insert apply result for %s: %w", item.ResourceKey, err)
			}
		}
	}

	if err := tx.Commit(ctx); err != nil {
		if isUniqueViolation(err) {
			return ErrRoundExists
		}
		return fmt.Errorf("commit round: %w", err)
	}
	return nil
}

func (s *Store) LatestClusterStatus(ctx context.Context, cluster string) (model.ClusterStatus, error) {
	return s.queryClusterStatus(ctx, cluster, false)
}

func (s *Store) LastObservable(ctx context.Context, cluster string) (model.PreviousObservation, error) {
	status, err := s.queryClusterStatus(ctx, cluster, true)
	if err != nil {
		return model.PreviousObservation{}, err
	}
	return model.PreviousObservation{
		RoundID:            status.RoundID,
		Differences:        status.Differences,
		DiffCount:          status.DiffCount,
		ConvergedResources: status.ConvergedResources,
	}, nil
}

func (s *Store) queryClusterStatus(ctx context.Context, cluster string, onlyObservable bool) (model.ClusterStatus, error) {
	query := `SELECT rc.round_id, rc.cluster, rc.branch, rc.git_commit, r.committed_at,
	                 rc.observable, rc.comparison_success, rc.expected_count, rc.diff_count, rc.policy_block_count,
	                 rc.blocked_count, rc.converged_resources, rc.convergence,
	                 rc.error_code, rc.error_message, rc.error_resource_key, rc.error_field,
	                 rc.error_source_branch, rc.last_observed_round_id
	          FROM drift_round_clusters rc
	          JOIN drift_rounds r ON r.round_id = rc.round_id
	          WHERE rc.cluster = $1`
	if onlyObservable {
		query += ` AND rc.observable = true AND rc.comparison_success = true`
	}
	query += ` ORDER BY r.committed_at DESC, rc.round_id DESC LIMIT 1`

	var status model.ClusterStatus
	err := s.pool.QueryRow(ctx, query, cluster).Scan(
		&status.RoundID,
		&status.Cluster,
		&status.Branch,
		&status.GitCommit,
		&status.CommittedAt,
		&status.Observable,
		&status.ComparisonSuccess,
		&status.ExpectedCount,
		&status.DiffCount,
		&status.PolicyBlockCount,
		&status.BlockedCount,
		&status.ConvergedResources,
		&status.Convergence,
		&status.ErrorCode,
		&status.ErrorMessage,
		&status.ErrorResourceKey,
		&status.ErrorField,
		&status.ErrorSourceBranch,
		&status.LastObservedRoundID,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return model.ClusterStatus{}, ErrNotFound
	}
	if err != nil {
		return model.ClusterStatus{}, fmt.Errorf("query latest cluster status: %w", err)
	}
	status.CommittedAt = status.CommittedAt.UTC()
	status.Differences, err = s.queryDifferences(ctx, status.RoundID, cluster)
	if err != nil {
		return model.ClusterStatus{}, err
	}
	status.PolicyBlocks, err = s.queryPolicyBlocks(ctx, status.RoundID, cluster)
	if err != nil {
		return model.ClusterStatus{}, err
	}
	status.ApplyResults, err = s.queryApplyResults(ctx, status.RoundID, cluster)
	if err != nil {
		return model.ClusterStatus{}, err
	}
	return status, nil
}

func (s *Store) queryDifferences(ctx context.Context, roundID, cluster string) ([]model.Difference, error) {
	rows, err := s.pool.Query(
		ctx,
		`SELECT resource_key, field, component, env_key, expected, actual, resolution, reason
		 FROM drift_differences
		 WHERE round_id = $1 AND cluster = $2
		 ORDER BY resource_key, field, component, env_key`,
		roundID,
		cluster,
	)
	if err != nil {
		return nil, fmt.Errorf("query differences: %w", err)
	}
	defer rows.Close()
	out := make([]model.Difference, 0)
	for rows.Next() {
		var item model.Difference
		var expectedRaw, actualRaw []byte
		if err := rows.Scan(
			&item.ResourceKey,
			&item.Field,
			&item.Component,
			&item.EnvKey,
			&expectedRaw,
			&actualRaw,
			&item.Resolution,
			&item.Reason,
		); err != nil {
			return nil, fmt.Errorf("scan difference: %w", err)
		}
		if err := json.Unmarshal(expectedRaw, &item.Expected); err != nil {
			return nil, fmt.Errorf("decode expected value: %w", err)
		}
		if err := json.Unmarshal(actualRaw, &item.Actual); err != nil {
			return nil, fmt.Errorf("decode actual value: %w", err)
		}
		out = append(out, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate differences: %w", err)
	}
	model.SortDifferences(out)
	return out, nil
}

func (s *Store) queryPolicyBlocks(ctx context.Context, roundID, cluster string) ([]model.PolicyBlock, error) {
	rows, err := s.pool.Query(
		ctx,
		`SELECT resource_key, field, value, source_branch, declared_in_branch, reason
		 FROM drift_policy_blocks
		 WHERE round_id = $1 AND cluster = $2
		 ORDER BY resource_key, field, source_branch, reason`,
		roundID,
		cluster,
	)
	if err != nil {
		return nil, fmt.Errorf("query policy blocks: %w", err)
	}
	defer rows.Close()
	out := make([]model.PolicyBlock, 0)
	for rows.Next() {
		var item model.PolicyBlock
		var valueRaw []byte
		if err := rows.Scan(
			&item.ResourceKey,
			&item.Field,
			&valueRaw,
			&item.SourceBranch,
			&item.DeclaredInBranch,
			&item.Reason,
		); err != nil {
			return nil, fmt.Errorf("scan policy block: %w", err)
		}
		if err := json.Unmarshal(valueRaw, &item.Value); err != nil {
			return nil, fmt.Errorf("decode policy value: %w", err)
		}
		out = append(out, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate policy blocks: %w", err)
	}
	model.SortPolicyBlocks(out)
	return out, nil
}

func (s *Store) queryApplyResults(ctx context.Context, roundID, cluster string) ([]model.ApplyResult, error) {
	rows, err := s.pool.Query(
		ctx,
		`SELECT resource_key, action, status, error_code, message, applied_at
		 FROM drift_apply_results
		 WHERE round_id = $1 AND cluster = $2
		 ORDER BY resource_key, action, applied_at`,
		roundID,
		cluster,
	)
	if err != nil {
		return nil, fmt.Errorf("query apply results: %w", err)
	}
	defer rows.Close()
	out := make([]model.ApplyResult, 0)
	for rows.Next() {
		var item model.ApplyResult
		if err := rows.Scan(
			&item.ResourceKey,
			&item.Action,
			&item.Status,
			&item.ErrorCode,
			&item.Message,
			&item.AppliedAt,
		); err != nil {
			return nil, fmt.Errorf("scan apply result: %w", err)
		}
		item.AppliedAt = item.AppliedAt.UTC()
		out = append(out, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate apply results: %w", err)
	}
	return out, nil
}

func marshalJSON(value any) ([]byte, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return data, nil
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

const schema = `
CREATE TABLE IF NOT EXISTS drift_rounds (
    round_id     text PRIMARY KEY,
    started_at   timestamptz NOT NULL,
    committed_at timestamptz NOT NULL,
    duration_ms  bigint NOT NULL CHECK (duration_ms >= 0),
    status       text NOT NULL CHECK (status = 'committed')
);

CREATE TABLE IF NOT EXISTS drift_round_clusters (
    round_id              text NOT NULL REFERENCES drift_rounds(round_id),
    cluster               text NOT NULL,
    branch                text NOT NULL,
    git_commit            text NOT NULL DEFAULT '',
    observable            boolean NOT NULL,
    comparison_success    boolean NOT NULL DEFAULT false,
    expected_count        integer NOT NULL CHECK (expected_count >= 0),
    diff_count            integer NOT NULL CHECK (diff_count >= 0),
    policy_block_count    integer NOT NULL CHECK (policy_block_count >= 0),
    blocked_count         integer NOT NULL CHECK (blocked_count >= 0),
    converged_resources   integer NOT NULL CHECK (converged_resources >= 0),
    convergence           text NOT NULL CHECK (convergence IN ('converged','partial','failed')),
    error_code            text NOT NULL DEFAULT '',
    error_message         text NOT NULL DEFAULT '',
    error_resource_key    text NOT NULL DEFAULT '',
    error_field           text NOT NULL DEFAULT '',
    error_source_branch   text NOT NULL DEFAULT '',
    last_observed_round_id text NOT NULL DEFAULT '',
    PRIMARY KEY (round_id, cluster)
);
ALTER TABLE drift_round_clusters ADD COLUMN IF NOT EXISTS error_resource_key text NOT NULL DEFAULT '';
ALTER TABLE drift_round_clusters ADD COLUMN IF NOT EXISTS error_field text NOT NULL DEFAULT '';
ALTER TABLE drift_round_clusters ADD COLUMN IF NOT EXISTS error_source_branch text NOT NULL DEFAULT '';
ALTER TABLE drift_round_clusters ADD COLUMN IF NOT EXISTS comparison_success boolean NOT NULL DEFAULT false;

CREATE TABLE IF NOT EXISTS drift_differences (
    id           bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    round_id     text NOT NULL,
    cluster      text NOT NULL,
    resource_key text NOT NULL,
    field        text NOT NULL,
    component    text NOT NULL DEFAULT '',
    env_key      text NOT NULL DEFAULT '',
    expected     jsonb,
    actual       jsonb,
    resolution   text NOT NULL,
    reason       text NOT NULL DEFAULT '',
    FOREIGN KEY (round_id, cluster) REFERENCES drift_round_clusters(round_id, cluster)
);
CREATE INDEX IF NOT EXISTS drift_differences_round_idx
    ON drift_differences (round_id, cluster, resource_key, field, component, env_key);

CREATE TABLE IF NOT EXISTS drift_policy_blocks (
    id                 bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    round_id           text NOT NULL,
    cluster            text NOT NULL,
    resource_key       text NOT NULL,
    field              text NOT NULL,
    value              jsonb,
    source_branch      text NOT NULL,
    declared_in_branch text NOT NULL,
    reason             text NOT NULL,
    FOREIGN KEY (round_id, cluster) REFERENCES drift_round_clusters(round_id, cluster)
);
CREATE INDEX IF NOT EXISTS drift_policy_blocks_round_idx
    ON drift_policy_blocks (round_id, cluster, resource_key, field);

CREATE TABLE IF NOT EXISTS drift_apply_results (
    id           bigint GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    round_id     text NOT NULL,
    cluster      text NOT NULL,
    resource_key text NOT NULL,
    action       text NOT NULL,
    status       text NOT NULL,
    error_code   text NOT NULL DEFAULT '',
    message      text NOT NULL DEFAULT '',
    applied_at   timestamptz NOT NULL,
    FOREIGN KEY (round_id, cluster) REFERENCES drift_round_clusters(round_id, cluster)
);
CREATE INDEX IF NOT EXISTS drift_apply_results_round_idx
    ON drift_apply_results (round_id, cluster, resource_key);

CREATE OR REPLACE FUNCTION drift_reject_committed_mutation()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
    RAISE EXCEPTION 'committed drift records are immutable';
END;
$$;

DROP TRIGGER IF EXISTS drift_rounds_immutable ON drift_rounds;
CREATE TRIGGER drift_rounds_immutable
BEFORE UPDATE OR DELETE ON drift_rounds
FOR EACH ROW EXECUTE FUNCTION drift_reject_committed_mutation();

DROP TRIGGER IF EXISTS drift_round_clusters_immutable ON drift_round_clusters;
CREATE TRIGGER drift_round_clusters_immutable
BEFORE UPDATE OR DELETE ON drift_round_clusters
FOR EACH ROW EXECUTE FUNCTION drift_reject_committed_mutation();

DROP TRIGGER IF EXISTS drift_differences_immutable ON drift_differences;
CREATE TRIGGER drift_differences_immutable
BEFORE UPDATE OR DELETE ON drift_differences
FOR EACH ROW EXECUTE FUNCTION drift_reject_committed_mutation();

DROP TRIGGER IF EXISTS drift_policy_blocks_immutable ON drift_policy_blocks;
CREATE TRIGGER drift_policy_blocks_immutable
BEFORE UPDATE OR DELETE ON drift_policy_blocks
FOR EACH ROW EXECUTE FUNCTION drift_reject_committed_mutation();

DROP TRIGGER IF EXISTS drift_apply_results_immutable ON drift_apply_results;
CREATE TRIGGER drift_apply_results_immutable
BEFORE UPDATE OR DELETE ON drift_apply_results
FOR EACH ROW EXECUTE FUNCTION drift_reject_committed_mutation();
`

func (s *Store) Health(ctx context.Context) error {
	return s.pool.Ping(ctx)
}

func (s *Store) DatabaseName(ctx context.Context) (string, error) {
	var name string
	err := s.pool.QueryRow(ctx, `SELECT current_database()`).Scan(&name)
	return strings.TrimSpace(name), err
}
