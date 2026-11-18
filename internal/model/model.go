package model

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
)

const (
	RingCanary = "canary"
	RingFull   = "full"
)

const (
	ReleaseDraft        = "draft"
	ReleaseCanary       = "canary"
	ReleaseObservedPass = "observed_pass"
	ReleaseFull         = "full"
	ReleaseRolledBack   = "rolled_back"
)

type Item struct {
	Key     string `json:"key"`
	Value   string `json:"value"`
	Version int64  `json:"version"`
}

type ConfigSet struct {
	Namespace string `json:"namespace"`
	Version   int64  `json:"version"`
	Digest    string `json:"digest"`
	ReleaseID string `json:"release_id,omitempty"`
	Items     []Item `json:"items"`
}

type PullResponse struct {
	ConfigSet
	InstanceID    string `json:"instance_id"`
	Ring          string `json:"ring"`
	ClientVersion int64  `json:"client_version"`
	ServedAt      string `json:"served_at"`
}

type ItemChange struct {
	Key    string `json:"key"`
	Value  string `json:"value"`
	Delete bool   `json:"delete,omitempty"`
}

type Client struct {
	InstanceID string `json:"instance_id"`
	Ring       string `json:"ring"`
	Bucket     int    `json:"bucket"`
	AssignedAt string `json:"assigned_at"`
	LastSeenAt string `json:"last_seen_at"`
}

type Release struct {
	ID                 string  `json:"id"`
	Namespace          string  `json:"namespace"`
	State              string  `json:"state"`
	Operator           string  `json:"operator"`
	BaseSnapshotID     int64   `json:"base_snapshot_id"`
	CandidateSnapshot  int64   `json:"candidate_snapshot_id"`
	ObservationSeconds float64 `json:"observation_seconds"`
	CreatedAt          string  `json:"created_at"`
	CanaryStartedAt    string  `json:"canary_started_at,omitempty"`
	ObservationEndsAt  string  `json:"observation_ends_at,omitempty"`
	FullAt             string  `json:"full_at,omitempty"`
	PassedAt           string  `json:"passed_at,omitempty"`
	RolledBackAt       string  `json:"rolled_back_at,omitempty"`
	RollbackReason     string  `json:"rollback_reason,omitempty"`
	UpdatedAt          string  `json:"updated_at"`

	SuccessCount   int64   `json:"success_count"`
	ErrorCount     int64   `json:"error_count"`
	ErrorRate      float64 `json:"error_rate"`
	ErrorThreshold float64 `json:"error_threshold"`
}

type AuditEntry struct {
	ID         int64  `json:"id"`
	CreatedAt  string `json:"created_at"`
	Operation  string `json:"operation"`
	Namespace  string `json:"namespace,omitempty"`
	Key        string `json:"key,omitempty"`
	ReleaseID  string `json:"release_id,omitempty"`
	OldDigest  string `json:"old_digest,omitempty"`
	NewDigest  string `json:"new_digest,omitempty"`
	Operator   string `json:"operator"`
	StatusCode string `json:"status_code"`
	Details    string `json:"details,omitempty"`
}

func DigestItems(items []Item) string {
	ordered := append([]Item(nil), items...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].Key < ordered[j].Key })
	h := sha256.New()
	for _, item := range ordered {
		_, _ = h.Write([]byte(strconv.Itoa(len(item.Key))))
		_, _ = h.Write([]byte{':'})
		_, _ = h.Write([]byte(item.Key))
		_, _ = h.Write([]byte(strconv.Itoa(len(item.Value))))
		_, _ = h.Write([]byte{':'})
		_, _ = h.Write([]byte(item.Value))
		_, _ = h.Write([]byte(strconv.FormatInt(item.Version, 10)))
		_, _ = h.Write([]byte{'\n'})
	}
	return hex.EncodeToString(h.Sum(nil))
}

func DigestValue(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func ValidateItemChange(change ItemChange) error {
	if change.Key == "" {
		return fmt.Errorf("key is required")
	}
	return nil
}

func MarshalItems(items []Item) (string, error) {
	data, err := json.Marshal(items)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func UnmarshalItems(data string) ([]Item, error) {
	var items []Item
	if err := json.Unmarshal([]byte(data), &items); err != nil {
		return nil, err
	}
	return items, nil
}
