package verify

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"time"

	"terrainnormalverify/internal/checkpoint"
	"terrainnormalverify/internal/compare"
	"terrainnormalverify/internal/engine"
	"terrainnormalverify/internal/exportpng"
	"terrainnormalverify/internal/model"
	"terrainnormalverify/internal/truthcheck"
)

type Check struct {
	Name   string `json:"name"`
	Passed bool   `json:"passed"`
	Detail string `json:"detail"`
}

type Report struct {
	Passed          bool                `json:"passed"`
	Checks          []Check             `json:"checks"`
	Truth           *truthcheck.Result  `json:"truth,omitempty"`
	FullRun         *engine.RunReport   `json:"fullRun,omitempty"`
	IncrementalRuns []*engine.RunReport `json:"incrementalRuns,omitempty"`
	ResumeRun       *engine.RunReport   `json:"resumeRun,omitempty"`
	CrashResumeRun  *engine.RunReport   `json:"crashResumeRun,omitempty"`
	SerialRun       *engine.RunReport   `json:"serialRun,omitempty"`
	RetryRun        *engine.RunReport   `json:"retryRun,omitempty"`
	Attributions    map[string]int      `json:"attributions,omitempty"`
	FirstDifference *compare.Difference `json:"firstDifference,omitempty"`
	Artifacts       map[string]string   `json:"artifacts,omitempty"`
	ElapsedMS       float64             `json:"elapsedMs"`
	DatabasePaths   map[string]string   `json:"databasePaths"`
	CommitOrders    map[string][]int    `json:"commitOrders,omitempty"`
}

var EditSequence = []engine.Edit{
	{Kind: "height_delta", BlockID: 5, X: 5, Y: 4, Value: 0.09},
	{Kind: "layer_bias", LayerID: "grass", BiasX: -0.21, BiasY: 0.13, BiasZ: 0, Strength: 1.12},
	{Kind: "height_delta", BlockID: 2, X: 8, Y: 8, Value: 0.07},
	{Kind: "difference_operator", Operator: "sobel"},
}

func Run(project *model.Project, dir string, budget engine.Budget) (*Report, error) {
	started := time.Now()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, model.NewError(model.ErrIO, -1, "verify.dir", err.Error())
	}
	report := &Report{
		DatabasePaths: make(map[string]string),
		Artifacts:     make(map[string]string),
		CommitOrders:  make(map[string][]int),
	}
	add := func(name string, passed bool, detail string) {
		report.Checks = append(report.Checks, Check{Name: name, Passed: passed, Detail: detail})
	}

	truthDB := filepath.Join(dir, "truth.sqlite")
	truthStore, err := openFresh(truthDB)
	if err != nil {
		return report, err
	}
	report.DatabasePaths["truth"] = truthDB
	truthEngine, err := engine.New(project, truthStore, engine.Config{Budget: budget})
	if err != nil {
		truthStore.Close()
		return report, err
	}
	if _, err := truthEngine.Initialize(); err != nil {
		truthStore.Close()
		return report, err
	}
	baseSnapshot := truthEngine.Snapshot()
	truthResult, err := truthcheck.Validate(project, baseSnapshot.Blocks, baseSnapshot.Seams)
	report.Truth = truthResult
	if err != nil {
		truthStore.Close()
		add("analytic truth", false, err.Error())
		return report, err
	}
	add("analytic truth", true, fmt.Sprintf("%d pixels, max normal error %.6f, max slope error %.6f deg, max seam angle error %.6f deg",
		truthResult.PixelsChecked, truthResult.MaxNormalError, truthResult.MaxSlopeErrorDeg, truthResult.MaxSeamAngleErrorDeg))

	report.Attributions = map[string]int{}
	for _, seamReport := range baseSnapshot.Seams {
		for _, violation := range seamReport.Violations {
			report.Attributions[violation.Attribution]++
		}
	}
	attributionOK := report.Attributions["geometric_fold"] > 0 &&
		report.Attributions["material_mix"] > 0 && report.Attributions["difference_operator"] > 0
	add("seam attribution distinguishes geometry and bake artifacts", attributionOK, formatAttributions(report.Attributions))
	exportPaths, err := exportpng.Write(project, baseSnapshot, filepath.Join(dir, "preview"))
	if err != nil {
		truthStore.Close()
		return report, err
	}
	for key, path := range exportPaths {
		report.Artifacts[key] = path
	}
	baseHash := snapshotFingerprint(baseSnapshot)
	truthStore.Close()

	editedProject := project.Clone()
	for _, edit := range EditSequence {
		editedProject, err = engine.ApplyEditsToProject(editedProject, []engine.Edit{edit})
		if err != nil {
			return report, err
		}
	}

	fullDB := filepath.Join(dir, "full.sqlite")
	report.DatabasePaths["full"] = fullDB
	fullStore, err := openFresh(fullDB)
	if err != nil {
		return report, err
	}
	fullEngine, err := engine.New(editedProject, fullStore, engine.Config{Budget: budget})
	if err != nil {
		fullStore.Close()
		return report, err
	}
	fullRun, err := fullEngine.BakeAll("three-path verification full rebuild")
	report.FullRun = fullRun
	if err != nil {
		fullStore.Close()
		return report, err
	}
	fullSnapshot := fullEngine.Snapshot()
	fullCommitOrder, err := fullStore.LatestCommitOrder(editedProject.Revision)
	if err != nil {
		fullStore.Close()
		return report, err
	}
	report.CommitOrders["full"] = fullCommitOrder
	fullStore.Close()
	add("full bake", true, fmt.Sprintf("%d blocks committed, %d queue events", len(fullSnapshot.Blocks), len(fullRun.Queue)))

	incDB := filepath.Join(dir, "incremental.sqlite")
	report.DatabasePaths["incremental"] = incDB
	incStore, err := openFresh(incDB)
	if err != nil {
		return report, err
	}
	incEngine, err := engine.New(project, incStore, engine.Config{Budget: budget})
	if err != nil {
		incStore.Close()
		return report, err
	}
	initialRun, err := incEngine.Initialize()
	if err != nil {
		incStore.Close()
		return report, err
	}
	report.IncrementalRuns = append(report.IncrementalRuns, initialRun)
	expectedCounts := []int{1, 5, 4, 6}
	for i, edit := range EditSequence {
		run, err := incEngine.ApplyEdits([]engine.Edit{edit})
		report.IncrementalRuns = append(report.IncrementalRuns, run)
		if err != nil {
			incStore.Close()
			return report, err
		}
		if run.Rebaked != expectedCounts[i] {
			incStore.Close()
			return report, model.NewError(model.ErrDiff, -1, "incremental.rebaked",
				fmt.Sprintf("edit %d rebaked %d blocks, expected %d", i+1, run.Rebaked, expectedCounts[i]))
		}
	}
	incrementalSnapshot := incEngine.Snapshot()
	incrementalProject := incEngine.Project()
	add("incremental rebake scope", true, "affected counts 1,5,4,6; isolated blocks remained cache hits")
	incrementalCommitOrder, err := incStore.LatestCommitOrder(incrementalProject.Revision)
	if err != nil {
		incStore.Close()
		return report, err
	}
	report.CommitOrders["incremental"] = incrementalCommitOrder
	incStore.Close()

	diff, err := compare.Snapshots(fullSnapshot, incrementalSnapshot)
	report.FirstDifference = diff
	if err != nil {
		return report, err
	}
	if diff != nil {
		add("incremental equals full", false, formatDiff(diff))
		return report, compare.AsError(diff)
	}
	add("incremental equals full", true, "normals, slopes, bounding boxes and seam reports match exactly")
	orderOK := reflect.DeepEqual(fullSnapshot.SnapshotOrder, incrementalSnapshot.SnapshotOrder) &&
		reflect.DeepEqual(fullCommitOrder, incrementalCommitOrder)
	add("stable snapshot submission order", orderOK,
		fmt.Sprintf("full snapshot=%v incremental snapshot=%v sqlite=%v",
			fullSnapshot.SnapshotOrder, incrementalSnapshot.SnapshotOrder, incrementalCommitOrder))

	resumeStore, err := openExisting(incDB)
	if err != nil {
		return report, err
	}
	report.DatabasePaths["resume"] = incDB
	resumeEngine, err := engine.New(incrementalProject, resumeStore, engine.Config{Budget: budget})
	if err != nil {
		resumeStore.Close()
		return report, err
	}
	resumeRun, err := resumeEngine.Initialize()
	report.ResumeRun = resumeRun
	if err != nil {
		resumeStore.Close()
		return report, err
	}
	resumeSnapshot := resumeEngine.Snapshot()
	meta, err := resumeStore.LoadMeta()
	if err != nil {
		resumeStore.Close()
		return report, err
	}
	resumeCommitOrder, err := resumeStore.LatestCommitOrder(incrementalProject.Revision)
	resumeStore.Close()
	if err != nil {
		return report, err
	}
	report.CommitOrders["resume"] = resumeCommitOrder
	diff, err = compare.Snapshots(fullSnapshot, resumeSnapshot)
	if err != nil {
		return report, err
	}
	if diff != nil {
		report.FirstDifference = diff
		add("checkpoint resume equals full", false, formatDiff(diff))
		return report, compare.AsError(diff)
	}
	add("checkpoint resume equals full", resumeRun.Rebaked == 0 && meta.Complete,
		fmt.Sprintf("rebaked=%d cacheHits=%d complete=%t", resumeRun.Rebaked, resumeRun.CacheHits, meta.Complete))

	crashDB := filepath.Join(dir, "crash-resume.sqlite")
	report.DatabasePaths["crashResume"] = crashDB
	crashStore, err := openFresh(crashDB)
	if err != nil {
		return report, err
	}
	crashEngine, err := engine.New(editedProject, crashStore, engine.Config{Budget: budget, CrashAfterCommits: 2})
	if err != nil {
		crashStore.Close()
		return report, err
	}
	_, _ = crashEngine.BakeAll("simulated interrupted full bake")
	crashStore.Close()
	crashStore, err = openExisting(crashDB)
	if err != nil {
		return report, err
	}
	crashEngine, err = engine.New(editedProject, crashStore, engine.Config{Budget: budget})
	if err != nil {
		crashStore.Close()
		return report, err
	}
	crashResumeRun, err := crashEngine.Initialize()
	report.CrashResumeRun = crashResumeRun
	if err != nil {
		crashStore.Close()
		return report, err
	}
	crashSnapshot := crashEngine.Snapshot()
	crashCommitOrder, err := crashStore.LatestCommitOrder(editedProject.Revision)
	if err != nil {
		crashStore.Close()
		return report, err
	}
	report.CommitOrders["crashResume"] = crashCommitOrder
	crashStore.Close()
	diff, err = compare.Snapshots(fullSnapshot, crashSnapshot)
	if err != nil {
		return report, err
	}
	if diff != nil {
		report.FirstDifference = diff
		add("partial checkpoint recovery equals full", false, formatDiff(diff))
		return report, compare.AsError(diff)
	}
	add("partial checkpoint recovery equals full", crashResumeRun.Rebaked == 4,
		fmt.Sprintf("restored 2 committed blocks, rebaked %d missing blocks", crashResumeRun.Rebaked))

	serialDB := filepath.Join(dir, "serial.sqlite")
	report.DatabasePaths["serial"] = serialDB
	serialStore, err := openFresh(serialDB)
	if err != nil {
		return report, err
	}
	serialBudget := budget
	serialBudget.MaxParallelism = 1
	serialEngine, err := engine.New(editedProject, serialStore, engine.Config{Budget: serialBudget})
	if err != nil {
		serialStore.Close()
		return report, err
	}
	serialRun, err := serialEngine.BakeAll("serial consistency verification")
	report.SerialRun = serialRun
	if err != nil {
		serialStore.Close()
		return report, err
	}
	serialSnapshot := serialEngine.Snapshot()
	serialCommitOrder, err := serialStore.LatestCommitOrder(editedProject.Revision)
	if err != nil {
		serialStore.Close()
		return report, err
	}
	report.CommitOrders["serial"] = serialCommitOrder
	serialStore.Close()
	diff, err = compare.Snapshots(fullSnapshot, serialSnapshot)
	if err != nil {
		return report, err
	}
	if diff != nil {
		report.FirstDifference = diff
		add("parallel equals serial", false, formatDiff(diff))
		return report, compare.AsError(diff)
	}
	add("parallel equals serial", reflect.DeepEqual(fullSnapshot.SnapshotOrder, serialSnapshot.SnapshotOrder) &&
		reflect.DeepEqual(fullCommitOrder, serialCommitOrder),
		"pixel data, seam statistics and per-revision commit order are identical")

	faultDB := filepath.Join(dir, "fault-isolation.sqlite")
	report.DatabasePaths["faultIsolation"] = faultDB
	faultStore, err := openFresh(faultDB)
	if err != nil {
		return report, err
	}
	faultEngine, err := engine.New(project, faultStore, engine.Config{Budget: budget})
	if err != nil {
		faultStore.Close()
		return report, err
	}
	if _, err := faultEngine.Initialize(); err != nil {
		faultStore.Close()
		return report, err
	}
	faultBaseline := faultEngine.Snapshot()
	faultEngine.SetFaultBlock(2)
	heightEdit := engine.Edit{Kind: "height_delta", BlockID: 1, X: 8, Y: 8, Value: 0.06}
	_, injectedErr := faultEngine.ApplyEdits([]engine.Edit{heightEdit})
	afterFailure := faultEngine.Snapshot()
	diff, err = compare.Snapshots(faultBaseline, afterFailure)
	if err != nil {
		faultStore.Close()
		return report, err
	}
	isolated := injectedErr != nil && diff == nil
	add("failed block does not pollute committed snapshot", isolated,
		fmt.Sprintf("error=%v unchanged=%t", injectedErr, diff == nil))
	faultEngine.SetFaultBlock(0)
	retryRun, err := faultEngine.RetryFailed()
	report.RetryRun = retryRun
	if err != nil {
		faultStore.Close()
		return report, err
	}
	retrySnapshot := faultEngine.Snapshot()
	faultStore.Close()

	faultProject, err := engine.ApplyEditsToProject(project, []engine.Edit{heightEdit})
	if err != nil {
		return report, err
	}
	expectedDB := filepath.Join(dir, "fault-expected.sqlite")
	expectedStore, err := openFresh(expectedDB)
	if err != nil {
		return report, err
	}
	expectedEngine, err := engine.New(faultProject, expectedStore, engine.Config{Budget: budget})
	if err != nil {
		expectedStore.Close()
		return report, err
	}
	expectedRun, err := expectedEngine.BakeAll("fault retry reference")
	_ = expectedRun
	if err != nil {
		expectedStore.Close()
		return report, err
	}
	expectedSnapshot := expectedEngine.Snapshot()
	expectedStore.Close()
	diff, err = compare.Snapshots(retrySnapshot, expectedSnapshot)
	if err != nil {
		return report, err
	}
	if diff != nil {
		report.FirstDifference = diff
		add("retry failed block", false, formatDiff(diff))
		return report, compare.AsError(diff)
	}
	add("retry failed block", retryRun.Rebaked == 4 && retryRun.CacheHits == 2,
		fmt.Sprintf("rebaked=%d cacheHits=%d", retryRun.Rebaked, retryRun.CacheHits))

	report.Artifacts["fullNormalPng"], _ = exportOne(editedProject, fullSnapshot, filepath.Join(dir, "full-export"))
	report.Artifacts["baseFingerprint"] = baseHash
	report.Artifacts["fullFingerprint"] = snapshotFingerprint(fullSnapshot)
	commitOrderOK := reflect.DeepEqual(report.CommitOrders["full"], report.CommitOrders["incremental"]) &&
		reflect.DeepEqual(report.CommitOrders["full"], report.CommitOrders["resume"]) &&
		reflect.DeepEqual(report.CommitOrders["full"], report.CommitOrders["crashResume"]) &&
		reflect.DeepEqual(report.CommitOrders["full"], report.CommitOrders["serial"])
	add("three-path commit order", commitOrderOK, fmt.Sprintf("%v", report.CommitOrders))
	report.ElapsedMS = float64(time.Since(started).Microseconds()) / 1000
	report.Passed = true
	for _, check := range report.Checks {
		if !check.Passed {
			report.Passed = false
			break
		}
	}
	return report, nil
}

func openFresh(path string) (*checkpoint.Store, error) {
	for _, candidate := range []string{path, path + "-wal", path + "-shm"} {
		if err := os.Remove(candidate); err != nil && !os.IsNotExist(err) {
			return nil, model.NewError(model.ErrIO, -1, "verify.remove", err.Error())
		}
	}
	return openExisting(path)
}

func openExisting(path string) (*checkpoint.Store, error) {
	return checkpoint.Open(path)
}

func formatAttributions(values map[string]int) string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, fmt.Sprintf("%s=%d", key, values[key]))
	}
	return strings.Join(parts, ", ")
}

func formatDiff(diff *compare.Difference) string {
	return fmt.Sprintf("block=%d field=%s pixel=%d left=%v right=%v",
		diff.BlockID, diff.Field, diff.Pixel, diff.Left, diff.Right)
}

func snapshotFingerprint(snapshot engine.Snapshot) string {
	ids := make([]int, 0, len(snapshot.Blocks))
	for id := range snapshot.Blocks {
		ids = append(ids, id)
	}
	sort.Ints(ids)
	parts := make([]string, 0, len(ids))
	for _, id := range ids {
		result := snapshot.Blocks[id]
		parts = append(parts, fmt.Sprintf("%d:%s:%s", id, model.HashNormals(result.Normals), model.HashFloat64(result.Slopes)))
	}
	return model.HashJSON(parts)
}

func exportOne(project *model.Project, snapshot engine.Snapshot, dir string) (string, error) {
	paths, err := exportpng.Write(project, snapshot, dir)
	if err != nil {
		return "", err
	}
	return paths["normalPng"], nil
}
