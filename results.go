package mirror

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

type ScreenCommit struct {
	ScreenID      string      `json:"screenId"`
	State         ScreenState `json:"state"`
	Mapping       Mapping     `json:"mapping"`
	Packets       []Packet    `json:"packets"`
	MappingParams string      `json:"mappingParams"`
	Failed        bool        `json:"failed,omitempty"`
	Failure       string      `json:"failure,omitempty"`
}

type WindowCommit struct {
	Sequence         int                  `json:"sequence"`
	WindowIndex      int                  `json:"windowIndex"`
	WindowID         string               `json:"windowId"`
	Status           WindowState          `json:"status"`
	StartTick        int64                `json:"startTick"`
	EndTick          int64                `json:"endTick"`
	ContentHash      string               `json:"contentHash"`
	Screens          []ScreenCommit       `json:"screens"`
	Endpoints        []Endpoint           `json:"endpoints"`
	Arbitrations     []ArbitrationRecord  `json:"arbitrations,omitempty"`
	Invalidations    []WindowInvalidation `json:"invalidations,omitempty"`
	EventsConsumed   int                  `json:"eventsConsumed"`
	SourceVersion    string               `json:"sourceVersion"`
	AlgorithmVersion string               `json:"algorithmVersion"`
	State            StateSnapshot        `json:"state"`
}

func ComputeWindowCommitHash(w WindowCommit) string {
	cp := w
	cp.ContentHash = ""
	return HashJSON(cp)
}

type WindowErrorReport struct {
	ScreenID   string     `json:"screenId"`
	WindowID   string     `json:"windowId"`
	Errors     MapErrors  `json:"errors"`
	Cumulative ErrorBound `json:"cumulativeUpperBound"`
}

type RunResult struct {
	Mode             RunMode              `json:"mode"`
	SessionID        string               `json:"sessionId"`
	SourceVersion    string               `json:"sourceVersion"`
	AlgorithmVersion string               `json:"algorithmVersion"`
	Windows          []WindowCommit       `json:"windows"`
	ErrorReports     []WindowErrorReport  `json:"errorReports"`
	Arbitrations     []ArbitrationRecord  `json:"arbitrations"`
	Invalidations    []WindowInvalidation `json:"invalidations"`
	FinalState       StateSnapshot        `json:"finalState"`
	Checkpoint       *CheckpointState     `json:"checkpoint,omitempty"`
	Rejected         []RejectedCheckpoint `json:"rejectedCheckpoints,omitempty"`
	Scheduler        *SchedulerStats      `json:"scheduler,omitempty"`
}

func (r *RunResult) Normalize() {
	for i := range r.Windows {
		sort.Slice(r.Windows[i].Screens, func(a, b int) bool {
			return r.Windows[i].Screens[a].ScreenID < r.Windows[i].Screens[b].ScreenID
		})
		for j := range r.Windows[i].Screens {
			sort.Slice(r.Windows[i].Screens[j].Packets, func(a, b int) bool {
				return r.Windows[i].Screens[j].Packets[a].Seq < r.Windows[i].Screens[j].Packets[b].Seq
			})
		}
	}
	sort.Slice(r.Windows, func(i, j int) bool { return r.Windows[i].Sequence < r.Windows[j].Sequence })
	sort.Slice(r.ErrorReports, func(i, j int) bool {
		if r.ErrorReports[i].WindowID != r.ErrorReports[j].WindowID {
			return r.ErrorReports[i].WindowID < r.ErrorReports[j].WindowID
		}
		return r.ErrorReports[i].ScreenID < r.ErrorReports[j].ScreenID
	})
}

// CompareRunResults returns E_DIFF at the first screen/window/field mismatch.
func CompareRunResults(a, b *RunResult) error {
	if a == nil || b == nil {
		return &Error{Code: CodeDiff, Path: "result", Message: "one result is nil"}
	}
	ca, cb := *a, *b
	ca.Mode, cb.Mode = "", ""
	ca.Checkpoint, cb.Checkpoint = nil, nil
	ca.Rejected, cb.Rejected = nil, nil
	ca.Scheduler, cb.Scheduler = nil, nil
	path, av, bv, diff := FirstValueDifference(ca, cb)
	if !diff {
		return nil
	}
	e := &Error{Code: CodeDiff, Path: path, Field: path,
		Message: "run results differ", Operands: []int64{},
		Losses: []Loss{{Path: path, Value: stringifyPair(av, bv), Reason: "first mismatch", Disposition: "reject run"}}}
	e.WindowID, e.ScreenID, e.Field = locateDiff(a, path)
	return e
}

func FirstValueDifference(a, b any) (string, any, any, bool) {
	return reflectDifferenceAny(a, b, "")
}

func stringifyPair(a, b any) string {
	return HashJSON(a) + " != " + HashJSON(b)
}

func locateDiff(r *RunResult, path string) (windowID, screenID, field string) {
	field = path
	if idx := strings.LastIndex(path, "."); idx >= 0 && idx+1 < len(path) {
		field = path[idx+1:]
	}
	if idx, ok := parseBracketIndex(path, "ErrorReports["); ok && idx < len(r.ErrorReports) {
		return r.ErrorReports[idx].WindowID, r.ErrorReports[idx].ScreenID, field
	}
	if idx, ok := parseBracketIndex(path, "Invalidations["); ok && idx < len(r.Invalidations) {
		return r.Invalidations[idx].WindowID, r.Invalidations[idx].ScreenID, field
	}
	if idx, ok := parseBracketIndex(path, "Arbitrations["); ok && idx < len(r.Arbitrations) {
		return "", r.Arbitrations[idx].ScreenID, field
	}
	widx, sidx, ok := parseResultIndexes(path)
	if !ok || widx < 0 || widx >= len(r.Windows) {
		return "", "", field
	}
	windowID = r.Windows[widx].WindowID
	if sidx >= 0 && sidx < len(r.Windows[widx].Screens) {
		screenID = r.Windows[widx].Screens[sidx].ScreenID
	}
	return windowID, screenID, field
}

func parseBracketIndex(path, marker string) (int, bool) {
	start := strings.Index(path, marker)
	if start < 0 {
		return 0, false
	}
	start += len(marker)
	end := strings.Index(path[start:], "]")
	if end < 0 {
		return 0, false
	}
	idx, err := strconv.Atoi(path[start : start+end])
	return idx, err == nil
}

func parseResultIndexes(path string) (windowIndex, screenIndex int, ok bool) {
	screenIndex = -1
	marker := "Windows["
	start := strings.Index(path, marker)
	if start < 0 {
		return 0, -1, false
	}
	start += len(marker)
	end := strings.Index(path[start:], "]")
	if end < 0 {
		return 0, -1, false
	}
	widx, err := strconv.Atoi(path[start : start+end])
	if err != nil {
		return 0, -1, false
	}
	rest := path[start+end:]
	smarker := "Screens["
	if sidxStart := strings.Index(rest, smarker); sidxStart >= 0 {
		sidxStart += len(smarker)
		sidxEnd := strings.Index(rest[sidxStart:], "]")
		if sidxEnd >= 0 {
			screenIndex, err = strconv.Atoi(rest[sidxStart : sidxStart+sidxEnd])
			if err != nil {
				screenIndex = -1
			}
		}
	}
	return widx, screenIndex, true
}

func (e *Error) DiffLocation() string {
	return fmt.Sprintf("window=%s screen=%s field=%s", e.WindowID, e.ScreenID, e.Field)
}
