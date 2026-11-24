package loader

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"slices"

	"fixedsampler/internal/colorx"
	"fixedsampler/internal/fixedpoint"
	"fixedsampler/internal/hashutil"
	"fixedsampler/internal/model"
)

var DefaultSupportedDepths = []int{8, 10, 12, 16}

type planeGeom struct {
	Name       string
	Width      int
	Height     int
	SubsampleX int
	SubsampleY int
	BitDepth   int
}

func LoadProject(path string) (*model.Project, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, model.SchemaError("read project: %v", err)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, model.SchemaError("parse project JSON: %v", err)
	}
	for _, key := range []string{"schema_version", "project_id", "seed", "full_width", "full_height", "chroma_format", "value_scale", "planes", "config"} {
		if _, ok := raw[key]; !ok {
			return nil, model.SchemaError("missing required field %q", key)
		}
	}
	var p model.Project
	if err := json.Unmarshal(data, &p); err != nil {
		return nil, model.SchemaError("decode project: %v", err)
	}
	if err := ValidateProject(&p); err != nil {
		return nil, err
	}
	return &p, nil
}

func SaveProject(path string, p *model.Project) error {
	data, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

func ValidateProject(p *model.Project) error {
	if p == nil {
		return model.SchemaError("project is null")
	}
	if p.SchemaVersion == "" {
		return model.SchemaError("schema_version is required")
	}
	if p.ProjectID == "" {
		return model.SchemaError("project_id is required")
	}
	if p.FullWidth <= 0 || p.FullHeight <= 0 {
		return model.SchemaError("full_width and full_height must be positive")
	}
	if p.ValueScale == 0 {
		return model.SchemaError("value_scale is required")
	}
	if p.ValueScale < fixedpoint.MinScale || p.ValueScale > fixedpoint.MaxScale {
		return model.KernelError(0, "project value_scale %d out of range [%d,%d]", p.ValueScale, fixedpoint.MinScale, fixedpoint.MaxScale)
	}
	if len(p.SupportedDepths) == 0 {
		p.SupportedDepths = append([]int(nil), DefaultSupportedDepths...)
	}
	switch p.ChromaFormat {
	case "444", "422", "420", "400":
	default:
		return model.SchemaError("unsupported chroma_format %q", p.ChromaFormat)
	}
	if len(p.Planes) == 0 {
		return model.SchemaError("at least one plane is required")
	}

	geoms := make(map[string]planeGeom, len(p.Planes))
	seenNames := map[string]bool{}
	lumaCount := 0
	for _, plane := range p.Planes {
		if plane == nil {
			return model.SchemaError("plane must not be null")
		}
		if plane.Name == "" {
			return model.SchemaError("plane name is required")
		}
		if seenNames[plane.Name] {
			return model.SchemaError("duplicate plane name %q", plane.Name)
		}
		seenNames[plane.Name] = true
		if plane.Width <= 0 || plane.Height <= 0 {
			return model.SchemaError("plane %q dimensions must be positive", plane.Name)
		}
		if plane.SubsampleX <= 0 || plane.SubsampleY <= 0 {
			return model.SchemaError("plane %q subsample_x/subsample_y are required and positive", plane.Name)
		}
		if plane.BitDepth <= 0 {
			return model.SchemaError("plane %q bit_depth is required", plane.Name)
		}
		if !slices.Contains(p.SupportedDepths, plane.BitDepth) {
			return model.SchemaError("plane %q bit_depth %d not in supported depths %v", plane.Name, plane.BitDepth, p.SupportedDepths)
		}
		if len(plane.Values) != plane.Width*plane.Height {
			return model.SchemaError("plane %q has %d values but dimensions require %d", plane.Name, len(plane.Values), plane.Width*plane.Height)
		}
		expectedSX, expectedSY := 1, 1
		switch plane.Role {
		case "luma":
			lumaCount++
			if plane.Width != p.FullWidth || plane.Height != p.FullHeight {
				return model.DimError(plane.Name, 0, "luma plane must match full texture dimensions %dx%d", p.FullWidth, p.FullHeight)
			}
		case "chroma":
			switch p.ChromaFormat {
			case "444":
				expectedSX, expectedSY = 1, 1
			case "422":
				expectedSX, expectedSY = 2, 1
			case "420":
				expectedSX, expectedSY = 2, 2
			case "400":
				return model.DimError(plane.Name, 0, "chroma plane is not compatible with chroma format 400")
			}
		case "color", "alpha":
			expectedSX, expectedSY = 1, 1
		default:
			return model.SchemaError("plane %q has unsupported role %q", plane.Name, plane.Role)
		}
		if plane.SubsampleX != expectedSX || plane.SubsampleY != expectedSY {
			return model.DimError(plane.Name, 0, "subsample declaration (%d,%d) does not match chroma %s expected (%d,%d)",
				plane.SubsampleX, plane.SubsampleY, p.ChromaFormat, expectedSX, expectedSY)
		}
		expectedW := p.FullWidth / plane.SubsampleX
		expectedH := p.FullHeight / plane.SubsampleY
		if p.FullWidth%plane.SubsampleX != 0 || p.FullHeight%plane.SubsampleY != 0 || plane.Width != expectedW || plane.Height != expectedH {
			return model.DimError(plane.Name, 0, "plane dimensions %dx%d are incompatible with chroma %s/subsample (%d,%d), expected %dx%d",
				plane.Width, plane.Height, p.ChromaFormat, plane.SubsampleX, plane.SubsampleY, expectedW, expectedH)
		}
		geoms[plane.Name] = planeGeom{
			Name: plane.Name, Width: plane.Width, Height: plane.Height,
			SubsampleX: plane.SubsampleX, SubsampleY: plane.SubsampleY, BitDepth: plane.BitDepth,
		}
	}
	if lumaCount == 0 && p.ChromaFormat != "400" {
		return model.SchemaError("at least one luma plane is required")
	}
	if len(p.Config) == 0 {
		return model.SchemaError("config sequence must not be empty")
	}

	for i := range p.Config {
		stage := &p.Config[i]
		if stage.Seq == 0 {
			return model.SchemaError("config entry at position %d is missing seq", i+1)
		}
		if stage.Seq != i+1 {
			return model.SchemaError("config sequence ordinal %d at position %d is not continuous", stage.Seq, i+1)
		}
		if stage.ID == "" {
			stage.ID = fmt.Sprintf("stage-%d", stage.Seq)
		}
		if stage.Type == "" {
			return model.SchemaError("config %d type is required", stage.Seq)
		}
	}
	return validateConfigGeometry(p, geoms)
}

func validateConfigGeometry(p *model.Project, geoms map[string]planeGeom) error {
	for i := range p.Config {
		stage := &p.Config[i]
		switch stage.Type {
		case "resample":
			if stage.Kernel == "" {
				return model.SchemaError("config %d resample.kernel is required", stage.Seq)
			}
			switch stage.Kernel {
			case "nearest", "box":
			default:
				return model.KernelError(stage.Seq, "unknown resampling kernel %q", stage.Kernel)
			}
			if stage.Factor <= 0 && stage.FactorX <= 0 {
				return model.SchemaError("config %d resample.factor is required and positive", stage.Seq)
			}
			if stage.Factor > 0 {
				stage.FactorX = stage.Factor
				stage.FactorY = stage.Factor
			}
			if stage.FactorY <= 0 {
				stage.FactorY = stage.FactorX
			}
			if stage.FactorX <= 0 || stage.FactorY <= 0 {
				return model.SchemaError("config %d has invalid factor_x/factor_y", stage.Seq)
			}
			if stage.FixedScale == 0 {
				return model.SchemaError("config %d resample.fixed_scale is required", stage.Seq)
			}
			if stage.FixedScale < fixedpoint.MinScale || stage.FixedScale > fixedpoint.MaxScale {
				return model.KernelError(stage.Seq, "fixed_scale %d out of declared range [%d,%d]", stage.FixedScale, fixedpoint.MinScale, fixedpoint.MaxScale)
			}
			targets := stage.Planes
			if len(targets) == 0 {
				targets = sortedGeomNames(geoms)
				stage.Planes = append([]string(nil), targets...)
			}
			for _, name := range targets {
				g, ok := geoms[name]
				if !ok {
					return model.SchemaError("config %d targets unknown plane %q", stage.Seq, name)
				}
				factor := stage.Factor
				if factor == 0 {
					factor = stage.FactorX
				}
				if g.Width%stage.FactorX != 0 || g.Height%stage.FactorY != 0 {
					return model.DimError(name, factor, "factor %d(x=%d,y=%d) does not divide plane %s current dimensions %dx%d",
						factor, stage.FactorX, stage.FactorY, name, g.Width, g.Height)
				}
				g.Width /= stage.FactorX
				g.Height /= stage.FactorY
				geoms[name] = g
			}
		case "color":
			if stage.Intent == "" || stage.SourceSpace == "" || stage.TargetSpace == "" {
				return model.SchemaError("config %d color intent, source_space and target_space are required", stage.Seq)
			}
			if !colorx.Reachable(stage.Intent, stage.SourceSpace, stage.TargetSpace) {
				return model.ConvError(stage.Seq, stage.SourceSpace, stage.TargetSpace, "conversion intent %q is not reachable", stage.Intent)
			}
			if stage.Matrix == nil {
				return model.SchemaError("config %d color.fixed_matrix is required", stage.Seq)
			}
			if stage.Matrix.Scale < fixedpoint.MinScale || stage.Matrix.Scale > fixedpoint.MaxScale {
				return model.SchemaError("config %d color matrix scale %d out of range [%d,%d]", stage.Seq, stage.Matrix.Scale, fixedpoint.MinScale, fixedpoint.MaxScale)
			}
			if len(stage.InputNames) != 3 || len(stage.OutputNames) != 3 {
				return model.SchemaError("config %d color requires exactly three input_names and output_names", stage.Seq)
			}
			var first planeGeom
			for j, name := range stage.InputNames {
				g, ok := geoms[name]
				if !ok {
					return model.SchemaError("config %d input plane %q is missing", stage.Seq, name)
				}
				if j == 0 {
					first = g
				} else if g.Width != first.Width || g.Height != first.Height {
					return model.DimError(name, 0, "color inputs must have equal dimensions; %q is %dx%d vs %dx%d", name, g.Width, g.Height, first.Width, first.Height)
				}
			}
			for _, name := range stage.OutputNames {
				if g, ok := geoms[name]; ok {
					if g.Width != first.Width || g.Height != first.Height {
						return model.DimError(name, 0, "color output %q has incompatible existing dimensions", name)
					}
				} else {
					g := first
					g.Name = name
					geoms[name] = g
				}
			}
		case "quantize":
			if stage.BitDepth == 0 {
				return model.SchemaError("config %d quantize.bit_depth is required", stage.Seq)
			}
			if !slices.Contains(p.SupportedDepths, stage.BitDepth) {
				return model.SchemaError("config %d bit_depth %d not in supported range/set %v", stage.Seq, stage.BitDepth, p.SupportedDepths)
			}
			targets := stage.Planes
			if len(targets) == 0 {
				targets = sortedGeomNames(geoms)
				stage.Planes = append([]string(nil), targets...)
			}
			for _, name := range targets {
				g, ok := geoms[name]
				if !ok {
					return model.SchemaError("config %d quantize targets unknown plane %q", stage.Seq, name)
				}
				g.BitDepth = stage.BitDepth
				geoms[name] = g
			}
		default:
			return model.SchemaError("config %d has unsupported type %q", stage.Seq, stage.Type)
		}
	}
	return nil
}

func sortedGeomNames(m map[string]planeGeom) []string {
	names := make([]string, 0, len(m))
	for name := range m {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

func ConfigFingerprint(p *model.Project) string {
	return hashutil.CanonicalHash(struct {
		ProjectID  string              `json:"project_id"`
		ValueScale int64               `json:"value_scale"`
		Depths     []int               `json:"supported_depths"`
		Config     []model.StageConfig `json:"config"`
	}{p.ProjectID, p.ValueScale, p.SupportedDepths, p.Config})
}

func LoadTruth(path string) (*model.TruthManifest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, model.SchemaError("read truth: %v", err)
	}
	var t model.TruthManifest
	if err := json.Unmarshal(data, &t); err != nil {
		return nil, model.SchemaError("decode truth: %v", err)
	}
	if t.ProjectID == "" || len(t.KnownPoints) == 0 {
		return nil, model.SchemaError("truth project_id and known_points are required")
	}
	want := TruthFingerprint(&t)
	if t.Fingerprint != "" && t.Fingerprint != want {
		return nil, &model.PipelineError{Code: model.CodeDiff, Message: fmt.Sprintf("truth fingerprint mismatch: manifest=%s computed=%s", t.Fingerprint, want)}
	}
	t.Fingerprint = want
	return &t, nil
}

func TruthFingerprint(t *model.TruthManifest) string {
	copyValue := *t
	copyValue.Fingerprint = ""
	return hashutil.CanonicalHash(copyValue)
}

func IsPipelineCode(err error, code string) bool {
	var pe *model.PipelineError
	if errors.As(err, &pe) {
		return pe.Code == code
	}
	return false
}
