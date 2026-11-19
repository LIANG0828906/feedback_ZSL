package model

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
)

const (
	IngressQUIC = "quic"
	IngressTCP  = "tcp"
)

const (
	CodeOK                     = "OK"
	CodeBadRequest             = "BAD_REQUEST"
	CodeSessionLimit           = "SESSION_LIMIT_REACHED"
	CodeSendWindow             = "SEND_WINDOW_EXHAUSTED"
	CodeRateLimited            = "RATE_LIMITED_BACKEND_BACKPRESSURE"
	CodeSessionPaused          = "SESSION_BACKEND_REJECTION_PAUSED"
	CodeBackendRejected        = "BACKEND_FRAGMENT_REJECTED"
	CodeBackendTimeout         = "BACKEND_TIMEOUT"
	CodeBackendUnavailable     = "BACKEND_UNAVAILABLE"
	CodeSessionIngressMismatch = "SESSION_INGRESS_MISMATCH"
	CodeSessionCompleted       = "SESSION_COMPLETED"
	CodeResumeTokenInvalid     = "RESUME_TOKEN_INVALID"
	CodeConflict               = "CONFLICT"
	CodeNotFound               = "NOT_FOUND"
)

// Range is a closed interval of acknowledged fragment sequence numbers.
// JSON uses the compact [start,end] form in both persisted state and API
// responses.
type Range struct {
	Start int64 `json:"start"`
	End   int64 `json:"end"`
}

func (r Range) MarshalJSON() ([]byte, error) {
	return []byte(fmt.Sprintf("[%d,%d]", r.Start, r.End)), nil
}

func (r *Range) UnmarshalJSON(b []byte) error {
	var v [2]int64
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	if v[0] < 0 || v[1] < v[0] {
		return errors.New("invalid range")
	}
	r.Start, r.End = v[0], v[1]
	return nil
}

func MergeRange(ranges []Range, seq int64) []Range {
	if seq < 0 {
		return ranges
	}
	all := append(append([]Range(nil), ranges...), Range{Start: seq, End: seq})
	return normalizeRanges(all)
}

func MergeRanges(a, b []Range) []Range {
	all := append(append([]Range(nil), a...), b...)
	return normalizeRanges(all)
}

func normalizeRanges(ranges []Range) []Range {
	if len(ranges) == 0 {
		return nil
	}
	sort.Slice(ranges, func(i, j int) bool {
		if ranges[i].Start == ranges[j].Start {
			return ranges[i].End < ranges[j].End
		}
		return ranges[i].Start < ranges[j].Start
	})
	out := make([]Range, 0, len(ranges))
	for _, r := range ranges {
		if r.Start < 0 || r.End < r.Start {
			continue
		}
		if len(out) == 0 || r.Start > out[len(out)-1].End+1 {
			out = append(out, r)
			continue
		}
		if r.End > out[len(out)-1].End {
			out[len(out)-1].End = r.End
		}
	}
	return out
}

func Contains(ranges []Range, seq int64) bool {
	for _, r := range ranges {
		if seq >= r.Start && seq <= r.End {
			return true
		}
		if r.Start > seq {
			break
		}
	}
	return false
}

func AckThrough(ranges []Range) int64 {
	for _, r := range ranges {
		if r.Start == 0 {
			return r.End
		}
		if r.Start > 0 {
			break
		}
	}
	return -1
}

func EncodeRanges(ranges []Range) string {
	b, _ := json.Marshal(ranges)
	return string(b)
}

func DecodeRanges(s string) []Range {
	var out []Range
	if s == "" {
		return out
	}
	_ = json.Unmarshal([]byte(s), &out)
	return out
}

type Session struct {
	ID                 string    `json:"id"`
	DeviceID           string    `json:"device_id"`
	Ingress            string    `json:"ingress"`
	ConnectionID       string    `json:"connection_id"`
	Active             bool      `json:"active"`
	Completed          bool      `json:"completed"`
	Blocked            bool      `json:"blocked"`
	BlockedSeq         int64     `json:"blocked_seq"`
	BlockReason        string    `json:"block_reason"`
	AckRanges          []Range   `json:"accepted_ranges"`
	AckThrough         int64     `json:"ack_through"`
	LastSeq            int64     `json:"last_seq"`
	MigrationCount     int       `json:"migration_count"`
	LastAddr           string    `json:"last_peer_address"`
	LastAddrHost       string    `json:"-"`
	MigrationUntilUnix int64     `json:"migration_until_unix_nano"`
	RateLimitPerSec    float64   `json:"rate_limit_per_sec"`
	LimitActive        bool      `json:"limit_active"`
	LimitReason        string    `json:"limit_reason"`
	LimitBasis         string    `json:"limit_basis"`
	LimitSinceUnix     int64     `json:"limit_since_unix_nano"`
	FailureCount       int64     `json:"failure_count"`
	SuccessCount       int64     `json:"success_count"`
	RejectionCount     int64     `json:"rejection_count"`
	RTTMedianMS        float64   `json:"rtt_median_ms"`
	RTTSamplesMS       []float64 `json:"rtt_samples_ms"`
	BacklogPeak        int       `json:"backend_backlog_peak"`
	SendWindowPeak     int       `json:"send_window_peak"`
	TimeoutCount       int64     `json:"timeout_count"`
	CreatedUnix        int64     `json:"created_unix_nano"`
	UpdatedUnix        int64     `json:"updated_unix_nano"`
}

type Migration struct {
	ID               int64   `json:"id"`
	SessionID        string  `json:"session_id"`
	Seq              int64   `json:"fragment_seq"`
	FromAddr         string  `json:"from_address"`
	ToAddr           string  `json:"to_address"`
	AtUnix           int64   `json:"at_unix_nano"`
	AckRanges        []Range `json:"accepted_ranges_at_migration"`
	AckThrough       int64   `json:"ack_through_at_migration"`
	PreservedLimited bool    `json:"preserved_limit_active"`
	PreservedRate    float64 `json:"preserved_rate_limit_per_sec"`
	PreservedReason  string  `json:"preserved_limit_reason"`
	Trigger          string  `json:"trigger"`
}

type FeedbackEvent struct {
	ID          int64   `json:"id"`
	SessionID   string  `json:"session_id"`
	Ingress     string  `json:"ingress"`
	AtUnix      int64   `json:"at_unix_nano"`
	Source      string  `json:"source"`
	Basis       string  `json:"basis"`
	Reason      string  `json:"reason"`
	Backlog     int     `json:"backend_backlog"`
	RTTMedianMS float64 `json:"rtt_median_ms"`
	PriorRate   float64 `json:"prior_rate_limit_per_sec"`
	NewRate     float64 `json:"new_rate_limit_per_sec"`
}

type Rejection struct {
	ID          int64  `json:"id"`
	SessionID   string `json:"session_id"`
	Seq         int64  `json:"fragment_seq"`
	Reason      string `json:"reason"`
	BackendCode string `json:"backend_code"`
	AtUnix      int64  `json:"at_unix_nano"`
	RecoveredAt int64  `json:"recovered_at_unix_nano,omitempty"`
}

type FragmentState struct {
	Seq       int64  `json:"fragment_seq"`
	State     string `json:"state"`
	Attempts  int    `json:"attempts"`
	Reason    string `json:"reason,omitempty"`
	Size      int64  `json:"size"`
	Checksum  string `json:"checksum_sha256,omitempty"`
	UpdatedAt int64  `json:"updated_unix_nano"`
}
