package gateway

import (
	"fmt"
	"sort"
	"strings"
)

func (p *Project) Validate() error {
	if p.ProjectID == "" {
		return NewError(CodeFieldRequired, "project_id", "project id is required")
	}
	if p.ProjectVersion == "" {
		return NewError(CodeFieldRequired, "project_version", "project version is required")
	}
	if p.Window.WidthMS == 0 {
		return NewError(CodeWindowWidthInvalid, "window.width_ms", "window width must not be zero").
			WithContext("slide_ms", p.Window.SlideMS)
	}
	if p.Window.WidthMS < 0 {
		return NewError(CodeWindowWidthInvalid, "window.width_ms", "window width must be positive").
			WithContext("value", p.Window.WidthMS)
	}
	if p.Window.SlideMS <= 0 {
		return NewError(CodeWindowSlideInvalid, "window.slide_ms", "window slide must be positive").
			WithContext("value", p.Window.SlideMS).WithContext("width_ms", p.Window.WidthMS)
	}
	if p.Window.SlideMS > p.Window.WidthMS {
		return NewError(CodeWindowSlideInvalid, "window.slide_ms", "slide must not exceed width").
			WithContext("slide_ms", p.Window.SlideMS).WithContext("width_ms", p.Window.WidthMS)
	}
	if p.AllowedLatenessMS < 0 {
		return NewError(CodeFieldType, "allowed_lateness_ms", "allowed lateness must be non-negative").
			WithContext("value", p.AllowedLatenessMS)
	}
	if _, err := NewHistogram(p.HistogramBoundariesUS, p.DurationToleranceUS); err != nil {
		return err
	}
	if err := p.Budget.ValidateBudget(); err != nil {
		return err
	}
	if p.Export.MaxWindowsPerBatch <= 0 {
		return NewError(CodeExportConstraint, "export.max_windows_per_batch", "must be positive").
			WithContext("value", p.Export.MaxWindowsPerBatch)
	}
	if p.Export.MaxRecordsPerBatch <= 0 {
		return NewError(CodeExportConstraint, "export.max_records_per_batch", "must be positive").
			WithContext("value", p.Export.MaxRecordsPerBatch)
	}

	serviceIDs := map[string]bool{}
	for i, service := range p.Services {
		if service.ID == "" {
			return NewError(CodeFieldRequired, fmt.Sprintf("services[%d].id", i), "service id is required")
		}
		if serviceIDs[service.ID] {
			return NewError(CodeFieldAmbiguous, fmt.Sprintf("services[%d].id", i), "duplicate service id").
				WithContext("id", service.ID)
		}
		serviceIDs[service.ID] = true
	}
	routeIDs := map[string]bool{}
	for i, route := range p.Routes {
		if route.ID == "" {
			return NewError(CodeFieldRequired, fmt.Sprintf("routes[%d].id", i), "route id is required")
		}
		if routeIDs[route.ID] {
			return NewError(CodeFieldAmbiguous, fmt.Sprintf("routes[%d].id", i), "duplicate route id").
				WithContext("id", route.ID)
		}
		if !serviceIDs[route.ServiceID] {
			return NewError(CodeFieldRequired, fmt.Sprintf("routes[%d].service_id", i), "route references unknown service").
				WithContext("route_id", route.ID).WithContext("service_id", route.ServiceID)
		}
		if route.Prefix == "" {
			return NewError(CodeFieldRequired, fmt.Sprintf("routes[%d].prefix", i), "route prefix is required").
				WithContext("route_id", route.ID)
		}
		routeIDs[route.ID] = true
	}

	for i := range p.Records {
		record := &p.Records[i]
		if record.ID == "" {
			return NewError(CodeFieldRequired, fmt.Sprintf("records[%d].id", i), "record id is required")
		}
		if record.RequestBytes < 0 {
			return NewError(CodeNegativeBytes, fmt.Sprintf("records[%d].request_bytes", i), "request bytes must not be negative").
				WithContext("record_id", record.ID).WithContext("value", record.RequestBytes)
		}
		if record.ResponseBytes < 0 {
			return NewError(CodeNegativeBytes, fmt.Sprintf("records[%d].response_bytes", i), "response bytes must not be negative").
				WithContext("record_id", record.ID).WithContext("value", record.ResponseBytes)
		}
		if record.DurationUS < 0 {
			return NewError(CodeDurationNegative, fmt.Sprintf("records[%d].duration_us", i), "duration must not be negative").
				WithContext("record_id", record.ID).WithContext("value", record.DurationUS)
		}
		if record.ServiceID == "" || record.RouteID == "" {
			route, err := p.MatchRoute(record.Path)
			if err != nil {
				return err
			}
			record.RouteID = route.ID
			record.ServiceID = route.ServiceID
		}
		if !routeIDs[record.RouteID] {
			return NewError(CodeFieldRequired, fmt.Sprintf("records[%d].route_id", i), "record references unknown route").
				WithContext("record_id", record.ID).WithContext("route_id", record.RouteID)
		}
		if !serviceIDs[record.ServiceID] {
			return NewError(CodeFieldRequired, fmt.Sprintf("records[%d].service_id", i), "record references unknown service").
				WithContext("record_id", record.ID).WithContext("service_id", record.ServiceID)
		}
	}

	if p.CommittedBoundaryMS != nil && p.WatermarkMS != nil && *p.WatermarkMS < *p.CommittedBoundaryMS {
		return NewError(CodeWatermarkBeforeCommit, "watermark_ms", "watermark is earlier than committed boundary").
			WithContext("watermark_ms", *p.WatermarkMS).
			WithContext("committed_boundary_ms", *p.CommittedBoundaryMS)
	}
	for i, mark := range p.WatermarkMarks {
		if p.CommittedBoundaryMS != nil && mark.WatermarkMS < *p.CommittedBoundaryMS {
			return NewError(CodeWatermarkBeforeCommit, fmt.Sprintf("watermark_marks[%d]", i), "watermark is earlier than committed boundary").
				WithContext("watermark_ms", mark.WatermarkMS).
				WithContext("committed_boundary_ms", *p.CommittedBoundaryMS)
		}
	}
	return nil
}

func (b Budget) ValidateBudget() error {
	if b.Workers <= 0 {
		return NewError(CodeBudgetWorkersInvalid, "budget.workers", "workers must be positive").WithContext("value", b.Workers)
	}
	if b.MaxWorkers > 0 && b.Workers > b.MaxWorkers {
		return NewError(CodeBudgetWorkersInvalid, "budget.workers", "workers exceed declared budget").
			WithContext("workers", b.Workers).WithContext("max_workers", b.MaxWorkers)
	}
	if b.ServiceBuffer <= 0 {
		return NewError(CodeBudgetBufferInvalid, "budget.service_buffer", "service buffer must be positive").
			WithContext("value", b.ServiceBuffer)
	}
	if b.MaxServiceBuffer > 0 && b.ServiceBuffer > b.MaxServiceBuffer {
		return NewError(CodeBudgetBufferInvalid, "budget.service_buffer", "service buffer exceeds declared cap").
			WithContext("buffer", b.ServiceBuffer).WithContext("max_buffer", b.MaxServiceBuffer)
	}
	return nil
}

func (p *Project) RouteByID(id string) *Route {
	for i := range p.Routes {
		if p.Routes[i].ID == id {
			return &p.Routes[i]
		}
	}
	return nil
}

func (p *Project) MatchRoute(path string) (*Route, error) {
	var best *Route
	for i := range p.Routes {
		route := &p.Routes[i]
		if strings.HasPrefix(path, route.Prefix) {
			if best == nil || len(route.Prefix) > len(best.Prefix) || (len(route.Prefix) == len(best.Prefix) && route.Order < best.Order) {
				best = route
			}
		}
	}
	if best == nil {
		return nil, NewError(CodeFieldRequired, "record.path", "no route prefix matches path").WithContext("path", path)
	}
	return best, nil
}

func (r *Route) Classify(statusCode int, durationUS int64) (string, bool) {
	timedOut := r.TimeoutMS > 0 && durationUS > r.TimeoutMS*1000
	for _, rule := range r.ErrorRules {
		if rule.TimeoutOnly && timedOut {
			return rule.Class, timedOut
		}
		minOK := rule.StatusMin == 0 || statusCode >= rule.StatusMin
		maxOK := rule.StatusMax == 0 || statusCode <= rule.StatusMax
		if !rule.TimeoutOnly && minOK && maxOK {
			return rule.Class, timedOut
		}
	}
	switch {
	case timedOut:
		return "timeout", timedOut
	case statusCode >= 500:
		return "5xx", timedOut
	case statusCode >= 400:
		return "4xx", timedOut
	default:
		return "ok", timedOut
	}
}

// WindowIndices returns all aligned sliding windows containing eventTimeMS.
// Alignment is relative to TimeOriginMS. Half-open windows are
// [origin+index*slide, origin+index*slide+width).
func (p *Project) WindowIndices(eventTimeMS int64) ([]int64, error) {
	if p.Window.WidthMS <= 0 {
		return nil, NewError(CodeWindowWidthInvalid, "window.width_ms", "window width must be positive").
			WithContext("value", p.Window.WidthMS)
	}
	if p.Window.SlideMS <= 0 || p.Window.SlideMS > p.Window.WidthMS {
		return nil, NewError(CodeWindowSlideInvalid, "window.slide_ms", "invalid slide").
			WithContext("slide_ms", p.Window.SlideMS).WithContext("width_ms", p.Window.WidthMS)
	}
	offset := eventTimeMS - p.TimeOriginMS
	low := floorDiv(offset-p.Window.WidthMS, p.Window.SlideMS) + 1
	high := floorDiv(offset, p.Window.SlideMS)
	indices := make([]int64, 0, high-low+1)
	for index := low; index <= high; index++ {
		start := p.TimeOriginMS + index*p.Window.SlideMS
		end := start + p.Window.WidthMS
		if start <= eventTimeMS && eventTimeMS < end {
			indices = append(indices, index)
		}
	}
	return indices, nil
}

func (p *Project) WindowBounds(index int64) (int64, int64) {
	start := p.TimeOriginMS + index*p.Window.SlideMS
	return start, start + p.Window.WidthMS
}

func floorDiv(a, b int64) int64 {
	q := a / b
	r := a % b
	if r != 0 && ((r < 0) != (b < 0)) {
		q--
	}
	return q
}

func SortedRecords(records []Record) []Record {
	out := append([]Record(nil), records...)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].ArrivalSeq != out[j].ArrivalSeq {
			return out[i].ArrivalSeq < out[j].ArrivalSeq
		}
		return out[i].ID < out[j].ID
	})
	return out
}
