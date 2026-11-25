package pipeline

import (
	"context"
	"fmt"

	"battlekernel/internal/canonical"
	"battlekernel/internal/checkpoint"
	"battlekernel/internal/engine"
	"battlekernel/internal/errs"
	"battlekernel/internal/loader"
	"battlekernel/internal/model"
)

const DefaultWindowSize = 2

type RunOptions struct {
	WindowSize           int
	StopAfterWindows     int
	InjectFailureRound   int64
	InjectFailureCode    errs.Code
	InjectFailureMessage string
}

type RunReport struct {
	Path             string               `json:"path"`
	ProjectID        string               `json:"project_id"`
	WindowSize       int                  `json:"window_size"`
	Rounds           []model.RoundResult  `json:"rounds"`
	Windows          []model.WindowRecord `json:"windows"`
	SubmittedRounds  []int64              `json:"submitted_rounds"`
	SubmittedWindows []int                `json:"submitted_windows"`
	ReusedRounds     []int64              `json:"reused_rounds"`
	ComputedRounds   []int64              `json:"computed_rounds"`
	StopAfterWindows int                  `json:"stop_after_windows,omitempty"`
	Resumed          bool                 `json:"resumed"`
}

type Difference struct {
	Round    int64  `json:"round,omitempty"`
	Field    string `json:"field"`
	Expected string `json:"expected"`
	Got      string `json:"got"`
	PathA    string `json:"path_a"`
	PathB    string `json:"path_b"`
}

type CompareReport struct {
	ProjectID         string      `json:"project_id"`
	Equal             bool        `json:"equal"`
	Difference        *Difference `json:"difference,omitempty"`
	RoundCount        int         `json:"round_count"`
	WindowCount       int         `json:"window_count"`
	SubmittedSequence []int64     `json:"submitted_sequence"`
}

func FullRecompute(p *model.Project, opts RunOptions) (*RunReport, error) {
	size := normalizeWindowSize(opts.WindowSize)
	results, summaries, err := engine.SimulateAll(p)
	if err != nil {
		return nil, err
	}
	windows := make([]model.WindowRecord, 0, len(summaries))
	submittedRounds := make([]int64, 0, len(results))
	submittedWindows := make([]int, 0, len(summaries))
	for _, summary := range summaries {
		windows = append(windows, model.WindowRecord{
			WindowIndex: summary.WindowIndex, StartRound: summary.StartRound, EndRound: summary.EndRound,
			ContentHash: summary.Hash, FormatVersion: model.CanonicalFormatVersion,
			AlgorithmVersion: model.AlgorithmVersion, ReorderOffsets: summary.Offsets, RoundCount: len(summary.Rounds),
		})
		submittedWindows = append(submittedWindows, summary.WindowIndex)
	}
	for _, r := range results {
		submittedRounds = append(submittedRounds, r.Round)
	}
	return &RunReport{Path: "full", ProjectID: p.ID, WindowSize: size, Rounds: results, Windows: windows, SubmittedRounds: submittedRounds, SubmittedWindows: submittedWindows, ComputedRounds: submittedRounds}, nil
}

func WindowRecordsFromResults(p *model.Project, results []model.RoundResult, size int) []model.WindowRecord {
	size = normalizeWindowSize(size)
	summaries := engine.BuildWindowSummaries(p, results, size)
	windows := make([]model.WindowRecord, 0, len(summaries))
	for _, summary := range summaries {
		windows = append(windows, model.WindowRecord{
			WindowIndex: summary.WindowIndex, StartRound: summary.StartRound, EndRound: summary.EndRound,
			ContentHash: summary.Hash, FormatVersion: model.CanonicalFormatVersion,
			AlgorithmVersion: model.AlgorithmVersion, ReorderOffsets: summary.Offsets, RoundCount: len(summary.Rounds),
		})
	}
	return windows
}

func RunRealtime(ctx context.Context, p *model.Project, store *checkpoint.Store, opts RunOptions) (*RunReport, error) {
	semanticHash, err := loader.SemanticHash(p)
	if err != nil {
		return nil, err
	}
	if err := store.InitializeProject(ctx, p, semanticHash); err != nil {
		return nil, err
	}
	_ = store.ResetProject(ctx, p.ID)
	if err := store.InitializeProject(ctx, p, semanticHash); err != nil {
		return nil, err
	}
	return runCheckpointed(ctx, p, store, opts, false)
}

func RunResume(ctx context.Context, p *model.Project, store *checkpoint.Store, opts RunOptions) (*RunReport, error) {
	semanticHash, err := loader.SemanticHash(p)
	if err != nil {
		return nil, err
	}
	if err := store.ValidateSnapshot(ctx, p.ID); err != nil {
		if resetErr := store.ResetProject(ctx, p.ID); resetErr != nil {
			return nil, resetErr
		}
		if err := store.InitializeProject(ctx, p, semanticHash); err != nil {
			return nil, err
		}
	}
	return runCheckpointed(ctx, p, store, opts, true)
}

func runCheckpointed(ctx context.Context, p *model.Project, store *checkpoint.Store, opts RunOptions, resumed bool) (*RunReport, error) {
	size := normalizeWindowSize(opts.WindowSize)
	report := &RunReport{Path: "realtime", ProjectID: p.ID, WindowSize: size, Resumed: resumed}
	if resumed {
		report.Path = "resume"
	}

	loaded, loadedWindows, err := store.LoadCommittedRounds(ctx, p.ID)
	if err != nil {
		return nil, err
	}
	report.Rounds = append(report.Rounds, loaded...)
	report.Windows = append(report.Windows, loadedWindows...)
	for _, r := range loaded {
		report.ReusedRounds = append(report.ReusedRounds, r.Round)
		report.SubmittedRounds = append(report.SubmittedRounds, r.Round)
	}

	states := engine.InitialStateMap(p)
	nextIndex := 0
	if len(loaded) > 0 {
		last := loaded[len(loaded)-1]
		states = model.StateMap(last.States)
		for i, spec := range p.Rounds {
			if spec.Round == last.Round {
				nextIndex = i + 1
				break
			}
		}
	}

	var windowRounds []model.RoundResult
	windowStartIndex := nextIndex
	committedThisRun := 0

	flush := func(endIndex int) error {
		if len(windowRounds) == 0 {
			return nil
		}
		startRound := windowRounds[0].Round
		endRound := windowRounds[len(windowRounds)-1].Round
		windowIndex := windowIndexForRound(p, startRound, size)
		hash, err := canonicalJSONHash(windowRounds)
		if err != nil {
			return err
		}
		offsets := engine.ConsumedReorderOffsets(p, endRound)
		record := model.WindowRecord{
			WindowIndex: windowIndex, StartRound: startRound, EndRound: endRound, ContentHash: hash,
			FormatVersion: model.CanonicalFormatVersion, AlgorithmVersion: model.AlgorithmVersion,
			ReorderOffsets: offsets, RoundCount: len(windowRounds),
		}
		if err := store.CommitWindow(ctx, checkpoint.CommitWindowInput{ProjectID: p.ID, Window: record, Rounds: windowRounds, ConsumedOffsets: offsets}); err != nil {
			return err
		}
		report.Windows = appendWindowRecord(report.Windows, record)
		report.SubmittedWindows = append(report.SubmittedWindows, windowIndex)
		for _, r := range windowRounds {
			report.SubmittedRounds = append(report.SubmittedRounds, r.Round)
		}
		committedThisRun++
		windowRounds = nil
		_ = endIndex
		return nil
	}

	for i := nextIndex; i < len(p.Rounds); i++ {
		spec := p.Rounds[i]
		if opts.InjectFailureRound == spec.Round {
			windowIndex := windowIndexForRound(p, spec.Round, size)
			code := opts.InjectFailureCode
			if code == "" {
				code = errs.EState
			}
			message := opts.InjectFailureMessage
			if message == "" {
				message = "injected single-round failure"
			}
			if err := store.MarkRoundFailed(ctx, p.ID, windowIndex, spec.Round, code, message); err != nil {
				return nil, err
			}
			return report, &errs.Error{Code: code, Round: spec.Round, Field: "injected_failure", Message: message}
		}

		result, nextStates, err := engine.SettleRound(p, spec, states)
		if err != nil {
			windowIndex := windowIndexForRound(p, spec.Round, size)
			code := errs.CodeOf(err)
			if code == "" {
				code = errs.EState
			}
			_ = store.MarkRoundFailed(ctx, p.ID, windowIndex, spec.Round, code, err.Error())
			return report, err
		}
		states = nextStates
		report.Rounds = append(report.Rounds, result)
		report.ComputedRounds = append(report.ComputedRounds, result.Round)
		windowRounds = append(windowRounds, result)

		relative := int(spec.Round - p.StartRound)
		isWindowEnd := (relative+1)%size == 0 || i == len(p.Rounds)-1
		if isWindowEnd {
			if err := flush(i); err != nil {
				return nil, err
			}
			if opts.StopAfterWindows > 0 && committedThisRun >= opts.StopAfterWindows {
				report.StopAfterWindows = opts.StopAfterWindows
				return report, nil
			}
			windowStartIndex = i + 1
		}
	}
	_ = windowStartIndex
	sortReportRounds(report)
	return report, nil
}

func normalizeWindowSize(size int) int {
	if size <= 0 {
		return DefaultWindowSize
	}
	return size
}

func windowIndexForRound(p *model.Project, round int64, size int) int {
	relative := int(round - p.StartRound)
	if relative < 0 {
		relative = 0
	}
	return relative/size + 1
}

func canonicalJSONHash(v any) (string, error) {
	return canonical.JSONHash(v)
}

func appendWindowRecord(windows []model.WindowRecord, record model.WindowRecord) []model.WindowRecord {
	for i, existing := range windows {
		if existing.WindowIndex == record.WindowIndex {
			windows[i] = record
			return windows
		}
	}
	return append(windows, record)
}

func sortReportRounds(report *RunReport) {
	// Rounds are appended in execution order; this is a defensive stable boundary.
}

func CompareReports(projectID string, expected, actual *RunReport) CompareReport {
	cmp := CompareReport{ProjectID: projectID, Equal: true, RoundCount: len(expected.Rounds), WindowCount: len(expected.Windows)}
	if len(expected.Rounds) != len(actual.Rounds) {
		cmp.Equal = false
		cmp.Difference = &Difference{Field: "rounds.length", Expected: fmt.Sprint(len(expected.Rounds)), Got: fmt.Sprint(len(actual.Rounds)), PathA: expected.Path, PathB: actual.Path}
		return cmp
	}
	for i := range expected.Rounds {
		a := expected.Rounds[i]
		b := actual.Rounds[i]
		if a.Round != b.Round {
			cmp.fail(a.Round, "round", fmt.Sprint(a.Round), fmt.Sprint(b.Round), expected.Path, actual.Path)
			return cmp
		}
		if diff := compareIDSequences(a.Round, "queue", queueIDs(a.Queue), queueIDs(b.Queue), expected.Path, actual.Path); diff != nil {
			cmp.Equal = false
			cmp.Difference = diff
			return cmp
		}
		if diff := compareIDSequences(a.Round, "action_order", actionIDs(a.ActionOrder), actionIDs(b.ActionOrder), expected.Path, actual.Path); diff != nil {
			cmp.Equal = false
			cmp.Difference = diff
			return cmp
		}
		if len(a.States) != len(b.States) {
			cmp.fail(a.Round, "states.length", fmt.Sprint(len(a.States)), fmt.Sprint(len(b.States)), expected.Path, actual.Path)
			return cmp
		}
		bs := model.StateMap(b.States)
		for _, as := range a.States {
			other := bs[as.UnitID]
			if as.CooldownRemaining != other.CooldownRemaining {
				cmp.fail(a.Round, fmt.Sprintf("states[%s].cooldown_remaining", as.UnitID), fmt.Sprint(as.CooldownRemaining), fmt.Sprint(other.CooldownRemaining), expected.Path, actual.Path)
				return cmp
			}
			if as.Resource != other.Resource {
				cmp.fail(a.Round, fmt.Sprintf("states[%s].resource", as.UnitID), fmt.Sprint(as.Resource), fmt.Sprint(other.Resource), expected.Path, actual.Path)
				return cmp
			}
		}
		if a.ContentHash != b.ContentHash {
			cmp.fail(a.Round, "content_hash", a.ContentHash, b.ContentHash, expected.Path, actual.Path)
			return cmp
		}
	}
	if len(expected.Windows) != len(actual.Windows) {
		cmp.Equal = false
		cmp.Difference = &Difference{Field: "windows.length", Expected: fmt.Sprint(len(expected.Windows)), Got: fmt.Sprint(len(actual.Windows)), PathA: expected.Path, PathB: actual.Path}
		return cmp
	}
	for i := range expected.Windows {
		if expected.Windows[i].WindowIndex != actual.Windows[i].WindowIndex || expected.Windows[i].ContentHash != actual.Windows[i].ContentHash {
			cmp.Equal = false
			cmp.Difference = &Difference{Field: fmt.Sprintf("windows[%d].content_hash", i), Expected: expected.Windows[i].ContentHash, Got: actual.Windows[i].ContentHash, PathA: expected.Path, PathB: actual.Path}
			return cmp
		}
		cmp.SubmittedSequence = append(cmp.SubmittedSequence, expected.Windows[i].StartRound)
	}
	return cmp
}

func (c *CompareReport) fail(round int64, field, expected, got, pathA, pathB string) {
	c.Equal = false
	c.Difference = &Difference{Round: round, Field: field, Expected: expected, Got: got, PathA: pathA, PathB: pathB}
}

func queueIDs(entries []model.QueueEntry) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, fmt.Sprintf("%d:%s:%s", e.Position, e.UnitID, e.SkillID))
	}
	return out
}

func actionIDs(entries []model.ActionRecord) []string {
	out := make([]string, 0, len(entries))
	for _, e := range entries {
		out = append(out, fmt.Sprintf("%d:%s:%s", e.Position, e.UnitID, e.SkillID))
	}
	return out
}

func compareIDSequences(round int64, field string, a, b []string, pathA, pathB string) *Difference {
	if len(a) != len(b) {
		return &Difference{Round: round, Field: field + ".length", Expected: fmt.Sprint(len(a)), Got: fmt.Sprint(len(b)), PathA: pathA, PathB: pathB}
	}
	for i := range a {
		if a[i] != b[i] {
			return &Difference{Round: round, Field: fmt.Sprintf("%s[%d]", field, i), Expected: a[i], Got: b[i], PathA: pathA, PathB: pathB}
		}
	}
	return nil
}
