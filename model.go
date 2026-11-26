package slikernel

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"sort"
	"strings"
)

const (
	PPM                    int64 = 1_000_000
	MaxWorkers                   = 64
	MaxPerWorkerBuffer           = 4096
	MaxWindowMs            int64 = 7 * 24 * 60 * 60 * 1000
	MaxRollingWindows            = 4096
	DefaultPerWorkerBuffer       = 64
)

type SLIKind string

const (
	KindAvailability SLIKind = "availability"
	KindLatency      SLIKind = "latency"
	KindThroughput   SLIKind = "throughput"
)

func (kind SLIKind) Valid() bool {
	return kind == KindAvailability || kind == KindLatency || kind == KindThroughput
}

type GapPolicy string

const (
	// GapIncludeEmpty keeps empty base windows in the rolling span. Empty
	// windows contribute zero samples but are never fabricated into the
	// latency sample set.
	GapIncludeEmpty GapPolicy = "include_empty"
)

// Project is the canonical internal model. Source schema naming, units and
// time bases are intentionally absent from this structure.
type Project struct {
	ProjectID         string        `json:"project_id"`
	TimeOriginMs      int64         `json:"time_origin_ms"`
	EvaluationStartMs int64         `json:"evaluation_start_ms"`
	EvaluationEndMs   int64         `json:"evaluation_end_ms"`
	Services          []Service     `json:"services"`
	Observations      []Observation `json:"observations"`

	// SourceVersion is diagnostic only. It is deliberately excluded from
	// canonical equality and the rule hash.
	SourceVersion int `json:"-"`
}

type Service struct {
	Name string `json:"name"`
	// SLIs are evaluated in declaration order.
	SLIs []SLI `json:"slis"`
}

type SLI struct {
	ID                      string      `json:"id"`
	Kind                    SLIKind     `json:"kind"`
	Route                   string      `json:"route"`
	StatusCodes             []int       `json:"status_codes,omitempty"`
	WindowMs                int64       `json:"window_ms"`
	TargetPPM               int64       `json:"target_ppm"`
	RollingWindows          int         `json:"rolling_windows"`
	QuantilePPM             int64       `json:"quantile_ppm,omitempty"`
	LatencyThresholdUs      int64       `json:"latency_threshold_us,omitempty"`
	ExpectedEventsPerWindow int64       `json:"expected_events_per_window,omitempty"`
	Thresholds              []Threshold `json:"thresholds,omitempty"`
	GapPolicy               GapPolicy   `json:"gap_policy,omitempty"`
}

type Threshold struct {
	ID                 string `json:"id"`
	BurnPPM            int64  `json:"burn_ppm"`
	ConsecutiveWindows int    `json:"consecutive_windows"`
}

type Observation struct {
	Seq              int64   `json:"seq"`
	Service          string  `json:"service"`
	Route            string  `json:"route"`
	StatusCode       int     `json:"status_code"`
	TimestampMs      int64   `json:"timestamp_ms"`
	Events           int64   `json:"events"`
	Errors           int64   `json:"errors"`
	LatencySamplesUs []int64 `json:"latency_samples_us,omitempty"`
	ThroughputEvents int64   `json:"throughput_events"`
}

// CloneProject returns a deep copy. Loaders, engines and edits must never
// mutate the caller's source project.
func CloneProject(source Project) Project {
	clone := source
	clone.Services = make([]Service, len(source.Services))
	for i, service := range source.Services {
		clone.Services[i] = Service{Name: service.Name, SLIs: make([]SLI, len(service.SLIs))}
		for j, sli := range service.SLIs {
			copied := sli
			copied.StatusCodes = append([]int(nil), sli.StatusCodes...)
			copied.Thresholds = append([]Threshold(nil), sli.Thresholds...)
			clone.Services[i].SLIs[j] = copied
		}
	}
	clone.Observations = make([]Observation, len(source.Observations))
	for i, observation := range source.Observations {
		copied := observation
		copied.LatencySamplesUs = append([]int64(nil), observation.LatencySamplesUs...)
		clone.Observations[i] = copied
	}
	return clone
}

func (project Project) LastObservationSeq() int64 {
	if len(project.Observations) == 0 {
		return 0
	}
	return project.Observations[len(project.Observations)-1].Seq
}

func (project Project) FindService(name string) (*Service, bool) {
	for i := range project.Services {
		if project.Services[i].Name == name {
			return &project.Services[i], true
		}
	}
	return nil, false
}

func (project Project) FindSLI(serviceName, sliID string) (*SLI, bool) {
	service, ok := project.FindService(serviceName)
	if !ok {
		return nil, false
	}
	for i := range service.SLIs {
		if service.SLIs[i].ID == sliID {
			return &service.SLIs[i], true
		}
	}
	return nil, false
}

// ValidateProject checks all cross-field invariants in one place. It is called
// after loading and after edits, so malformed state cannot enter the engine.
func ValidateProject(project Project) error {
	if strings.TrimSpace(project.ProjectID) == "" {
		return biz(CodeInvalidInput, "validate_project", nil, ctx("field", "project_id"))
	}
	if project.TimeOriginMs <= 0 {
		return biz(CodeInvalidInput, "validate_project", nil, ctx("field", "time_origin_ms"))
	}
	if project.EvaluationStartMs < project.TimeOriginMs {
		return biz(CodeRule, "validate_project", nil, ctx("field", "evaluation_start_ms"))
	}
	if project.EvaluationEndMs <= project.EvaluationStartMs {
		return biz(CodeRule, "validate_project", nil, ctx("field", "evaluation_end_ms"))
	}

	services := make(map[string]struct{}, len(project.Services))
	for serviceIndex, service := range project.Services {
		if strings.TrimSpace(service.Name) == "" {
			return biz(CodeInvalidInput, "validate_project", nil, ctx("field", "service.name"))
		}
		if _, exists := services[service.Name]; exists {
			return biz(CodeDuplicateService, "validate_project", nil, ctx("service", service.Name))
		}
		services[service.Name] = struct{}{}
		if len(service.SLIs) == 0 {
			return biz(CodeRule, "validate_project", nil, ctx("service", service.Name))
		}
		slis := make(map[string]struct{}, len(service.SLIs))
		for sliIndex, sli := range service.SLIs {
			context := map[string]any{"service": service.Name, "sli": sli.ID, "service_index": serviceIndex, "sli_index": sliIndex}
			if strings.TrimSpace(sli.ID) == "" {
				return biz(CodeInvalidInput, "validate_project", nil, context)
			}
			if _, exists := slis[sli.ID]; exists {
				return biz(CodeDuplicateSLI, "validate_project", nil, context)
			}
			slis[sli.ID] = struct{}{}
			if !sli.Kind.Valid() {
				return biz(CodeRule, "validate_project", nil, context)
			}
			if strings.TrimSpace(sli.Route) == "" {
				return biz(CodeInvalidInput, "validate_project", nil, context)
			}
			if sli.WindowMs <= 0 || sli.WindowMs > MaxWindowMs {
				return biz(CodeWindowLength, "validate_project", nil, context)
			}
			if sli.RollingWindows < 1 || sli.RollingWindows > MaxRollingWindows {
				return biz(CodeWindowLength, "validate_project", nil, ctx("rolling_windows", sli.RollingWindows))
			}
			if sli.TargetPPM < 0 || sli.TargetPPM > PPM {
				return biz(CodeRatioRange, "validate_project", nil, context)
			}
			if (project.EvaluationStartMs-project.TimeOriginMs)%sli.WindowMs != 0 ||
				(project.EvaluationEndMs-project.EvaluationStartMs)%sli.WindowMs != 0 {
				return biz(CodeWindowLength, "validate_project", nil, context)
			}
			if sli.Kind == KindLatency {
				if sli.QuantilePPM <= 0 || sli.QuantilePPM >= PPM {
					return biz(CodeQuantileRange, "validate_project", nil, context)
				}
				if sli.LatencyThresholdUs <= 0 {
					return biz(CodeRule, "validate_project", nil, context)
				}
			} else if sli.QuantilePPM < 0 || sli.QuantilePPM > PPM {
				return biz(CodeQuantileRange, "validate_project", nil, context)
			}
			if sli.Kind == KindThroughput && sli.ExpectedEventsPerWindow <= 0 {
				return biz(CodeRule, "validate_project", nil, context)
			}
			if sli.GapPolicy == "" {
			} else if sli.GapPolicy != GapIncludeEmpty {
				return biz(CodeRule, "validate_project", nil, context)
			}
			for _, status := range sli.StatusCodes {
				if status < 100 || status > 599 {
					return biz(CodeObservation, "validate_project", nil, ctx("status_code", status))
				}
			}
			thresholds := make(map[string]struct{}, len(sli.Thresholds))
			for _, threshold := range sli.Thresholds {
				if strings.TrimSpace(threshold.ID) == "" {
					return biz(CodeRule, "validate_project", nil, context)
				}
				if _, exists := thresholds[threshold.ID]; exists {
					return biz(CodeRule, "validate_project", nil, ctx2("service", service.Name, "threshold", threshold.ID))
				}
				thresholds[threshold.ID] = struct{}{}
				if threshold.BurnPPM < 0 || threshold.BurnPPM > PPM*100 {
					return biz(CodeRatioRange, "validate_project", nil, ctx2("service", service.Name, "threshold", threshold.ID))
				}
				if threshold.ConsecutiveWindows < 1 || threshold.ConsecutiveWindows > MaxRollingWindows {
					return biz(CodeWindowLength, "validate_project", nil, ctx2("service", service.Name, "threshold", threshold.ID))
				}
			}
		}
	}

	var previousSeq int64
	for index, observation := range project.Observations {
		context := map[string]any{"observation_index": index, "seq": observation.Seq}
		if observation.Seq <= 0 {
			return biz(CodeObservationSeq, "validate_project", nil, context)
		}
		if observation.Seq <= previousSeq {
			return biz(CodeObservationSeq, "validate_project", nil, context)
		}
		if observation.TimestampMs < project.EvaluationStartMs || observation.TimestampMs >= project.EvaluationEndMs {
			return biz(CodeObservationRange, "validate_project", nil, context)
		}
		if _, ok := services[observation.Service]; !ok {
			return biz(CodeObservation, "validate_project", nil, ctx2("seq", observation.Seq, "service", observation.Service))
		}
		if strings.TrimSpace(observation.Route) == "" {
			return biz(CodeObservation, "validate_project", nil, context)
		}
		if observation.StatusCode < 100 || observation.StatusCode > 599 {
			return biz(CodeObservation, "validate_project", nil, context)
		}
		if observation.Events < 0 || observation.Errors < 0 || observation.Errors > observation.Events {
			return biz(CodeObservation, "validate_project", nil, context)
		}
		if observation.ThroughputEvents < 0 {
			return biz(CodeObservation, "validate_project", nil, context)
		}
		for _, latency := range observation.LatencySamplesUs {
			if latency < 0 {
				return biz(CodeObservation, "validate_project", nil, context)
			}
		}
		previousSeq = observation.Seq
	}
	return nil
}

func ProjectRuleVersion(project Project) string {
	type ruleDocument struct {
		ProjectID         string    `json:"project_id"`
		TimeOriginMs      int64     `json:"time_origin_ms"`
		EvaluationStartMs int64     `json:"evaluation_start_ms"`
		EvaluationEndMs   int64     `json:"evaluation_end_ms"`
		Services          []Service `json:"services"`
	}
	document := ruleDocument{
		ProjectID:         project.ProjectID,
		TimeOriginMs:      project.TimeOriginMs,
		EvaluationStartMs: project.EvaluationStartMs,
		EvaluationEndMs:   project.EvaluationEndMs,
		Services:          project.Services,
	}
	encoded, _ := json.Marshal(document)
	sum := sha256.Sum256(encoded)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func SLIAggregationRevision(serviceName string, sli SLI) string {
	revision := struct {
		Service                 string    `json:"service"`
		ID                      string    `json:"id"`
		Kind                    SLIKind   `json:"kind"`
		Route                   string    `json:"route"`
		StatusCodes             []int     `json:"status_codes"`
		WindowMs                int64     `json:"window_ms"`
		QuantilePPM             int64     `json:"quantile_ppm"`
		LatencyThresholdUs      int64     `json:"latency_threshold_us"`
		ExpectedEventsPerWindow int64     `json:"expected_events_per_window"`
		GapPolicy               GapPolicy `json:"gap_policy"`
	}{
		Service: serviceName, ID: sli.ID, Kind: sli.Kind, Route: sli.Route,
		StatusCodes: sli.StatusCodes, WindowMs: sli.WindowMs, QuantilePPM: sli.QuantilePPM,
		LatencyThresholdUs:      sli.LatencyThresholdUs,
		ExpectedEventsPerWindow: sli.ExpectedEventsPerWindow,
		GapPolicy:               sli.GapPolicy,
	}
	encoded, _ := json.Marshal(revision)
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:12])
}

func NormalizeStatusCodes(codes []int) []int {
	if len(codes) == 0 {
		return nil
	}
	copied := append([]int(nil), codes...)
	sort.Ints(copied)
	unique := copied[:0]
	for _, code := range copied {
		if len(unique) == 0 || unique[len(unique)-1] != code {
			unique = append(unique, code)
		}
	}
	return unique
}
