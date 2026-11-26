package mirror

import (
	"fmt"
	"sort"
)

type Truth struct {
	SessionID        string               `json:"sessionId"`
	Seed             int64                `json:"seed"`
	AlgorithmVersion string               `json:"algorithmVersion"`
	Windows          []TruthWindow        `json:"windows"`
	ErrorReports     []WindowErrorReport  `json:"errorReports"`
	Arbitrations     []ArbitrationRecord  `json:"arbitrations"`
	Invalidations    []WindowInvalidation `json:"invalidations"`
}

type TruthWindow struct {
	Sequence int           `json:"sequence"`
	WindowID string        `json:"windowId"`
	Status   WindowState   `json:"status"`
	Screens  []TruthScreen `json:"screens"`
}

type TruthScreen struct {
	ScreenID      string     `json:"screenId"`
	Failed        bool       `json:"failed,omitempty"`
	Failure       string     `json:"failure,omitempty"`
	Output        Region     `json:"output"`
	MappingParams string     `json:"mappingParams"`
	Packets       []Packet   `json:"packets"`
	Errors        MapErrors  `json:"errors"`
	Cumulative    ErrorBound `json:"cumulativeUpperBound"`
}

func BuildTruth(result *RunResult) *Truth {
	if result == nil {
		return nil
	}
	t := &Truth{
		SessionID: result.SessionID, AlgorithmVersion: result.AlgorithmVersion,
		Windows:       make([]TruthWindow, 0, len(result.Windows)),
		ErrorReports:  append([]WindowErrorReport(nil), result.ErrorReports...),
		Arbitrations:  append([]ArbitrationRecord(nil), result.Arbitrations...),
		Invalidations: append([]WindowInvalidation(nil), result.Invalidations...),
	}
	cumulative := map[string]ErrorBound{}
	for _, w := range result.Windows {
		tw := TruthWindow{Sequence: w.Sequence, WindowID: w.WindowID, Status: w.Status}
		screens := append([]ScreenCommit(nil), w.Screens...)
		sort.Slice(screens, func(i, j int) bool { return screens[i].ScreenID < screens[j].ScreenID })
		for _, sc := range screens {
			ts := TruthScreen{
				ScreenID: sc.ScreenID, Failed: sc.Failed, Failure: sc.Failure,
				Output: sc.Mapping.Output, MappingParams: sc.MappingParams,
				Packets: append([]Packet{}, sc.Packets...), Errors: sc.Mapping.Errors,
			}
			if !sc.Failed && !sc.Mapping.Output.Empty() {
				bound := cumulative[sc.ScreenID]
				bound.Add(sc.Mapping.Errors)
				cumulative[sc.ScreenID] = bound
				ts.Cumulative = bound
			}
			tw.Screens = append(tw.Screens, ts)
		}
		t.Windows = append(t.Windows, tw)
	}
	return t
}

// BuildGeneratorTruth constructs truth directly from the canonical generator
// session using only timeline, mapping, and packet primitives. It deliberately
// does not consume a RunResult, state machine, or checkpoint store.
func BuildGeneratorTruth(s *Session) (*Truth, error) {
	if s == nil {
		return nil, errf(CodeSchema, "session", "session is nil")
	}
	t := &Truth{SessionID: s.ID, Seed: s.Seed, AlgorithmVersion: s.AlgorithmVersion}
	timeline := NewTimeline(s)
	cumulative := map[string]ErrorBound{}
	failedAt := map[string]int{}
	for seq, window := range s.Windows {
		if err := timeline.AdvanceTo(window.StartTick); err != nil {
			return nil, err
		}
		pre := timeline.Snapshot(window.ID, window.StartTick)
		active := map[string]Endpoint{}
		for _, ep := range pre.Endpoints {
			active[ep.ScreenID] = ep
		}
		eligible := timeline.EligibleScreens()
		filtered := make([]Screen, 0, len(eligible))
		for _, sc := range eligible {
			if generation, failed := failedAt[sc.ID]; failed {
				if ep, ok := active[sc.ID]; ok && ep.Generation == generation {
					continue
				}
				delete(failedAt, sc.ID)
			}
			filtered = append(filtered, sc)
		}
		sort.Slice(filtered, func(i, j int) bool { return filtered[i].ID < filtered[j].ID })
		screens := make([]TruthScreen, 0, len(filtered))
		for _, sc := range filtered {
			mapping, err := MapScreen(sc, window)
			if err != nil {
				screens = append(screens, TruthScreen{
					ScreenID: sc.ID, Failed: true, Failure: err.Error(), Packets: []Packet{},
				})
				failedAt[sc.ID] = active[sc.ID].Generation
				continue
			}
			packets, err := DistributeScreen(sc, window, mapping)
			if err != nil {
				screens = append(screens, TruthScreen{
					ScreenID: sc.ID, Failed: true, Failure: err.Error(),
					Output: mapping.Output, MappingParams: mappingParams(sc, window),
					Packets: []Packet{}, Errors: mapping.Errors,
				})
				failedAt[sc.ID] = active[sc.ID].Generation
				continue
			}
			screens = append(screens, TruthScreen{
				ScreenID: sc.ID, Output: mapping.Output,
				MappingParams: mappingParams(sc, window), Packets: packets,
				Errors: mapping.Errors,
			})
		}
		if err := timeline.AdvanceTo(window.EndTick); err != nil {
			return nil, err
		}
		for i := range screens {
			if inv, ok := timeline.InvalidationFor(screens[i].ScreenID, window.ID); ok {
				screens[i].Failed = true
				screens[i].Failure = inv.Reason
				screens[i].Packets = []Packet{}
				if ep, ok := active[screens[i].ScreenID]; ok {
					failedAt[screens[i].ScreenID] = ep.Generation
				}
			}
		}
		successCount := 0
		for _, sc := range screens {
			if !sc.Failed {
				successCount++
			}
		}
		status := WindowCommitted
		if successCount == 0 {
			status = WindowFailed
		}
		tw := TruthWindow{Sequence: seq, WindowID: window.ID, Status: status, Screens: screens}
		for ti := range tw.Screens {
			ts := &tw.Screens[ti]
			if ts.Failed || ts.Output.Empty() {
				continue
			}
			bound := cumulative[ts.ScreenID]
			bound.Add(ts.Errors)
			cumulative[ts.ScreenID] = bound
			ts.Cumulative = bound
			t.ErrorReports = append(t.ErrorReports, WindowErrorReport{
				ScreenID: ts.ScreenID, WindowID: window.ID,
				Errors: ts.Errors, Cumulative: bound,
			})
		}
		t.Windows = append(t.Windows, tw)
	}
	t.Arbitrations = timeline.Arbitrations()
	t.Invalidations = timeline.Invalidations()
	sort.Slice(t.ErrorReports, func(i, j int) bool {
		if t.ErrorReports[i].WindowID != t.ErrorReports[j].WindowID {
			return t.ErrorReports[i].WindowID < t.ErrorReports[j].WindowID
		}
		return t.ErrorReports[i].ScreenID < t.ErrorReports[j].ScreenID
	})
	return t, nil
}

// CompareResultToTruth performs an independent truth check and points at the
// first differing window, screen, and field.
func CompareResultToTruth(result *RunResult, truth *Truth) error {
	if result == nil || truth == nil {
		return &Error{Code: CodeDiff, Path: "truth", Field: "truth", Message: "result or truth is nil"}
	}
	if len(result.Windows) != len(truth.Windows) {
		return &Error{Code: CodeDiff, Path: "windows.len", Field: "len",
			Message: fmt.Sprintf("window count differs: %d != %d", len(result.Windows), len(truth.Windows))}
	}
	for wi := range result.Windows {
		rw, tw := result.Windows[wi], truth.Windows[wi]
		if rw.Sequence != tw.Sequence || rw.WindowID != tw.WindowID || rw.Status != tw.Status {
			return &Error{Code: CodeDiff, WindowID: rw.WindowID, Field: "window",
				Message: "window header differs from truth"}
		}
		if len(rw.Screens) != len(tw.Screens) {
			return &Error{Code: CodeDiff, WindowID: rw.WindowID, Field: "screens.len",
				Message: "screen count differs from truth"}
		}
		for si := range rw.Screens {
			rs, ts := rw.Screens[si], tw.Screens[si]
			if rs.ScreenID != ts.ScreenID {
				return &Error{Code: CodeDiff, WindowID: rw.WindowID, ScreenID: rs.ScreenID, Field: "screenId",
					Message: "screen order or identity differs from truth"}
			}
			if rs.Failed != ts.Failed || rs.Failure != ts.Failure {
				return &Error{Code: CodeDiff, WindowID: rw.WindowID, ScreenID: rs.ScreenID, Field: "failure",
					Message: "screen failure state differs from truth"}
			}
			if rs.Mapping.Output != ts.Output {
				return &Error{Code: CodeDiff, WindowID: rw.WindowID, ScreenID: rs.ScreenID, Field: "output",
					Message: fmt.Sprintf("output %+v != truth %+v", rs.Mapping.Output, ts.Output)}
			}
			if rs.MappingParams != ts.MappingParams {
				return &Error{Code: CodeDiff, WindowID: rw.WindowID, ScreenID: rs.ScreenID, Field: "mappingParams",
					Message: "mapping parameters differ from truth"}
			}
			if len(rs.Packets) != len(ts.Packets) {
				return &Error{Code: CodeDiff, WindowID: rw.WindowID, ScreenID: rs.ScreenID, Field: "packets.len",
					Message: "packet count differs from truth"}
			}
			for pi := range rs.Packets {
				if rs.Packets[pi] != ts.Packets[pi] {
					return &Error{Code: CodeDiff, WindowID: rw.WindowID, ScreenID: rs.ScreenID, Field: fmt.Sprintf("packets[%d]", pi),
						Message: "packet differs from truth"}
				}
			}
			if rs.Mapping.Errors != ts.Errors {
				return &Error{Code: CodeDiff, WindowID: rw.WindowID, ScreenID: rs.ScreenID, Field: "errors",
					Message: "mapping error report differs from truth"}
			}
		}
	}
	return nil
}
