package paths

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"mediaforge/internal/domain"
	"mediaforge/internal/errs"
	"mediaforge/internal/mfxfmt"
	"mediaforge/internal/project"
	"mediaforge/internal/runner"
	"mediaforge/internal/state"
	"mediaforge/internal/store"
)

type VariantSnapshot struct {
	ID             string   `json:"id"`
	Ordinal        int      `json:"ordinal"`
	CommitSeq      int64    `json:"commitSeq"`
	OutputSHA256   string   `json:"outputSha256"`
	OutputSize     int64    `json:"outputSize"`
	MetadataFields []string `json:"metadataFields"`
}

type PathReport struct {
	Mode      runner.Mode       `json:"mode"`
	Committed int               `json:"committed"`
	Total     int               `json:"total"`
	Variants  []VariantSnapshot `json:"variants"`
	Paused    bool              `json:"paused"`
}

type Heatmap struct {
	Variant  string `json:"variant"`
	Grid     int    `json:"grid"`
	Width    int    `json:"width"`
	Height   int    `json:"height"`
	MaxDelta int    `json:"maxDelta"`
	Changed  int    `json:"changed"`
	Cells    []int  `json:"cells"`
}

type Comparison struct {
	ProjectID     string                `json:"projectId"`
	Equivalent    bool                  `json:"equivalent"`
	Paths         map[string]PathReport `json:"paths"`
	FirstMismatch *errs.Mismatch        `json:"firstMismatch,omitempty"`
	Heatmaps      []Heatmap             `json:"heatmaps"`
	PauseAfter    int                   `json:"pauseAfter"`
	ElapsedMs     int64                 `json:"elapsedMs"`
}

func Compare(ctx context.Context, projectPath string, workers, pauseAfter int) (*Comparison, error) {
	start := time.Now()
	p, err := project.Load(projectPath)
	if err != nil {
		return nil, err
	}
	dir := filepath.Dir(projectPath)
	rp, err := project.Validate(p, dir)
	if err != nil {
		return nil, err
	}
	fp := project.Fingerprint(p, rp)
	if pauseAfter <= 0 {
		pauseAfter = len(rp.Variants) / 2
		if pauseAfter < 1 {
			pauseAfter = 1
		}
	}
	tmp, err := os.MkdirTemp("", "mediaforge-paths-*")
	if err != nil {
		return nil, errs.Wrap(errs.EIO, "create path comparison workspace", err)
	}
	defer os.RemoveAll(tmp)
	report := &Comparison{
		ProjectID: p.ID, PauseAfter: pauseAfter,
		Paths: map[string]PathReport{},
	}
	type loadedPath struct {
		report PathReport
		store  *store.Store
		blobs  map[string][]byte
	}
	results := map[string]*loadedPath{}
	runPath := func(name string, mode runner.Mode, pause int, resumeAgain bool) error {
		st, err := store.Open(filepath.Join(tmp, name+".sqlite"))
		if err != nil {
			return err
		}
		r := runner.New(st, p, rp, fp, dir)
		rep, err := r.Run(ctx, runner.Options{
			Mode: mode, Workers: workers, PauseAfter: pause,
		})
		if err != nil {
			st.Close()
			return err
		}
		if resumeAgain {
			rep, err = r.Run(ctx, runner.Options{Mode: runner.ModeResume, Workers: workers})
			if err != nil {
				st.Close()
				return err
			}
		}
		recs, err := st.Variants(ctx)
		if err != nil {
			st.Close()
			return err
		}
		pathRep := PathReport{Mode: mode, Paused: rep.Paused}
		blobs := map[string][]byte{}
		for _, rec := range recs {
			pathRep.Total++
			if rec.State != state.Committed {
				continue
			}
			pathRep.Committed++
			pathRep.Variants = append(pathRep.Variants, VariantSnapshot{
				ID: rec.ID, Ordinal: rec.Ordinal, CommitSeq: rec.CommitSeq,
				OutputSHA256: rec.OutputSHA256, OutputSize: rec.OutputSize,
				MetadataFields: append([]string(nil), rec.MetadataFields...),
			})
			blobs[rec.ID] = append([]byte(nil), rec.OutputBlob...)
		}
		sort.Slice(pathRep.Variants, func(i, j int) bool {
			return pathRep.Variants[i].Ordinal < pathRep.Variants[j].Ordinal
		})
		report.Paths[name] = pathRep
		results[name] = &loadedPath{report: pathRep, store: st, blobs: blobs}
		return nil
	}
	if err := runPath("realtime", runner.ModeRealtime, 0, false); err != nil {
		return report, err
	}
	if err := runPath("full", runner.ModeFull, 0, false); err != nil {
		return report, err
	}
	if err := runPath("resume", runner.ModeResume, pauseAfter, true); err != nil {
		return report, err
	}
	defer func() {
		for _, r := range results {
			_ = r.store.Close()
		}
	}()
	base := results["realtime"]
	if base == nil || len(base.report.Variants) != len(rp.Variants) {
		return report, errs.New(errs.EVerify, "realtime path did not commit every variant")
	}
	for _, name := range []string{"full", "resume"} {
		other := results[name]
		if other == nil {
			return report, errs.New(errs.EVerify, "missing path result "+name)
		}
		if len(other.report.Variants) != len(base.report.Variants) {
			m := errs.Mismatch{Artifact: "commitSequence", Field: "variantCount",
				Actual: len(other.report.Variants), Expected: len(base.report.Variants)}
			report.FirstMismatch = &m
			report.Equivalent = false
			return report, errs.Diff(m, "three-path comparison found commit count mismatch")
		}
		for i := range base.report.Variants {
			a, b := base.report.Variants[i], other.report.Variants[i]
			if a.ID != b.ID {
				m := errs.Mismatch{Variant: a.ID, Artifact: "commitSequence", Field: "variantId",
					Actual: b.ID, Expected: a.ID}
				return diffReport(report, m)
			}
			if a.CommitSeq != b.CommitSeq {
				m := errs.Mismatch{Variant: a.ID, Artifact: "commitSequence", Field: "commitSeq",
					Actual: b.CommitSeq, Expected: a.CommitSeq}
				return diffReport(report, m)
			}
			if a.OutputSHA256 != b.OutputSHA256 {
				m := errs.Mismatch{Variant: a.ID, Artifact: "mfx", Field: "fingerprint",
					Actual: b.OutputSHA256, Expected: a.OutputSHA256}
				return diffReport(report, m)
			}
			if a.OutputSize != b.OutputSize {
				m := errs.Mismatch{Variant: a.ID, Artifact: "mfx", Field: "sizeBytes",
					Actual: b.OutputSize, Expected: a.OutputSize}
				return diffReport(report, m)
			}
			if !sameStrings(a.MetadataFields, b.MetadataFields) {
				m := errs.Mismatch{Variant: a.ID, Artifact: "mfx", Field: "metadataFields",
					Actual: b.MetadataFields, Expected: a.MetadataFields}
				return diffReport(report, m)
			}
		}
	}
	report.Equivalent = true
	for _, snap := range base.report.Variants {
		fullBlob := results["full"].blobs[snap.ID]
		rtBlob := base.blobs[snap.ID]
		maxDelta, changed, cells, w, h, grid := mfxfmt.DiffImage(rtBlob, fullBlob)
		report.Heatmaps = append(report.Heatmaps, Heatmap{
			Variant: snap.ID, Grid: grid, Width: w, Height: h,
			MaxDelta: maxDelta, Changed: changed, Cells: cells,
		})
	}
	report.ElapsedMs = time.Since(start).Milliseconds()
	return report, nil
}

func diffReport(report *Comparison, m errs.Mismatch) (*Comparison, error) {
	report.Equivalent = false
	report.FirstMismatch = &m
	return report, errs.Diff(m, "three-path comparison found first mismatch")
}

func sameStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func MarshalReport(r *Comparison) string {
	b, _ := json.MarshalIndent(r, "", "  ")
	return string(b)
}

var _ = domain.AlgorithmVersion
var _ = fmt.Sprintf
