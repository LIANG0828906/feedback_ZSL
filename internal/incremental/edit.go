package incremental

import (
	"encoding/json"

	"featurekernel/internal/kernel"
)

type EventPatch struct {
	TimeMs  *int64   `json:"time_ms,omitempty"`
	Channel *string  `json:"channel,omitempty"`
	Value   *float64 `json:"value,omitempty"`
}

type Edit struct {
	ChangeWidth        map[string]int64            `json:"change_width,omitempty"`
	ChangeAggregations map[string][]kernel.AggFunc `json:"change_aggregations,omitempty"`
	AppendEvents       []kernel.Event              `json:"append_events,omitempty"`
	UpdateEvents       map[int64]EventPatch        `json:"update_events,omitempty"`
	DeleteEvents       []int64                     `json:"delete_events,omitempty"`
}

type ChangeSet struct {
	WidthFeatureIDs       []string `json:"width_feature_ids,omitempty"`
	AggregationFeatureIDs []string `json:"aggregation_feature_ids,omitempty"`
	AppendedEventSeqs     []int64  `json:"appended_event_seqs,omitempty"`
	UpdatedEventSeqs      []int64  `json:"updated_event_seqs,omitempty"`
	DeletedEventSeqs      []int64  `json:"deleted_event_seqs,omitempty"`
}

func CloneProject(project kernel.Project) (kernel.Project, error) {
	data, err := json.Marshal(project)
	if err != nil {
		return kernel.Project{}, kernel.WrapError(kernel.CodeInvalidArgument, err, map[string]any{"stage": "clone_project"})
	}
	var clone kernel.Project
	if err := json.Unmarshal(data, &clone); err != nil {
		return kernel.Project{}, kernel.WrapError(kernel.CodeInvalidArgument, err, map[string]any{"stage": "clone_project_decode"})
	}
	return clone, nil
}

func (e Edit) IsEmpty() bool {
	return len(e.ChangeWidth) == 0 && len(e.ChangeAggregations) == 0 &&
		len(e.AppendEvents) == 0 && len(e.UpdateEvents) == 0 && len(e.DeleteEvents) == 0
}
