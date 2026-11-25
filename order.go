package colortrace

import "time"

// StageTiming records observed processing and upstream completion times.
type StageTiming struct {
	Stage               int       `json:"stage"`
	StartedAt           time.Time `json:"started_at"`
	FinishedAt          time.Time `json:"finished_at"`
	UpstreamCommittedAt time.Time `json:"upstream_committed_at,omitempty"`
}

// Validate checks local time order and downstream-after-upstream ordering.
func (t StageTiming) Validate() error {
	if t.FinishedAt.Before(t.StartedAt) {
		return &Error{
			Code: EOrder, Stage: t.Stage, HasStage: true,
			Message: "stage finished before it started",
		}
	}
	if t.Stage > 0 && !t.UpstreamCommittedAt.IsZero() && t.StartedAt.Before(t.UpstreamCommittedAt) {
		return &Error{
			Code: EOrder, Stage: t.Stage, HasStage: true,
			Message: "downstream stage started before upstream commit",
		}
	}
	return nil
}

// VerifyTimeline validates every stage timing in sequence order.
func VerifyTimeline(timings []StageTiming) error {
	for _, timing := range timings {
		if err := timing.Validate(); err != nil {
			return err
		}
	}
	return nil
}
