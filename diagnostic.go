package strokelab

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html/template"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type SampleManifest struct {
	Name          string        `json:"name"`
	Version       string        `json:"version"`
	ProjectID     string        `json:"project_id"`
	Seed          int64         `json:"seed"`
	Width         int           `json:"width"`
	Height        int           `json:"height"`
	BlockSize     int           `json:"block_size"`
	OrderVersion  string        `json:"order_version"`
	SourceHash    string        `json:"source_hash"`
	CandidateHash string        `json:"candidate_hash"`
	TruthHash     string        `json:"truth_hash"`
	Strokes       int           `json:"strokes"`
	Export        *ExportReport `json:"export,omitempty"`
}

type SuiteManifest struct {
	Seed    int64            `json:"seed"`
	Created string           `json:"created"`
	Samples []SampleManifest `json:"samples"`
}

func WriteSampleDir(dir string, sample *Sample) (*SampleManifest, error) {
	if sample == nil || sample.Project == nil {
		return nil, coded(CodeVersion, "sample is nil")
	}
	if err := os.MkdirAll(filepath.Join(dir, "assets"), 0o755); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(dir, "project.json"), sample.VersionJSON, 0o644); err != nil {
		return nil, err
	}
	if sample.Icon != nil {
		if err := writePNGImage(filepath.Join(dir, "assets", "icon.png"), sample.Icon); err != nil {
			return nil, err
		}
	}
	ordered, err := OrderStrokes(sample.Project.Strokes)
	if err != nil {
		return nil, err
	}
	manifest := &SampleManifest{
		Name: sample.Name, Version: sample.Version, ProjectID: sample.Project.ID,
		Seed: sample.Project.Seed, Width: sample.Project.Width, Height: sample.Project.Height,
		BlockSize: sample.Project.BlockSize, OrderVersion: OrderVersion(ordered),
		SourceHash:    digestBytes(sample.VersionJSON),
		CandidateHash: digestBytes(sample.Project.CandidateMask),
		TruthHash:     digestBytes(sample.Project.Truth.ReferenceMask),
		Strokes:       len(sample.Project.Strokes), Export: sample.ExportReport,
	}
	if err := writeMaskPNG(filepath.Join(dir, "assets", "candidate.png"),
		sample.Project.CandidateMask, sample.Project.Width, sample.Project.Height); err != nil {
		return nil, err
	}
	if err := writeMaskPNG(filepath.Join(dir, "assets", "composite.png"),
		sample.CompositeMask, sample.Project.Width, sample.Project.Height); err != nil {
		return nil, err
	}
	if err := writeMaskPNG(filepath.Join(dir, "assets", "truth.png"),
		sample.Project.Truth.ReferenceMask, sample.Project.Width, sample.Project.Height); err != nil {
		return nil, err
	}
	man, _ := json.MarshalIndent(manifest, "", "  ")
	if err := os.WriteFile(filepath.Join(dir, "sample.json"), append(man, '\n'), 0o644); err != nil {
		return nil, err
	}
	return manifest, nil
}

func WriteSuite(root string, suite *Suite) (*SuiteManifest, error) {
	manifest := &SuiteManifest{Seed: suite.Seed, Created: time.Now().UTC().Format(time.RFC3339)}
	for _, sample := range suite.Samples {
		dir := filepath.Join(root, sample.Name)
		m, err := WriteSampleDir(dir, sample)
		if err != nil {
			return nil, err
		}
		manifest.Samples = append(manifest.Samples, *m)
	}
	data, _ := json.MarshalIndent(manifest, "", "  ")
	if err := os.WriteFile(filepath.Join(root, "suite.json"), append(data, '\n'), 0o644); err != nil {
		return nil, err
	}
	return manifest, nil
}

func LoadSampleDir(dir string) (*Sample, error) {
	data, err := os.ReadFile(filepath.Join(dir, "project.json"))
	if err != nil {
		return nil, withDetails(CodeVersion, "read project.json: %v", nil, err)
	}
	p, err := LoadProjectJSON(data)
	if err != nil {
		return nil, err
	}
	sample := &Sample{
		Name: p.Name, Version: p.SourceVersion, Project: p,
		VersionJSON: data, CandidateMask: append([]uint8(nil), p.CandidateMask...),
	}
	if iconFile, err := os.Open(filepath.Join(dir, "assets", "icon.png")); err == nil {
		defer iconFile.Close()
		if img, err := png.Decode(iconFile); err == nil {
			if rgba, ok := img.(*image.RGBA); ok {
				sample.Icon = rgba
			} else {
				b := img.Bounds()
				rgba := image.NewRGBA(image.Rect(0, 0, b.Dx(), b.Dy()))
				for y := b.Min.Y; y < b.Max.Y; y++ {
					for x := b.Min.X; x < b.Max.X; x++ {
						rgba.Set(x-b.Min.X, y-b.Min.Y, img.At(x, y))
					}
				}
				sample.Icon = rgba
			}
		}
	}
	return sample, nil
}

type DiagnosticStroke struct {
	StrokeID       string `json:"stroke_id"`
	Group          string `json:"group"`
	Stage          int    `json:"stage"`
	Seq            int    `json:"seq"`
	Order          int    `json:"order"`
	Start          Point  `json:"start"`
	End            Point  `json:"end"`
	Radius         int    `json:"radius"`
	Hardness       int    `json:"hardness_permille"`
	Weight         int    `json:"weight_pp10000"`
	NonzeroPixels  int    `json:"nonzero_pixels"`
	MeanAlphaMilli int    `json:"mean_alpha_milli"`
	Image          string `json:"image"`
}

type DiagnosticBlock struct {
	BlockID      int      `json:"block_id"`
	Rect         Rect     `json:"rect"`
	Status       string   `json:"status"`
	CacheKey     string   `json:"cache_key"`
	WinnerID     string   `json:"winner_id"`
	Participants []string `json:"participants"`
}

type DiagnosticReport struct {
	Format         string             `json:"format"`
	GeneratedAt    string             `json:"generated_at"`
	ProjectID      string             `json:"project_id"`
	Name           string             `json:"name"`
	SourceVersion  string             `json:"source_version"`
	Mode           RunMode            `json:"mode"`
	Width          int                `json:"width"`
	Height         int                `json:"height"`
	BlockSize      int                `json:"block_size"`
	CandidateHash  string             `json:"candidate_hash"`
	CompositeHash  string             `json:"composite_hash"`
	ReferenceHash  string             `json:"reference_hash"`
	OrderVersion   string             `json:"order_version"`
	Checkpoint     CheckpointStatus   `json:"checkpoint"`
	Topology       *TopologyReport    `json:"topology"`
	Attribution    *AttributionReport `json:"attribution"`
	Queue          []QueueRecord      `json:"queue"`
	Blocks         []DiagnosticBlock  `json:"blocks"`
	Strokes        []DiagnosticStroke `json:"strokes"`
	Invalidation   *InvalidationPlan  `json:"invalidation,omitempty"`
	PathComparison *PathComparison    `json:"path_comparison,omitempty"`
	Assets         map[string]string  `json:"assets"`
	DiffPreview    []PixelAttribution `json:"diff_preview"`
}

type diagTemplateData struct {
	Report       *DiagnosticReport
	QueuePreview []QueueRecord
	BlockRows    []DiagnosticBlock
	Issues       []TopologyIssue
	DiffRows     []PixelAttribution
	Culprits     []CulpritSummary
	Invalidation []InvalidationEntry
}

func WriteEvaluationDiagnostics(outDir string, sample *Sample, eval *Evaluation,
	plan *InvalidationPlan, comparison *PathComparison) (string, *DiagnosticReport, error) {
	if sample == nil || sample.Project == nil || eval == nil {
		return "", nil, coded(CodeVersion, "sample and evaluation are required")
	}
	assetsDir := filepath.Join(outDir, "assets")
	if err := os.MkdirAll(assetsDir, 0o755); err != nil {
		return "", nil, err
	}
	report := &DiagnosticReport{
		Format:      DiagnosticFormat,
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
		ProjectID:   sample.Project.ID, Name: sample.Project.Name,
		SourceVersion: sample.Project.SourceVersion, Mode: eval.Mode,
		Width: sample.Project.Width, Height: sample.Project.Height,
		BlockSize:     sample.Project.BlockSize,
		CandidateHash: digestBytes(sample.Project.CandidateMask),
		CompositeHash: digestBytes(eval.Composite),
		ReferenceHash: digestBytes(sample.Project.Truth.ReferenceMask),
		OrderVersion:  eval.OrderVersion, Checkpoint: eval.Checkpoint,
		Topology: eval.Topology, Attribution: eval.Attribution,
		Queue: eval.Queue, Invalidation: plan, PathComparison: comparison,
		Assets: make(map[string]string),
	}
	if err := writeMaskPNG(filepath.Join(assetsDir, "candidate.png"),
		sample.Project.CandidateMask, sample.Project.Width, sample.Project.Height); err != nil {
		return "", nil, err
	}
	if err := writeMaskPNG(filepath.Join(assetsDir, "composite.png"),
		eval.Composite, sample.Project.Width, sample.Project.Height); err != nil {
		return "", nil, err
	}
	if err := writeMaskPNG(filepath.Join(assetsDir, "reference.png"),
		sample.Project.Truth.ReferenceMask, sample.Project.Width, sample.Project.Height); err != nil {
		return "", nil, err
	}
	if err := writeRawPNG(filepath.Join(assetsDir, "diff.png"),
		diffMaskPNG(eval.Composite, sample.Project.Truth.ReferenceMask,
			sample.Project.Width, sample.Project.Height),
		sample.Project.Width, sample.Project.Height); err != nil {
		return "", nil, err
	}
	report.Assets["candidate"] = "assets/candidate.png"
	report.Assets["composite"] = "assets/composite.png"
	report.Assets["reference"] = "assets/reference.png"
	report.Assets["diff"] = "assets/diff.png"
	if sample.Icon != nil {
		_ = writePNGImage(filepath.Join(assetsDir, "icon.png"), sample.Icon)
		report.Assets["icon"] = "assets/icon.png"
	}
	invalidated := make(map[int]bool)
	if plan != nil {
		for _, id := range plan.Affected {
			invalidated[id] = true
		}
	}
	for _, b := range eval.Blocks {
		status := "recomputed"
		if invalidated[b.BlockID] {
			status = "invalidated"
		} else if b.Reused {
			status = "reused"
		}
		report.Blocks = append(report.Blocks, DiagnosticBlock{
			BlockID: b.BlockID, Rect: b.Rect, Status: status,
			CacheKey: b.CacheKey, WinnerID: b.OrderTrace.WinnerID,
			Participants: b.OrderTrace.Participants,
		})
	}
	if err := writeBlockStatusPNG(filepath.Join(assetsDir, "blocks.png"), report.Blocks,
		sample.Project.Width, sample.Project.Height, sample.Project.BlockSize); err != nil {
		return "", nil, err
	}
	report.Assets["blocks"] = "assets/blocks.png"
	for i, os := range eval.OrderedStrokes {
		comp := RasterizeStroke(os.Stroke, sample.Project.Width, sample.Project.Height)
		nonzero := 0
		sum := int64(0)
		for _, a := range comp {
			if a != 0 {
				nonzero++
				sum += int64(a)
			}
		}
		mean := 0
		if nonzero > 0 {
			mean = int((sum*1000 + int64(nonzero)/2) / int64(nonzero))
		}
		name := fmt.Sprintf("stroke-%02d.png", i+1)
		if err := writeMaskPNG(filepath.Join(assetsDir, name), comp,
			sample.Project.Width, sample.Project.Height); err != nil {
			return "", nil, err
		}
		report.Strokes = append(report.Strokes, DiagnosticStroke{
			StrokeID: os.Stroke.ID, Group: os.Stroke.Group,
			Stage: os.Stroke.Stage, Seq: os.Stroke.Seq, Order: os.Position,
			Start: os.Stroke.Start, End: os.Stroke.End, Radius: os.Stroke.Radius,
			Hardness: os.Stroke.Hardness, Weight: os.Stroke.Weight,
			NonzeroPixels: nonzero, MeanAlphaMilli: mean,
			Image: "assets/" + name,
		})
	}
	if eval.Attribution != nil {
		n := len(eval.Attribution.Differences)
		if n > 200 {
			n = 200
		}
		report.DiffPreview = append([]PixelAttribution(nil), eval.Attribution.Differences[:n]...)
	}
	jsonData, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return "", nil, err
	}
	if err := os.WriteFile(filepath.Join(outDir, "evaluation.json"), append(jsonData, '\n'), 0o644); err != nil {
		return "", nil, err
	}
	reportPath := filepath.Join(outDir, "report.html")
	view := &diagTemplateData{Report: report}
	view.QueuePreview = report.Queue
	if len(view.QueuePreview) > 200 {
		view.QueuePreview = view.QueuePreview[:200]
	}
	view.BlockRows = report.Blocks
	if report.Topology != nil {
		view.Issues = report.Topology.Issues
	}
	view.DiffRows = report.DiffPreview
	if report.Attribution != nil {
		view.Culprits = report.Attribution.Culprits
	}
	if report.Invalidation != nil {
		view.Invalidation = report.Invalidation.Entries
	}
	tpl, err := template.New("report").Funcs(template.FuncMap{
		"short": func(s string) string {
			if len(s) > 16 {
				return s[:16]
			}
			return s
		},
		"join": strings.Join,
		"pct": func(v int) string {
			return fmt.Sprintf("%d.%02d%%", v/100, v%100)
		},
	}).Parse(diagnosticHTML)
	if err != nil {
		return "", nil, err
	}
	var buf bytes.Buffer
	if err := tpl.Execute(&buf, view); err != nil {
		return "", nil, err
	}
	if err := os.WriteFile(reportPath, buf.Bytes(), 0o644); err != nil {
		return "", nil, err
	}
	return reportPath, report, nil
}

func writeMaskPNG(path string, mask []uint8, width, height int) error {
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			v := mask[y*width+x]
			img.SetRGBA(x, y, color.RGBA{R: v, G: v, B: v, A: 255})
		}
	}
	return writePNGImage(path, img)
}

func writeRawPNG(path string, rgba []uint8, width, height int) error {
	if len(rgba) != width*height*4 {
		return coded(CodeVersion, "raw RGBA buffer length mismatch")
	}
	img := &image.RGBA{Pix: rgba, Stride: width * 4, Rect: image.Rect(0, 0, width, height)}
	return writePNGImage(path, img)
}

func writePNGImage(path string, img image.Image) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return err
	}
	return os.WriteFile(path, buf.Bytes(), 0o644)
}

func writeBlockStatusPNG(path string, blocks []DiagnosticBlock, width, height, blockSize int) error {
	const scale = 8
	img := image.NewRGBA(image.Rect(0, 0, width*scale, height*scale))
	for i := range img.Pix {
		img.Pix[i] = 24
	}
	for _, b := range blocks {
		fill := color.RGBA{R: 40, G: 135, B: 90, A: 255}
		switch b.Status {
		case "reused":
			fill = color.RGBA{R: 50, G: 100, B: 180, A: 255}
		case "invalidated":
			fill = color.RGBA{R: 190, G: 65, B: 55, A: 255}
		}
		for y := b.Rect.Y; y < b.Rect.Y+b.Rect.H; y++ {
			for x := b.Rect.X; x < b.Rect.X+b.Rect.W; x++ {
				for sy := 0; sy < scale; sy++ {
					for sx := 0; sx < scale; sx++ {
						img.SetRGBA(x*scale+sx, y*scale+sy, fill)
					}
				}
			}
		}
		for y := b.Rect.Y; y < b.Rect.Y+b.Rect.H; y++ {
			img.SetRGBA(b.Rect.X*scale, y*scale, color.RGBA{R: 250, G: 250, B: 250, A: 255})
			img.SetRGBA((b.Rect.X+b.Rect.W)*scale-1, y*scale, color.RGBA{R: 250, G: 250, B: 250, A: 255})
		}
		for x := b.Rect.X; x < b.Rect.X+b.Rect.W; x++ {
			img.SetRGBA(x*scale, b.Rect.Y*scale, color.RGBA{R: 250, G: 250, B: 250, A: 255})
			img.SetRGBA(x*scale, (b.Rect.Y+b.Rect.H)*scale-1, color.RGBA{R: 250, G: 250, B: 250, A: 255})
		}
	}
	return writePNGImage(path, img)
}

func SortedQueueRecords(records []QueueRecord) []QueueRecord {
	out := append([]QueueRecord(nil), records...)
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].StrokeOrder != out[j].StrokeOrder {
			return out[i].StrokeOrder < out[j].StrokeOrder
		}
		if out[i].BlockID != out[j].BlockID {
			return out[i].BlockID < out[j].BlockID
		}
		if out[i].Attempt != out[j].Attempt {
			return out[i].Attempt < out[j].Attempt
		}
		if out[i].Outcome != out[j].Outcome {
			return out[i].Outcome < out[j].Outcome
		}
		return out[i].QueueReason < out[j].QueueReason
	})
	return out
}

func HashHex(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

const diagnosticHTML = `<!doctype html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<title>StrokeLab diagnostic · {{.Report.Name}}</title>
<style>
:root{color-scheme:dark;--bg:#15181d;--panel:#20252d;--line:#39414d;--text:#e8edf4;--muted:#98a3b3;--ok:#58c58a;--warn:#e8b34b;--bad:#e06a5f;--blue:#6fa8ff}
*{box-sizing:border-box}body{margin:0;background:var(--bg);color:var(--text);font:14px/1.45 ui-monospace,SFMono-Regular,Menlo,monospace}
header{padding:24px 32px 18px;border-bottom:1px solid var(--line);background:#181c22}
h1{margin:0 0 6px;font-size:22px}h2{font-size:17px;margin:0 0 12px}h3{font-size:14px;margin:16px 0 8px;color:var(--muted)}
main{padding:24px 32px 60px;max-width:1500px;margin:auto}.grid{display:grid;gap:16px}.cols-2{grid-template-columns:repeat(2,minmax(0,1fr))}.cols-3{grid-template-columns:repeat(3,minmax(0,1fr))}
.panel{background:var(--panel);border:1px solid var(--line);border-radius:10px;padding:16px;margin-bottom:16px;overflow:auto}
.meta{display:flex;flex-wrap:wrap;gap:8px 22px;color:var(--muted)}.meta b{color:var(--text);font-weight:600}
.pill{display:inline-block;padding:2px 8px;border-radius:99px;border:1px solid var(--line);font-size:12px;color:var(--muted)}
.ok{color:var(--ok)}.bad{color:var(--bad)}.warn{color:var(--warn)}.blue{color:var(--blue)}
img.mask{width:100%;max-width:300px;image-rendering:pixelated;border:1px solid var(--line);border-radius:6px;background:#0b0d10}
.mask-wrap{text-align:center}.mask-wrap p{color:var(--muted);margin:6px 0 0}
table{width:100%;border-collapse:collapse;font-size:12px}th,td{padding:6px 8px;border-bottom:1px solid #2d343e;text-align:left;vertical-align:top}
th{color:var(--muted);font-weight:600;position:sticky;top:0;background:var(--panel)}tr:hover td{background:#252b34}
code{color:#c7d7ee}.scroll{max-height:420px;overflow:auto}.legend span{display:inline-flex;align-items:center;gap:6px;margin-right:18px}.dot{width:10px;height:10px;border-radius:2px;display:inline-block}
ul{margin:6px 0 0;padding-left:20px}li{margin:3px 0}
</style>
</head>
<body>
<header>
<h1>笔画施工还原诊断 · {{.Report.Name}}</h1>
<div class="meta">
<span>project <b>{{.Report.ProjectID}}</b></span>
<span>version <b>{{.Report.SourceVersion}}</b></span>
<span>mode <b>{{.Report.Mode}}</b></span>
<span>canvas <b>{{.Report.Width}}×{{.Report.Height}}</b></span>
<span>block <b>{{.Report.BlockSize}}</b></span>
<span>generated <b>{{.Report.GeneratedAt}}</b></span>
</div>
</header>
<main>
<section class="panel">
<h2>路径与完整性</h2>
<div class="meta">
<span>candidate hash <code>{{short .Report.CandidateHash}}</code></span>
<span>composite hash <code>{{short .Report.CompositeHash}}</code></span>
<span>reference hash <code>{{short .Report.ReferenceHash}}</code></span>
<span>order <code>{{short .Report.OrderVersion}}</code></span>
<span>checkpoint <b>{{if .Report.Checkpoint.Complete}}complete{{else}}partial{{end}}</b> ({{.Report.Checkpoint.CommittedStrokes}} strokes, {{.Report.Checkpoint.Records}} tx)</span>
</div>
{{if .Report.Topology}}<p>topology: <b class="{{if .Report.Topology.Passed}}ok{{else}}bad{{end}}">{{if .Report.Topology.Passed}}PASS{{else}}FAIL{{end}}</b>
 · differences <b>{{.Report.Topology.DifferencePixels}}</b> · issues <b>{{len .Report.Topology.Issues}}</b></p>{{end}}
{{if .Report.PathComparison}}<p>path comparison: <b class="{{if .Report.PathComparison.Equal}}ok{{else}}bad{{end}}">{{if .Report.PathComparison.Equal}}identical{{else}}mismatch{{end}}</b>
 ({{len .Report.PathComparison.Mismatches}} mismatch records)</p>{{end}}
</section>

<section class="panel">
<h2>候选掩码 / 积算掩码 / 参考真值 / 差异</h2>
<div class="grid cols-3">
<div class="mask-wrap"><img class="mask" src="{{.Report.Assets.candidate}}" alt="candidate"><p>candidate mask</p></div>
<div class="mask-wrap"><img class="mask" src="{{.Report.Assets.composite}}" alt="composite"><p>accumulated mask</p></div>
<div class="mask-wrap"><img class="mask" src="{{.Report.Assets.reference}}" alt="reference"><p>reference foreground</p></div>
</div>
<div class="grid cols-2" style="margin-top:16px">
<div class="mask-wrap"><img class="mask" src="{{.Report.Assets.diff}}" alt="diff"><p>difference: red=extra coverage, blue=missing coverage</p></div>
<div class="mask-wrap"><img class="mask" src="{{.Report.Assets.blocks}}" alt="blocks"><p>blocks</p>
<div class="legend"><span><i class="dot" style="background:#32845a"></i>recomputed</span><span><i class="dot" style="background:#3264b4"></i>reused</span><span><i class="dot" style="background:#be4137"></i>invalidated</span></div></div>
</div>
</section>

<section class="panel">
<h2>逐笔贡献（规范合成顺序）</h2>
<div class="scroll"><table>
<tr><th>order</th><th>stroke</th><th>image</th><th>group</th><th>stage / seq</th><th>geometry</th><th>radius</th><th>hardness‰</th><th>weight‱</th><th>nonzero px</th><th>mean α‰</th></tr>
{{range .Report.Strokes}}
<tr><td>{{.Order}}</td><td><code>{{.StrokeID}}</code></td><td><img src="{{.Image}}" style="width:72px;height:72px;image-rendering:pixelated;border:1px solid #39414d"></td><td>{{.Group}}</td><td>{{.Stage}} / {{.Seq}}</td><td>({{.Start.X}},{{.Start.Y}})→({{.End.X}},{{.End.Y}})</td><td>{{.Radius}}</td><td>{{.Hardness}}</td><td>{{.Weight}}</td><td>{{.NonzeroPixels}}</td><td>{{.MeanAlphaMilli}}</td></tr>
{{end}}
</table></div>
</section>

<div class="grid cols-2">
<section class="panel">
<h2>连通拓扑</h2>
{{if .Report.Topology}}
<div class="scroll"><table><tr><th>id</th><th>area</th><th>centroid‰</th><th>bbox</th><th>holes</th><th>ring‰</th></tr>
{{range .Report.Topology.Actual}}<tr><td>{{.ID}}</td><td>{{.Area}}</td><td>{{.CentroidXMilli}},{{.CentroidYMilli}}</td><td>{{.BBox.X}},{{.BBox.Y}},{{.BBox.W}}×{{.BBox.H}}</td><td>{{.Holes}}</td><td>{{.RingWidthMilli}}</td></tr>{{end}}
</table></div>
{{if .Issues}}<h3>Issues</h3><ul>{{range .Issues}}<li class="bad">domain {{.DomainID}} / actual {{.ActualID}} · {{.Metric}} expected {{.Expected}} actual {{.Actual}} limit {{.Limit}}</li>{{end}}</ul>{{end}}
{{end}}
</section>
<section class="panel">
<h2>偏差归因</h2>
{{if .Culprits}}<table><tr><th>stroke</th><th>order</th><th>pixels</th><th>share sum‱</th></tr>
{{range .Culprits}}<tr><td><code>{{.StrokeID}}</code></td><td>{{.StrokeSeq}}</td><td>{{.Pixels}}</td><td>{{.ShareTotal}}</td></tr>{{end}}</table>{{else}}<p class="ok">no differing pixels</p>{{end}}
{{if .DiffRows}}<div class="scroll"><table><tr><th>pixel</th><th>block</th><th>actual/ref</th><th>first</th><th>primary</th><th>share</th><th>reason</th></tr>
{{range .DiffRows}}<tr><td>{{.X}},{{.Y}}</td><td>{{.BlockID}}</td><td>{{.ActualAlpha}}/{{.ReferenceAlpha}}</td><td>{{if gt .FirstStrokeSeq 0}}#{{.FirstStrokeSeq}} {{.FirstStrokeID}}{{else}}base{{end}}</td><td>{{if gt .PrimaryStrokeSeq 0}}#{{.PrimaryStrokeSeq}} {{.PrimaryStrokeID}}{{else}}base{{end}}</td><td>{{pct .ContributionShare}}</td><td>{{.Reason}}</td></tr>{{end}}
</table></div>{{end}}
</section>
</div>

{{if .Report.Invalidation}}
<section class="panel">
<h2>失效传播</h2>
<p>{{.Report.Invalidation.StrokeID}} · fields {{join .Report.Invalidation.ChangedFields "+"}} · affected blocks {{len .Report.Invalidation.Affected}}</p>
<p class="muted">downstream: {{join .Report.Invalidation.Downstream ", "}}</p>
{{if .Report.Invalidation.Dependencies}}<p class="muted">dependency graph: {{len .Report.Invalidation.Dependencies.Nodes}} nodes / {{len .Report.Invalidation.Dependencies.Edges}} edges (stroke → order → block → topology → attribution/diagnostic)</p>{{end}}
<div class="scroll"><table><tr><th>block</th><th>stroke seq</th><th>reason</th><th>field</th></tr>
{{range .Invalidation}}<tr><td>{{.BlockID}}</td><td>{{.StrokeSeq}}</td><td>{{.Reason}}</td><td>{{.Field}}</td></tr>{{end}}
</table></div>
</section>
{{end}}

<section class="panel">
<h2>检查点状态</h2>
<div class="meta">
<span>path <code>{{.Report.Checkpoint.Path}}</code></span><span>records <b>{{.Report.Checkpoint.Records}}</b></span>
<span>bytes <b>{{.Report.Checkpoint.Bytes}}</b></span><span>tail ignored <b>{{.Report.Checkpoint.TailIgnored}}</b></span>
<span>last tx <b>{{.Report.Checkpoint.LastTx}}</b></span><span>identity <code>{{short .Report.Checkpoint.Identity}}</code></span>
</div>
</section>

<section class="panel">
<h2>分块状态</h2>
<div class="scroll"><table><tr><th>block</th><th>rect</th><th>status</th><th>winner</th><th>cache key</th><th>participants</th></tr>
{{range .BlockRows}}<tr><td>{{.BlockID}}</td><td>{{.Rect.X}},{{.Rect.Y}} {{.Rect.W}}×{{.Rect.H}}</td><td>{{.Status}}</td><td>{{.WinnerID}}</td><td><code>{{short .CacheKey}}</code></td><td>{{join .Participants ", "}}</td></tr>{{end}}
</table></div>
</section>

<section class="panel">
<h2>调度与排队明细</h2>
<div class="scroll"><table><tr><th>stroke order</th><th>stroke</th><th>block</th><th>attempt</th><th>queue reason</th><th>queue wait ms</th><th>budget reason</th><th>budget wait ms</th><th>outcome</th><th>error</th></tr>
{{range .QueuePreview}}<tr><td>{{.StrokeOrder}}</td><td>{{.StrokeID}}</td><td>{{.BlockID}}</td><td>{{.Attempt}}</td><td>{{.QueueReason}}</td><td>{{.QueueWaitMS}}</td><td>{{.BudgetReason}}</td><td>{{.BudgetWaitMS}}</td><td>{{.Outcome}}</td><td>{{.Error}}</td></tr>{{end}}
</table></div>
</section>
</main>
</body>
</html>
`
