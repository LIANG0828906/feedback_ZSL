package dataset

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"

	"qualitybench/internal/core"
	"qualitybench/internal/model"
)

type Dataset struct {
	Project   *model.Project
	Truth     *model.Truth
	Baseline  map[string]*core.Frame
	ProjectFP string
	TruthFP   string
}

type projectWire struct {
	SchemaVersion    *string      `json:"schema_version"`
	Seed             *int64       `json:"seed"`
	Width            *int         `json:"width"`
	Height           *int         `json:"height"`
	Channels         *int         `json:"channels"`
	BlockSize        *int         `json:"block_size"`
	AttributionShare *float64     `json:"attribution_share"`
	MetricTolerance  *float64     `json:"metric_tolerance"`
	AlgorithmVersion *string      `json:"algorithm_version"`
	Images           *[]imageWire `json:"images"`
	Chains           *[]chainWire `json:"chains"`
	Edits            []editWire   `json:"edits"`
}

type imageWire struct {
	ID        *string      `json:"id"`
	Frame     *int         `json:"frame"`
	Seed      *int64       `json:"seed"`
	Shapes    *[]shapeWire `json:"shapes"`
	Watermark *rectWire    `json:"watermark"`
	Details   []detailWire `json:"details"`
}

type shapeWire struct {
	ID    *string   `json:"id"`
	Kind  *string   `json:"kind"`
	BBox  *rectWire `json:"bbox"`
	Color *[3]uint8 `json:"color"`
}

type detailWire struct {
	BBox   *rectWire `json:"bbox"`
	Period *int      `json:"period"`
	Delta  *int      `json:"delta"`
}

type rectWire struct {
	X *int `json:"x"`
	Y *int `json:"y"`
	W *int `json:"w"`
	H *int `json:"h"`
}

type stepWire struct {
	Type     *string   `json:"type"`
	Strength *float64  `json:"strength"`
	Region   *rectWire `json:"region"`
}

type chainWire struct {
	ID      *string     `json:"id"`
	ImageID *string     `json:"image_id"`
	Steps   *[]stepWire `json:"steps"`
}

type editWire struct {
	Op       *string   `json:"op"`
	ImageID  *string   `json:"image_id"`
	Step     *int      `json:"step"`
	Strength *float64  `json:"strength"`
	Region   *rectWire `json:"region"`
	From     *int      `json:"from"`
	To       *int      `json:"to"`
	Version  *string   `json:"version"`
}

type truthWire struct {
	SchemaVersion        *string           `json:"schema_version"`
	ProjectFingerprint   *string           `json:"project_fingerprint"`
	AlgorithmVersion     *string           `json:"algorithm_version"`
	Seed                 *int64            `json:"seed"`
	Width                *int              `json:"width"`
	Height               *int              `json:"height"`
	Channels             *int              `json:"channels"`
	BlockSize            *int              `json:"block_size"`
	AttributionShare     *float64          `json:"attribution_share"`
	AttributionTolerance *float64          `json:"attribution_tolerance"`
	Images               *[]imageTruthWire `json:"images"`
}

type imageTruthWire struct {
	ID             *string           `json:"id"`
	Frame          *int              `json:"frame"`
	StepCount      *int              `json:"step_count"`
	Steps          *[]stepWire       `json:"steps"`
	BaselineSHA256 *string           `json:"baseline_sha256"`
	Shapes         *[]shapeWire      `json:"shapes"`
	Watermark      *rectWire         `json:"watermark"`
	Samples        *[]sampleWire     `json:"samples"`
	Blocks         *[]blockTruthWire `json:"blocks"`
}

type sampleWire struct {
	X         *float64  `json:"x"`
	Y         *float64  `json:"y"`
	Expected  *[3]uint8 `json:"expected"`
	Tolerance *int      `json:"tolerance"`
	Label     string    `json:"label"`
}

type blockTruthWire struct {
	X           *int                  `json:"x"`
	Y           *int                  `json:"y"`
	W           *int                  `json:"w"`
	H           *int                  `json:"h"`
	Metrics     *metricIntervalWire   `json:"metrics"`
	Attribution *attributionTruthWire `json:"attribution"`
}

type metricIntervalWire struct {
	NoiseVariance *[]float64 `json:"noise_variance"`
	EdgeSharpness *[]float64 `json:"edge_sharpness"`
	ChromaShift   *[]float64 `json:"chroma_shift"`
}

type attributionTruthWire struct {
	Step      *int     `json:"step"`
	Category  *string  `json:"category"`
	Share     *float64 `json:"share"`
	Tolerance *float64 `json:"tolerance"`
}

func Load(projectPath, truthPath string) (*Dataset, error) {
	project, projectFP, err := LoadProject(projectPath)
	if err != nil {
		return nil, err
	}
	truth, truthFP, err := LoadTruth(truthPath)
	if err != nil {
		return nil, err
	}
	if err := model.ValidateTruth(truth, project); err != nil {
		return nil, err
	}
	baseline := make(map[string]*core.Frame, len(project.Images))
	for _, spec := range project.Images {
		path := filepath.Join(filepath.Dir(projectPath), "baseline", spec.ID+".png")
		frame, err := core.DecodePNG(path)
		if err != nil {
			return nil, core.Schema("baseline."+spec.ID, "cannot load %s: %v", path, err)
		}
		if frame.Width != project.Width || frame.Height != project.Height || frame.Channels != project.Channels {
			return nil, core.Range("baseline."+spec.ID, "frame geometry %dx%dx%d does not match project",
				frame.Width, frame.Height, frame.Channels)
		}
		it, ok := truth.Image(spec.ID)
		if !ok {
			return nil, core.Schema("truth.images", "missing truth for %s", spec.ID)
		}
		if frame.Hash() != it.BaselineSHA256 {
			return nil, core.Range("truth.images."+spec.ID+".baseline_sha256",
				"baseline content hash mismatch")
		}
		baseline[spec.ID] = frame
	}
	return &Dataset{
		Project: project, Truth: truth, Baseline: baseline,
		ProjectFP: projectFP, TruthFP: truthFP,
	}, nil
}

func LoadProject(path string) (*model.Project, string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, "", core.Schema("project", "cannot read %s: %v", path, err)
	}
	var wire projectWire
	if err := json.Unmarshal(data, &wire); err != nil {
		return nil, "", core.Schema("project", "invalid JSON: %v", err)
	}
	p, err := projectFromWire(&wire)
	if err != nil {
		return nil, "", err
	}
	if err := model.ValidateProject(p); err != nil {
		return nil, "", err
	}
	fp, err := model.Fingerprint(p)
	if err != nil {
		return nil, "", core.Schema("project", "cannot fingerprint: %v", err)
	}
	return p, fp, nil
}

func LoadTruth(path string) (*model.Truth, string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, "", core.Schema("truth", "cannot read %s: %v", path, err)
	}
	var wire truthWire
	if err := json.Unmarshal(data, &wire); err != nil {
		return nil, "", core.Schema("truth", "invalid JSON: %v", err)
	}
	t, err := truthFromWire(&wire)
	if err != nil {
		return nil, "", err
	}
	fp, err := model.Fingerprint(t)
	if err != nil {
		return nil, "", core.Schema("truth", "cannot fingerprint: %v", err)
	}
	return t, fp, nil
}

func projectFromWire(w *projectWire) (*model.Project, error) {
	p := &model.Project{}
	var err error
	if p.SchemaVersion, err = reqString(w.SchemaVersion, "schema_version"); err != nil {
		return nil, err
	}
	if p.Seed, err = reqInt64(w.Seed, "seed"); err != nil {
		return nil, err
	}
	if p.Width, err = reqInt(w.Width, "width"); err != nil {
		return nil, err
	}
	if p.Height, err = reqInt(w.Height, "height"); err != nil {
		return nil, err
	}
	if p.Channels, err = reqInt(w.Channels, "channels"); err != nil {
		return nil, err
	}
	if p.BlockSize, err = reqInt(w.BlockSize, "block_size"); err != nil {
		return nil, err
	}
	if p.AttributionShare, err = reqFloat(w.AttributionShare, "attribution_share"); err != nil {
		return nil, err
	}
	if p.MetricTolerance, err = reqFloat(w.MetricTolerance, "metric_tolerance"); err != nil {
		return nil, err
	}
	if p.AlgorithmVersion, err = reqString(w.AlgorithmVersion, "algorithm_version"); err != nil {
		return nil, err
	}
	if w.Images == nil {
		return nil, core.Schema("images", "required field is missing")
	}
	for i, iw := range *w.Images {
		path := fmt.Sprintf("images[%d]", i)
		spec, err := imageFromWire(&iw, path)
		if err != nil {
			return nil, err
		}
		p.Images = append(p.Images, spec)
	}
	if w.Chains == nil {
		return nil, core.Schema("chains", "required field is missing")
	}
	for i, cw := range *w.Chains {
		path := fmt.Sprintf("chains[%d]", i)
		chain, err := chainFromWire(&cw, path)
		if err != nil {
			return nil, err
		}
		p.Chains = append(p.Chains, chain)
	}
	for i, ew := range w.Edits {
		path := fmt.Sprintf("edits[%d]", i)
		if ew.Op == nil {
			return nil, core.Schema(path+".op", "required field is missing")
		}
		edit := model.Edit{Op: *ew.Op}
		if ew.ImageID != nil {
			edit.ImageID = *ew.ImageID
		}
		if ew.Step != nil {
			edit.Step = *ew.Step
		}
		edit.Strength = ew.Strength
		if ew.Region != nil {
			r, err := rectFromWire(ew.Region, path+".region")
			if err != nil {
				return nil, err
			}
			edit.Region = &r
		}
		if ew.From != nil {
			edit.From = *ew.From
		}
		if ew.To != nil {
			edit.To = *ew.To
		}
		if ew.Version != nil {
			edit.Version = *ew.Version
		}
		p.Edits = append(p.Edits, edit)
	}
	return p, nil
}

func imageFromWire(w *imageWire, path string) (model.ImageSpec, error) {
	spec := model.ImageSpec{}
	var err error
	if spec.ID, err = reqString(w.ID, path+".id"); err != nil {
		return spec, err
	}
	if spec.Frame, err = reqInt(w.Frame, path+".frame"); err != nil {
		return spec, err
	}
	if spec.Seed, err = reqInt64(w.Seed, path+".seed"); err != nil {
		return spec, err
	}
	if w.Shapes == nil {
		return spec, core.Schema(path+".shapes", "required field is missing")
	}
	for i, sw := range *w.Shapes {
		sp := fmt.Sprintf("%s.shapes[%d]", path, i)
		shape, err := shapeFromWire(&sw, sp)
		if err != nil {
			return spec, err
		}
		spec.Shapes = append(spec.Shapes, shape)
	}
	if w.Watermark == nil {
		return spec, core.Schema(path+".watermark", "required field is missing")
	}
	if spec.Watermark, err = rectFromWire(w.Watermark, path+".watermark"); err != nil {
		return spec, err
	}
	for i, dw := range w.Details {
		dp := fmt.Sprintf("%s.details[%d]", path, i)
		if dw.BBox == nil {
			return spec, core.Schema(dp+".bbox", "required field is missing")
		}
		bbox, err := rectFromWire(dw.BBox, dp+".bbox")
		if err != nil {
			return spec, err
		}
		period, err := reqInt(dw.Period, dp+".period")
		if err != nil {
			return spec, err
		}
		delta, err := reqInt(dw.Delta, dp+".delta")
		if err != nil {
			return spec, err
		}
		spec.Details = append(spec.Details, model.Detail{BBox: bbox, Period: period, Delta: delta})
	}
	return spec, nil
}

func shapeFromWire(w *shapeWire, path string) (model.Shape, error) {
	out := model.Shape{}
	var err error
	if out.ID, err = reqString(w.ID, path+".id"); err != nil {
		return out, err
	}
	if out.Kind, err = reqString(w.Kind, path+".kind"); err != nil {
		return out, err
	}
	if w.BBox == nil {
		return out, core.Schema(path+".bbox", "required field is missing")
	}
	if out.BBox, err = rectFromWire(w.BBox, path+".bbox"); err != nil {
		return out, err
	}
	if w.Color == nil {
		return out, core.Schema(path+".color", "required field is missing")
	}
	out.Color = *w.Color
	return out, nil
}

func chainFromWire(w *chainWire, path string) (model.Chain, error) {
	out := model.Chain{}
	var err error
	if out.ID, err = reqString(w.ID, path+".id"); err != nil {
		return out, err
	}
	if out.ImageID, err = reqString(w.ImageID, path+".image_id"); err != nil {
		return out, err
	}
	if w.Steps == nil {
		return out, core.Schema(path+".steps", "required field is missing")
	}
	for i, sw := range *w.Steps {
		sp := fmt.Sprintf("%s.steps[%d]", path, i)
		if sw.Type == nil {
			return out, core.Schema(sp+".type", "required field is missing")
		}
		strength, err := reqFloat(sw.Strength, sp+".strength")
		if err != nil {
			return out, err
		}
		step := model.Step{Type: model.StepType(*sw.Type), Strength: strength}
		if sw.Region != nil {
			r, err := rectFromWire(sw.Region, sp+".region")
			if err != nil {
				return out, err
			}
			step.Region = &r
		}
		out.Steps = append(out.Steps, step)
	}
	return out, nil
}

func truthFromWire(w *truthWire) (*model.Truth, error) {
	t := &model.Truth{}
	var err error
	if t.SchemaVersion, err = reqString(w.SchemaVersion, "truth.schema_version"); err != nil {
		return nil, err
	}
	if t.ProjectFingerprint, err = reqString(w.ProjectFingerprint, "truth.project_fingerprint"); err != nil {
		return nil, err
	}
	if t.AlgorithmVersion, err = reqString(w.AlgorithmVersion, "truth.algorithm_version"); err != nil {
		return nil, err
	}
	if t.Seed, err = reqInt64(w.Seed, "truth.seed"); err != nil {
		return nil, err
	}
	if t.Width, err = reqInt(w.Width, "truth.width"); err != nil {
		return nil, err
	}
	if t.Height, err = reqInt(w.Height, "truth.height"); err != nil {
		return nil, err
	}
	if t.Channels, err = reqInt(w.Channels, "truth.channels"); err != nil {
		return nil, err
	}
	if t.BlockSize, err = reqInt(w.BlockSize, "truth.block_size"); err != nil {
		return nil, err
	}
	if t.AttributionShare, err = reqFloat(w.AttributionShare, "truth.attribution_share"); err != nil {
		return nil, err
	}
	if t.AttributionTolerance, err = reqFloat(w.AttributionTolerance, "truth.attribution_tolerance"); err != nil {
		return nil, err
	}
	if w.Images == nil {
		return nil, core.Schema("truth.images", "required field is missing")
	}
	for i, iw := range *w.Images {
		path := fmt.Sprintf("truth.images[%d]", i)
		image, err := imageTruthFromWire(&iw, path)
		if err != nil {
			return nil, err
		}
		t.Images = append(t.Images, image)
	}
	return t, nil
}

func imageTruthFromWire(w *imageTruthWire, path string) (model.ImageTruth, error) {
	out := model.ImageTruth{}
	var err error
	if out.ID, err = reqString(w.ID, path+".id"); err != nil {
		return out, err
	}
	if out.Frame, err = reqInt(w.Frame, path+".frame"); err != nil {
		return out, err
	}
	if out.StepCount, err = reqInt(w.StepCount, path+".step_count"); err != nil {
		return out, err
	}
	if w.Steps == nil {
		return out, core.Schema(path+".steps", "required field is missing")
	}
	for i, sw := range *w.Steps {
		sp := fmt.Sprintf("%s.steps[%d]", path, i)
		if sw.Type == nil {
			return out, core.Schema(sp+".type", "required field is missing")
		}
		strength, err := reqFloat(sw.Strength, sp+".strength")
		if err != nil {
			return out, err
		}
		step := model.Step{Type: model.StepType(*sw.Type), Strength: strength}
		if sw.Region != nil {
			r, err := rectFromWire(sw.Region, sp+".region")
			if err != nil {
				return out, err
			}
			step.Region = &r
		}
		out.Steps = append(out.Steps, step)
	}
	if out.BaselineSHA256, err = reqString(w.BaselineSHA256, path+".baseline_sha256"); err != nil {
		return out, err
	}
	if w.Shapes == nil {
		return out, core.Schema(path+".shapes", "required field is missing")
	}
	for i, sw := range *w.Shapes {
		shape, err := shapeFromWire(&sw, fmt.Sprintf("%s.shapes[%d]", path, i))
		if err != nil {
			return out, err
		}
		out.Shapes = append(out.Shapes, shape)
	}
	if w.Watermark == nil {
		return out, core.Schema(path+".watermark", "required field is missing")
	}
	if out.Watermark, err = rectFromWire(w.Watermark, path+".watermark"); err != nil {
		return out, err
	}
	if w.Samples == nil {
		return out, core.Schema(path+".samples", "required field is missing")
	}
	for i, sw := range *w.Samples {
		sp := fmt.Sprintf("%s.samples[%d]", path, i)
		x, err := reqFloat(sw.X, sp+".x")
		if err != nil {
			return out, err
		}
		y, err := reqFloat(sw.Y, sp+".y")
		if err != nil {
			return out, err
		}
		if sw.Expected == nil {
			return out, core.Schema(sp+".expected", "required field is missing")
		}
		tol, err := reqInt(sw.Tolerance, sp+".tolerance")
		if err != nil {
			return out, err
		}
		out.Samples = append(out.Samples, model.SampleTruth{
			X: x, Y: y, Expected: *sw.Expected, Tolerance: tol, Label: sw.Label,
		})
	}
	if w.Blocks == nil {
		return out, core.Schema(path+".blocks", "required field is missing")
	}
	for i, bw := range *w.Blocks {
		bp := fmt.Sprintf("%s.blocks[%d]", path, i)
		block, err := blockTruthFromWire(&bw, bp)
		if err != nil {
			return out, err
		}
		out.Blocks = append(out.Blocks, block)
	}
	return out, nil
}

func blockTruthFromWire(w *blockTruthWire, path string) (model.BlockTruth, error) {
	out := model.BlockTruth{}
	var err error
	if out.X, err = reqInt(w.X, path+".x"); err != nil {
		return out, err
	}
	if out.Y, err = reqInt(w.Y, path+".y"); err != nil {
		return out, err
	}
	if out.W, err = reqInt(w.W, path+".w"); err != nil {
		return out, err
	}
	if out.H, err = reqInt(w.H, path+".h"); err != nil {
		return out, err
	}
	if w.Metrics == nil {
		return out, core.Schema(path+".metrics", "required field is missing")
	}
	if w.Metrics.NoiseVariance == nil {
		return out, core.Schema(path+".metrics.noise_variance", "required field is missing")
	}
	if w.Metrics.EdgeSharpness == nil {
		return out, core.Schema(path+".metrics.edge_sharpness", "required field is missing")
	}
	if w.Metrics.ChromaShift == nil {
		return out, core.Schema(path+".metrics.chroma_shift", "required field is missing")
	}
	noise, err := pair2(*w.Metrics.NoiseVariance, path+".metrics.noise_variance")
	if err != nil {
		return out, err
	}
	edge, err := pair2(*w.Metrics.EdgeSharpness, path+".metrics.edge_sharpness")
	if err != nil {
		return out, err
	}
	chroma, err := pair2(*w.Metrics.ChromaShift, path+".metrics.chroma_shift")
	if err != nil {
		return out, err
	}
	out.Metrics = model.MetricInterval{NoiseVariance: noise, EdgeSharpness: edge, ChromaShift: chroma}
	if w.Attribution == nil {
		return out, core.Schema(path+".attribution", "required field is missing")
	}
	step, err := reqInt(w.Attribution.Step, path+".attribution.step")
	if err != nil {
		return out, err
	}
	category, err := reqString(w.Attribution.Category, path+".attribution.category")
	if err != nil {
		return out, err
	}
	share, err := reqFloat(w.Attribution.Share, path+".attribution.share")
	if err != nil {
		return out, err
	}
	tol, err := reqFloat(w.Attribution.Tolerance, path+".attribution.tolerance")
	if err != nil {
		return out, err
	}
	out.Attribution = model.AttributionTruth{
		Step: step, Category: category, Share: share, Tolerance: tol,
	}
	return out, nil
}

func rectFromWire(w *rectWire, path string) (core.Rect, error) {
	if w == nil {
		return core.Rect{}, core.Schema(path, "required field is missing")
	}
	x, err := reqInt(w.X, path+".x")
	if err != nil {
		return core.Rect{}, err
	}
	y, err := reqInt(w.Y, path+".y")
	if err != nil {
		return core.Rect{}, err
	}
	width, err := reqInt(w.W, path+".w")
	if err != nil {
		return core.Rect{}, err
	}
	height, err := reqInt(w.H, path+".h")
	if err != nil {
		return core.Rect{}, err
	}
	return core.Rect{X: x, Y: y, W: width, H: height}, nil
}

func pair2(v []float64, path string) ([2]float64, error) {
	if len(v) != 2 {
		return [2]float64{}, core.Schema(path, "expected a two-element interval, got %d values", len(v))
	}
	if !model.IsFinite(v[0]) || !model.IsFinite(v[1]) {
		return [2]float64{}, core.Range(path, "interval bounds must be finite")
	}
	return [2]float64{v[0], v[1]}, nil
}

func reqString(v *string, path string) (string, error) {
	if v == nil || strings.TrimSpace(*v) == "" {
		return "", core.Schema(path, "required field is missing")
	}
	return *v, nil
}

func reqInt(v *int, path string) (int, error) {
	if v == nil {
		return 0, core.Schema(path, "required field is missing")
	}
	return *v, nil
}

func reqInt64(v *int64, path string) (int64, error) {
	if v == nil {
		return 0, core.Schema(path, "required field is missing")
	}
	return *v, nil
}

func reqFloat(v *float64, path string) (float64, error) {
	if v == nil {
		return 0, core.Schema(path, "required field is missing")
	}
	if math.IsNaN(*v) || math.IsInf(*v, 0) {
		return 0, core.Range(path, "value must be finite")
	}
	return *v, nil
}
