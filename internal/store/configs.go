package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"

	"example.com/dualring-config/internal/model"
)

type UpdateResult struct {
	Namespace       string `json:"namespace"`
	Key             string `json:"key"`
	Value           string `json:"value,omitempty"`
	Deleted         bool   `json:"deleted"`
	ItemVersion     int64  `json:"item_version"`
	SnapshotVersion int64  `json:"snapshot_version"`
	ResultCode      string `json:"result_code"`
}

type ItemVersionRecord struct {
	Version   int64  `json:"version"`
	Value     string `json:"value"`
	Digest    string `json:"digest"`
	Action    string `json:"action"`
	Operator  string `json:"operator"`
	ReleaseID string `json:"release_id,omitempty"`
	CreatedAt string `json:"created_at"`
}

func (s *Store) Bootstrap(ctx context.Context, namespace, operator string, values map[string]string) (model.ConfigSet, error) {
	if namespace == "" {
		return model.ConfigSet{}, model.Biz(model.CodeInvalidArgument, "namespace is required")
	}
	if operator == "" {
		return model.ConfigSet{}, model.Biz(model.CodeInvalidArgument, "operator is required")
	}
	if len(values) == 0 {
		return model.ConfigSet{}, model.Biz(model.CodeInvalidArgument, "at least one configuration item is required")
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		if key == "" {
			return model.ConfigSet{}, model.Biz(model.CodeInvalidArgument, "configuration key is required")
		}
		keys = append(keys, key)
	}
	sort.Strings(keys)

	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.ConfigSet{}, err
	}
	defer tx.Rollback()

	var existing int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM namespace_heads WHERE namespace = ?`, namespace,
	).Scan(&existing); err != nil {
		return model.ConfigSet{}, err
	}
	if existing > 0 {
		return model.ConfigSet{}, model.Biz(model.CodeConflict, "namespace already initialized")
	}

	now := s.nowText()
	items := make([]model.Item, 0, len(keys))
	for _, key := range keys {
		items = append(items, model.Item{Key: key, Value: values[key], Version: 1})
	}
	snapshotID, digest, err := createSnapshotTx(
		ctx, tx, namespace, "head", "", operator, now, items,
	)
	if err != nil {
		return model.ConfigSet{}, err
	}
	for _, item := range items {
		valueDigest := model.DigestValue(item.Value)
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO config_items
			 (namespace, key, value, digest, item_version, updated_at, updated_by)
			 VALUES (?, ?, ?, ?, ?, ?, ?)`,
			namespace, item.Key, item.Value, valueDigest, item.Version, now, operator,
		); err != nil {
			return model.ConfigSet{}, err
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO config_item_versions
			 (namespace, key, version, value, digest, action, operator, release_id, created_at)
			 VALUES (?, ?, ?, ?, ?, 'CREATE', ?, '', ?)`,
			namespace, item.Key, item.Version, item.Value, valueDigest, operator, now,
		); err != nil {
			return model.ConfigSet{}, err
		}
		if err := s.insertAuditTx(
			ctx, tx, "config.bootstrap", namespace, item.Key, "", "", valueDigest,
			operator, model.CodeOK, "initial configuration",
		); err != nil {
			return model.ConfigSet{}, err
		}
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO namespace_heads(namespace, snapshot_id, updated_at) VALUES (?, ?, ?)`,
		namespace, snapshotID, now,
	); err != nil {
		return model.ConfigSet{}, err
	}
	if err := s.insertAuditTx(
		ctx, tx, "namespace.bootstrap", namespace, "", "", "", digest,
		operator, model.CodeOK, fmt.Sprintf("snapshot=%d items=%d", snapshotID, len(items)),
	); err != nil {
		return model.ConfigSet{}, err
	}
	if err := tx.Commit(); err != nil {
		return model.ConfigSet{}, err
	}

	config := model.ConfigSet{
		Namespace: namespace,
		Version:   snapshotID,
		Digest:    digest,
		Items:     items,
	}
	return config, nil
}

func (s *Store) UpdateItem(
	ctx context.Context,
	namespace, key, value string,
	deleted bool,
	expectedVersion int64,
	operator, requestID string,
) (UpdateResult, error) {
	if namespace == "" || key == "" {
		return UpdateResult{}, model.Biz(model.CodeInvalidArgument, "namespace and key are required")
	}
	if operator == "" {
		return UpdateResult{}, model.Biz(model.CodeInvalidArgument, "operator is required")
	}
	if expectedVersion < 0 {
		return UpdateResult{}, model.Biz(model.CodeInvalidArgument, "expected_version cannot be negative")
	}

	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return UpdateResult{}, err
	}
	defer tx.Rollback()

	var active int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM releases
		 WHERE namespace = ? AND state IN ('draft', 'canary', 'observed_pass')`, namespace,
	).Scan(&active); err != nil {
		return UpdateResult{}, err
	}
	if active > 0 {
		tx.Rollback()
		s.recordFailureAudit(
			"config.update", namespace, key, "", "", model.DigestValue(value),
			operator, model.CodeReleaseActive, "direct update rejected while a release is active",
		)
		return UpdateResult{}, model.Biz(model.CodeReleaseActive, "direct update is not allowed while a release is active")
	}

	var oldValue, oldDigest string
	var currentVersion int64
	exists := true
	err = tx.QueryRowContext(ctx,
		`SELECT value, digest, item_version FROM config_items
		 WHERE namespace = ? AND key = ?`, namespace, key,
	).Scan(&oldValue, &oldDigest, &currentVersion)
	if errors.Is(err, sql.ErrNoRows) {
		exists = false
		currentVersion, err = latestItemVersionTx(ctx, tx, namespace, key)
		if err != nil {
			return UpdateResult{}, err
		}
	} else if err != nil {
		return UpdateResult{}, err
	}
	if err != nil {
		return UpdateResult{}, err
	}
	if currentVersion != expectedVersion {
		attemptDigest := model.DigestValue(value)
		_ = tx.Rollback()
		details := fmt.Sprintf("expected_version=%d actual_version=%d request_id=%s",
			expectedVersion, currentVersion, requestID)
		s.recordFailureAudit(
			"config.update", namespace, key, "", oldDigest, attemptDigest,
			operator, model.CodeConflict, details,
		)
		return UpdateResult{}, model.Biz(model.CodeConflict,
			fmt.Sprintf("version conflict: expected %d, current %d", expectedVersion, currentVersion))
	}
	if deleted && !exists {
		_ = tx.Rollback()
		s.recordFailureAudit(
			"config.delete", namespace, key, "", "", "",
			operator, model.CodeNotFound, "configuration key does not exist",
		)
		return UpdateResult{}, model.Biz(model.CodeNotFound, "configuration key does not exist")
	}

	var headID int64
	if err := tx.QueryRowContext(ctx,
		`SELECT snapshot_id FROM namespace_heads WHERE namespace = ?`, namespace,
	).Scan(&headID); errors.Is(err, sql.ErrNoRows) {
		return UpdateResult{}, model.Biz(model.CodeNotFound, "namespace not found")
	} else if err != nil {
		return UpdateResult{}, err
	}

	now := s.nowText()
	newVersion := currentVersion + 1
	action := "UPDATE"
	newValue := value
	if deleted {
		action = "DELETE"
		newValue = ""
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM config_items WHERE namespace = ? AND key = ?`, namespace, key,
		); err != nil {
			return UpdateResult{}, err
		}
	} else {
		newDigest := model.DigestValue(newValue)
		if !exists {
			action = "CREATE"
			_, err = tx.ExecContext(ctx,
				`INSERT INTO config_items
				 (namespace, key, value, digest, item_version, updated_at, updated_by)
				 VALUES (?, ?, ?, ?, ?, ?, ?)`,
				namespace, key, newValue, newDigest, newVersion, now, operator,
			)
		} else {
			_, err = tx.ExecContext(ctx,
				`UPDATE config_items
				 SET value = ?, digest = ?, item_version = ?, updated_at = ?, updated_by = ?
				 WHERE namespace = ? AND key = ?`,
				newValue, newDigest, newVersion, now, operator, namespace, key,
			)
		}
		if err != nil {
			return UpdateResult{}, err
		}
	}

	newDigest := ""
	if !deleted {
		newDigest = model.DigestValue(newValue)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO config_item_versions
		 (namespace, key, version, value, digest, action, operator, release_id, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, '', ?)`,
		namespace, key, newVersion, newValue, newDigest, action, operator, now,
	); err != nil {
		return UpdateResult{}, err
	}
	if err := pruneHistoryTx(ctx, tx, namespace, key, s.historyLimit); err != nil {
		return UpdateResult{}, err
	}

	items, err := loadConfigItemsTx(ctx, tx, namespace)
	if err != nil {
		return UpdateResult{}, err
	}
	snapshotID, _, err := createSnapshotTx(
		ctx, tx, namespace, "head", "", operator, now, items,
	)
	if err != nil {
		return UpdateResult{}, err
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE namespace_heads SET snapshot_id = ?, updated_at = ? WHERE namespace = ?`,
		snapshotID, now, namespace,
	); err != nil {
		return UpdateResult{}, err
	}
	details := fmt.Sprintf("item_version=%d snapshot=%d request_id=%s", newVersion, snapshotID, requestID)
	if err := s.insertAuditTx(
		ctx, tx, "config."+lowerAction(action), namespace, key, "", oldDigest, newDigest,
		operator, model.CodeOK, details,
	); err != nil {
		return UpdateResult{}, err
	}
	if err := tx.Commit(); err != nil {
		return UpdateResult{}, err
	}
	return UpdateResult{
		Namespace:       namespace,
		Key:             key,
		Value:           newValue,
		Deleted:         deleted,
		ItemVersion:     newVersion,
		SnapshotVersion: snapshotID,
		ResultCode:      model.CodeOK,
	}, nil
}

func lowerAction(action string) string {
	switch action {
	case "CREATE":
		return "create"
	case "DELETE":
		return "delete"
	default:
		return "update"
	}
}

func latestItemVersionTx(ctx context.Context, tx *sql.Tx, namespace, key string) (int64, error) {
	var version int64
	err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(version), 0) FROM config_item_versions
		 WHERE namespace = ? AND key = ?`, namespace, key,
	).Scan(&version)
	return version, err
}

func loadConfigItemsTx(ctx context.Context, tx *sql.Tx, namespace string) ([]model.Item, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT key, value, item_version FROM config_items
		 WHERE namespace = ? ORDER BY key`, namespace,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var items []model.Item
	for rows.Next() {
		var item model.Item
		if err := rows.Scan(&item.Key, &item.Value, &item.Version); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

func pruneHistoryTx(ctx context.Context, tx *sql.Tx, namespace, key string, limit int) error {
	_, err := tx.ExecContext(ctx,
		`DELETE FROM config_item_versions
		 WHERE namespace = ? AND key = ?
		   AND version NOT IN (
		     SELECT version FROM config_item_versions
		     WHERE namespace = ? AND key = ?
		     ORDER BY version DESC LIMIT ?
		   )`,
		namespace, key, namespace, key, limit,
	)
	return err
}

func (s *Store) ItemVersion(ctx context.Context, namespace, key string) (ItemVersionRecord, error) {
	var record ItemVersionRecord
	err := s.db.QueryRowContext(ctx,
		`SELECT version, value, digest, action, operator, release_id, created_at
		 FROM config_item_versions
		 WHERE namespace = ? AND key = ?
		 ORDER BY version DESC LIMIT 1`, namespace, key,
	).Scan(
		&record.Version, &record.Value, &record.Digest, &record.Action,
		&record.Operator, &record.ReleaseID, &record.CreatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return record, model.Biz(model.CodeNotFound, "configuration key not found")
	}
	return record, err
}

func (s *Store) History(ctx context.Context, namespace, key string, limit int) ([]ItemVersionRecord, error) {
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT version, value, digest, action, operator, release_id, created_at
		 FROM config_item_versions
		 WHERE namespace = ? AND key = ?
		 ORDER BY version DESC LIMIT ?`, namespace, key, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var records []ItemVersionRecord
	for rows.Next() {
		var record ItemVersionRecord
		if err := rows.Scan(
			&record.Version, &record.Value, &record.Digest, &record.Action,
			&record.Operator, &record.ReleaseID, &record.CreatedAt,
		); err != nil {
			return nil, err
		}
		records = append(records, record)
	}
	return records, rows.Err()
}
