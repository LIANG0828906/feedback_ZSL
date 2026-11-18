package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"

	"example.com/dualring-config/internal/model"
)

func createSnapshotTx(
	ctx context.Context,
	tx *sql.Tx,
	namespace, kind, releaseID, operator, createdAt string,
	items []model.Item,
) (int64, string, error) {
	ordered := append([]model.Item(nil), items...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Key < ordered[j].Key })
	for i := 1; i < len(ordered); i++ {
		if ordered[i-1].Key == ordered[i].Key {
			return 0, "", fmt.Errorf("duplicate key %q in snapshot", ordered[i].Key)
		}
	}
	digest := model.DigestItems(ordered)
	result, err := tx.ExecContext(ctx,
		`INSERT INTO snapshots(namespace, kind, release_id, digest, operator, created_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		namespace, kind, releaseID, digest, operator, createdAt,
	)
	if err != nil {
		return 0, "", err
	}
	snapshotID, err := result.LastInsertId()
	if err != nil {
		return 0, "", err
	}
	for _, item := range ordered {
		_, err = tx.ExecContext(ctx,
			`INSERT INTO snapshot_items(snapshot_id, namespace, key, value, digest, item_version)
			 VALUES (?, ?, ?, ?, ?, ?)`,
			snapshotID, namespace, item.Key, item.Value, model.DigestValue(item.Value), item.Version,
		)
		if err != nil {
			return 0, "", err
		}
	}
	return snapshotID, digest, nil
}

func loadSnapshotTx(ctx context.Context, tx *sql.Tx, snapshotID int64) (model.ConfigSet, error) {
	var config model.ConfigSet
	err := tx.QueryRowContext(ctx,
		`SELECT namespace, digest FROM snapshots WHERE id = ?`, snapshotID,
	).Scan(&config.Namespace, &config.Digest)
	if err != nil {
		return config, err
	}
	config.Version = snapshotID
	rows, err := tx.QueryContext(ctx,
		`SELECT key, value, item_version FROM snapshot_items
		 WHERE snapshot_id = ? ORDER BY key`, snapshotID,
	)
	if err != nil {
		return config, err
	}
	defer rows.Close()
	for rows.Next() {
		var item model.Item
		if err := rows.Scan(&item.Key, &item.Value, &item.Version); err != nil {
			return config, err
		}
		config.Items = append(config.Items, item)
	}
	if err := rows.Err(); err != nil {
		return config, err
	}
	if config.Items == nil {
		config.Items = []model.Item{}
	}
	return config, nil
}

func snapshotDigestTx(ctx context.Context, tx *sql.Tx, snapshotID int64) (string, error) {
	var digest string
	err := tx.QueryRowContext(ctx,
		`SELECT digest FROM snapshots WHERE id = ?`, snapshotID,
	).Scan(&digest)
	return digest, err
}

func (s *Store) HeadConfig(ctx context.Context, namespace string) (model.ConfigSet, error) {
	if namespace == "" {
		return model.ConfigSet{}, model.Biz(model.CodeInvalidArgument, "namespace is required")
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return model.ConfigSet{}, err
	}
	defer tx.Rollback()
	var snapshotID int64
	err = tx.QueryRowContext(ctx,
		`SELECT snapshot_id FROM namespace_heads WHERE namespace = ?`, namespace,
	).Scan(&snapshotID)
	if errors.Is(err, sql.ErrNoRows) {
		return model.ConfigSet{}, model.Biz(model.CodeNotFound, "namespace not found")
	}
	if err != nil {
		return model.ConfigSet{}, err
	}
	config, err := loadSnapshotTx(ctx, tx, snapshotID)
	if err != nil {
		return model.ConfigSet{}, err
	}
	if err := tx.Commit(); err != nil {
		return model.ConfigSet{}, err
	}
	return config, nil
}

func (s *Store) ResolvePull(ctx context.Context, instanceID, namespace string) (model.PullResponse, error) {
	if instanceID == "" || namespace == "" {
		return model.PullResponse{}, model.Biz(model.CodeInvalidArgument, "instance_id and namespace are required")
	}
	client, err := s.EnsureClient(ctx, instanceID)
	if err != nil {
		return model.PullResponse{}, err
	}

	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return model.PullResponse{}, err
	}
	defer tx.Rollback()

	var headID int64
	err = tx.QueryRowContext(ctx,
		`SELECT snapshot_id FROM namespace_heads WHERE namespace = ?`, namespace,
	).Scan(&headID)
	if errors.Is(err, sql.ErrNoRows) {
		return model.PullResponse{}, model.Biz(model.CodeNotFound, "namespace not found")
	}
	if err != nil {
		return model.PullResponse{}, err
	}

	servedID := headID
	releaseID := ""
	if client.Ring == model.RingCanary {
		err = tx.QueryRowContext(ctx,
			`SELECT id, candidate_snapshot_id FROM releases
			 WHERE namespace = ? AND state IN ('canary', 'observed_pass')
			 ORDER BY created_at DESC LIMIT 1`, namespace,
		).Scan(&releaseID, &servedID)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return model.PullResponse{}, err
		}
		if errors.Is(err, sql.ErrNoRows) {
			servedID = headID
			releaseID = ""
		}
	}
	config, err := loadSnapshotTx(ctx, tx, servedID)
	if err != nil {
		return model.PullResponse{}, err
	}
	if err := tx.Commit(); err != nil {
		return model.PullResponse{}, err
	}
	config.ReleaseID = releaseID
	return model.PullResponse{
		ConfigSet:  config,
		InstanceID: instanceID,
		Ring:       client.Ring,
		ServedAt:   s.nowText(),
	}, nil
}

func (s *Store) ConfigAtVersion(ctx context.Context, namespace string, version int64) (model.ConfigSet, error) {
	if namespace == "" || version <= 0 {
		return model.ConfigSet{}, model.Biz(model.CodeInvalidArgument, "namespace and version are required")
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return model.ConfigSet{}, err
	}
	defer tx.Rollback()
	var namespaceFromDB string
	err = tx.QueryRowContext(ctx,
		`SELECT namespace FROM snapshots WHERE id = ?`, version,
	).Scan(&namespaceFromDB)
	if errors.Is(err, sql.ErrNoRows) || namespaceFromDB != namespace {
		return model.ConfigSet{}, model.Biz(model.CodeNotFound, "configuration version not found")
	}
	if err != nil {
		return model.ConfigSet{}, err
	}
	config, err := loadSnapshotTx(ctx, tx, version)
	if err != nil {
		return model.ConfigSet{}, err
	}
	if err := tx.Commit(); err != nil {
		return model.ConfigSet{}, err
	}
	return config, nil
}
