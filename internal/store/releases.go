package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"

	"example.com/dualring-config/internal/model"
)

type ReleaseInput struct {
	Namespace          string
	Operator           string
	Changes            []model.ItemChange
	ObservationSeconds float64
}

type ObservationInput struct {
	ReleaseID    string
	InstanceID   string
	Successes    int64
	Errors       int64
	LocalVersion int64
	Offline      bool
}

type ObservationStats struct {
	ReleaseID      string  `json:"release_id"`
	SuccessCount   int64   `json:"success_count"`
	ErrorCount     int64   `json:"error_count"`
	ErrorRate      float64 `json:"error_rate"`
	ErrorThreshold float64 `json:"error_threshold"`
	State          string  `json:"state"`
}

func newReleaseID(now time.Time) (string, error) {
	random := make([]byte, 4)
	if _, err := rand.Read(random); err != nil {
		return "", err
	}
	return "rel-" + now.UTC().Format("20060102T150405.000000000Z") + "-" + hex.EncodeToString(random), nil
}

func (s *Store) CreateRelease(ctx context.Context, input ReleaseInput) (model.Release, error) {
	if input.Namespace == "" || input.Operator == "" {
		return model.Release{}, model.Biz(model.CodeInvalidArgument, "namespace and operator are required")
	}
	seen := map[string]bool{}
	for _, change := range input.Changes {
		if err := model.ValidateItemChange(change); err != nil {
			return model.Release{}, model.Biz(model.CodeInvalidArgument, err.Error())
		}
		if seen[change.Key] {
			return model.Release{}, model.Biz(model.CodeInvalidArgument, "duplicate key in release: "+change.Key)
		}
		seen[change.Key] = true
	}
	observationSeconds := input.ObservationSeconds
	if observationSeconds <= 0 {
		observationSeconds = s.observationPeriod.Seconds()
	}
	if observationSeconds < 0.05 {
		return model.Release{}, model.Biz(model.CodeInvalidArgument, "observation_seconds must be at least 0.05")
	}

	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.Release{}, err
	}
	defer tx.Rollback()

	var headID int64
	var headDigest string
	err = tx.QueryRowContext(ctx,
		`SELECT h.snapshot_id, s.digest
		 FROM namespace_heads h JOIN snapshots s ON s.id = h.snapshot_id
		 WHERE h.namespace = ?`, input.Namespace,
	).Scan(&headID, &headDigest)
	if errors.Is(err, sql.ErrNoRows) {
		return model.Release{}, model.Biz(model.CodeNotFound, "namespace not found")
	}
	if err != nil {
		return model.Release{}, err
	}

	var active int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM releases
		 WHERE namespace = ? AND state IN ('draft', 'canary', 'observed_pass')`,
		input.Namespace,
	).Scan(&active); err != nil {
		return model.Release{}, err
	}
	if active > 0 {
		return model.Release{}, model.Biz(model.CodeReleaseActive, "another release is active for this namespace")
	}

	base, err := loadSnapshotTx(ctx, tx, headID)
	if err != nil {
		return model.Release{}, err
	}
	items := make(map[string]model.Item, len(base.Items))
	for _, item := range base.Items {
		items[item.Key] = item
	}
	for _, change := range input.Changes {
		item, exists := items[change.Key]
		if change.Delete {
			if !exists {
				return model.Release{}, model.Biz(model.CodeNotFound, "cannot delete missing key: "+change.Key)
			}
			delete(items, change.Key)
			continue
		}
		version := int64(1)
		if exists {
			version = item.Version + 1
		} else {
			latest, err := latestItemVersionTx(ctx, tx, input.Namespace, change.Key)
			if err != nil {
				return model.Release{}, err
			}
			version = latest + 1
		}
		items[change.Key] = model.Item{Key: change.Key, Value: change.Value, Version: version}
	}

	candidateItems := make([]model.Item, 0, len(items))
	for _, item := range items {
		candidateItems = append(candidateItems, item)
	}
	sort.Slice(candidateItems, func(i, j int) bool { return candidateItems[i].Key < candidateItems[j].Key })

	nowTime := s.now()
	now := nowTime.UTC().Format(time.RFC3339Nano)
	releaseID, err := newReleaseID(nowTime)
	if err != nil {
		return model.Release{}, err
	}
	candidateID, candidateDigest, err := createSnapshotTx(
		ctx, tx, input.Namespace, "candidate", releaseID, input.Operator, now, candidateItems,
	)
	if err != nil {
		return model.Release{}, err
	}
	_, err = tx.ExecContext(ctx,
		`INSERT INTO releases
		 (id, namespace, state, operator, base_snapshot_id, candidate_snapshot_id,
		  observation_seconds, created_at, updated_at)
		 VALUES (?, ?, 'draft', ?, ?, ?, ?, ?, ?)`,
		releaseID, input.Namespace, input.Operator, headID, candidateID,
		observationSeconds, now, now,
	)
	if err != nil {
		return model.Release{}, err
	}
	if err := s.insertAuditTx(
		ctx, tx, "release.create", input.Namespace, "", releaseID, headDigest, candidateDigest,
		input.Operator, model.CodeOK,
		fmt.Sprintf("draft created base=%d candidate=%d changes=%d", headID, candidateID, len(input.Changes)),
	); err != nil {
		return model.Release{}, err
	}
	if err := tx.Commit(); err != nil {
		return model.Release{}, err
	}
	return s.GetRelease(ctx, releaseID)
}

func (s *Store) TransitionRelease(
	ctx context.Context,
	releaseID, target, operator, reason string,
) (model.Release, error) {
	if releaseID == "" || target == "" || operator == "" {
		return model.Release{}, model.Biz(model.CodeInvalidArgument, "release_id, target, and operator are required")
	}
	target = strings.ToLower(strings.TrimSpace(target))
	if target == "roll_back" {
		target = "rollback"
	}

	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return model.Release{}, err
	}
	defer tx.Rollback()

	release, err := getReleaseTx(ctx, tx, releaseID)
	if errors.Is(err, sql.ErrNoRows) {
		return model.Release{}, model.Biz(model.CodeNotFound, "release not found")
	}
	if err != nil {
		return model.Release{}, err
	}

	var headID int64
	var headDigest string
	err = tx.QueryRowContext(ctx,
		`SELECT h.snapshot_id, s.digest
		 FROM namespace_heads h JOIN snapshots s ON s.id = h.snapshot_id
		 WHERE h.namespace = ?`, release.Namespace,
	).Scan(&headID, &headDigest)
	if err != nil {
		return model.Release{}, model.Biz(model.CodeInternal, "published head is unavailable")
	}
	candidateDigest, err := snapshotDigestTx(ctx, tx, release.CandidateSnapshot)
	if err != nil {
		return model.Release{}, model.Biz(model.CodeInternal, "candidate snapshot is unavailable")
	}
	fail := func(code, message string) (model.Release, error) {
		_ = tx.Rollback()
		s.recordFailureAudit(
			"release.transition", release.Namespace, "", release.ID,
			headDigest, candidateDigest, operator, code,
			fmt.Sprintf("from=%s to=%s reason=%s", release.State, target, message),
		)
		return release, model.Biz(code, message)
	}
	nowTime := s.now()
	now := nowTime.UTC().Format(time.RFC3339Nano)

	switch target {
	case "canary":
		if release.State != model.ReleaseDraft {
			return fail(model.CodeInvalidTransition,
				fmt.Sprintf("cannot move from %s to canary", release.State))
		}
		if release.BaseSnapshotID != headID {
			return fail(model.CodeStaleBase, "published head changed after the draft was created")
		}
		endsAt := nowTime.Add(time.Duration(release.ObservationSeconds * float64(time.Second)))
		_, err = tx.ExecContext(ctx,
			`UPDATE releases
			 SET state = 'canary', canary_started_at = ?, observation_ends_at = ?, updated_at = ?
			 WHERE id = ?`,
			now, endsAt.UTC().Format(time.RFC3339Nano), now, release.ID,
		)
		if err != nil {
			return model.Release{}, err
		}
		if err := s.insertAuditTx(
			ctx, tx, "release.transition", release.Namespace, "", release.ID,
			headDigest, candidateDigest,
			operator, model.CodeOK,
			fmt.Sprintf("from=draft to=canary observation_seconds=%.3f", release.ObservationSeconds),
		); err != nil {
			return model.Release{}, err
		}
	case "full":
		if release.State != model.ReleaseObservedPass {
			if release.State == model.ReleaseCanary {
				endsAt, parseErr := time.Parse(time.RFC3339Nano, release.ObservationEndsAt)
				if parseErr == nil && nowTime.Before(endsAt) {
					return fail(model.CodeObservationActive, "observation period has not ended")
				}
			}
			return fail(model.CodeInvalidTransition,
				fmt.Sprintf("cannot move from %s to full", release.State))
		}
		if release.BaseSnapshotID != headID {
			return fail(model.CodeStaleBase, "published head changed after the draft was created")
		}
		candidate, err := loadSnapshotTx(ctx, tx, release.CandidateSnapshot)
		if err != nil {
			return model.Release{}, err
		}
		if err := s.applyCandidateTx(ctx, tx, release, candidate, operator, now); err != nil {
			return model.Release{}, err
		}
		_, err = tx.ExecContext(ctx,
			`UPDATE releases SET state = 'full', full_at = ?, updated_at = ? WHERE id = ?`,
			now, now, release.ID,
		)
		if err != nil {
			return model.Release{}, err
		}
		if err := s.insertAuditTx(
			ctx, tx, "release.transition", release.Namespace, "", release.ID,
			headDigest, candidate.Digest, operator, model.CodeOK,
			fmt.Sprintf("from=%s to=full snapshot=%d", release.State, candidate.Version),
		); err != nil {
			return model.Release{}, err
		}
	case "rollback":
		if release.State != model.ReleaseCanary && release.State != model.ReleaseObservedPass {
			return fail(model.CodeInvalidTransition,
				fmt.Sprintf("cannot roll back from %s", release.State))
		}
		if reason == "" {
			reason = "manual rollback"
		}
		_, err = tx.ExecContext(ctx,
			`UPDATE releases
			 SET state = 'rolled_back', rolled_back_at = ?, rollback_reason = ?, updated_at = ?
			 WHERE id = ?`,
			now, reason, now, release.ID,
		)
		if err != nil {
			return model.Release{}, err
		}
		if err := s.insertAuditTx(
			ctx, tx, "release.transition", release.Namespace, "", release.ID,
			candidateDigest, headDigest,
			operator, model.CodeOK,
			fmt.Sprintf("from=%s to=rolled_back reason=%s", release.State, reason),
		); err != nil {
			return model.Release{}, err
		}
	default:
		return fail(model.CodeInvalidTransition, "target must be canary, full, or rollback")
	}
	if err := tx.Commit(); err != nil {
		return model.Release{}, err
	}
	return s.GetRelease(ctx, releaseID)
}

func (s *Store) applyCandidateTx(
	ctx context.Context,
	tx *sql.Tx,
	release model.Release,
	candidate model.ConfigSet,
	operator, now string,
) error {
	currentItems, err := loadConfigItemsTx(ctx, tx, release.Namespace)
	if err != nil {
		return err
	}
	current := make(map[string]model.Item, len(currentItems))
	for _, item := range currentItems {
		current[item.Key] = item
	}
	candidateMap := make(map[string]model.Item, len(candidate.Items))
	for _, item := range candidate.Items {
		candidateMap[item.Key] = item
	}

	for _, item := range candidate.Items {
		old, exists := current[item.Key]
		if exists && old.Value == item.Value && old.Version == item.Version {
			continue
		}
		oldDigest := ""
		if exists {
			oldDigest = model.DigestValue(old.Value)
		}
		action := "UPDATE"
		if !exists {
			action = "CREATE"
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO config_items
				 (namespace, key, value, digest, item_version, updated_at, updated_by)
				 VALUES (?, ?, ?, ?, ?, ?, ?)`,
				release.Namespace, item.Key, item.Value, model.DigestValue(item.Value),
				item.Version, now, operator,
			); err != nil {
				return err
			}
		} else {
			if item.Version <= old.Version {
				return fmt.Errorf("candidate item %s has non-increasing version %d <= %d",
					item.Key, item.Version, old.Version)
			}
			if _, err := tx.ExecContext(ctx,
				`UPDATE config_items
				 SET value = ?, digest = ?, item_version = ?, updated_at = ?, updated_by = ?
				 WHERE namespace = ? AND key = ?`,
				item.Value, model.DigestValue(item.Value), item.Version, now, operator,
				release.Namespace, item.Key,
			); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO config_item_versions
			 (namespace, key, version, value, digest, action, operator, release_id, created_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			release.Namespace, item.Key, item.Version, item.Value,
			model.DigestValue(item.Value), action, operator, release.ID, now,
		); err != nil {
			return err
		}
		if err := s.insertAuditTx(
			ctx, tx, "config."+lowerAction(action), release.Namespace, item.Key,
			release.ID, oldDigest, model.DigestValue(item.Value), operator, model.CodeOK,
			fmt.Sprintf("release promotion snapshot=%d item_version=%d",
				candidate.Version, item.Version),
		); err != nil {
			return err
		}
		if err := pruneHistoryTx(ctx, tx, release.Namespace, item.Key, s.historyLimit); err != nil {
			return err
		}
	}

	for key, old := range current {
		if _, remains := candidateMap[key]; remains {
			continue
		}
		deletedVersion := old.Version + 1
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM config_items WHERE namespace = ? AND key = ?`,
			release.Namespace, key,
		); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO config_item_versions
			 (namespace, key, version, value, digest, action, operator, release_id, created_at)
			 VALUES (?, ?, ?, '', '', 'DELETE', ?, ?, ?)`,
			release.Namespace, key, deletedVersion, operator, release.ID, now,
		); err != nil {
			return err
		}
		if err := s.insertAuditTx(
			ctx, tx, "config.delete", release.Namespace, key, release.ID,
			model.DigestValue(old.Value), "", operator, model.CodeOK,
			fmt.Sprintf("release promotion snapshot=%d item_version=%d",
				candidate.Version, deletedVersion),
		); err != nil {
			return err
		}
		if err := pruneHistoryTx(ctx, tx, release.Namespace, key, s.historyLimit); err != nil {
			return err
		}
	}

	if _, err := tx.ExecContext(ctx,
		`UPDATE namespace_heads SET snapshot_id = ?, updated_at = ? WHERE namespace = ?`,
		candidate.Version, now, release.Namespace,
	); err != nil {
		return err
	}
	return nil
}

func (s *Store) EvaluateDueReleases(ctx context.Context) (int, error) {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	rows, err := s.db.QueryContext(ctx,
		`SELECT id, observation_ends_at FROM releases WHERE state = 'canary'`,
	)
	if err != nil {
		return 0, err
	}
	var due []string
	for rows.Next() {
		var id, endsAt string
		if err := rows.Scan(&id, &endsAt); err != nil {
			rows.Close()
			return 0, err
		}
		parsed, err := time.Parse(time.RFC3339Nano, endsAt)
		if err != nil || !s.now().Before(parsed) {
			due = append(due, id)
		}
	}
	if err := rows.Close(); err != nil {
		return 0, err
	}

	changed := 0
	for _, id := range due {
		didChange, err := s.evaluateReleaseLocked(ctx, id)
		if err != nil {
			log.Printf("evaluate release %s: %v", id, err)
			continue
		}
		if didChange {
			changed++
		}
	}
	return changed, nil
}

func (s *Store) evaluateReleaseLocked(ctx context.Context, releaseID string) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	release, err := getReleaseTx(ctx, tx, releaseID)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if release.State != model.ReleaseCanary {
		return false, nil
	}
	endsAt, err := time.Parse(time.RFC3339Nano, release.ObservationEndsAt)
	if err == nil && s.now().Before(endsAt) {
		return false, nil
	}
	successes, errorsCount, rate, err := aggregateReportsTx(ctx, tx, releaseID)
	if err != nil {
		return false, err
	}
	candidateDigest, err := snapshotDigestTx(ctx, tx, release.CandidateSnapshot)
	if err != nil {
		return false, err
	}
	headDigest, err := snapshotDigestTx(ctx, tx, release.BaseSnapshotID)
	if err != nil {
		return false, err
	}
	now := s.nowText()
	details := fmt.Sprintf("success=%d error=%d rate=%.6f threshold=%.6f",
		successes, errorsCount, rate, s.errorThreshold)
	if rate > s.errorThreshold {
		reason := fmt.Sprintf("automatic rollback: error rate %.6f exceeds %.6f (%d/%d)",
			rate, s.errorThreshold, errorsCount, successes+errorsCount)
		_, err = tx.ExecContext(ctx,
			`UPDATE releases
			 SET state = 'rolled_back', rolled_back_at = ?, rollback_reason = ?, updated_at = ?
			 WHERE id = ?`,
			now, reason, now, releaseID,
		)
		if err != nil {
			return false, err
		}
		if err := s.insertAuditTx(
			ctx, tx, "release.transition", release.Namespace, "", release.ID,
			candidateDigest, headDigest,
			"system:auto-rollback", model.CodeOK, "from=canary to=rolled_back "+details,
		); err != nil {
			return false, err
		}
	} else {
		_, err = tx.ExecContext(ctx,
			`UPDATE releases SET state = 'observed_pass', passed_at = ?, updated_at = ?
			 WHERE id = ?`, now, now, releaseID,
		)
		if err != nil {
			return false, err
		}
		if err := s.insertAuditTx(
			ctx, tx, "release.transition", release.Namespace, "", release.ID,
			candidateDigest, headDigest,
			"system:observer", model.CodeOK, "from=canary to=observed_pass "+details,
		); err != nil {
			return false, err
		}
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return true, nil
}

func (s *Store) ReportObservation(ctx context.Context, input ObservationInput) (ObservationStats, error) {
	if input.ReleaseID == "" || input.InstanceID == "" {
		return ObservationStats{}, model.Biz(model.CodeInvalidArgument, "release_id and instance_id are required")
	}
	if input.Successes < 0 || input.Errors < 0 {
		return ObservationStats{}, model.Biz(model.CodeInvalidArgument, "counts cannot be negative")
	}
	client, err := s.EnsureClient(ctx, input.InstanceID)
	if err != nil {
		return ObservationStats{}, err
	}
	if client.Ring != model.RingCanary {
		return ObservationStats{}, model.Biz(model.CodeNotCanary, "only canary instances may report observation data")
	}

	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return ObservationStats{}, err
	}
	defer tx.Rollback()

	release, err := getReleaseTx(ctx, tx, input.ReleaseID)
	if errors.Is(err, sql.ErrNoRows) {
		return ObservationStats{}, model.Biz(model.CodeNotFound, "release not found")
	}
	if err != nil {
		return ObservationStats{}, err
	}
	if release.State != model.ReleaseCanary {
		return ObservationStats{}, model.Biz(model.CodeInvalidTransition, "release is not in the observation period")
	}
	if input.LocalVersion != 0 && input.LocalVersion != release.CandidateSnapshot {
		return ObservationStats{}, model.Biz(model.CodeConflict,
			"local_version does not match the candidate snapshot")
	}
	offline := 0
	if input.Offline {
		offline = 1
	}
	now := s.nowText()
	_, err = tx.ExecContext(ctx,
		`INSERT INTO release_reports
		 (release_id, instance_id, success_count, error_count, local_version, offline, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(release_id, instance_id) DO UPDATE SET
		   success_count = MAX(release_reports.success_count, excluded.success_count),
		   error_count = MAX(release_reports.error_count, excluded.error_count),
		   local_version = excluded.local_version,
		   offline = excluded.offline,
		   updated_at = excluded.updated_at`,
		release.ID, input.InstanceID, input.Successes, input.Errors,
		input.LocalVersion, offline, now,
	)
	if err != nil {
		return ObservationStats{}, err
	}
	successes, errorsCount, rate, err := aggregateReportsTx(ctx, tx, release.ID)
	if err != nil {
		return ObservationStats{}, err
	}
	if err := tx.Commit(); err != nil {
		return ObservationStats{}, err
	}
	return ObservationStats{
		ReleaseID:      release.ID,
		SuccessCount:   successes,
		ErrorCount:     errorsCount,
		ErrorRate:      rate,
		ErrorThreshold: s.errorThreshold,
		State:          release.State,
	}, nil
}

func aggregateReportsTx(ctx context.Context, tx *sql.Tx, releaseID string) (int64, int64, float64, error) {
	var successes, errorsCount int64
	err := tx.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(success_count), 0), COALESCE(SUM(error_count), 0)
		 FROM release_reports WHERE release_id = ?`, releaseID,
	).Scan(&successes, &errorsCount)
	if err != nil {
		return 0, 0, 0, err
	}
	rate := 0.0
	if total := successes + errorsCount; total > 0 {
		rate = float64(errorsCount) / float64(total)
	}
	return successes, errorsCount, rate, nil
}

type rowScanner interface {
	Scan(dest ...any) error
}

func scanRelease(scanner rowScanner) (model.Release, error) {
	var release model.Release
	err := scanner.Scan(
		&release.ID, &release.Namespace, &release.State, &release.Operator,
		&release.BaseSnapshotID, &release.CandidateSnapshot, &release.ObservationSeconds,
		&release.CreatedAt, &release.CanaryStartedAt, &release.ObservationEndsAt,
		&release.FullAt, &release.PassedAt, &release.RolledBackAt,
		&release.RollbackReason, &release.UpdatedAt,
	)
	return release, err
}

const releaseColumns = `id, namespace, state, operator, base_snapshot_id,
	candidate_snapshot_id, observation_seconds, created_at, canary_started_at,
	observation_ends_at, full_at, passed_at, rolled_back_at, rollback_reason, updated_at`

func getReleaseTx(ctx context.Context, tx *sql.Tx, releaseID string) (model.Release, error) {
	return scanRelease(tx.QueryRowContext(ctx,
		`SELECT `+releaseColumns+` FROM releases WHERE id = ?`, releaseID,
	))
}

func (s *Store) GetRelease(ctx context.Context, releaseID string) (model.Release, error) {
	var release model.Release
	err := s.db.QueryRowContext(ctx,
		`SELECT `+releaseColumns+` FROM releases WHERE id = ?`, releaseID,
	).Scan(
		&release.ID, &release.Namespace, &release.State, &release.Operator,
		&release.BaseSnapshotID, &release.CandidateSnapshot, &release.ObservationSeconds,
		&release.CreatedAt, &release.CanaryStartedAt, &release.ObservationEndsAt,
		&release.FullAt, &release.PassedAt, &release.RolledBackAt,
		&release.RollbackReason, &release.UpdatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return release, model.Biz(model.CodeNotFound, "release not found")
	}
	if err != nil {
		return release, err
	}
	successes, errorsCount, rate, err := s.releaseReportStats(ctx, releaseID)
	if err != nil {
		return release, err
	}
	release.SuccessCount = successes
	release.ErrorCount = errorsCount
	release.ErrorRate = rate
	release.ErrorThreshold = s.errorThreshold
	return release, nil
}

func (s *Store) releaseReportStats(ctx context.Context, releaseID string) (int64, int64, float64, error) {
	var successes, errorsCount int64
	err := s.db.QueryRowContext(ctx,
		`SELECT COALESCE(SUM(success_count), 0), COALESCE(SUM(error_count), 0)
		 FROM release_reports WHERE release_id = ?`, releaseID,
	).Scan(&successes, &errorsCount)
	if err != nil {
		return 0, 0, 0, err
	}
	rate := 0.0
	if total := successes + errorsCount; total > 0 {
		rate = float64(errorsCount) / float64(total)
	}
	return successes, errorsCount, rate, nil
}

func (s *Store) ListReleases(ctx context.Context, namespace string, limit int) ([]model.Release, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	query := `SELECT ` + releaseColumns + ` FROM releases`
	args := []any{}
	if namespace != "" {
		query += ` WHERE namespace = ?`
		args = append(args, namespace)
	}
	query += ` ORDER BY created_at DESC, id DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var releases []model.Release
	for rows.Next() {
		release, err := scanRelease(rows)
		if err != nil {
			return nil, err
		}
		release.ErrorThreshold = s.errorThreshold
		releases = append(releases, release)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range releases {
		successes, errorsCount, rate, err := s.releaseReportStats(ctx, releases[i].ID)
		if err != nil {
			return nil, err
		}
		releases[i].SuccessCount = successes
		releases[i].ErrorCount = errorsCount
		releases[i].ErrorRate = rate
	}
	return releases, nil
}
