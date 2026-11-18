package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"math"

	"example.com/dualring-config/internal/model"
)

func (s *Store) EnsureClient(ctx context.Context, instanceID string) (model.Client, error) {
	if instanceID == "" {
		return model.Client{}, model.Biz(model.CodeInvalidArgument, "instance_id is required")
	}
	sum := sha256.Sum256([]byte(instanceID))
	bucket := int(binary.BigEndian.Uint64(sum[:8]) % 10000)
	ring := model.RingFull
	if bucket < int(math.Round(s.canaryPercent*100)) {
		ring = model.RingCanary
	}

	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	now := s.nowText()
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO clients(instance_id, ring, bucket, assigned_at, last_seen_at)
		 VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT(instance_id) DO UPDATE SET last_seen_at = excluded.last_seen_at`,
		instanceID, ring, bucket, now, now,
	); err != nil {
		return model.Client{}, err
	}
	var client model.Client
	if err := s.db.QueryRowContext(ctx,
		`SELECT instance_id, ring, bucket, assigned_at, last_seen_at
		 FROM clients WHERE instance_id = ?`, instanceID,
	).Scan(
		&client.InstanceID, &client.Ring, &client.Bucket, &client.AssignedAt, &client.LastSeenAt,
	); err != nil {
		return model.Client{}, err
	}
	return client, nil
}

func (s *Store) ListClients(ctx context.Context, ring string) ([]model.Client, error) {
	if ring != "" && ring != model.RingCanary && ring != model.RingFull {
		return nil, model.Biz(model.CodeInvalidArgument, "ring must be canary or full")
	}
	query := `SELECT instance_id, ring, bucket, assigned_at, last_seen_at FROM clients`
	args := []any{}
	if ring != "" {
		query += ` WHERE ring = ?`
		args = append(args, ring)
	}
	query += ` ORDER BY instance_id`
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var clients []model.Client
	for rows.Next() {
		var client model.Client
		if err := rows.Scan(
			&client.InstanceID, &client.Ring, &client.Bucket,
			&client.AssignedAt, &client.LastSeenAt,
		); err != nil {
			return nil, err
		}
		clients = append(clients, client)
	}
	return clients, rows.Err()
}

func (s *Store) SetClientRing(ctx context.Context, instanceID, ring, operator string) (model.Client, error) {
	if instanceID == "" || operator == "" {
		return model.Client{}, model.Biz(model.CodeInvalidArgument, "instance_id and operator are required")
	}
	if ring != model.RingCanary && ring != model.RingFull {
		return model.Client{}, model.Biz(model.CodeInvalidArgument, "ring must be canary or full")
	}
	existing, err := s.EnsureClient(ctx, instanceID)
	if err != nil {
		return model.Client{}, err
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	now := s.nowText()
	result, err := s.db.ExecContext(ctx,
		`UPDATE clients SET ring = ?, last_seen_at = ? WHERE instance_id = ?`,
		ring, now, instanceID,
	)
	if err != nil {
		return model.Client{}, err
	}
	affected, _ := result.RowsAffected()
	if affected == 0 {
		return model.Client{}, model.Biz(model.CodeNotFound, "client not found")
	}
	oldDigest := model.DigestValue(existing.Ring)
	newDigest := model.DigestValue(ring)
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO audit_log
		 (created_at, operation, namespace, key, release_id, old_digest, new_digest, operator, status_code, details)
		 VALUES (?, 'client.ring_override', '', ?, '', ?, ?, ?, ?, ?)`,
		now, instanceID, oldDigest, newDigest, operator, model.CodeOK,
		fmt.Sprintf("assigned bucket=%d ring=%s", existing.Bucket, ring),
	)
	if err != nil {
		return model.Client{}, err
	}
	return model.Client{
		InstanceID: instanceID,
		Ring:       ring,
		Bucket:     existing.Bucket,
		AssignedAt: existing.AssignedAt,
		LastSeenAt: now,
	}, nil
}

type NamespaceSummary struct {
	Namespace       string `json:"namespace"`
	SnapshotVersion int64  `json:"snapshot_version"`
	Digest          string `json:"digest"`
	ItemCount       int    `json:"item_count"`
}

func (s *Store) ListNamespaces(ctx context.Context) ([]NamespaceSummary, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT h.namespace, h.snapshot_id, s.digest,
		        (SELECT COUNT(*) FROM snapshot_items si WHERE si.snapshot_id = h.snapshot_id)
		 FROM namespace_heads h
		 JOIN snapshots s ON s.id = h.snapshot_id
		 ORDER BY h.namespace`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var summaries []NamespaceSummary
	for rows.Next() {
		var summary NamespaceSummary
		if err := rows.Scan(
			&summary.Namespace, &summary.SnapshotVersion, &summary.Digest, &summary.ItemCount,
		); err != nil {
			return nil, err
		}
		summaries = append(summaries, summary)
	}
	return summaries, rows.Err()
}

type Stats struct {
	ClientsCanary       int            `json:"clients_canary"`
	ClientsFull         int            `json:"clients_full"`
	ReleasesByState     map[string]int `json:"releases_by_state"`
	Namespaces          int            `json:"namespaces"`
	HistoryRetention    int            `json:"history_retention"`
	CanaryPercent       float64        `json:"canary_percent"`
	ErrorThreshold      float64        `json:"error_threshold"`
	ObservationPeriodMS int64          `json:"observation_period_ms"`
}

func (s *Store) GetStats(ctx context.Context) (Stats, error) {
	stats := Stats{
		ReleasesByState:     map[string]int{},
		HistoryRetention:    s.historyLimit,
		CanaryPercent:       s.canaryPercent,
		ErrorThreshold:      s.errorThreshold,
		ObservationPeriodMS: s.observationPeriod.Milliseconds(),
	}
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM clients WHERE ring = 'canary'`,
	).Scan(&stats.ClientsCanary); err != nil {
		return stats, err
	}
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM clients WHERE ring = 'full'`,
	).Scan(&stats.ClientsFull); err != nil {
		return stats, err
	}
	if err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM namespace_heads`,
	).Scan(&stats.Namespaces); err != nil {
		return stats, err
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT state, COUNT(*) FROM releases GROUP BY state`,
	)
	if err != nil {
		return stats, err
	}
	defer rows.Close()
	for rows.Next() {
		var state string
		var count int
		if err := rows.Scan(&state, &count); err != nil {
			return stats, err
		}
		stats.ReleasesByState[state] = count
	}
	return stats, rows.Err()
}

func (s *Store) CountCanaryClients(ctx context.Context) (int, error) {
	var count int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM clients WHERE ring = 'canary'`,
	).Scan(&count)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return count, err
}
