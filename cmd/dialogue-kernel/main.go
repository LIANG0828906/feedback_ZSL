package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"dialoguecal"
)

type schemaReport struct {
	Version     int    `json:"version"`
	Bytes       int    `json:"bytes"`
	Fingerprint string `json:"fingerprint"`
	Defaults    int    `json:"defaultTraces"`
	Conversions int    `json:"conversionTraces"`
	Mappings    int    `json:"fieldMappings"`
	LossyIssues int    `json:"lossyIssues"`
}

type invalidationReport struct {
	IntervalID      string `json:"intervalId"`
	DialogueID      string `json:"dialogueId"`
	SegmentID       string `json:"segmentId"`
	StartFrame      int    `json:"startFrame"`
	EndFrame        int    `json:"endFrame"`
	Reason          string `json:"reason"`
	FeatureReusable bool   `json:"featureReusable"`
}

type report struct {
	Status               string               `json:"status"`
	Seed                 int64                `json:"seed"`
	SemanticFingerprint  string               `json:"semanticFingerprint"`
	Dialogues            int                  `json:"dialogues"`
	Segments             int                  `json:"segments"`
	TruthFrames          int                  `json:"truthFrames"`
	MaxLoudnessDeviation int64                `json:"maxLoudnessDeviation"`
	Schemas              []schemaReport       `json:"schemas"`
	FullIntervalCount    int                  `json:"fullIntervalCount"`
	ResumeReused         int                  `json:"resumeReused"`
	RealtimeIntervals    int                  `json:"realtimeIntervals"`
	CrashCommitted       int                  `json:"crashCommitted"`
	CrashResumeReused    int                  `json:"crashResumeReused"`
	GainInvalidation     []invalidationReport `json:"gainInvalidation"`
	ReusedAfterGainEdit  []string             `json:"reusedAfterGainEdit"`
	QueueEvents          int                  `json:"queueEvents"`
	CheckedAtUTC         string               `json:"checkedAtUtc"`
}

func fail(err error) {
	fmt.Fprintln(os.Stderr, "dialogue-kernel:", err)
	os.Exit(1)
}

func countSegments(p *dialoguecal.Project) int {
	n := 0
	for _, d := range p.Dialogues {
		n += len(d.Segments)
	}
	return n
}

func main() {
	seed := flag.Int64("seed", 20261127, "deterministic fixture seed")
	out := flag.String("out", filepath.Join("artifacts", "demo"), "artifact output directory")
	flag.Parse()

	if err := os.MkdirAll(*out, 0o755); err != nil {
		fail(err)
	}
	generated, err := dialoguecal.Generate(dialoguecal.DefaultGenerateOptions(*seed))
	if err != nil {
		fail(err)
	}
	project := generated.Project
	fingerprint := project.SemanticFingerprint()

	for _, version := range []dialoguecal.SchemaVersion{
		dialoguecal.VersionV1, dialoguecal.VersionV2, dialoguecal.VersionV3,
	} {
		path := filepath.Join(*out, fmt.Sprintf("dialogue-%s.json", version))
		if err := os.WriteFile(path, generated.Docs[version], 0o600); err != nil {
			fail(err)
		}
	}
	truthJSON, err := json.MarshalIndent(generated.Truth, "", "  ")
	if err != nil {
		fail(err)
	}
	if err := os.WriteFile(filepath.Join(*out, "truth.json"), append(truthJSON, '\n'), 0o600); err != nil {
		fail(err)
	}

	rep := report{
		Status: "running", Seed: *seed, SemanticFingerprint: fingerprint,
		Dialogues: len(project.Dialogues), Segments: countSegments(project),
		TruthFrames: len(generated.Truth), CheckedAtUTC: time.Now().UTC().Format(time.RFC3339Nano),
	}
	for _, version := range []dialoguecal.SchemaVersion{
		dialoguecal.VersionV1, dialoguecal.VersionV2, dialoguecal.VersionV3,
	} {
		exported, err := dialoguecal.ExportProject(project, version)
		if err != nil {
			fail(fmt.Errorf("export %s: %w", version, err))
		}
		lossyJSON, err := json.MarshalIndent(exported.Lossy, "", "  ")
		if err != nil {
			fail(err)
		}
		if err := os.WriteFile(filepath.Join(*out, fmt.Sprintf("lossy-%s.json", version)), append(lossyJSON, '\n'), 0o600); err != nil {
			fail(err)
		}
		p, err := dialoguecal.LoadProject(generated.Docs[version])
		if err != nil {
			fail(fmt.Errorf("load generated %s: %w", version, err))
		}
		got := p.SemanticFingerprint()
		if got != fingerprint {
			fail(fmt.Errorf("%s fingerprint mismatch: %s", version, got))
		}
		rep.Schemas = append(rep.Schemas, schemaReport{
			Version: int(version), Bytes: len(generated.Docs[version]), Fingerprint: got,
			Defaults: len(p.Defaults), Conversions: len(p.Conversions), Mappings: len(p.Mappings),
			LossyIssues: len(exported.Lossy),
		})
	}

	dbPath := filepath.Join(*out, "checkpoints.db")
	_ = os.Remove(dbPath)
	store, err := dialoguecal.OpenStore(dbPath)
	if err != nil {
		fail(err)
	}
	defer store.Close()
	engine := dialoguecal.NewEngine(store)

	full, err := engine.Run(context.Background(), project, dialoguecal.RunOptions{Mode: dialoguecal.ModeFull})
	if err != nil {
		fail(fmt.Errorf("full run: %w", err))
	}
	if err := dialoguecal.CompareTruth(generated.Truth, full.Intervals, project.Tolerance); err != nil {
		fail(fmt.Errorf("truth comparison: %w", err))
	}
	dev, err := dialoguecal.TruthDeviation(generated.Truth, full.Intervals)
	if err != nil {
		fail(err)
	}
	rep.MaxLoudnessDeviation = dev
	rep.FullIntervalCount = len(full.Intervals)
	rep.QueueEvents = len(full.QueueEvents)

	resume, err := engine.Run(context.Background(), project, dialoguecal.RunOptions{Mode: dialoguecal.ModeResume})
	if err != nil {
		fail(fmt.Errorf("resume run: %w", err))
	}
	rep.ResumeReused = len(resume.ReusedIntervals)
	if err := dialoguecal.CompareRunResults(full, resume); err != nil {
		fail(fmt.Errorf("full/resume compare: %w", err))
	}
	realtime, err := engine.Run(context.Background(), project, dialoguecal.RunOptions{Mode: dialoguecal.ModeRealTime})
	if err != nil {
		fail(fmt.Errorf("realtime run: %w", err))
	}
	rep.RealtimeIntervals = len(realtime.Intervals)
	if err := dialoguecal.CompareRunResults(full, realtime); err != nil {
		fail(fmt.Errorf("full/realtime compare: %w", err))
	}

	crashPath := filepath.Join(*out, "crash-checkpoints.db")
	_ = os.Remove(crashPath)
	crashStore, err := dialoguecal.OpenStore(crashPath)
	if err != nil {
		fail(err)
	}
	crashEngine := dialoguecal.NewEngine(crashStore)
	partial, crashErr := crashEngine.Run(context.Background(), project, dialoguecal.RunOptions{
		Mode: dialoguecal.ModeFull, StopAfterCommits: 4,
	})
	if crashErr == nil {
		_ = crashStore.Close()
		fail(fmt.Errorf("crash simulation did not stop"))
	}
	rep.CrashCommitted = len(partial.PhysicalCommitOrder)
	recovered, err := crashEngine.Run(context.Background(), project, dialoguecal.RunOptions{Mode: dialoguecal.ModeResume})
	if err != nil {
		_ = crashStore.Close()
		fail(fmt.Errorf("crash recovery: %w", err))
	}
	rep.CrashResumeReused = len(recovered.ReusedIntervals)
	if err := dialoguecal.CompareRunResults(full, recovered); err != nil {
		_ = crashStore.Close()
		fail(fmt.Errorf("crash replay compare: %w", err))
	}
	_ = crashStore.Close()

	gainEdited, err := dialoguecal.ApplyEdit(project, dialoguecal.ProjectEdit{
		Kind: dialoguecal.EditSegmentGain, DialogueID: "dlg-01", SegmentID: "seg-01-01", IntValue: 2500,
	})
	if err != nil {
		fail(err)
	}
	gainRun, err := engine.Run(context.Background(), gainEdited, dialoguecal.RunOptions{
		Mode: dialoguecal.ModeResume, PreviousProject: project,
	})
	if err != nil {
		fail(fmt.Errorf("gain edit: %w", err))
	}
	for _, item := range gainRun.Invalidation.Items {
		rep.GainInvalidation = append(rep.GainInvalidation, invalidationReport{
			IntervalID: item.IntervalID, DialogueID: item.DialogueID, SegmentID: item.SegmentID,
			StartFrame: item.StartFrame, EndFrame: item.EndFrame, Reason: item.Reason,
			FeatureReusable: item.FeatureReusable,
		})
	}
	rep.ReusedAfterGainEdit = append([]string(nil), gainRun.ReusedIntervals...)
	sort.Strings(rep.ReusedAfterGainEdit)
	baselineGain, err := dialoguecal.NewEngine(nil).Run(context.Background(), gainEdited, dialoguecal.RunOptions{Mode: dialoguecal.ModeFull})
	if err != nil {
		fail(fmt.Errorf("gain full baseline: %w", err))
	}
	if err := dialoguecal.CompareRunResults(baselineGain, gainRun); err != nil {
		fail(fmt.Errorf("gain incremental compare: %w", err))
	}
	rep.Status = "ok"

	reportJSON, err := json.MarshalIndent(rep, "", "  ")
	if err != nil {
		fail(err)
	}
	if err := os.WriteFile(filepath.Join(*out, "report.json"), append(reportJSON, '\n'), 0o600); err != nil {
		fail(err)
	}
	fmt.Printf("status=%s seed=%d fingerprint=%s\n", rep.Status, rep.Seed, rep.SemanticFingerprint)
	fmt.Printf("schemas=v1/v2/v3 frames=%d deviation=%d full=%d resumeReused=%d crashResumeReused=%d\n",
		rep.TruthFrames, rep.MaxLoudnessDeviation, rep.FullIntervalCount, rep.ResumeReused, rep.CrashResumeReused)
	fmt.Printf("gainInvalidation=%d reusedAfterEdit=%d queueEvents=%d artifacts=%s\n",
		len(rep.GainInvalidation), len(rep.ReusedAfterGainEdit), rep.QueueEvents, *out)
}
