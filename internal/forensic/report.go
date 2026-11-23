package forensic

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

type Report struct {
	Schema               string          `json:"schema"`
	GeneratedAt          string          `json:"generatedAt"`
	ImageID              string          `json:"imageId"`
	ManifestPath         string          `json:"manifestPath"`
	Width                int             `json:"width"`
	Height               int             `json:"height"`
	Channels             int             `json:"channels"`
	Params               Params          `json:"params"`
	ParamsVersion        string          `json:"paramsVersion"`
	AlgorithmVersion     string          `json:"algorithmVersion"`
	HeatmapKey           string          `json:"heatmapKey"`
	HeatmapSHA256        string          `json:"heatmapSha256"`
	Summary              Summary         `json:"summary"`
	Invalidations        []Invalidation  `json:"invalidations"`
	Reuses               []ReuseRecord   `json:"reuses"`
	QueueEvents          []QueueEvent    `json:"queueEvents"`
	Truth                []BlockTruth    `json:"truth"`
	TruthConfusion       TruthConfusion  `json:"truthConfusion"`
	TruthAgreement       float64         `json:"truthAgreement"`
	TruthWithinTolerance bool            `json:"truthWithinTolerance"`
	DependencyGraph      DependencyGraph `json:"dependencyGraph"`
	Comparison           *Comparison     `json:"comparison,omitempty"`
	CommitSequence       []int           `json:"commitSequence"`
	CommitLedger         []int           `json:"commitLedger"`
	SnapshotResetReason  string          `json:"snapshotResetReason,omitempty"`
	BaselineSHA256       string          `json:"baselineSha256"`
	SamplePrimitives     []Primitive     `json:"samplePrimitives"`
}

type ReportFiles struct {
	ReportJSON  string `json:"reportJson"`
	ReportCSV   string `json:"reportCsv"`
	HeatmapPNG  string `json:"heatmapPng"`
	OriginalPNG string `json:"originalPng"`
}

func WriteReport(dir string, img *Image, result *RunResult, comparison *Comparison) (ReportFiles, error) {
	if dir == "" {
		return ReportFiles{}, ParamError("reportDir", "report output directory is required")
	}
	if img == nil || result == nil {
		return ReportFiles{}, ParamError("report", "image and run result are required")
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return ReportFiles{}, DecodeError("create report directory", err)
	}
	report := Report{
		Schema: "forensiclab.report/v1", GeneratedAt: time.Now().UTC().Format(time.RFC3339Nano),
		ImageID: img.ID, ManifestPath: img.ManifestPath, Width: img.Width, Height: img.Height,
		Channels: img.Channels, Params: result.Params, ParamsVersion: result.ParamsVersion,
		AlgorithmVersion: result.AlgorithmVersion, HeatmapKey: result.HeatmapKey,
		HeatmapSHA256: result.HeatmapSHA256, Summary: result.Summary,
		Invalidations: result.Invalidations, Reuses: result.Reuses, QueueEvents: result.QueueEvents,
		Truth: result.Truth, TruthConfusion: result.TruthConfusion, TruthAgreement: result.TruthAgreement,
		TruthWithinTolerance: result.TruthWithinTolerance, DependencyGraph: result.DependencyGraph,
		Comparison: comparison, CommitSequence: result.CommitSequence, CommitLedger: result.CommitLedger,
		SnapshotResetReason: result.SnapshotResetReason, BaselineSHA256: img.Entry.BaselineSHA256,
		SamplePrimitives: img.Entry.Primitives,
	}
	data, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return ReportFiles{}, DecodeError("encode forensic report", err)
	}
	data = append(data, '\n')
	files := ReportFiles{
		ReportJSON:  filepath.Join(dir, img.ID+"_report.json"),
		ReportCSV:   filepath.Join(dir, img.ID+"_blocks.csv"),
		HeatmapPNG:  filepath.Join(dir, img.ID+"_heatmap.png"),
		OriginalPNG: filepath.Join(dir, img.ID+"_original.png"),
	}
	if err := atomicWrite(files.ReportJSON, data, 0o644); err != nil {
		return files, DecodeError("write forensic report", err)
	}
	if err := atomicWrite(files.HeatmapPNG, result.HeatmapPNG, 0o644); err != nil {
		return files, DecodeError("write heatmap report asset", err)
	}
	if err := atomicWrite(files.OriginalPNG, img.ImagePNG, 0o644); err != nil {
		return files, DecodeError("write original report asset", err)
	}
	if err := writeBlockCSV(files.ReportCSV, result.States); err != nil {
		return files, err
	}
	return files, nil
}

func writeBlockCSV(path string, states []BlockState) error {
	f, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return DecodeError("create block report", err)
	}
	name := f.Name()
	defer os.Remove(name)
	w := csv.NewWriter(f)
	header := []string{
		"block_index", "x0", "y0", "x1", "y1", "content_digest", "content_hash", "fingerprint",
		"noise_variance", "noise_rms", "residual_coverage", "edge_consistency", "contrast", "tamper_score", "tampered",
		"params_version", "algorithm_version", "commit_seq",
	}
	if err := w.Write(header); err != nil {
		f.Close()
		return DecodeError("write block report header", err)
	}
	for _, s := range states {
		row := []string{
			strconv.Itoa(s.BlockIndex), strconv.Itoa(s.X0), strconv.Itoa(s.Y0),
			strconv.Itoa(s.X1), strconv.Itoa(s.Y1), s.ContentDigest, s.ContentHash, s.Fingerprint,
			strconv.FormatFloat(s.Stats.NoiseVariance, 'g', 17, 64),
			strconv.FormatFloat(s.Stats.NoiseRMS, 'g', 17, 64),
			strconv.FormatFloat(s.Stats.ResidualCoverage, 'g', 17, 64),
			strconv.FormatFloat(s.Stats.EdgeConsistency, 'g', 17, 64),
			strconv.FormatFloat(s.Stats.Contrast, 'g', 17, 64),
			strconv.FormatFloat(s.Stats.TamperScore, 'g', 17, 64),
			strconv.FormatBool(s.Tampered), s.ParamsVersion, s.AlgorithmVersion,
			strconv.FormatInt(s.CommitSeq, 10),
		}
		if err := w.Write(row); err != nil {
			f.Close()
			return DecodeError("write block report row", err)
		}
	}
	w.Flush()
	if err := w.Error(); err != nil {
		f.Close()
		return DecodeError("flush block report", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return DecodeError("sync block report", err)
	}
	if err := f.Close(); err != nil {
		return DecodeError("close block report", err)
	}
	if err := os.Rename(name, path); err != nil {
		return DecodeError("replace block report", err)
	}
	return nil
}

func (r Report) String() string {
	return fmt.Sprintf("report image=%s blocks=%d tampered=%d agreement=%.3f heatmap=%s",
		r.ImageID, r.Summary.TotalBlocks, r.Summary.TamperedBlocks, r.TruthAgreement, r.HeatmapSHA256)
}
