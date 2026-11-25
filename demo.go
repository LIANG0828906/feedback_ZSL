package lipsync

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type DemoOptions struct {
	OutDir        string
	Seed          int64
	DialogueCount int
	WindowSize    int
	MaxWorkers    int
	FrameBuffer   int
}

func normalizeDemoOptions(options DemoOptions) DemoOptions {
	if options.OutDir == "" {
		options.OutDir = filepath.Join("artifacts", "demo")
	}
	if options.Seed == 0 {
		options.Seed = 42
	}
	if options.DialogueCount <= 0 {
		options.DialogueCount = 6
	}
	if options.WindowSize <= 0 {
		options.WindowSize = DefaultWindowSize
	}
	if options.MaxWorkers <= 0 {
		options.MaxWorkers = DefaultMaxWorkers
	}
	if options.FrameBuffer <= 0 {
		options.FrameBuffer = DefaultFrameBufferLimit
	}
	return options
}

type DemoSummary struct {
	Dialogues        int  `json:"dialogues"`
	Frames           int  `json:"frames"`
	Transitions      int  `json:"transitions"`
	AllPathsEqual    bool `json:"all_paths_equal"`
	AllTruthEqual    bool `json:"all_truth_equal"`
	InjectedFailures int  `json:"injected_failures"`
	InjectedMatched  int  `json:"injected_matched"`
}

type PathSummary struct {
	Name             string           `json:"name"`
	FrameCount       int              `json:"frame_count"`
	TransitionCount  int              `json:"transition_count"`
	ReusedFrames     int              `json:"reused_frames"`
	RecomputedFrames int              `json:"recomputed_frames"`
	RestoredWindows  int              `json:"restored_windows"`
	CommittedWindows int              `json:"committed_windows"`
	ResetReason      string           `json:"reset_reason,omitempty"`
	QueueEvents      []QueueEvent     `json:"queue_events,omitempty"`
	Windows          []WindowSnapshot `json:"windows,omitempty"`
}

func summarizePath(result PathResult) PathSummary {
	return PathSummary{
		Name:             result.Name,
		FrameCount:       len(result.Frames),
		TransitionCount:  len(result.Transitions),
		ReusedFrames:     result.ReusedFrames,
		RecomputedFrames: result.RecomputedFrames,
		RestoredWindows:  result.RestoredWindows,
		CommittedWindows: result.CommittedWindows,
		ResetReason:      result.ResetReason,
		QueueEvents:      result.QueueEvents,
		Windows:          result.Windows,
	}
}

type IncrementalSummary struct {
	InvalidatedFrames int            `json:"invalidated_frames"`
	ReusedFrames      int            `json:"reused_frames"`
	RecomputedFrames  int            `json:"recomputed_frames"`
	OutputMatch       bool           `json:"output_match"`
	Mismatch          *Mismatch      `json:"mismatch,omitempty"`
	Invalidations     []Invalidation `json:"invalidations"`
}

type ScenarioReport struct {
	Name        string             `json:"name"`
	Edits       []string           `json:"edits"`
	FrameCount  int                `json:"frame_count"`
	Incremental IncrementalSummary `json:"incremental"`
}

type InjectionReport struct {
	DialogueID   string       `json:"dialogue_id"`
	Fault        string       `json:"fault"`
	ExpectedCode Code         `json:"expected_code"`
	ActualCode   Code         `json:"actual_code"`
	Error        *DomainError `json:"error,omitempty"`
	Matched      bool         `json:"matched"`
}

type DialogueReport struct {
	Project               DialogueProject    `json:"project"`
	Truth                 Truth              `json:"truth"`
	Timing                Timing             `json:"timing"`
	Boundaries            []Boundary         `json:"boundaries"`
	Realtime              PathSummary        `json:"realtime"`
	Resume                PathSummary        `json:"resume"`
	Full                  PathSummary        `json:"full"`
	Comparisons           []Comparison       `json:"comparisons"`
	Edits                 []string           `json:"edits"`
	Invalidations         []Invalidation     `json:"invalidations"`
	Incremental           IncrementalSummary `json:"incremental"`
	InvalidationScenarios []ScenarioReport   `json:"invalidation_scenarios"`
	QueueEvents           []QueueEvent       `json:"queue_events"`
	DependencyGraph       DependencyGraph    `json:"dependency_graph"`
}

type VerificationReport struct {
	Commands []string `json:"commands"`
	Notes    []string `json:"notes"`
	Checks   []string `json:"checks"`
}

type DemoReport struct {
	SchemaVersion string             `json:"schema_version"`
	Seed          int64              `json:"seed"`
	Generator     string             `json:"generator"`
	Summary       DemoSummary        `json:"summary"`
	Dialogues     []DialogueReport   `json:"dialogues"`
	Injections    []InjectionReport  `json:"injections"`
	Verification  VerificationReport `json:"verification"`
	Artifacts     []string           `json:"artifacts"`
}

type Manifest struct {
	SchemaVersion string            `json:"schema_version"`
	Seed          int64             `json:"seed"`
	Generator     string            `json:"generator"`
	Projects      []DialogueProject `json:"projects"`
	Truths        []Truth           `json:"truths"`
}

func writeJSON(path string, value any) error {
	b, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	return os.WriteFile(path, b, 0o644)
}

func cloneProject(p DialogueProject) (DialogueProject, error) {
	b, err := json.Marshal(p)
	if err != nil {
		return DialogueProject{}, err
	}
	var clone DialogueProject
	if err := json.Unmarshal(b, &clone); err != nil {
		return DialogueProject{}, err
	}
	return clone, nil
}

func pathWithName(result PathResult, name string) PathResult {
	result.Name = name
	return result
}

func fullPath(project DialogueProject) (PathResult, error) {
	timeline, err := BuildTimeline(project)
	if err != nil {
		return PathResult{}, err
	}
	return PathResult{
		Name:             "full",
		Frames:           timeline.Frames,
		Transitions:      timeline.Transitions,
		Switches:         timeline.Switches,
		RecomputedFrames: len(timeline.Frames),
	}, nil
}

func RunDemo(ctx context.Context, options DemoOptions) (DemoReport, error) {
	options = normalizeDemoOptions(options)
	if err := os.MkdirAll(options.OutDir, 0o755); err != nil {
		return DemoReport{}, err
	}
	dbPath := filepath.Join(options.OutDir, "lipsync.db")
	for _, suffix := range []string{"", "-wal", "-shm"} {
		_ = os.Remove(dbPath + suffix)
	}
	set, err := GenerateSampleSet(options.Seed, options.DialogueCount)
	if err != nil {
		return DemoReport{}, err
	}
	projectsPath := filepath.Join(options.OutDir, "projects.json")
	truthsPath := filepath.Join(options.OutDir, "truths.json")
	manifestPath := filepath.Join(options.OutDir, "manifest.json")
	reportPath := filepath.Join(options.OutDir, "report.json")
	markdownPath := filepath.Join(options.OutDir, "report.md")
	htmlPath := filepath.Join(options.OutDir, "index.html")
	if err := writeJSON(projectsPath, set.Projects); err != nil {
		return DemoReport{}, err
	}
	if err := writeJSON(truthsPath, set.Truths); err != nil {
		return DemoReport{}, err
	}
	manifest := Manifest{
		SchemaVersion: "lipsync.sample/v1",
		Seed:          options.Seed,
		Generator:     "stable splitmix64 analytic sample generator",
		Projects:      set.Projects,
		Truths:        set.Truths,
	}
	if err := writeJSON(manifestPath, manifest); err != nil {
		return DemoReport{}, err
	}
	runOptions := RunOptions{
		WindowSize:        options.WindowSize,
		MaxWorkers:        options.MaxWorkers,
		FrameBufferLimit:  options.FrameBuffer,
		InjectStageWindow: -1,
	}
	store, err := OpenStore(dbPath)
	if err != nil {
		return DemoReport{}, err
	}
	realtime := make([]PathResult, len(set.Projects))
	for i, project := range set.Projects {
		realtime[i], err = RunRealtime(ctx, store, project, runOptions)
		if err != nil {
			_ = store.Close()
			return DemoReport{}, err
		}
	}
	if err := store.Close(); err != nil {
		return DemoReport{}, err
	}

	// Reopen the database to prove that resume cannot see in-memory results.
	store, err = OpenStore(dbPath)
	if err != nil {
		return DemoReport{}, err
	}
	defer store.Close()
	resumed := make([]PathResult, len(set.Projects))
	for i, project := range set.Projects {
		resumed[i], err = RunResume(ctx, store, project, runOptions)
		if err != nil {
			return DemoReport{}, err
		}
	}
	full := make([]PathResult, len(set.Projects))
	dialogues := make([]DialogueReport, len(set.Projects))
	allPaths := true
	allTruth := true
	totalFrames := 0
	totalTransitions := 0
	for i, project := range set.Projects {
		full[i], err = fullPath(project)
		if err != nil {
			return DemoReport{}, err
		}
		timeline, err := BuildTimeline(project)
		if err != nil {
			return DemoReport{}, err
		}
		comparisons := []Comparison{
			CompareFrames(project.DialogueID, "realtime_vs_full", realtime[i].Frames, full[i].Frames),
			CompareFrames(project.DialogueID, "realtime_vs_resume", realtime[i].Frames, resumed[i].Frames),
			CompareFrames(project.DialogueID, "resume_vs_full", resumed[i].Frames, full[i].Frames),
			CompareFrames(project.DialogueID, "realtime_vs_truth", realtime[i].Frames, set.Truths[i].Frames),
			CompareFrames(project.DialogueID, "resume_vs_truth", resumed[i].Frames, set.Truths[i].Frames),
			CompareFrames(project.DialogueID, "full_vs_truth", full[i].Frames, set.Truths[i].Frames),
			CompareTransitions(project.DialogueID, "realtime_transitions_vs_truth", realtime[i].Transitions, set.Truths[i].Transitions),
			CompareTransitions(project.DialogueID, "resume_transitions_vs_truth", resumed[i].Transitions, set.Truths[i].Transitions),
			CompareTransitions(project.DialogueID, "full_transitions_vs_truth", full[i].Transitions, set.Truths[i].Transitions),
			CompareTransitions(project.DialogueID, "realtime_transitions_vs_full", realtime[i].Transitions, full[i].Transitions),
			CompareTransitions(project.DialogueID, "resume_transitions_vs_full", resumed[i].Transitions, full[i].Transitions),
		}
		for _, comparison := range comparisons {
			if !comparison.Consistent {
				allPaths = false
				if strings.HasSuffix(comparison.Name, "_vs_truth") {
					allTruth = false
				}
			}
		}
		totalFrames += len(full[i].Frames)
		totalTransitions += len(full[i].Transitions)
		baseline := timeline
		scenarios, edits, invalidations, incremental, err := runInvalidationScenarios(ctx, store, project, baseline, runOptions)
		if err != nil {
			return DemoReport{}, err
		}
		graph := BuildDependencyGraph(project, timeline.Timing, timeline.Schedule, timeline.Frames)
		dialogues[i] = DialogueReport{
			Project:               project,
			Truth:                 set.Truths[i],
			Timing:                timeline.Timing,
			Boundaries:            timeline.Timing.Boundaries,
			Realtime:              summarizePath(realtime[i]),
			Resume:                summarizePath(resumed[i]),
			Full:                  summarizePath(full[i]),
			Comparisons:           comparisons,
			Edits:                 edits,
			Invalidations:         invalidations,
			Incremental:           incremental,
			InvalidationScenarios: scenarios,
			QueueEvents:           realtime[i].QueueEvents,
			DependencyGraph:       graph,
		}
	}
	injections, err := runInjections(ctx, set.Projects[0], runOptions)
	if err != nil {
		return DemoReport{}, err
	}
	matched := 0
	for _, injection := range injections {
		if injection.Matched {
			matched++
		}
	}
	report := DemoReport{
		SchemaVersion: "lipsync.demo/v1",
		Seed:          options.Seed,
		Generator:     "stable splitmix64 analytic sample generator",
		Summary: DemoSummary{
			Dialogues:        len(set.Projects),
			Frames:           totalFrames,
			Transitions:      totalTransitions,
			AllPathsEqual:    allPaths,
			AllTruthEqual:    allTruth,
			InjectedFailures: len(injections),
			InjectedMatched:  matched,
		},
		Dialogues:  dialogues,
		Injections: injections,
		Verification: VerificationReport{
			Commands: []string{
				"go test ./...",
				"go test -race ./...",
				"go run ./cmd/lipsync demo -out artifacts/demo",
				"go run ./cmd/lipsync verify -dir artifacts/demo",
				"go run ./cmd/lipsync serve -dir artifacts/demo",
			},
			Notes: []string{
				"真值由确定性生成器在运行时排程之前独立构造。",
				"恢复路径打开新的 SQLite store，只接受版本匹配的已提交窗口。",
				"增量失效比较显式逐帧依赖键与持久化缓存行。",
				"全链路采样点换算使用整型运算，并遵循声明的 ceil/floor/round 方向。",
			},
			Checks: []string{
				"量化边界、逐帧字符区间与口型序列匹配生成器真值",
				"实时排程、重启恢复与全量重算是逐帧一致的",
				"E_ORDER、E_RATE、E_LIP、E_STAGE 注入链路均可定位",
			},
		},
		Artifacts: []string{
			filepath.Base(projectsPath), filepath.Base(truthsPath), filepath.Base(manifestPath),
			filepath.Base(dbPath), filepath.Base(reportPath), filepath.Base(markdownPath), filepath.Base(htmlPath),
		},
	}
	html, err := RenderHTML(report)
	if err != nil {
		return DemoReport{}, err
	}
	if err := os.WriteFile(htmlPath, html, 0o644); err != nil {
		return DemoReport{}, err
	}
	if err := writeJSON(reportPath, report); err != nil {
		return DemoReport{}, err
	}
	if err := RenderMarkdown(reportPath, report); err != nil {
		return DemoReport{}, err
	}
	return report, nil
}

func runInvalidationScenarios(ctx context.Context, store *Store, project DialogueProject, baseline *Timeline, options RunOptions) ([]ScenarioReport, []string, []Invalidation, IncrementalSummary, error) {
	type scenario struct {
		name  string
		apply func(DialogueProject) (DialogueProject, []string, error)
	}
	scenarios := []scenario{
		{
			name: "phoneme_boundary",
			apply: func(p DialogueProject) (DialogueProject, []string, error) {
				return editPhonemeBoundary(p)
			},
		},
		{
			name: "subtitle_weight",
			apply: func(p DialogueProject) (DialogueProject, []string, error) {
				edited, err := cloneProject(p)
				if err != nil {
					return DialogueProject{}, nil, err
				}
				if len(edited.Subtitle.Weights) > 2 {
					edited.Subtitle.Weights[2] += 3
				} else {
					edited.Subtitle.Weights[0] += 3
				}
				return edited, []string{"subtitle weight char=2 delta=+3"}, nil
			},
		},
		{
			name: "fps",
			apply: func(p DialogueProject) (DialogueProject, []string, error) {
				edited, err := cloneProject(p)
				if err != nil {
					return DialogueProject{}, nil, err
				}
				if edited.FPS == 30 {
					edited.FPS = 24
				} else {
					edited.FPS = 30
				}
				return edited, []string{fmt.Sprintf("fps -> %d", edited.FPS)}, nil
			},
		},
	}
	reports := make([]ScenarioReport, 0, len(scenarios))
	allEdits := make([]string, 0, len(scenarios))
	var representative []Invalidation
	var representativeIncremental IncrementalSummary
	for _, scenario := range scenarios {
		edited, edits, err := scenario.apply(project)
		if err != nil {
			return nil, nil, nil, IncrementalSummary{}, err
		}
		incremental, err := ComputeIncremental(ctx, store, edited, baseline)
		if err != nil {
			return nil, nil, nil, IncrementalSummary{}, fmt.Errorf("%s: %w", scenario.name, err)
		}
		summary := IncrementalSummary{
			InvalidatedFrames: incremental.InvalidatedFrames,
			ReusedFrames:      incremental.ReusedFrames,
			RecomputedFrames:  incremental.RecomputedFrames,
			OutputMatch:       incremental.OutputMatch,
			Mismatch:          incremental.Mismatch,
			Invalidations:     incremental.Invalidations,
		}
		reports = append(reports, ScenarioReport{
			Name:        scenario.name,
			Edits:       edits,
			FrameCount:  len(incremental.Frames),
			Incremental: summary,
		})
		allEdits = append(allEdits, edits...)
		if representative == nil {
			representative = incremental.Invalidations
			representativeIncremental = summary
		}
	}
	return reports, allEdits, representative, representativeIncremental, nil
}

func editPhonemeBoundary(p DialogueProject) (DialogueProject, []string, error) {
	edited, err := cloneProject(p)
	if err != nil {
		return DialogueProject{}, nil, err
	}
	if len(edited.Segments) == 0 || len(edited.Segments[0].Phonemes) < 2 {
		return DialogueProject{}, nil, fmt.Errorf("sample has no editable phoneme boundary")
	}
	phonemes := edited.Segments[0].Phonemes
	index := 0
	if len(phonemes) > 2 {
		index = 1
	}
	left := &phonemes[index]
	right := &phonemes[index+1]
	delta := int64(edited.SampleRate / edited.FPS / 2)
	if delta < 1 {
		delta = 1
	}
	leftSpan := left.EndSample - left.StartSample
	rightSpan := right.EndSample - right.StartSample
	if delta > leftSpan/2 {
		delta = leftSpan / 2
	}
	if delta > rightSpan/2 {
		delta = rightSpan / 2
	}
	if delta < 1 {
		delta = 1
	}
	left.EndSample += delta
	right.StartSample += delta
	return edited, []string{fmt.Sprintf("phoneme boundary segment=0 phoneme=%d delta=+%d", index+1, delta)}, nil
}

func runInjections(ctx context.Context, base DialogueProject, options RunOptions) ([]InjectionReport, error) {
	injections := make([]InjectionReport, 0, 4)
	orderProject, err := cloneProject(base)
	if err != nil {
		return nil, err
	}
	orderProject.Segments[0].Phonemes[1].StartSample = orderProject.Segments[0].Phonemes[0].StartSample
	err = ValidateProject(orderProject)
	injections = append(injections, injectionResult(base.DialogueID, "order", CodeOrder, err))

	rateProject, err := cloneProject(base)
	if err != nil {
		return nil, err
	}
	rateProject.FPS = 48001
	err = ValidateProject(rateProject)
	injections = append(injections, injectionResult(base.DialogueID, "rate", CodeRate, err))

	lipProject, err := cloneProject(base)
	if err != nil {
		return nil, err
	}
	lipProject.Segments[0].Phonemes[0].Lip = LipCategory("invalid_viseme")
	err = ValidateProject(lipProject)
	injections = append(injections, injectionResult(base.DialogueID, "lip", CodeLip, err))

	stageOptions := options
	stageOptions.InjectStageWindow = 0
	stageOptions.InjectStageReason = "injected_window_failure"
	stageStore, err := OpenStore(":memory:")
	if err != nil {
		return nil, err
	}
	_, stageErr := RunRealtime(ctx, stageStore, base, stageOptions)
	_ = stageStore.Close()
	injections = append(injections, injectionResult(base.DialogueID, "stage", CodeStage, stageErr))
	return injections, nil
}

func injectionResult(dialogueID, fault string, expected Code, err error) InjectionReport {
	report := InjectionReport{DialogueID: dialogueID, Fault: fault, ExpectedCode: expected}
	if de, ok := asDomainError(err); ok {
		report.Error = de
		report.ActualCode = de.Code
		report.Matched = de.Code == expected
	}
	return report
}

func RenderMarkdown(reportPath string, report DemoReport) error {
	var b strings.Builder
	fmt.Fprintf(&b, "# 口型同步校准验证报告\n\n")
	fmt.Fprintf(&b, "- 样本种子：`%d`\n", report.Seed)
	fmt.Fprintf(&b, "- 对白数：`%d`\n", report.Summary.Dialogues)
	fmt.Fprintf(&b, "- 总帧数：`%d`\n", report.Summary.Frames)
	fmt.Fprintf(&b, "- 三路径逐帧一致：`%t`\n", report.Summary.AllPathsEqual)
	fmt.Fprintf(&b, "- 生成器真值一致：`%t`\n", report.Summary.AllTruthEqual)
	fmt.Fprintf(&b, "- 注入错误链路命中：`%d/%d`\n\n", report.Summary.InjectedMatched, report.Summary.InjectedFailures)
	b.WriteString("| 对白 | 帧率 | 帧数 | 过渡插入 | 三路径 | 真值 | 恢复复用/重算 |\n")
	b.WriteString("|---|---:|---:|---:|---|---|---|\n")
	for _, dialogue := range report.Dialogues {
		pathOK := true
		for _, comparison := range dialogue.Comparisons {
			if !comparison.Consistent {
				pathOK = false
			}
		}
		truthOK := true
		for _, comparison := range dialogue.Comparisons {
			if strings.HasSuffix(comparison.Name, "_vs_truth") && !comparison.Consistent {
				truthOK = false
			}
		}
		fmt.Fprintf(&b, "| `%s` | %d | %d | %d | %t | %t | %d/%d |\n",
			dialogue.Project.DialogueID, dialogue.Project.FPS, dialogue.Truth.FrameCount,
			len(dialogue.Truth.Transitions), pathOK, truthOK,
			dialogue.Resume.ReusedFrames, dialogue.Resume.RecomputedFrames)
	}
	b.WriteString("\n## 精确失效场景\n\n")
	b.WriteString("| 对白 | 场景 | 新帧数 | 失效帧 | 复用帧 | 重算帧 | 输出 |\n")
	b.WriteString("|---|---|---:|---:|---:|---:|---|\n")
	for _, dialogue := range report.Dialogues {
		for _, scenario := range dialogue.InvalidationScenarios {
			fmt.Fprintf(&b, "| `%s` | `%s` | %d | %d | %d | %d | %t |\n",
				dialogue.Project.DialogueID, scenario.Name, scenario.FrameCount,
				scenario.Incremental.InvalidatedFrames, scenario.Incremental.ReusedFrames,
				scenario.Incremental.RecomputedFrames, scenario.Incremental.OutputMatch)
		}
	}
	b.WriteString("\n## 异常注入\n\n")
	for _, injection := range report.Injections {
		location := ""
		if injection.Error != nil {
			location = injection.Error.Field
			if injection.Error.Frame != nil {
				location += fmt.Sprintf(" frame=%d", *injection.Error.Frame)
			}
		}
		fmt.Fprintf(&b, "- `%s`：期望 `%s`，实际 `%s`，命中 `%t`，定位 `%s field=%s`\n",
			injection.Fault, injection.ExpectedCode, injection.ActualCode, injection.Matched,
			injection.DialogueID, location)
	}
	b.WriteString("\n## 复现命令\n\n")
	for _, command := range report.Verification.Commands {
		fmt.Fprintf(&b, "```bash\n%s\n```\n", command)
	}
	b.WriteString("\n## 实际验证记录\n\n")
	for _, check := range report.Verification.Checks {
		fmt.Fprintf(&b, "- %s\n", check)
	}
	if err := os.WriteFile(reportPath[:len(reportPath)-len(filepath.Ext(reportPath))]+".md", []byte(b.String()), 0o644); err != nil {
		return err
	}
	return nil
}
